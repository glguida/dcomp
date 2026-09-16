package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/lifecycle"
)

// The view document is the machine-readable description of one system: its
// wiring topology and, when observed from durable state, the live status of
// its resources. It is the single contract consumed by `dcomp view --json`,
// the dashboard HTTP API, and external viewers. See docs/view.md.

type viewDocument struct {
	APIVersion int `json:"api_version"`
	// Source is "file" for a parsed system description with no runtime facts,
	// or "state" for a system observed from durable state and Docker.
	Source      string                  `json:"source"`
	Name        string                  `json:"name"`
	Digest      string                  `json:"digest,omitempty"`
	Desired     bool                    `json:"desired"`
	Operational bool                    `json:"operational"`
	Operation   string                  `json:"operation,omitempty"`
	Phase       string                  `json:"phase,omitempty"`
	Components  []viewComponentDocument `json:"components"`
	Links       []viewLinkDocument      `json:"links"`
	Globals     []composition.Global    `json:"globals,omitempty"`
	Networks    []networkStatusDocument `json:"networks,omitempty"`
	Proxy       *viewProxyDocument      `json:"proxy,omitempty"`
}

type viewProxyDocument struct {
	Ready             bool   `json:"ready"`
	Inputs            int    `json:"inputs"`
	Outputs           int    `json:"outputs"`
	ActiveConnections int64  `json:"active_connections"`
	Problem           string `json:"problem"`
}

type viewEndpointDocument struct {
	Name    string `json:"name"`
	Service string `json:"service"`
}

type viewEndpointRefDocument struct {
	Component string `json:"component"`
	Endpoint  string `json:"endpoint"`
}

type viewLinkDocument struct {
	Global            string                    `json:"global,omitempty"`
	Service           string                    `json:"service"`
	Input             viewEndpointRefDocument   `json:"input"`
	Output            viewEndpointRefDocument   `json:"output"`
	Active            *bool                     `json:"active,omitempty"`
	ActiveConnections *int64                    `json:"active_connections,omitempty"`
	Activity          *viewLinkActivityDocument `json:"activity,omitempty"`
}

type viewLinkActivityDocument struct {
	BytesInputToOutput uint64 `json:"bytes_input_to_output"`
	BytesOutputToInput uint64 `json:"bytes_output_to_input"`
}

type viewLinkIdentity struct {
	inputComponent  string
	inputEndpoint   string
	outputComponent string
	outputEndpoint  string
}

