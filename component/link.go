// Package component contains optional Go helpers for DComp components. DComp
// treats application protocols and payloads as opaque.
package component

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/glguida/dcomp/internal/runtimecontract"
)

func InputEnv(slot string) (string, error) {
	return endpointEnv(runtimecontract.InputEnvironment, slot)
}

func OutputEnv(slot string) (string, error) {
	return endpointEnv(runtimecontract.OutputEnvironment, slot)
}

func InputTarget(slot string) (string, error) {
	return endpointTarget(runtimecontract.InputEnvironment, "input", slot)
}

func OutputTarget(slot string) (string, error) {
	return endpointTarget(runtimecontract.OutputEnvironment, "output", slot)
}

func endpointEnv(environment func(string) string, slot string) (string, error) {
	if !runtimecontract.ValidEndpointName(slot) {
		return "", fmt.Errorf("invalid dcomp interface name %q: use lower-case letters, digits, and hyphens", slot)
	}
	return environment(slot), nil
}

func endpointTarget(environment func(string) string, direction, slot string) (string, error) {
	name, err := endpointEnv(environment, slot)
	if err != nil {
		return "", err
	}
	target, ok := os.LookupEnv(name)
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return "", fmt.Errorf("required dcomp %s %q is not configured (%s is empty)", direction, slot, name)
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "unix" || !filepath.IsAbs(parsed.Path) || parsed.Host != "" {
		return "", fmt.Errorf("%s must contain an absolute unix:/// path", name)
	}
	return target, nil
}

func unixPath(target string) (string, error) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "unix" || !filepath.IsAbs(parsed.Path) || parsed.Host != "" {
		return "", fmt.Errorf("invalid DComp Unix address %q", target)
	}
	return parsed.Path, nil
}
