package composition

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseComponent(t *testing.T) {
	component, err := ParseComponent(strings.NewReader(`
		# A component owns application interface declarations.
		docker example/filter:dev
		input acme.echo.v1.Echo upstream
		output acme.echo.v1.Echo filtered
	`))
	if err != nil {
		t.Fatal(err)
	}
	want := Component{
		Image: "example/filter:dev",
		Definition: Definition{
			Inputs: []Endpoint{{
				Name: "upstream", Service: "acme.echo.v1.Echo",
			}},
			Outputs: []Endpoint{{
				Name: "filtered", Service: "acme.echo.v1.Echo",
			}},
		},
	}
	if !reflect.DeepEqual(component, want) {
		t.Fatalf("component:\n got: %#v\nwant: %#v", component, want)
	}
}

func TestParseComponentRejectsInvalidDeclarations(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		message string
	}{
		{name: "missing docker", text: "output acme.v1.Service server", message: "docker image is missing"},
		{
			name:    "duplicate docker",
			text:    "docker one\ndocker two",
			message: "docker image is already declared",
		},
		{name: "malformed endpoint", text: "docker one\ninput acme.v1.Service", message: "expected input"},
		{name: "unknown directive", text: "docker one\nport 50051", message: "unknown directive"},
		{
			name:    "invalid service",
			text:    "docker one\noutput invalid server",
			message: "invalid protobuf service",
		},
		{
			name: "duplicate input endpoint",
			text: strings.Join([]string{
				"docker one",
				"input acme.v1.Service endpoint",
				"input acme.v1.Service endpoint",
			}, "\n"),
			message: "input endpoint name \"endpoint\" is declared more than once",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseComponent(strings.NewReader(test.text))
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("ParseComponent error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestLoadComponentAcceptsDirectoryAndExactManifestPath(t *testing.T) {
	root := t.TempDir()
	directory := writeComponent(t, root, "echo", `
		docker echo:dev
		output acme.echo.v1.Echo server
	`)
	manifest := filepath.Join(directory, componentFileName)

	fromDirectory, err := LoadComponent(directory)
	if err != nil {
		t.Fatal(err)
	}
	fromManifest, err := LoadComponent(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromDirectory, fromManifest) {
		t.Fatalf(
			"directory and exact manifest loaded differently:\n dir: %#v\nfile: %#v",
			fromDirectory, fromManifest,
		)
	}
}

func TestParseLoadsRelativeComponentDirectoriesAndFiles(t *testing.T) {
	root := t.TempDir()
	sourceDirectory := writeComponent(t, root, "components/source", `
		docker source:dev
		output acme.echo.v1.Echo server
	`)
	filterDirectory := writeComponent(t, root, "components/filter", `
		docker filter:dev
		input acme.echo.v1.Echo upstream
		output acme.echo.v1.Echo server
	`)
	systemText := `
		system demo
		component source components/source/component.dcomp
		component filter components/filter
		link filter.upstream source.server
	`

	spec, err := Parse(strings.NewReader(systemText), root)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "demo" || len(spec.Components) != 2 || len(spec.Links) != 1 {
		t.Fatalf("unexpected parsed system: %#v", spec)
	}
	if got, want := spec.Components[0].Path, filepath.Join(sourceDirectory, componentFileName); got != want {
		t.Fatalf("source path = %q, want %q", got, want)
	}
	if got, want := spec.Components[1].Path, filepath.Join(filterDirectory, componentFileName); got != want {
		t.Fatalf("filter path = %q, want %q", got, want)
	}

	systemPath := filepath.Join(root, "demo.dcomp")
	if err := os.WriteFile(systemPath, []byte(systemText), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(systemPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, spec) {
		t.Fatalf("Load and Parse differ:\nload: %#v\nparse: %#v", loaded, spec)
	}
}

func TestParseAcceptsAbsoluteComponentPaths(t *testing.T) {
	root := t.TempDir()
	componentDirectory := writeComponent(t, root, "external", "docker external:dev")
	spec, err := Parse(strings.NewReader(
		"system demo\ncomponent external "+componentDirectory,
	), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := spec.Components[0].Path, filepath.Join(componentDirectory, componentFileName); got != want {
		t.Fatalf("absolute component path = %q, want %q", got, want)
	}
}

func TestParseRuntimeDirectives(t *testing.T) {
	root := t.TempDir()
	writeComponent(t, root, "service", "docker service:dev")
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(root, "data-link")); err != nil {
		t.Fatal(err)
	}
	canonicalData, err := filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}

	spec, err := Parse(strings.NewReader(`
		system demo
		component service service
		bind service data-link /etc/service/config ro
		volume service state /var/lib/service rw
		args service serve --mode=production # ordinary whitespace arguments
		publish service udp ::1 15353 5353
		publish service tcp 127.0.0.1 0 8080
		egress service
	`), root)
	if err != nil {
		t.Fatal(err)
	}

	want := Runtime{
		Binds: []BindMount{{
			Source: canonicalData, Target: "/etc/service/config", ReadOnly: true,
		}},
		Volumes: []VolumeMount{{
			Name: "state", Target: "/var/lib/service", ReadOnly: false,
		}},
		Args: []string{"serve", "--mode=production"},
		Ports: []PublishedPort{
			{
				Protocol: "tcp", HostIP: "127.0.0.1",
				HostPort: 0, ContainerPort: 8080,
			},
			{
				Protocol: "udp", HostIP: "::1",
				HostPort: 15353, ContainerPort: 5353,
			},
		},
		ExternalEgress: true,
	}
	if got := spec.Components[0].Runtime; !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestParseRejectsMalformedRuntimeDirectives(t *testing.T) {
	root := t.TempDir()
	writeComponent(t, root, "service", "docker service:dev")
	writeComponent(t, root, "other", "docker other:dev")
	if err := os.Mkdir(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	header := "system demo\ncomponent service service\n"
	tests := []struct {
		name    string
		text    string
		message string
	}{
		{
			name: "component must precede runtime",
			text: strings.Join([]string{
				"system demo",
				"volume service state /state rw",
				"component service service",
			}, "\n"),
			message: "must be declared before volume",
		},
		{
			name:    "malformed bind",
			text:    header + "bind service data /data",
			message: "expected bind",
		},
		{
			name:    "missing bind source",
			text:    header + "bind service missing /data ro",
			message: "must exist",
		},
		{
			name:    "invalid mount mode",
			text:    header + "bind service data /data read-only",
			message: "must be ro or rw",
		},
		{
			name:    "relative target",
			text:    header + "volume service state data rw",
			message: "absolute container path",
		},
		{
			name:    "unclean target",
			text:    header + "volume service state /var/../data rw",
			message: "must be clean",
		},
		{
			name:    "root target",
			text:    header + "volume service state / rw",
			message: "container root",
		},
		{
			name: "overlapping targets",
			text: header +
				"bind service data /workspace ro\n" +
				"volume service state /workspace/state rw",
			message: "overlap",
		},
		{
			name:    "invalid volume name",
			text:    header + "volume service Bad /data rw",
			message: "invalid volume name",
		},
		{
			name: "duplicate volume",
			text: header +
				"volume service state /first rw\n" +
				"volume service state /second rw",
			message: "mounted more than once",
		},
		{
			name:    "args require values",
			text:    header + "args service",
			message: "expected args",
		},
		{
			name: "duplicate args",
			text: header +
				"args service first\n" +
				"args service second",
			message: "already declared",
		},
		{
			name:    "invalid protocol",
			text:    header + "publish service sctp 127.0.0.1 8080 80",
			message: "invalid published port protocol",
		},
		{
			name:    "host must be IP literal",
			text:    header + "publish service tcp localhost 8080 80",
			message: "not an IP literal",
		},
		{
			name:    "invalid host port token",
			text:    header + "publish service tcp 127.0.0.1 eighty 80",
			message: "invalid published host port",
		},
		{
			name:    "host port range",
			text:    header + "publish service tcp 127.0.0.1 -1 80",
			message: "outside 0..65535",
		},
		{
			name:    "container port range",
			text:    header + "publish service tcp 127.0.0.1 8080 65536",
			message: "outside 1..65535",
		},
		{
			name: "duplicate host tuple in one component",
			text: header +
				"publish service tcp 127.0.0.1 8080 80\n" +
				"publish service tcp 127.0.0.1 8080 81",
			message: "published more than once",
		},
		{
			name: "IPv4 wildcard overlaps specific address",
			text: header +
				"publish service tcp 0.0.0.0 8080 80\n" +
				"publish service tcp 127.0.0.1 8080 81",
			message: "published more than once",
		},
		{
			name: "duplicate host tuple across components",
			text: header +
				"component other other\n" +
				"publish service tcp 127.0.0.1 8080 80\n" +
				"publish other tcp 127.0.0.1 8080 81",
			message: "published by both",
		},
		{
			name: "IPv4 wildcard overlaps another component",
			text: header +
				"component other other\n" +
				"publish service udp 0.0.0.0 5353 5353\n" +
				"publish other udp 127.0.0.1 5353 5353",
			message: "published by both",
		},
		{
			name: "IPv6 wildcard overlaps specific address",
			text: header +
				"publish service tcp :: 8080 80\n" +
				"publish service tcp ::1 8080 81",
			message: "published more than once",
		},
		{
			name: "IPv6 wildcard overlaps another component",
			text: header +
				"component other other\n" +
				"publish service udp :: 5353 5353\n" +
				"publish other udp ::1 5353 5353",
			message: "published by both",
		},
		{
			name:    "publish requires externally routed bridge",
			text:    header + "publish service tcp 127.0.0.1 8080 80",
			message: "has no egress directive",
		},
		{
			name: "duplicate egress",
			text: header +
				"egress service\n" +
				"egress service",
			message: "already declared",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(test.text), root)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Parse error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestParseAllowsSamePortOnDifferentAddressFamilies(t *testing.T) {
	root := t.TempDir()
	writeComponent(t, root, "service", "docker service:dev")
	spec, err := Parse(strings.NewReader(strings.Join([]string{
		"system demo",
		"component service service",
		"publish service tcp 0.0.0.0 8080 80",
		"publish service tcp :: 8080 80",
		"egress service",
	}, "\n")), root)
	if err != nil {
		t.Fatalf("Parse rejected separate IPv4 and IPv6 bindings: %v", err)
	}
	if got, want := len(spec.Components[0].Runtime.Ports), 2; got != want {
		t.Fatalf("published port count = %d, want %d", got, want)
	}
}

func TestParseRejectsMalformedSystem(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		text    string
		base    string
		message string
	}{
		{
			name:    "missing base directory",
			text:    "system demo",
			base:    "",
			message: "base directory is missing",
		},
		{
			name:    "missing system",
			text:    "",
			base:    root,
			message: "system declaration is missing",
		},
		{
			name:    "duplicate system",
			text:    "system one\nsystem two",
			base:    root,
			message: "system is already declared",
		},
		{
			name:    "bad link input",
			text:    "system demo\nlink not-an-endpoint service.output",
			base:    root,
			message: "invalid input endpoint",
		},
		{
			name:    "unknown directive",
			text:    "system demo\nnetwork private",
			base:    root,
			message: "unknown directive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(test.text), test.base)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Parse error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestParseReportsComponentLoadAtSystemLine(t *testing.T) {
	_, err := Parse(strings.NewReader(`
		system demo
		component missing components/missing
	`), t.TempDir())
	if err == nil ||
		!strings.Contains(err.Error(), "line 3: load component \"missing\"") ||
		!strings.Contains(err.Error(), componentFileName) {
		t.Fatalf("unexpected component load error: %v", err)
	}
}

func writeComponent(t *testing.T, root, relative, contents string) string {
	t.Helper()
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, componentFileName),
		[]byte(contents),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	return directory
}
