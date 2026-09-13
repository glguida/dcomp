package composition

import (
	"fmt"
	"regexp"
	"strings"
)

// Global names one typed output interface within a system. A zero Target is
// unbound. Consumers retain their global reference across target changes.
type Global struct {
	Name    string      `json:"name"`
	Service string      `json:"service"`
	Target  EndpointRef `json:"target"`
}

var globalNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func ValidGlobalName(name string) bool { return globalNamePattern.MatchString(name) }

func (ref EndpointRef) String() string {
	if ref.Global != "" {
		return "@" + ref.Global
	}
	if ref.Component == "" && ref.Endpoint == "" {
		return "-"
	}
	return ref.Component + "." + ref.Endpoint
}

func ParseEndpointRef(value string) (EndpointRef, error) { return parseEndpointRef(value) }

func ParseTarget(value string) (EndpointRef, error) {
	if name, ok := strings.CutPrefix(value, "@"); ok {
		if !ValidGlobalName(name) {
			return EndpointRef{}, fmt.Errorf("invalid global interface name %q", name)
		}
		return EndpointRef{Global: name}, nil
	}
	return ParseEndpointRef(value)
}

// ResolveTarget performs one level of indirection. Globals may only name
// concrete outputs, never other globals.
func ResolveTarget(globals []Global, ref EndpointRef) EndpointRef {
	if ref.Global == "" {
		return ref
	}
	for _, global := range globals {
		if global.Name == ref.Global {
			return global.Target
		}
	}
	return EndpointRef{}
}

func directOutput(components map[string]Instance, ref EndpointRef) (Endpoint, error) {
	if ref.Global != "" {
		return Endpoint{}, fmt.Errorf("global assignment must name a direct output")
	}
	component, ok := components[ref.Component]
	if !ok {
		return Endpoint{}, fmt.Errorf("link output component %q is not declared", ref.Component)
	}
	output, ok := component.Component.Definition.Output(ref.Endpoint)
	if !ok {
		return Endpoint{}, fmt.Errorf("component %q has no output endpoint %q", ref.Component, ref.Endpoint)
	}
	return output, nil
}

func validateGlobals(globals []Global, components map[string]Instance) (map[string]Global, error) {
	result := make(map[string]Global, len(globals))
	for _, global := range globals {
		if !ValidGlobalName(global.Name) || !validProtobufService(global.Service) {
			return nil, fmt.Errorf("invalid global interface %q or service %q", global.Name, global.Service)
		}
		if _, exists := result[global.Name]; exists {
			return nil, fmt.Errorf("global interface %q is declared more than once", global.Name)
		}
		if global.Target != (EndpointRef{}) {
			output, err := directOutput(components, global.Target)
			if err != nil {
				return nil, fmt.Errorf("global %s: %w", global.Name, err)
			}
			if output.Service != global.Service {
				return nil, fmt.Errorf("global %s expects %s, but %s provides %s", global.Name, global.Service, global.Target, output.Service)
			}
		}
		result[global.Name] = global
	}
	return result, nil
}

// Authored reconstructs an editable composition from committed state. Source
// paths are unnecessary: definitions and normalized runtime policy are stored.
func (spec ResolvedSpec) Authored() Spec {
	result := Spec{Name: spec.Name, Links: append([]Link(nil), spec.Links...), Globals: append([]Global(nil), spec.Globals...)}
	for _, item := range spec.Components {
		result.Components = append(result.Components, Instance{Name: item.Name,
			Component: Component{Image: item.ImageID, Definition: cloneDefinition(item.Definition)}, Runtime: cloneRuntime(item.Runtime)})
	}
	return result
}
