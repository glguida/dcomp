package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/glguida/dcomp/lifecycle"
)

const (
	apiVersion = 1
	version    = "0.1.2"
)

type versionDocument struct {
	Version    string `json:"version"`
	APIVersion int    `json:"api_version"`
}

type statusDocument struct {
	APIVersion  int                       `json:"api_version"`
	Name        string                    `json:"name"`
	Desired     bool                      `json:"desired"`
	Operational bool                      `json:"operational"`
	Digest      string                    `json:"digest"`
	Operation   string                    `json:"operation"`
	Phase       string                    `json:"phase"`
	Networks    []networkStatusDocument   `json:"networks"`
	Components  []componentStatusDocument `json:"components"`
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
		Networks:    make([]networkStatusDocument, 0, len(status.Networks)),
		Components:  make([]componentStatusDocument, 0, len(status.Components)),
	}
	for _, network := range status.Networks {
		document.Networks = append(document.Networks, networkStatusDocument{
			Key: network.Key, ID: network.ID,
			Internal: network.Internal, Problem: network.Problem,
		})
	}
	for _, component := range status.Components {
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
		document.Components = append(document.Components, item)
	}
	return writeDocument(output, document)
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
