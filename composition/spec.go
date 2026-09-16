// Package composition defines, loads, and resolves dcomp systems.
package composition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/glguida/dcomp/internal/runtimecontract"
)

// ComponentSocketRoot is reserved for orchestrator-owned interface mounts.
const ComponentSocketRoot = runtimecontract.ContainerRoot

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	servicePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
)

// Endpoint is one locally named input or output of a component.
type Endpoint struct {
	Name    string `json:"name"`
	Service string `json:"service"`
}

// Definition is the application interface declared by component.dcomp.
type Definition struct {
	Inputs  []Endpoint `json:"inputs,omitempty"`
	Outputs []Endpoint `json:"outputs,omitempty"`
}

// Component is a reusable component declaration loaded from component.dcomp.
type Component struct {
	Image      string     `json:"image"`
	Definition Definition `json:"definition"`
}

// BindMount maps one existing canonical host path into a component.
type BindMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// VolumeMount maps one system-scoped persistent volume into a component.
// Name is a logical DComp name, not an arbitrary Docker volume name.
type VolumeMount struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// PublishedPort maps one host socket to a component port. HostPort zero asks
// Docker to allocate the host port.
type PublishedPort struct {
	Protocol      string `json:"protocol"`
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

// Runtime is the bounded, normalized host policy for one component instance.
// Args replaces the image command arguments, but never its entrypoint.
type Runtime struct {
	User           string          `json:"user,omitempty"`
	Binds          []BindMount     `json:"binds,omitempty"`
	Volumes        []VolumeMount   `json:"volumes,omitempty"`
	Args           []string        `json:"args,omitempty"`
	Ports          []PublishedPort `json:"ports,omitempty"`
	ExternalEgress bool            `json:"external_egress,omitempty"`
}

// Spec describes component instances, typed globals, and direct or symbolic links.
type Spec struct {
	Name       string     `json:"name"`
	Components []Instance `json:"components"`
	Links      []Link     `json:"links"`
	Globals    []Global   `json:"globals,omitempty"`
}

// Instance gives a reusable component definition a system-local name.
// Path is source metadata and is deliberately absent from ResolvedComponent.
type Instance struct {
	Name      string    `json:"name"`
	Path      string    `json:"path,omitempty"`
	Component Component `json:"component"`
	Runtime   Runtime   `json:"runtime"`
}

// EndpointRef identifies a concrete component endpoint or a symbolic global.
// A global reference sets only Global; a concrete reference sets Component
// and Endpoint.
type EndpointRef struct {
	Component string `json:"component"`
	Endpoint  string `json:"endpoint"`
	Global    string `json:"global,omitempty"`
}

// Link binds one concrete input to a concrete output or a symbolic global.
type Link struct {
	Input  EndpointRef `json:"input"`
	Output EndpointRef `json:"output"`
}

// ResolvedImage is the small part of image inspection needed by composition
// resolution. ID must be the immutable content ID, never a mutable tag.
type ResolvedImage struct {
	ID              string   `json:"id"`
	HasHealthcheck  bool     `json:"has_healthcheck"`
	DeclaredVolumes []string `json:"declared_volumes,omitempty"`
}

// ResolvedComponent contains only runtime-relevant component facts.
type ResolvedComponent struct {
	Name       string     `json:"name"`
	ImageRef   string     `json:"image_ref"`
	ImageID    string     `json:"image_id"`
	Definition Definition `json:"definition"`
	Runtime    Runtime    `json:"runtime"`
	Digest     string     `json:"digest"`
}

// ResolvedSpec is the canonical runtime description of a system. Digest covers
// immutable image IDs, definitions, normalized runtime policy, globals, and links.
// Descriptor paths remain metadata; canonical bind source paths are runtime
// identity.
type ResolvedSpec struct {
	Name       string              `json:"name"`
	Digest     string              `json:"digest"`
	Components []ResolvedComponent `json:"components"`
	Links      []Link              `json:"links"`
	Globals    []Global            `json:"globals,omitempty"`
}

// ValidName reports whether name is valid for a system, instance, or endpoint.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

// ValidateComponent verifies a component independently of any system.
func ValidateComponent(component Component) error {
	if component.Image == "" {
		return fmt.Errorf("docker image is missing")
	}
	return ValidateDefinition(component.Definition)
}

// ValidateRuntime verifies runtime structure without accessing host resources.
// Bind sources are checked against the filesystem when a mount is prepared.
func ValidateRuntime(runtime Runtime) error {
	if runtime.User != "" {
		ids := strings.Split(runtime.User, ":")
		if len(ids) != 2 {
			return fmt.Errorf("user must be numeric UID:GID")
		}
		for _, id := range ids {
			value, err := strconv.ParseUint(id, 10, 32)
			if err != nil || value == 4294967295 || strconv.FormatUint(value, 10) != id {
				return fmt.Errorf("user must be numeric UID:GID")
			}
		}
	}
	targets := make([]string, 0, len(runtime.Binds)+len(runtime.Volumes))
	for _, bind := range runtime.Binds {
		if err := validateBindPath(bind.Source); err != nil {
			return fmt.Errorf("bind source %q: %w", bind.Source, err)
		}
		if err := validateMountTarget(bind.Target); err != nil {
			return fmt.Errorf("bind target %q: %w", bind.Target, err)
		}
		if mountTargetsOverlap(bind.Target, ComponentSocketRoot) {
			return fmt.Errorf("bind target %q overlaps reserved DComp interface root %s", bind.Target, ComponentSocketRoot)
		}
		targets = append(targets, bind.Target)
	}

	volumeNames := make(map[string]struct{}, len(runtime.Volumes))
	for _, volume := range runtime.Volumes {
		if !ValidName(volume.Name) {
			return fmt.Errorf("invalid volume name %q", volume.Name)
		}
		if _, exists := volumeNames[volume.Name]; exists {
			return fmt.Errorf("volume %q is mounted more than once by one component", volume.Name)
		}
		volumeNames[volume.Name] = struct{}{}
		if err := validateMountTarget(volume.Target); err != nil {
			return fmt.Errorf("volume target %q: %w", volume.Target, err)
		}
		if mountTargetsOverlap(volume.Target, ComponentSocketRoot) {
			return fmt.Errorf("volume target %q overlaps reserved DComp interface root %s", volume.Target, ComponentSocketRoot)
		}
		targets = append(targets, volume.Target)
	}

	for left := 0; left < len(targets); left++ {
		for right := left + 1; right < len(targets); right++ {
			if mountTargetsOverlap(targets[left], targets[right]) {
				return fmt.Errorf(
					"mount targets %q and %q overlap", targets[left], targets[right],
				)
			}
		}
	}

	for index, argument := range runtime.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("argument %d contains a NUL byte", index)
		}
	}

	var hostBindings []PublishedPort
	for _, published := range runtime.Ports {
		if err := validatePublishedPort(published); err != nil {
			return err
		}
		for _, previous := range hostBindings {
			if publishedPortsConflict(previous, published) {
				return fmt.Errorf(
					"host port %s/%s:%d overlaps %s/%s:%d and is "+
						"published more than once",
					published.Protocol,
					published.HostIP,
					published.HostPort,
					previous.Protocol,
					previous.HostIP,
					previous.HostPort,
				)
			}
		}
		hostBindings = append(hostBindings, published)
	}
	return nil
}

