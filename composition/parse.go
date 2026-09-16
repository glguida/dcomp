package composition

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const componentFileName = "component.dcomp"

// ParseComponent reads the deliberately small component.dcomp format:
//
//	docker IMAGE
//	input PROTOBUF_SERVICE LOCAL_NAME
//	output PROTOBUF_SERVICE LOCAL_NAME
//
// Blank lines and text following # are ignored.
func ParseComponent(reader io.Reader) (Component, error) {
	var component Component
	scanner := newScanner(reader)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		fields := lineFields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "docker":
			if len(fields) != 2 {
				return Component{}, lineError(lineNumber, "expected docker IMAGE")
			}
			if component.Image != "" {
				return Component{}, lineError(lineNumber, "docker image is already declared")
			}
			component.Image = fields[1]
		case "input", "output":
			if len(fields) != 3 {
				return Component{}, lineError(
					lineNumber, "expected %s PROTOBUF_SERVICE LOCAL_NAME", fields[0],
				)
			}
			endpoint := Endpoint{Service: fields[1], Name: fields[2]}
			if fields[0] == "input" {
				component.Definition.Inputs = append(component.Definition.Inputs, endpoint)
			} else {
				component.Definition.Outputs = append(component.Definition.Outputs, endpoint)
			}
			if err := ValidateDefinition(component.Definition); err != nil {
				return Component{}, lineError(lineNumber, "%v", err)
			}
		default:
			return Component{}, lineError(lineNumber, "unknown directive %q", fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return Component{}, fmt.Errorf("read component: %w", err)
	}
	if err := ValidateComponent(component); err != nil {
		return Component{}, err
	}
	return component, nil
}

// LoadComponent loads either a component directory or an exact component.dcomp
// path. A non-component.dcomp path is always interpreted as a directory.
func LoadComponent(path string) (Component, error) {
	manifestPath := componentManifestPath(path)
	file, err := os.Open(manifestPath)
	if err != nil {
		return Component{}, fmt.Errorf("%s: %w", manifestPath, err)
	}
	defer file.Close()
	component, err := ParseComponent(file)
	if err != nil {
		return Component{}, fmt.Errorf("%s: %w", manifestPath, err)
	}
	return component, nil
}

