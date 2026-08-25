package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/glguida/dcomp/lifecycle"
	"github.com/glguida/dcomp/proxy"
)

const (
	apiVersion = 2
	version    = "0.2.1"
)

type versionDocument struct {
	Version    string `json:"version"`
	APIVersion int    `json:"api_version"`
}

type statusDocument struct {
	APIVersion         int                       `json:"api_version"`
	Name               string                    `json:"name"`
	Desired            bool                      `json:"desired"`
	Operational        bool                      `json:"operational"`
	Digest             string                    `json:"digest"`
	Operation          string                    `json:"operation"`
	Phase              string                    `json:"phase"`
	Proxy              proxyStatusDocument       `json:"proxy"`
	Networks           []networkStatusDocument   `json:"networks"`
	Components         []componentStatusDocument `json:"components"`
	RetiringNetworks   []networkStatusDocument   `json:"retiring_networks,omitempty"`
	RetiringComponents []componentStatusDocument `json:"retiring_components,omitempty"`
}

type proxyStatusDocument struct {
	InstanceID        string              `json:"instance_id"`
	Digest            string              `json:"digest,omitempty"`
	PID               int                 `json:"pid"`
	Ready             bool                `json:"ready"`
	Inputs            int                 `json:"inputs"`
	Outputs           int                 `json:"outputs"`
	ActiveConnections int64               `json:"active_connections"`
	Links             []proxy.LinkMetrics `json:"links,omitempty"`
	Problem           string              `json:"problem"`
}

type volumeDocument struct {
	APIVersion  int    `json:"api_version"`
	System      string `json:"system"`
	Component   string `json:"component"`
	LogicalName string `json:"logical_name"`
	Name        string `json:"name"`
}

type processesDocument struct {
	APIVersion int                        `json:"api_version"`
	Components []processComponentDocument `json:"components"`
}

type processComponentDocument struct {
	System         string                  `json:"system"`
	Component      string                  `json:"component"`
	ContainerID    string                  `json:"container_id"`
	Status         string                  `json:"status"`
	Health         string                  `json:"health"`
	ExitCode       int                     `json:"exit_code"`
	Problem        string                  `json:"problem"`
	Operation      string                  `json:"operation"`
	Phase          string                  `json:"phase"`
	PublishedPorts []publishedPortDocument `json:"published_ports"`
}

type networkStatusDocument struct {
	Key      string `json:"key"`
	ID       string `json:"id"`
	Internal bool   `json:"internal"`
	Problem  string `json:"problem"`
}

type componentStatusDocument struct {
	Name           string                  `json:"name"`
	ContainerID    string                  `json:"container_id"`
	Status         string                  `json:"status"`
	Health         string                  `json:"health"`
	ExitCode       int                     `json:"exit_code"`
	Problem        string                  `json:"problem"`
	PublishedPorts []publishedPortDocument `json:"published_ports"`
}

type publishedPortDocument struct {
	Protocol      string `json:"protocol"`
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

func writeVersion(output io.Writer, jsonOutput bool) error {
	if !jsonOutput {
		_, err := fmt.Fprintf(output, "dcomp %s\n", version)
		return err
	}
	return writeDocument(output, versionDocument{
		Version: version, APIVersion: apiVersion,
	})
}

func writeStatusJSON(output io.Writer, status lifecycle.Status) error {
	document := statusDocument{
		APIVersion:  apiVersion,
		Name:        status.Name,
		Desired:     status.Desired,
		Operational: status.Operational(),
		Digest:      status.Digest,
		Operation:   status.Operation,
		Phase:       status.Phase,
		Proxy: proxyStatusDocument{
			InstanceID:        status.Proxy.InstanceID,
			Digest:            status.Proxy.Digest,
			PID:               status.Proxy.PID,
			Ready:             status.Proxy.Ready,
			Inputs:            status.Proxy.Inputs,
			Outputs:           status.Proxy.Outputs,
			ActiveConnections: status.Proxy.ActiveConnections,
			Links:             append([]proxy.LinkMetrics(nil), status.Proxy.Links...),
			Problem:           status.Proxy.Problem,
		},
		Networks:   make([]networkStatusDocument, 0, len(status.Networks)),
		Components: make([]componentStatusDocument, 0, len(status.Components)),
	}
	for _, network := range status.Networks {
		document.Networks = append(document.Networks, networkStatusDocument{
			Key: network.Key, ID: network.ID,
			Internal: network.Internal, Problem: network.Problem,
		})
	}
	for _, component := range status.Components {
		document.Components = append(document.Components, componentStatusJSON(component))
	}
	for _, network := range status.RetiringNetworks {
		document.RetiringNetworks = append(
			document.RetiringNetworks,
			networkStatusDocument{
				Key: network.Key, ID: network.ID,
				Internal: network.Internal, Problem: network.Problem,
			},
		)
	}
	for _, component := range status.RetiringComponents {
		document.RetiringComponents = append(
			document.RetiringComponents,
			componentStatusJSON(component),
		)
	}
	return writeDocument(output, document)
}

func componentStatusJSON(component lifecycle.ComponentStatus) componentStatusDocument {
	item := componentStatusDocument{
		Name: component.Name, ContainerID: component.ID,
		Status: component.Status, Health: string(component.Health),
		ExitCode: component.ExitCode, Problem: component.Problem,
		PublishedPorts: make(
			[]publishedPortDocument, 0, len(component.PublishedPorts),
		),
	}
	for _, published := range component.PublishedPorts {
		item.PublishedPorts = append(item.PublishedPorts, publishedPortDocument{
			Protocol: string(published.Protocol), HostIP: published.HostIP,
			HostPort: published.HostPort, ContainerPort: published.ContainerPort,
		})
	}
	return item
}

func writeProcessesJSON(
	output io.Writer,
	processes []lifecycle.ComponentProcess,
) error {
	document := processesDocument{
		APIVersion: apiVersion,
		Components: make([]processComponentDocument, 0, len(processes)),
	}
	for _, process := range processes {
		item := processComponentDocument{
			System: process.System, Component: process.Name,
			ContainerID: process.ID, Status: process.Status,
			Health: string(process.Health), ExitCode: process.ExitCode,
			Problem: process.Problem, Operation: process.Operation,
			Phase: process.Phase,
			PublishedPorts: make(
				[]publishedPortDocument,
				0,
				len(process.PublishedPorts),
			),
		}
		for _, published := range process.PublishedPorts {
			item.PublishedPorts = append(
				item.PublishedPorts,
				publishedPortDocument{
					Protocol:      string(published.Protocol),
					HostIP:        published.HostIP,
					HostPort:      published.HostPort,
					ContainerPort: published.ContainerPort,
				},
			)
		}
		document.Components = append(document.Components, item)
	}
	return writeDocument(output, document)
}

func writeVolumeJSON(output io.Writer, volume lifecycle.PersistentVolume) error {
	return writeDocument(output, volumeDocument{
		APIVersion: apiVersion, System: volume.System,
		Component: volume.Component, LogicalName: volume.LogicalName,
		Name: volume.Name,
	})
}

func writeDocument(output io.Writer, value interface{}) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
