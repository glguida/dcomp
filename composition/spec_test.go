package composition

import (
	"path/filepath"
	"strings"
	"testing"
)

const peerService = "acme.peer.v1.Peer"

func TestValidateAcceptsCyclesAndOutputFanout(t *testing.T) {
	spec := Spec{
		Name: "mesh",
		Components: []Instance{
			{
				Name: "one",
				Component: testComponent(
					"one:dev",
					[]Endpoint{{Name: "peer", Service: peerService}},
					[]Endpoint{{Name: "server", Service: peerService}},
				),
			},
			{
				Name: "two",
				Component: testComponent(
					"two:dev",
					[]Endpoint{{Name: "peer", Service: peerService}},
					[]Endpoint{{Name: "server", Service: peerService}},
				),
			},
			{
				Name: "observer",
				Component: testComponent(
					"observer:dev",
					[]Endpoint{{Name: "source", Service: peerService}},
					nil,
				),
			},
		},
		Links: []Link{
			testLink("one", "peer", "two", "server"),
			testLink("two", "peer", "one", "server"),
			testLink("observer", "source", "one", "server"),
		},
	}

	if err := Validate(spec); err != nil {
		t.Fatalf("Validate rejected a cycle with output fanout: %v", err)
	}
	resolved, err := Resolve(spec, map[string]ResolvedImage{
		"one":      testImage("sha256:one"),
		"two":      testImage("sha256:two"),
		"observer": testImage("sha256:observer"),
	})
	if err != nil {
		t.Fatal(err)
	}
	target, output, exists := resolved.LinkTarget("observer", "source")
	if !exists {
		t.Fatal("LinkTarget did not find observer.source")
	}
	if target.Name != "one" || output.Name != "server" || output.Service != peerService {
		t.Fatalf("unexpected link target: component=%#v output=%#v", target, output)
	}
	if target.Port != ComponentPort {
		t.Fatalf("resolved component port = %d, want %d", target.Port, ComponentPort)
	}
}