// Parse reads a system file and loads its component definitions relative to
// baseDir:
//
//	system NAME
//	component INSTANCE PATH
//	bind INSTANCE SOURCE TARGET ro|rw
//	volume INSTANCE LOGICAL_NAME TARGET ro|rw
//	args INSTANCE ARG...
//	publish INSTANCE tcp|udp HOST_IP HOST_PORT CONTAINER_PORT
//	egress INSTANCE
//	global NAME SERVICE [INSTANCE.OUTPUT]
//	link INSTANCE.INPUT INSTANCE.OUTPUT|@GLOBAL
//
// Relative component and bind source paths are resolved from baseDir. Bind
// sources must exist and are stored as canonical absolute paths. Component
// paths normally name a directory containing component.dcomp, but may name
// component.dcomp directly.
func Parse(reader io.Reader, baseDir string) (Spec, error) {
	if strings.TrimSpace(baseDir) == "" {
		return Spec{}, fmt.Errorf("system base directory is missing")
	}
	absoluteBase, err := filepath.Abs(baseDir)
	if err != nil {
		return Spec{}, fmt.Errorf("resolve system base directory %q: %w", baseDir, err)
	}

	var spec Spec
	componentIndexes := make(map[string]int)
	scanner := newScanner(reader)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		fields := lineFields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "system":
			if len(fields) != 2 {
				return Spec{}, lineError(lineNumber, "expected system NAME")
			}
			if spec.Name != "" {
				return Spec{}, lineError(lineNumber, "system is already declared")
			}
			spec.Name = fields[1]
		case "component":
			if len(fields) != 3 {
				return Spec{}, lineError(lineNumber, "expected component INSTANCE PATH")
			}
			if _, exists := componentIndexes[fields[1]]; exists {
				return Spec{}, lineError(
					lineNumber, "component %q is declared more than once", fields[1],
				)
			}
			path := filepath.Clean(fields[2])
			if !filepath.IsAbs(path) {
				path = filepath.Join(absoluteBase, path)
			}
			path = componentManifestPath(path)
			component, err := LoadComponent(path)
			if err != nil {
				return Spec{}, fmt.Errorf(
					"line %d: load component %q: %w", lineNumber, fields[1], err,
				)
			}
			spec.Components = append(spec.Components, Instance{
				Name: fields[1], Path: path, Component: component,
			})
			componentIndexes[fields[1]] = len(spec.Components) - 1
		case "bind":
			if len(fields) != 5 {
				return Spec{}, lineError(
					lineNumber, "expected bind INSTANCE SOURCE TARGET ro|rw",
				)
			}
			index, err := declaredInstance(componentIndexes, fields[1], fields[0])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			source, err := canonicalBindSource(fields[2], absoluteBase)
			if err != nil {
				return Spec{}, lineError(lineNumber, "bind source %q: %v", fields[2], err)
			}
			readOnly, err := parseMountMode(fields[4])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			runtime := &spec.Components[index].Runtime
			runtime.Binds = append(runtime.Binds, BindMount{
				Source: source, Target: fields[3], ReadOnly: readOnly,
			})
			if err := ValidateRuntime(*runtime); err != nil {
				return Spec{}, lineError(
					lineNumber, "component %q runtime: %v", fields[1], err,
				)
			}
		case "volume":
			if len(fields) != 5 {
				return Spec{}, lineError(
					lineNumber, "expected volume INSTANCE LOGICAL_NAME TARGET ro|rw",
				)
			}
			index, err := declaredInstance(componentIndexes, fields[1], fields[0])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			readOnly, err := parseMountMode(fields[4])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			runtime := &spec.Components[index].Runtime
			runtime.Volumes = append(runtime.Volumes, VolumeMount{
				Name: fields[2], Target: fields[3], ReadOnly: readOnly,
			})
			if err := ValidateRuntime(*runtime); err != nil {
				return Spec{}, lineError(
					lineNumber, "component %q runtime: %v", fields[1], err,
				)
			}
		case "args":
			if len(fields) < 3 {
				return Spec{}, lineError(lineNumber, "expected args INSTANCE ARG...")
			}
			index, err := declaredInstance(componentIndexes, fields[1], fields[0])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			runtime := &spec.Components[index].Runtime
			if runtime.Args != nil {
				return Spec{}, lineError(
					lineNumber, "args for component %q are already declared", fields[1],
				)
			}
			runtime.Args = append([]string(nil), fields[2:]...)
			if err := ValidateRuntime(*runtime); err != nil {
				return Spec{}, lineError(
					lineNumber, "component %q runtime: %v", fields[1], err,
				)
			}
		case "publish":
			if len(fields) != 6 {
				return Spec{}, lineError(
					lineNumber,
					"expected publish INSTANCE tcp|udp HOST_IP HOST_PORT CONTAINER_PORT",
				)
			}
			index, err := declaredInstance(componentIndexes, fields[1], fields[0])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			published, err := parsePublishedPort(fields[2:])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			runtime := &spec.Components[index].Runtime
			runtime.Ports = append(runtime.Ports, published)
			if err := ValidateRuntime(*runtime); err != nil {
				return Spec{}, lineError(
					lineNumber, "component %q runtime: %v", fields[1], err,
				)
			}
		case "user":
			if len(fields) != 3 {
				return Spec{}, lineError(lineNumber, "expected user INSTANCE UID:GID")
			}
			index, err := declaredInstance(componentIndexes, fields[1], fields[0])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			runtime := &spec.Components[index].Runtime
			if runtime.User != "" {
				return Spec{}, lineError(lineNumber, "user is already set for %s", fields[1])
			}
			runtime.User = fields[2]
			if err := ValidateRuntime(*runtime); err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
		case "egress":
			if len(fields) != 2 {
				return Spec{}, lineError(lineNumber, "expected egress INSTANCE")
			}
			index, err := declaredInstance(componentIndexes, fields[1], fields[0])
			if err != nil {
				return Spec{}, lineError(lineNumber, "%v", err)
			}
			runtime := &spec.Components[index].Runtime
			if runtime.ExternalEgress {
				return Spec{}, lineError(
					lineNumber, "egress for component %q is already declared", fields[1],
				)
			}
			runtime.ExternalEgress = true
		case "global":
			if len(fields) != 3 && len(fields) != 4 {
				return Spec{}, lineError(lineNumber, "global expects NAME SERVICE [COMPONENT.OUTPUT]")
			}
			global := Global{Name: fields[1], Service: fields[2]}
			if len(fields) == 4 {
				target, err := ParseEndpointRef(fields[3])
				if err != nil {
					return Spec{}, lineError(lineNumber, "%v", err)
				}
				global.Target = target
			}
			spec.Globals = append(spec.Globals, global)
		case "link":
			if len(fields) != 3 {
				return Spec{}, lineError(
					lineNumber, "expected link INSTANCE.INPUT INSTANCE.OUTPUT",
				)
			}
			input, err := parseEndpointRef(fields[1])
			if err != nil {
				return Spec{}, lineError(lineNumber, "invalid input endpoint: %v", err)
			}
			output, err := ParseTarget(fields[2])
			if err != nil {
				return Spec{}, lineError(lineNumber, "invalid output endpoint: %v", err)
			}
			spec.Links = append(spec.Links, Link{Input: input, Output: output})
		default:
			return Spec{}, lineError(lineNumber, "unknown directive %q", fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return Spec{}, fmt.Errorf("read system: %w", err)
	}
	if spec.Name == "" {
		return Spec{}, fmt.Errorf("system declaration is missing")
	}
	if err := Validate(spec); err != nil {
		return Spec{}, err
	}
	for index := range spec.Components {
		canonicalizeRuntime(&spec.Components[index].Runtime)
	}
	return spec, nil
}