// ValidateDefinition verifies endpoint names and protobuf service names.
// Input and output names are separate namespaces.
func ValidateDefinition(definition Definition) error {
	for _, item := range []struct {
		kind      string
		endpoints []Endpoint
	}{
		{kind: "input", endpoints: definition.Inputs},
		{kind: "output", endpoints: definition.Outputs},
	} {
		seen := make(map[string]struct{}, len(item.endpoints))
		for _, endpoint := range item.endpoints {
			if !ValidName(endpoint.Name) {
				return fmt.Errorf("invalid %s endpoint name %q", item.kind, endpoint.Name)
			}
			if !validProtobufService(endpoint.Service) {
				return fmt.Errorf(
					"%s endpoint %q has invalid protobuf service %q",
					item.kind, endpoint.Name, endpoint.Service,
				)
			}
			if _, exists := seen[endpoint.Name]; exists {
				return fmt.Errorf(
					"%s endpoint name %q is declared more than once",
					item.kind, endpoint.Name,
				)
			}
			seen[endpoint.Name] = struct{}{}
		}
	}
	return nil
}

// Validate verifies the composition and global namespace. Inputs have at most
// one target. Outputs may fan out, and cycles are valid.
func Validate(spec Spec) error {
	if !ValidName(spec.Name) {
		return fmt.Errorf("invalid system name %q", spec.Name)
	}

	components := make(map[string]Instance, len(spec.Components))
	type ownedPort struct {
		port      PublishedPort
		component string
	}
	var hostBindings []ownedPort
	for _, instance := range spec.Components {
		if !ValidName(instance.Name) {
			return fmt.Errorf("invalid component name %q", instance.Name)
		}
		if _, exists := components[instance.Name]; exists {
			return fmt.Errorf("component %q is declared more than once", instance.Name)
		}
		if err := ValidateComponent(instance.Component); err != nil {
			return fmt.Errorf("component %q: %w", instance.Name, err)
		}
		if err := ValidateRuntime(instance.Runtime); err != nil {
			return fmt.Errorf("component %q runtime: %w", instance.Name, err)
		}
		for _, published := range instance.Runtime.Ports {
			for _, previous := range hostBindings {
				if publishedPortsConflict(previous.port, published) {
					return fmt.Errorf(
						"overlapping host ports %s/%s:%d and %s/%s:%d "+
							"are published by both %q and %q",
						published.Protocol,
						published.HostIP,
						published.HostPort,
						previous.port.Protocol,
						previous.port.HostIP,
						previous.port.HostPort,
						previous.component,
						instance.Name,
					)
				}
			}
			hostBindings = append(hostBindings, ownedPort{
				port: published, component: instance.Name,
			})
		}
		components[instance.Name] = instance
	}
	for _, instance := range spec.Components {
		if len(instance.Runtime.Ports) != 0 &&
			!instance.Runtime.ExternalEgress {
			return fmt.Errorf(
				"component %q publishes host ports but has no egress directive; "+
					"Docker cannot publish ports with network mode none",
				instance.Name,
			)
		}
	}

	globals, err := validateGlobals(spec.Globals, components)
	if err != nil {
		return err
	}
	bound := make(map[string]struct{}, len(spec.Links))
	for _, link := range spec.Links {
		if link.Input.Global != "" {
			return fmt.Errorf("link input must be a direct endpoint")
		}
		consumer, exists := components[link.Input.Component]
		if !exists {
			return fmt.Errorf("link input component %q is not declared", link.Input.Component)
		}
		input, exists := consumer.Component.Definition.Input(link.Input.Endpoint)
		if !exists {
			return fmt.Errorf("component %q has no input endpoint %q", link.Input.Component, link.Input.Endpoint)
		}
		var service string
		if link.Output.Global != "" {
			if link.Output.Component != "" || link.Output.Endpoint != "" {
				return fmt.Errorf("link target mixes global and direct references")
			}
			global, exists := globals[link.Output.Global]
			if !exists {
				return fmt.Errorf("global interface %q is not declared", link.Output.Global)
			}
			service = global.Service
		} else {
			output, err := directOutput(components, link.Output)
			if err != nil {
				return err
			}
			service = output.Service
		}
		if input.Service != service {
			return fmt.Errorf("%s expects %s, but %s provides %s", link.Input, input.Service, link.Output, service)
		}
		key := endpointKey(link.Input)
		if _, exists := bound[key]; exists {
			return fmt.Errorf("input %s is linked more than once", link.Input)
		}
		bound[key] = struct{}{}
	}
	return nil
}