func TestValidateRequiresEveryInputExactlyOnceWithMatchingService(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Spec)
		message string
	}{
		{
			name:    "unlinked input",
			mutate:  func(spec *Spec) { spec.Links = nil },
			message: "input consumer.upstream is not linked",
		},
		{
			name: "duplicate input",
			mutate: func(spec *Spec) {
				spec.Links = append(spec.Links, spec.Links[0])
			},
			message: "input consumer.upstream is linked more than once",
		},
		{
			name: "service mismatch",
			mutate: func(spec *Spec) {
				spec.Components[1].Component.Definition.Outputs[0].Service = "acme.v1.Other"
			},
			message: "expects acme.v1.Wanted",
		},
		{
			name: "unknown input component",
			mutate: func(spec *Spec) {
				spec.Links[0].Input.Component = "missing"
			},
			message: "link input component \"missing\" is not declared",
		},
		{
			name: "unknown output component",
			mutate: func(spec *Spec) {
				spec.Links[0].Output.Component = "missing"
			},
			message: "link output component \"missing\" is not declared",
		},
		{
			name: "unknown input endpoint",
			mutate: func(spec *Spec) {
				spec.Links[0].Input.Endpoint = "missing"
			},
			message: "has no input endpoint \"missing\"",
		},
		{
			name: "unknown output endpoint",
			mutate: func(spec *Spec) {
				spec.Links[0].Output.Endpoint = "missing"
			},
			message: "has no output endpoint \"missing\"",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := linkedTestSpec()
			test.mutate(&spec)
			err := Validate(spec)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Validate error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestValidateRejectsInvalidSystemStructure(t *testing.T) {
	validComponent := Instance{
		Name:      "service",
		Component: testComponent("service:dev", nil, nil),
	}
	tests := []struct {
		name    string
		spec    Spec
		message string
	}{
		{
			name:    "invalid system name",
			spec:    Spec{Name: "Demo", Components: []Instance{validComponent}},
			message: "invalid system name",
		},
		{
			name:    "no components",
			spec:    Spec{Name: "demo"},
			message: "has no components",
		},
		{
			name: "invalid instance name",
			spec: Spec{
				Name: "demo",
				Components: []Instance{{
					Name:      "Service",
					Component: validComponent.Component,
				}},
			},
			message: "invalid component name",
		},
		{
			name: "duplicate instance",
			spec: Spec{
				Name:       "demo",
				Components: []Instance{validComponent, validComponent},
			},
			message: "declared more than once",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.spec)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Validate error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestValidateDefinitionRejectsInvalidAndDuplicateEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		definition Definition
		message    string
	}{
		{
			name: "invalid local name",
			definition: Definition{
				Inputs: []Endpoint{{Name: "UPSTREAM", Service: peerService}},
			},
			message: "invalid input endpoint name",
		},
		{
			name: "invalid protobuf service",
			definition: Definition{
				Outputs: []Endpoint{{Name: "server", Service: "not-a-service"}},
			},
			message: "invalid protobuf service",
		},
		{
			name: "duplicate input name",
			definition: Definition{
				Inputs: []Endpoint{
					{Name: "peer", Service: peerService},
					{Name: "peer", Service: peerService},
				},
			},
			message: "input endpoint name \"peer\" is declared more than once",
		},
		{
			name: "duplicate output name",
			definition: Definition{
				Outputs: []Endpoint{
					{Name: "peer", Service: peerService},
					{Name: "peer", Service: peerService},
				},
			},
			message: "output endpoint name \"peer\" is declared more than once",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateDefinition(test.definition)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("ValidateDefinition error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestValidateDefinitionAllowsSameInputAndOutputName(t *testing.T) {
	definition := Definition{
		Inputs:  []Endpoint{{Name: "peer", Service: peerService}},
		Outputs: []Endpoint{{Name: "peer", Service: peerService}},
	}
	if err := ValidateDefinition(definition); err != nil {
		t.Fatalf("ValidateDefinition rejected directional endpoint namespaces: %v", err)
	}
}

func TestResolvePreservesRuntimeChecks(t *testing.T) {
	tests := []struct {
		name    string
		images  map[string]ResolvedImage
		message string
	}{
		{
			name:    "image not resolved",
			images:  map[string]ResolvedImage{},
			message: "was not image-resolved",
		},
		{
			name: "empty immutable ID",
			images: map[string]ResolvedImage{
				"service": {HasHealthcheck: true},
			},
			message: "empty image ID",
		},
		{
			name: "missing healthcheck",
			images: map[string]ResolvedImage{
				"service": {ID: "sha256:service"},
			},
			message: "has no Docker HEALTHCHECK",
		},
		{
			name: "uncovered declared volume",
			images: map[string]ResolvedImage{
				"service": {
					ID: "sha256:service", HasHealthcheck: true,
					DeclaredVolumes: []string{"/var/lib/service"},
				},
			},
			message: "declares VOLUME target",
		},
	}

	spec := Spec{
		Name: "demo",
		Components: []Instance{{
			Name:      "service",
			Component: testComponent("service:dev", nil, nil),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Resolve(spec, test.images)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Resolve error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestValidateRuntimeRequiresCanonicalNonOverlappingResources(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	valid := Runtime{
		Binds: []BindMount{{
			Source: canonicalRoot, Target: "/workspace", ReadOnly: true,
		}},
		Volumes: []VolumeMount{{
			Name: "state", Target: "/state",
		}},
		Args: []string{"serve", "--mode=test"},
		Ports: []PublishedPort{{
			Protocol: "tcp", HostIP: "127.0.0.1",
			HostPort: 18080, ContainerPort: 8080,
		}},
	}
	if err := ValidateRuntime(valid); err != nil {
		t.Fatalf("ValidateRuntime rejected valid runtime: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Runtime)
		message string
	}{
		{
			name: "relative bind source",
			mutate: func(runtime *Runtime) {
				runtime.Binds[0].Source = "relative"
			},
			message: "must be absolute",
		},
		{
			name: "missing bind source",
			mutate: func(runtime *Runtime) {
				runtime.Binds[0].Source = filepath.Join(canonicalRoot, "missing")
			},
			message: "must exist",
		},
		{
			name: "overlapping mount",
			mutate: func(runtime *Runtime) {
				runtime.Volumes[0].Target = "/workspace/state"
			},
			message: "overlap",
		},
		{
			name: "noncanonical IP",
			mutate: func(runtime *Runtime) {
				runtime.Ports[0].HostIP = "0:0:0:0:0:0:0:1"
			},
			message: "not canonical",
		},
		{
			name: "duplicate host tuple",
			mutate: func(runtime *Runtime) {
				runtime.Ports = append(runtime.Ports, PublishedPort{
					Protocol: "tcp", HostIP: "127.0.0.1",
					HostPort: 18080, ContainerPort: 9090,
				})
			},
			message: "published more than once",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := cloneRuntime(valid)
			test.mutate(&runtime)
			err := ValidateRuntime(runtime)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("ValidateRuntime error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestDynamicPublishedPortsDoNotConflict(t *testing.T) {
	dynamic := PublishedPort{
		Protocol: "tcp", HostIP: "127.0.0.1",
		HostPort: 0, ContainerPort: 8080,
	}
	anotherDynamic := dynamic
	anotherDynamic.ContainerPort = 9090
	fixed := dynamic
	fixed.HostPort = 18080
	if publishedPortsConflict(dynamic, anotherDynamic) {
		t.Fatal("two Docker-allocated ports conflict")
	}
	if publishedPortsConflict(dynamic, fixed) {
		t.Fatal("a Docker-allocated port conflicts with a fixed port")
	}
	if !publishedPortsConflict(fixed, fixed) {
		t.Fatal("identical fixed ports do not conflict")
	}
}

func TestResolvePermitsOnlyExplicitlyCoveredImageVolumes(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		Name: "demo",
		Components: []Instance{{
			Name:      "service",
			Component: testComponent("service:dev", nil, nil),
			Runtime: Runtime{
				Binds: []BindMount{{
					Source: canonicalRoot, Target: "/etc/service", ReadOnly: true,
				}},
				Volumes: []VolumeMount{{
					Name: "state", Target: "/var/lib/service",
				}},
			},
		}},
	}
	image := testImage("sha256:service")
	image.DeclaredVolumes = []string{"/etc/service/", "/var/lib/service"}
	if _, err := Resolve(spec, map[string]ResolvedImage{"service": image}); err != nil {
		t.Fatalf("Resolve rejected explicitly covered image volumes: %v", err)
	}

	image.DeclaredVolumes = append(image.DeclaredVolumes, "/uncovered")
	_, err = Resolve(spec, map[string]ResolvedImage{"service": image})
	if err == nil || !strings.Contains(err.Error(), `VOLUME target "/uncovered"`) {
		t.Fatalf("Resolve uncovered volume error = %v", err)
	}
}

func TestRuntimeCanonicalizationAndDigestIdentity(t *testing.T) {
	firstSource := t.TempDir()
	secondSource := t.TempDir()
	var err error
	firstSource, err = filepath.EvalSymlinks(firstSource)
	if err != nil {
		t.Fatal(err)
	}
	secondSource, err = filepath.EvalSymlinks(secondSource)
	if err != nil {
		t.Fatal(err)
	}
	firstRuntime := Runtime{
		Binds: []BindMount{
			{Source: secondSource, Target: "/z-bind", ReadOnly: true},
			{Source: firstSource, Target: "/a-bind"},
		},
		Volumes: []VolumeMount{
			{Name: "z-state", Target: "/z-state"},
			{Name: "a-state", Target: "/a-state", ReadOnly: true},
		},
		Args: []string{"serve", "--mode=test"},
		Ports: []PublishedPort{
			{Protocol: "udp", HostIP: "::1", HostPort: 19000, ContainerPort: 9000},
			{Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 18000, ContainerPort: 8000},
		},
		ExternalEgress: true,
	}
	secondRuntime := Runtime{
		Binds:          []BindMount{firstRuntime.Binds[1], firstRuntime.Binds[0]},
		Volumes:        []VolumeMount{firstRuntime.Volumes[1], firstRuntime.Volumes[0]},
		Args:           append([]string(nil), firstRuntime.Args...),
		Ports:          []PublishedPort{firstRuntime.Ports[1], firstRuntime.Ports[0]},
		ExternalEgress: true,
	}
	makeSpec := func(runtime Runtime) Spec {
		return Spec{
			Name: "demo",
			Components: []Instance{{
				Name: "service", Path: "/source/component.dcomp",
				Component: testComponent("service:dev", nil, nil),
				Runtime:   runtime,
			}},
		}
	}
	images := map[string]ResolvedImage{"service": testImage("sha256:service")}
	firstResolved, err := Resolve(makeSpec(firstRuntime), images)
	if err != nil {
		t.Fatal(err)
	}
	secondResolved, err := Resolve(makeSpec(secondRuntime), images)
	if err != nil {
		t.Fatal(err)
	}
	if firstResolved.Digest != secondResolved.Digest {
		t.Fatalf("runtime declaration order changed system digest")
	}
	if firstResolved.Components[0].Digest != secondResolved.Components[0].Digest {
		t.Fatalf("runtime declaration order changed component digest")
	}
	if got := firstResolved.Components[0].Runtime.Binds[0].Target; got != "/a-bind" {
		t.Fatalf("resolved binds were not canonicalized: first target = %q", got)
	}
	if got := firstRuntime.Binds[0].Target; got != "/z-bind" {
		t.Fatalf("Resolve mutated source runtime: first target = %q", got)
	}

	changedRuntime := cloneRuntime(firstRuntime)
	changedRuntime.Args = []string{"--mode=test", "serve"}
	changed, err := Resolve(makeSpec(changedRuntime), images)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == firstResolved.Digest {
		t.Fatal("argument order did not change system digest")
	}
	if changed.Components[0].Digest == firstResolved.Components[0].Digest {
		t.Fatal("argument order did not change component digest")
	}
}

func TestComponentDigestCoversImageDefinitionAndRuntime(t *testing.T) {
	source := t.TempDir()
	var err error
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	makeSpec := func() Spec {
		return Spec{
			Name: "demo",
			Components: []Instance{{
				Name: "service",
				Component: testComponent(
					"service:dev", nil,
					[]Endpoint{{Name: "server", Service: "acme.v1.Service"}},
				),
				Runtime: Runtime{
					Binds: []BindMount{{
						Source: source, Target: "/config", ReadOnly: true,
					}},
					Volumes: []VolumeMount{{
						Name: "state", Target: "/state",
					}},
					Args:           []string{"serve"},
					ExternalEgress: true,
				},
			}},
		}
	}
	baseImages := map[string]ResolvedImage{"service": testImage("sha256:service")}
	baseline, err := Resolve(makeSpec(), baseImages)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Spec, map[string]ResolvedImage)
	}{
		{
			name: "image ID",
			mutate: func(_ *Spec, images map[string]ResolvedImage) {
				images["service"] = testImage("sha256:changed")
			},
		},
		{
			name: "definition",
			mutate: func(spec *Spec, _ map[string]ResolvedImage) {
				spec.Components[0].Component.Definition.Outputs[0].Service = "acme.v2.Service"
			},
		},
		{
			name: "bind mode",
			mutate: func(spec *Spec, _ map[string]ResolvedImage) {
				spec.Components[0].Runtime.Binds[0].ReadOnly = false
			},
		},
		{
			name: "volume mode",
			mutate: func(spec *Spec, _ map[string]ResolvedImage) {
				spec.Components[0].Runtime.Volumes[0].ReadOnly = true
			},
		},
		{
			name: "args",
			mutate: func(spec *Spec, _ map[string]ResolvedImage) {
				spec.Components[0].Runtime.Args[0] = "inspect"
			},
		},
		{
			name: "published port",
			mutate: func(spec *Spec, _ map[string]ResolvedImage) {
				spec.Components[0].Runtime.Ports = []PublishedPort{{
					Protocol: "tcp", HostIP: "127.0.0.1",
					HostPort: 18080, ContainerPort: 8080,
				}}
			},
		},
		{
			name: "egress",
			mutate: func(spec *Spec, _ map[string]ResolvedImage) {
				spec.Components[0].Runtime.ExternalEgress = false
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := makeSpec()
			images := map[string]ResolvedImage{"service": baseImages["service"]}
			test.mutate(&spec, images)
			changed, err := Resolve(spec, images)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Components[0].Digest == baseline.Components[0].Digest {
				t.Fatalf("%s did not change component digest", test.name)
			}
			if changed.Digest == baseline.Digest {
				t.Fatalf("%s did not change system digest", test.name)
			}
		})
	}
}

func TestComponentDigestTracksInboundTargetButNotOutgoingLinks(t *testing.T) {
	const service = "acme.v1.Service"
	components := []Instance{
		{
			Name: "consumer",
			Component: testComponent(
				"consumer:dev",
				[]Endpoint{{Name: "upstream", Service: service}},
				nil,
			),
		},
		{
			Name: "provider-a",
			Component: testComponent(
				"provider-a:dev", nil,
				[]Endpoint{{Name: "server", Service: service}},
			),
		},
		{
			Name: "provider-b",
			Component: testComponent(
				"provider-b:dev", nil,
				[]Endpoint{{Name: "server", Service: service}},
			),
		},
	}
	first := Spec{
		Name:       "demo",
		Components: components,
		Links: []Link{
			testLink("consumer", "upstream", "provider-a", "server"),
		},
	}
	second := Spec{
		Name:       "demo",
		Components: components,
		Links: []Link{
			testLink("consumer", "upstream", "provider-b", "server"),
		},
	}
	images := map[string]ResolvedImage{
		"consumer":   testImage("sha256:consumer"),
		"provider-a": testImage("sha256:provider-a"),
		"provider-b": testImage("sha256:provider-b"),
	}
	firstResolved, err := Resolve(first, images)
	if err != nil {
		t.Fatal(err)
	}
	secondResolved, err := Resolve(second, images)
	if err != nil {
		t.Fatal(err)
	}
	if firstResolved.Digest == secondResolved.Digest {
		t.Fatal("changed link did not change full system digest")
	}
	if resolvedComponent(t, firstResolved, "consumer").Digest ==
		resolvedComponent(t, secondResolved, "consumer").Digest {
		t.Fatal("changed inbound target component did not change consumer digest")
	}
	for _, provider := range []string{"provider-a", "provider-b"} {
		if resolvedComponent(t, firstResolved, provider).Digest !=
			resolvedComponent(t, secondResolved, provider).Digest {
			t.Fatalf("outgoing link changed %s component digest", provider)
		}
	}
}

func TestComponentDigestIgnoresInboundOutputEndpointOnSameTarget(t *testing.T) {
	const service = "acme.v1.Service"
	spec := Spec{
		Name: "demo",
		Components: []Instance{
			{
				Name: "consumer",
				Component: testComponent(
					"consumer:dev",
					[]Endpoint{{Name: "upstream", Service: service}},
					nil,
				),
			},
			{
				Name: "provider",
				Component: testComponent(
					"provider:dev", nil,
					[]Endpoint{
						{Name: "first", Service: service},
						{Name: "second", Service: service},
					},
				),
			},
		},
		Links: []Link{
			testLink("consumer", "upstream", "provider", "first"),
		},
	}
	images := map[string]ResolvedImage{
		"consumer": testImage("sha256:consumer"),
		"provider": testImage("sha256:provider"),
	}
	first, err := Resolve(spec, images)
	if err != nil {
		t.Fatal(err)
	}
	spec.Links[0].Output.Endpoint = "second"
	second, err := Resolve(spec, images)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("changed output endpoint did not change full system digest")
	}
	if resolvedComponent(t, first, "consumer").Digest !=
		resolvedComponent(t, second, "consumer").Digest {
		t.Fatal("output endpoint on same target changed consumer component digest")
	}
}

func TestDigestUsesDefinitionsAndImmutableImageIdentityNotSourcePathsOrTags(t *testing.T) {
	first := Spec{
		Name: "demo",
		Components: []Instance{{
			Name: "service", Path: "/first/component.dcomp",
			Component: testComponent(
				"image:first", nil,
				[]Endpoint{
					{Name: "zeta", Service: "acme.v1.Zeta"},
					{Name: "alpha", Service: "acme.v1.Alpha"},
				},
			),
		}},
	}
	second := Spec{
		Name: "demo",
		Components: []Instance{{
			Name: "service", Path: "/second/component.dcomp",
			Component: testComponent(
				"image:second", nil,
				[]Endpoint{
					{Name: "alpha", Service: "acme.v1.Alpha"},
					{Name: "zeta", Service: "acme.v1.Zeta"},
				},
			),
		}},
	}
	images := map[string]ResolvedImage{"service": testImage("sha256:same")}

	firstResolved, err := Resolve(first, images)
	if err != nil {
		t.Fatal(err)
	}
	secondResolved, err := Resolve(second, images)
	if err != nil {
		t.Fatal(err)
	}
	if firstResolved.Digest != secondResolved.Digest {
		t.Fatalf(
			"equivalent immutable systems produced different digests: %s != %s",
			firstResolved.Digest, secondResolved.Digest,
		)
	}
	if firstResolved.Components[0].Digest != secondResolved.Components[0].Digest {
		t.Fatalf(
			"mutable image references changed component digest: %s != %s",
			firstResolved.Components[0].Digest, secondResolved.Components[0].Digest,
		)
	}
	if firstResolved.Components[0].ImageRef != "image:first" {
		t.Fatalf(
			"digest computation mutated diagnostic image reference: %q",
			firstResolved.Components[0].ImageRef,
		)
	}
	if got := firstResolved.Components[0].Definition.Outputs[0].Name; got != "alpha" {
		t.Fatalf("resolved outputs were not canonicalized: first endpoint = %q", got)
	}
	if got := first.Components[0].Component.Definition.Outputs[0].Name; got != "zeta" {
		t.Fatalf("Resolve mutated source definition: first endpoint = %q", got)
	}

	changed := second
	changed.Components = append([]Instance(nil), second.Components...)
	changed.Components[0].Component.Definition.Outputs = append(
		[]Endpoint(nil), second.Components[0].Component.Definition.Outputs...,
	)
	changed.Components[0].Component.Definition.Outputs[0].Service = "acme.v2.Alpha"
	changedResolved, err := Resolve(changed, images)
	if err != nil {
		t.Fatal(err)
	}
	if firstResolved.Digest == changedResolved.Digest {
		t.Fatal("changed component definition did not change resolved digest")
	}
	if firstResolved.Components[0].Digest == changedResolved.Components[0].Digest {
		t.Fatal("changed component definition did not change component digest")
	}
}

func TestDigestCanonicalizesComponentAndLinkOrder(t *testing.T) {
	const service = "acme.v1.Service"
	provider := Instance{
		Name: "provider",
		Component: testComponent(
			"provider:dev", nil,
			[]Endpoint{{Name: "server", Service: service}},
		),
	}
	firstConsumer := Instance{
		Name: "first",
		Component: testComponent(
			"first:dev",
			[]Endpoint{{Name: "upstream", Service: service}},
			nil,
		),
	}
	secondConsumer := Instance{
		Name: "second",
		Component: testComponent(
			"second:dev",
			[]Endpoint{{Name: "upstream", Service: service}},
			nil,
		),
	}
	firstLink := testLink("first", "upstream", "provider", "server")
	secondLink := testLink("second", "upstream", "provider", "server")
	first := Spec{
		Name:       "demo",
		Components: []Instance{provider, firstConsumer, secondConsumer},
		Links:      []Link{firstLink, secondLink},
	}
	second := Spec{
		Name:       "demo",
		Components: []Instance{secondConsumer, provider, firstConsumer},
		Links:      []Link{secondLink, firstLink},
	}
	images := map[string]ResolvedImage{
		"provider": testImage("sha256:provider"),
		"first":    testImage("sha256:first"),
		"second":   testImage("sha256:second"),
	}
	firstResolved, err := Resolve(first, images)
	if err != nil {
		t.Fatal(err)
	}
	secondResolved, err := Resolve(second, images)
	if err != nil {
		t.Fatal(err)
	}
	if firstResolved.Digest != secondResolved.Digest {
		t.Fatalf(
			"declaration order changed digest: %s != %s",
			firstResolved.Digest, secondResolved.Digest,
		)
	}
}

func linkedTestSpec() Spec {
	const service = "acme.v1.Wanted"
	return Spec{
		Name: "demo",
		Components: []Instance{
			{
				Name: "consumer",
				Component: testComponent(
					"consumer:dev",
					[]Endpoint{{Name: "upstream", Service: service}},
					nil,
				),
			},
			{
				Name: "provider",
				Component: testComponent(
					"provider:dev",
					nil,
					[]Endpoint{{Name: "server", Service: service}},
				),
			},
		},
		Links: []Link{testLink("consumer", "upstream", "provider", "server")},
	}
}

func testComponent(image string, inputs, outputs []Endpoint) Component {
	return Component{
		Image: image,
		Definition: Definition{
			Inputs:  append([]Endpoint(nil), inputs...),
			Outputs: append([]Endpoint(nil), outputs...),
		},
	}
}

func testImage(id string) ResolvedImage {
	return ResolvedImage{ID: id, HasHealthcheck: true}
}

func resolvedComponent(t *testing.T, spec ResolvedSpec, name string) ResolvedComponent {
	t.Helper()
	component, exists := spec.Component(name)
	if !exists {
		t.Fatalf("resolved component %q is missing", name)
	}
	return component
}

func testLink(
	inputComponent, inputEndpoint, outputComponent, outputEndpoint string,
) Link {
	return Link{
		Input:  EndpointRef{Component: inputComponent, Endpoint: inputEndpoint},
		Output: EndpointRef{Component: outputComponent, Endpoint: outputEndpoint},
	}
}