// Load opens a system file and resolves component paths relative to that file.
func Load(path string) (Spec, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Spec{}, fmt.Errorf("resolve system path %q: %w", path, err)
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return Spec{}, err
	}
	defer file.Close()
	spec, err := Parse(file, filepath.Dir(absolutePath))
	if err != nil {
		return Spec{}, fmt.Errorf("%s: %w", absolutePath, err)
	}
	return spec, nil
}

func declaredInstance(indexes map[string]int, name, directive string) (int, error) {
	index, exists := indexes[name]
	if !exists {
		return 0, fmt.Errorf(
			"component %q must be declared before %s", name, directive,
		)
	}
	return index, nil
}

func canonicalBindSource(source, baseDir string) (string, error) {
	candidate := filepath.Clean(source)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(baseDir, candidate)
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("make absolute: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("must exist and resolve: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("make canonical path absolute: %w", err)
	}
	return filepath.Clean(canonical), nil
}

func parseMountMode(value string) (bool, error) {
	switch value {
	case "ro":
		return true, nil
	case "rw":
		return false, nil
	default:
		return false, fmt.Errorf("mount mode %q must be ro or rw", value)
	}
}

func parsePublishedPort(fields []string) (PublishedPort, error) {
	if len(fields) != 4 {
		return PublishedPort{}, fmt.Errorf("internal error: invalid published port fields")
	}
	ip := net.ParseIP(fields[1])
	if ip == nil {
		return PublishedPort{}, fmt.Errorf("published host IP %q is not an IP literal", fields[1])
	}
	hostPort, err := strconv.Atoi(fields[2])
	if err != nil {
		return PublishedPort{}, fmt.Errorf("invalid published host port %q", fields[2])
	}
	containerPort, err := strconv.Atoi(fields[3])
	if err != nil {
		return PublishedPort{}, fmt.Errorf("invalid published container port %q", fields[3])
	}
	published := PublishedPort{
		Protocol: fields[0], HostIP: ip.String(),
		HostPort: hostPort, ContainerPort: containerPort,
	}
	if err := validatePublishedPort(published); err != nil {
		return PublishedPort{}, err
	}
	return published, nil
}

func componentManifestPath(path string) string {
	clean := filepath.Clean(path)
	if filepath.Base(clean) == componentFileName {
		return clean
	}
	return filepath.Join(clean, componentFileName)
}

func parseEndpointRef(value string) (EndpointRef, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || !ValidName(parts[0]) || !ValidName(parts[1]) {
		return EndpointRef{}, fmt.Errorf(
			"%q must be INSTANCE.ENDPOINT using valid local names", value,
		)
	}
	return EndpointRef{Component: parts[0], Endpoint: parts[1]}, nil
}

func newScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	return scanner
}

func lineFields(line string) []string {
	return strings.Fields(strings.TrimSpace(strings.SplitN(line, "#", 2)[0]))
}

func lineError(line int, format string, args ...interface{}) error {
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, args...))
}