// Resolve validates image runtime contracts and replaces mutable image
// references with immutable image IDs. It has no Docker side effects.
func Resolve(spec Spec, images map[string]ResolvedImage) (ResolvedSpec, error) {
	if err := Validate(spec); err != nil {
		return ResolvedSpec{}, err
	}

	resolved := ResolvedSpec{
		Name:    spec.Name,
		Links:   append([]Link(nil), spec.Links...),
		Globals: append([]Global(nil), spec.Globals...),
	}
	for _, instance := range spec.Components {
		image, ok := images[instance.Name]
		if !ok {
			return ResolvedSpec{}, fmt.Errorf("component %q was not image-resolved", instance.Name)
		}
		if image.ID == "" {
			return ResolvedSpec{}, fmt.Errorf("component %q resolved to an empty image ID", instance.Name)
		}
		if !image.HasHealthcheck {
			return ResolvedSpec{}, fmt.Errorf("component %q image has no Docker HEALTHCHECK", instance.Name)
		}
		if err := validateDeclaredVolumes(instance.Runtime, image.DeclaredVolumes); err != nil {
			return ResolvedSpec{}, fmt.Errorf("component %q: %w", instance.Name, err)
		}
		resolved.Components = append(resolved.Components, ResolvedComponent{
			Name:       instance.Name,
			ImageRef:   instance.Component.Image,
			ImageID:    image.ID,
			Definition: cloneDefinition(instance.Component.Definition),
			Runtime:    cloneRuntime(instance.Runtime),
		})
	}

	resolved.canonicalize()
	for index := range resolved.Components {
		digest, err := resolved.computeComponentDigest(resolved.Components[index])
		if err != nil {
			return ResolvedSpec{}, err
		}
		resolved.Components[index].Digest = digest
	}
	digest, err := resolved.computeDigest()
	if err != nil {
		return ResolvedSpec{}, err
	}
	resolved.Digest = digest
	return resolved, nil
}