type viewMountDocument struct {
	Source   string `json:"source,omitempty"`
	Name     string `json:"name,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type viewComponentDocument struct {
	User           string                   `json:"user,omitempty"`
	Name           string                   `json:"name"`
	ImageRef       string                   `json:"image_ref"`
	ImageID        string                   `json:"image_id,omitempty"`
	Inputs         []viewEndpointDocument   `json:"inputs"`
	Outputs        []viewEndpointDocument   `json:"outputs"`
	Egress         bool                     `json:"egress"`
	Binds          []viewMountDocument      `json:"binds"`
	Volumes        []viewMountDocument      `json:"volumes"`
	Args           []string                 `json:"args"`
	PublishedPorts []publishedPortDocument  `json:"published_ports"`
	Status         *componentStatusDocument `json:"status,omitempty"`
}

// viewFromSpec renders a parsed system description. It reports topology only:
// no image IDs, no container status, no networks.
func viewFromSpec(spec composition.Spec) viewDocument {
	document := viewDocument{
		APIVersion: apiVersion,
		Source:     "file",
		Name:       spec.Name,
		Components: make([]viewComponentDocument, 0, len(spec.Components)),
		Links:      make([]viewLinkDocument, 0, len(spec.Links)),
	}
	services := make(map[string]string)
	for _, instance := range spec.Components {
		document.Components = append(document.Components, viewComponent(
			instance.Name,
			instance.Component.Image,
			"",
			instance.Component.Definition,
			instance.Runtime,
			nil,
		))
		for _, input := range instance.Component.Definition.Inputs {
			services[instance.Name+"."+input.Name] = input.Service
		}
	}
	sort.Slice(document.Components, func(i, j int) bool {
		return document.Components[i].Name < document.Components[j].Name
	})
	for _, link := range spec.Links {
		document.Links = append(document.Links, viewGlobalLink(link, services, spec.Globals))
	}
	document.Globals = sortedGlobals(spec.Globals)
	sortViewLinks(document.Links)
	return document
}

// viewFromStatus renders a system observed from durable state. A status
// without a spec (absent system) yields an empty topology.
func viewFromStatus(status lifecycle.Status) viewDocument {
	document := viewDocument{
		APIVersion:  apiVersion,
		Source:      "state",
		Name:        status.Name,
		Digest:      status.Digest,
		Desired:     status.Desired,
		Operational: status.Operational(),
		Operation:   status.Operation,
		Phase:       status.Phase,
		Components:  []viewComponentDocument{},
		Links:       []viewLinkDocument{},
	}
	for _, network := range status.Networks {
		document.Networks = append(document.Networks, networkStatusDocument{
			Key: network.Key, ID: network.ID,
			Internal: network.Internal, Problem: network.Problem,
		})
	}
	if status.Spec == nil {
		return document
	}
	document.Proxy = &viewProxyDocument{
		Ready:             status.Proxy.Ready,
		Inputs:            status.Proxy.Inputs,
		Outputs:           status.Proxy.Outputs,
		ActiveConnections: status.Proxy.ActiveConnections,
		Problem:           status.Proxy.Problem,
	}
	componentStatus := make(map[string]*componentStatusDocument, len(status.Components))
	for _, component := range status.Components {
		item := componentStatusJSON(component)
		componentStatus[component.Name] = &item
	}
	services := make(map[string]string)
	for _, component := range status.Spec.Components {
		document.Components = append(document.Components, viewComponent(
			component.Name,
			component.ImageRef,
			component.ImageID,
			component.Definition,
			component.Runtime,
			componentStatus[component.Name],
		))
		for _, input := range component.Definition.Inputs {
			services[component.Name+"."+input.Name] = input.Service
		}
	}
	sort.Slice(document.Components, func(i, j int) bool {
		return document.Components[i].Name < document.Components[j].Name
	})
	metricsByLink := make(map[viewLinkIdentity]int, len(status.Proxy.Links))
	for index, metrics := range status.Proxy.Links {
		metricsByLink[viewLinkIdentity{
			inputComponent: metrics.InputComponent, inputEndpoint: metrics.InputEndpoint,
			outputComponent: metrics.OutputComponent, outputEndpoint: metrics.OutputEndpoint,
		}] = index
	}
	for _, link := range status.Spec.Links {
		item := viewGlobalLink(link, services, status.Spec.Globals)
		identity := viewLinkIdentity{
			inputComponent: link.Input.Component, inputEndpoint: link.Input.Endpoint,
			outputComponent: item.Output.Component, outputEndpoint: item.Output.Endpoint,
		}
		if index, exists := metricsByLink[identity]; exists {
			metrics := status.Proxy.Links[index]
			active := metrics.ActiveConnections > 0
			connections := metrics.ActiveConnections
			item.Active = &active
			item.ActiveConnections = &connections
			item.Activity = &viewLinkActivityDocument{
				BytesInputToOutput: metrics.BytesInputToOutput,
				BytesOutputToInput: metrics.BytesOutputToInput,
			}
		}
		document.Links = append(document.Links, item)
	}
	document.Globals = sortedGlobals(status.Spec.Globals)
	sortViewLinks(document.Links)
	return document
}

func viewComponent(
	name string,
	imageRef string,
	imageID string,
	definition composition.Definition,
	runtime composition.Runtime,
	status *componentStatusDocument,
) viewComponentDocument {
	component := viewComponentDocument{
		User:           runtime.User,
		Name:           name,
		ImageRef:       imageRef,
		ImageID:        imageID,
		Inputs:         make([]viewEndpointDocument, 0, len(definition.Inputs)),
		Outputs:        make([]viewEndpointDocument, 0, len(definition.Outputs)),
		Egress:         runtime.ExternalEgress,
		Binds:          make([]viewMountDocument, 0, len(runtime.Binds)),
		Volumes:        make([]viewMountDocument, 0, len(runtime.Volumes)),
		Args:           append([]string{}, runtime.Args...),
		PublishedPorts: make([]publishedPortDocument, 0, len(runtime.Ports)),
		Status:         status,
	}
	for _, endpoint := range definition.Inputs {
		component.Inputs = append(component.Inputs, viewEndpointDocument{
			Name: endpoint.Name, Service: endpoint.Service,
		})
	}
	for _, endpoint := range definition.Outputs {
		component.Outputs = append(component.Outputs, viewEndpointDocument{
			Name: endpoint.Name, Service: endpoint.Service,
		})
	}
	sort.Slice(component.Inputs, func(i, j int) bool {
		return component.Inputs[i].Name < component.Inputs[j].Name
	})
	sort.Slice(component.Outputs, func(i, j int) bool {
		return component.Outputs[i].Name < component.Outputs[j].Name
	})
	for _, bind := range runtime.Binds {
		component.Binds = append(component.Binds, viewMountDocument{
			Source: bind.Source, Target: bind.Target, ReadOnly: bind.ReadOnly,
		})
	}
	for _, volume := range runtime.Volumes {
		component.Volumes = append(component.Volumes, viewMountDocument{
			Name: volume.Name, Target: volume.Target, ReadOnly: volume.ReadOnly,
		})
	}
	for _, published := range runtime.Ports {
		component.PublishedPorts = append(component.PublishedPorts, publishedPortDocument{
			Protocol: published.Protocol, HostIP: published.HostIP,
			HostPort: published.HostPort, ContainerPort: published.ContainerPort,
		})
	}
	return component
}

func viewLink(link composition.Link, inputServices map[string]string) viewLinkDocument {
	return viewLinkDocument{
		Service: inputServices[link.Input.Component+"."+link.Input.Endpoint],
		Input: viewEndpointRefDocument{
			Component: link.Input.Component, Endpoint: link.Input.Endpoint,
		},
		Output: viewEndpointRefDocument{
			Component: link.Output.Component, Endpoint: link.Output.Endpoint,
		},
	}
}

func sortViewLinks(links []viewLinkDocument) {
	sort.Slice(links, func(i, j int) bool {
		left := links[i].Input.Component + "." + links[i].Input.Endpoint
		right := links[j].Input.Component + "." + links[j].Input.Endpoint
		if left != right {
			return left < right
		}
		return links[i].Output.Component+"."+links[i].Output.Endpoint <
			links[j].Output.Component+"."+links[j].Output.Endpoint
	})
}

func writeViewJSON(output io.Writer, document viewDocument) error {
	return writeDocument(output, document)
}

func printView(output io.Writer, document viewDocument) {
	switch {
	case document.Source == "file":
		fmt.Fprintf(output, "%s source=file\n", document.Name)
	case document.Operation != "":
		fmt.Fprintf(
			output,
			"%s operation=%s phase=%s digest=%s\n",
			document.Name, document.Operation, document.Phase, document.Digest,
		)
	case document.Desired:
		fmt.Fprintf(output, "%s desired=running digest=%s\n", document.Name, document.Digest)
	default:
		fmt.Fprintf(output, "%s desired=absent\n", document.Name)
	}
	if len(document.Components) != 0 {
		fmt.Fprintln(output, "COMPONENT\tIMAGE\tROUTE\tSTATUS\tHEALTH")
		for _, component := range document.Components {
			route := "internal"
			if component.Egress {
				route = "egress"
			}
			status, health := "-", "-"
			if component.Status != nil {
				status = component.Status.Status
				health = component.Status.Health
			}
			fmt.Fprintf(
				output,
				"%s\t%s\t%s\t%s\t%s\n",
				component.Name, component.ImageRef, route, status, health,
			)
		}
	}
	if len(document.Links) != 0 {
		fmt.Fprintln(output, "INPUT\tOUTPUT\tSERVICE")
		for _, link := range document.Links {
			fmt.Fprintf(
				output,
				"%s.%s\t%s\t%s\n",
				link.Input.Component, link.Input.Endpoint,
				viewTargetText(link),
				link.Service,
			)
		}
	}
	for _, global := range document.Globals {
		fmt.Fprintf(output, "global\t%s\t%s\t%s\n", global.Name, global.Service, global.Target.String())
	}
	for _, component := range document.Components {
		for _, published := range component.PublishedPorts {
			fmt.Fprintf(
				output,
				"publish\t%s\t%s\t%s:%d->%d\n",
				component.Name,
				published.Protocol,
				published.HostIP,
				published.HostPort,
				published.ContainerPort,
			)
		}
	}
	problems := make([]string, 0)
	for _, component := range document.Components {
		if component.Status != nil && component.Status.Problem != "" {
			problems = append(problems, fmt.Sprintf(
				"%s: %s",
				component.Name,
				strings.ReplaceAll(component.Status.Problem, "\n", " "),
			))
		}
	}
	for _, network := range document.Networks {
		if network.Problem != "" {
			problems = append(problems, fmt.Sprintf(
				"network %s: %s",
				network.Key,
				strings.ReplaceAll(network.Problem, "\n", " "),
			))
		}
	}
	if document.Proxy != nil && document.Proxy.Problem != "" {
		problems = append(problems, fmt.Sprintf(
			"proxy: %s",
			strings.ReplaceAll(document.Proxy.Problem, "\n", " "),
		))
	}
	for _, problem := range problems {
		fmt.Fprintf(output, "problem\t%s\n", problem)
	}
}

func sortedGlobals(globals []composition.Global) []composition.Global {
	result := append([]composition.Global(nil), globals...)
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func viewGlobalLink(link composition.Link, services map[string]string, globals []composition.Global) viewLinkDocument {
	name := link.Output.Global
	link.Output = composition.ResolveTarget(globals, link.Output)
	item := viewLink(link, services)
	item.Global = name
	return item
}

func viewTargetText(link viewLinkDocument) string {
	target := "unbound"
	if link.Output.Component != "" {
		target = link.Output.Component + "." + link.Output.Endpoint
	}
	if link.Global != "" {
		return "@" + link.Global + " -> " + target
	}
	return target
}
