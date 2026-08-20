// Package runtimecontract defines the component-facing interface addresses
// shared by the orchestrator and component helpers.
package runtimecontract

import (
	"path"
	"regexp"
	"strings"
)

// ContainerRoot is reserved for orchestrator-owned interface socket mounts.
const ContainerRoot = "/run/dcomp"

var endpointNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidEndpointName reports whether name can be used as an interface name.
func ValidEndpointName(name string) bool {
	return endpointNamePattern.MatchString(name)
}

func InputEnvironment(name string) string {
	return environment("DCOMP_IN_", name)
}

func OutputEnvironment(name string) string {
	return environment("DCOMP_OUT_", name)
}

func InputSocket(name string) string {
	return path.Join(ContainerRoot, "in", name)
}

func OutputSocket(name string) string {
	return path.Join(ContainerRoot, "out", name)
}

func InputURI(name string) string {
	return "unix://" + InputSocket(name)
}

func OutputURI(name string) string {
	return "unix://" + OutputSocket(name)
}

func environment(prefix, name string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}