func (spec *ResolvedSpec) canonicalize() {
	sort.Slice(spec.Globals, func(i, j int) bool { return spec.Globals[i].Name < spec.Globals[j].Name })
	for index := range spec.Components {
		canonicalizeDefinition(&spec.Components[index].Definition)
		canonicalizeRuntime(&spec.Components[index].Runtime)
	}
	sort.Slice(spec.Components, func(i, j int) bool {
		return spec.Components[i].Name < spec.Components[j].Name
	})
	sort.Slice(spec.Links, func(i, j int) bool {
		left := endpointKey(spec.Links[i].Input)
		right := endpointKey(spec.Links[j].Input)
		if left != right {
			return left < right
		}
		return endpointKey(spec.Links[i].Output) < endpointKey(spec.Links[j].Output)
	})
}

func canonicalizeDefinition(definition *Definition) {
	sort.Slice(definition.Inputs, func(i, j int) bool {
		return definition.Inputs[i].Name < definition.Inputs[j].Name
	})
	sort.Slice(definition.Outputs, func(i, j int) bool {
		return definition.Outputs[i].Name < definition.Outputs[j].Name
	})
}

func canonicalizeRuntime(runtime *Runtime) {
	sort.Slice(runtime.Binds, func(i, j int) bool {
		if runtime.Binds[i].Target != runtime.Binds[j].Target {
			return runtime.Binds[i].Target < runtime.Binds[j].Target
		}
		if runtime.Binds[i].Source != runtime.Binds[j].Source {
			return runtime.Binds[i].Source < runtime.Binds[j].Source
		}
		return !runtime.Binds[i].ReadOnly && runtime.Binds[j].ReadOnly
	})
	sort.Slice(runtime.Volumes, func(i, j int) bool {
		if runtime.Volumes[i].Target != runtime.Volumes[j].Target {
			return runtime.Volumes[i].Target < runtime.Volumes[j].Target
		}
		if runtime.Volumes[i].Name != runtime.Volumes[j].Name {
			return runtime.Volumes[i].Name < runtime.Volumes[j].Name
		}
		return !runtime.Volumes[i].ReadOnly && runtime.Volumes[j].ReadOnly
	})
	sort.Slice(runtime.Ports, func(i, j int) bool {
		left, right := runtime.Ports[i], runtime.Ports[j]
		if left.Protocol != right.Protocol {
			return left.Protocol < right.Protocol
		}
		if left.HostIP != right.HostIP {
			return left.HostIP < right.HostIP
		}
		if left.HostPort != right.HostPort {
			return left.HostPort < right.HostPort
		}
		return left.ContainerPort < right.ContainerPort
	})
}

// componentRuntimePolicyVersion domains the fixed container launch contract.
// Change it whenever that contract or the container-definition identity
// algorithm changes, so distinct semantics cannot produce interchangeable
// component identities.
//
// Version 4 separates container definition from wiring identity: the component
// targeted by each input link does not affect the Docker container request.
const componentRuntimePolicyVersion = 4

func (spec ResolvedSpec) computeComponentDigest(component ResolvedComponent) (string, error) {
	copy := component
	copy.ImageRef = ""
	copy.Digest = ""
	identity := struct {
		RuntimePolicy int               `json:"runtime_policy"`
		Component     ResolvedComponent `json:"component"`
	}{
		RuntimePolicy: componentRuntimePolicyVersion,
		Component:     copy,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode resolved component %q: %w", component.Name, err)
	}
	return digestBytes(encoded), nil
}

func (spec ResolvedSpec) computeDigest() (string, error) {
	copy := spec
	copy.Digest = ""
	copy.Components = append([]ResolvedComponent(nil), spec.Components...)
	// A mutable input reference is useful diagnostics, but it is not runtime
	// identity. Two references resolving to the same immutable image ID produce
	// the same system digest.
	for index := range copy.Components {
		copy.Components[index].ImageRef = ""
	}
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("encode resolved system: %w", err)
	}
	return digestBytes(encoded), nil
}

// Input returns the named input endpoint.
func (definition Definition) Input(name string) (Endpoint, bool) {
	for _, endpoint := range definition.Inputs {
		if endpoint.Name == name {
			return endpoint, true
		}
	}
	return Endpoint{}, false
}

// Output returns the named output endpoint.
func (definition Definition) Output(name string) (Endpoint, bool) {
	for _, endpoint := range definition.Outputs {
		if endpoint.Name == name {
			return endpoint, true
		}
	}
	return Endpoint{}, false
}

// Component returns the named resolved component instance.
func (spec ResolvedSpec) Component(name string) (ResolvedComponent, bool) {
	for _, component := range spec.Components {
		if component.Name == name {
			return component, true
		}
	}
	return ResolvedComponent{}, false
}

// LinkTarget returns the output component and endpoint wired to one input.
func (spec ResolvedSpec) LinkTarget(
	inputComponent, inputEndpoint string,
) (ResolvedComponent, Endpoint, bool) {
	for _, link := range spec.Links {
		if link.Input.Component != inputComponent || link.Input.Endpoint != inputEndpoint {
			continue
		}
		target := ResolveTarget(spec.Globals, link.Output)
		component, exists := spec.Component(target.Component)
		if !exists {
			return ResolvedComponent{}, Endpoint{}, false
		}
		endpoint, exists := component.Definition.Output(target.Endpoint)
		if !exists {
			return ResolvedComponent{}, Endpoint{}, false
		}
		return component, endpoint, true
	}
	return ResolvedComponent{}, Endpoint{}, false
}

func endpointKey(ref EndpointRef) string {
	return ref.Component + "\x00" + ref.Endpoint
}

func validProtobufService(service string) bool {
	return servicePattern.MatchString(service)
}

func validateBindPath(source string) error {
	if source == "" || !filepath.IsAbs(source) {
		return fmt.Errorf("must be absolute")
	}
	if clean := filepath.Clean(source); clean != source {
		return fmt.Errorf("must be clean (use %q)", clean)
	}
	if strings.IndexByte(source, 0) >= 0 {
		return fmt.Errorf("contains a NUL byte")
	}
	return nil
}

// ValidateBindSources checks host resources for a new or recreated container.
// Recorded mounts on retained containers do not require the old path to exist.
func ValidateBindSources(runtime Runtime) error {
	for _, bind := range runtime.Binds {
		if err := validateBindSource(bind.Source); err != nil {
			return fmt.Errorf("bind source %q: %w", bind.Source, err)
		}
	}
	return nil
}

func validateBindSource(source string) error {
	if err := validateBindPath(source); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("must exist and resolve without symlinks: %w", err)
	}
	if canonical != source {
		return fmt.Errorf("must be canonical (use %q)", canonical)
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("must exist: %w", err)
	}
	return nil
}

func validateMountTarget(target string) error {
	if target == "" || !path.IsAbs(target) {
		return fmt.Errorf("must be an absolute container path")
	}
	if clean := path.Clean(target); clean != target {
		return fmt.Errorf("must be clean (use %q)", clean)
	}
	if target == "/" {
		return fmt.Errorf("must not be the container root")
	}
	if strings.IndexByte(target, 0) >= 0 {
		return fmt.Errorf("contains a NUL byte")
	}
	return nil
}

func mountTargetsOverlap(left, right string) bool {
	return left == right ||
		strings.HasPrefix(left, right+"/") ||
		strings.HasPrefix(right, left+"/")
}

func validatePublishedPort(published PublishedPort) error {
	if published.Protocol != "tcp" && published.Protocol != "udp" {
		return fmt.Errorf("invalid published port protocol %q", published.Protocol)
	}
	ip := net.ParseIP(published.HostIP)
	if ip == nil {
		return fmt.Errorf("published host IP %q is not an IP literal", published.HostIP)
	}
	if canonical := ip.String(); canonical != published.HostIP {
		return fmt.Errorf(
			"published host IP %q is not canonical (use %q)",
			published.HostIP, canonical,
		)
	}
	if published.HostPort < 0 || published.HostPort > 65535 {
		return fmt.Errorf("published host port %d is outside 0..65535", published.HostPort)
	}
	if published.ContainerPort < 1 || published.ContainerPort > 65535 {
		return fmt.Errorf(
			"published container port %d is outside 1..65535",
			published.ContainerPort,
		)
	}
	return nil
}

func publishedPortsConflict(left, right PublishedPort) bool {
	// Zero delegates allocation to Docker. Two dynamic requests do not reserve
	// the same host socket and therefore cannot conflict at composition time.
	if left.HostPort == 0 || right.HostPort == 0 {
		return false
	}
	if left.Protocol != right.Protocol || left.HostPort != right.HostPort {
		return false
	}
	if left.HostIP == right.HostIP {
		return true
	}
	leftIP := net.ParseIP(left.HostIP)
	rightIP := net.ParseIP(right.HostIP)
	leftIPv4 := leftIP.To4() != nil
	rightIPv4 := rightIP.To4() != nil
	if leftIPv4 != rightIPv4 {
		return false
	}
	return leftIP.IsUnspecified() || rightIP.IsUnspecified()
}

func validateDeclaredVolumes(runtime Runtime, declared []string) error {
	if len(declared) == 0 {
		return nil
	}
	explicit := make(map[string]struct{}, len(runtime.Binds)+len(runtime.Volumes))
	for _, bind := range runtime.Binds {
		explicit[bind.Target] = struct{}{}
	}
	for _, volume := range runtime.Volumes {
		explicit[volume.Target] = struct{}{}
	}
	for _, target := range declared {
		canonical := path.Clean(target)
		if !path.IsAbs(target) || canonical == "/" {
			return fmt.Errorf("image declares invalid VOLUME target %q", target)
		}
		if _, covered := explicit[canonical]; !covered {
			return fmt.Errorf(
				"image declares VOLUME target %q without an explicit bind or volume mount",
				canonical,
			)
		}
	}
	return nil
}

func digestBytes(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneDefinition(input Definition) Definition {
	return Definition{
		Inputs:  append([]Endpoint(nil), input.Inputs...),
		Outputs: append([]Endpoint(nil), input.Outputs...),
	}
}

func cloneRuntime(input Runtime) Runtime {
	return Runtime{
		User:           input.User,
		Binds:          append([]BindMount(nil), input.Binds...),
		Volumes:        append([]VolumeMount(nil), input.Volumes...),
		Args:           append([]string(nil), input.Args...),
		Ports:          append([]PublishedPort(nil), input.Ports...),
		ExternalEgress: input.ExternalEgress,
	}
}
