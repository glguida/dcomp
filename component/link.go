// Package component contains optional Go helpers for DComp components that use
// gRPC. DComp itself treats application protocols and payloads as opaque.
package component

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

const linkPrefix = "DCOMP_LINK_"

var slotPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// LinkEnv returns the environment variable used for a required interface.
//
// Slot names are deliberately restricted so that every slot has exactly one
// portable environment-variable spelling. Hyphens become underscores.
func LinkEnv(slot string) (string, error) {
	if !slotPattern.MatchString(slot) {
		return "", fmt.Errorf("invalid dcomp link slot %q: use lower-case letters, digits, and hyphens", slot)
	}
	return linkPrefix + strings.ToUpper(strings.ReplaceAll(slot, "-", "_")), nil
}

// LinkTarget returns the address wired to slot by the DComp runtime.
//
// A typical value is "dns:///echo:50051". The value is opaque to dcomp and is
// returned unchanged. A gRPC client can use the value directly; another
// protocol implementation may interpret the injected address itself.
func LinkTarget(slot string) (string, error) {
	name, err := LinkEnv(slot)
	if err != nil {
		return "", err
	}
	target, ok := os.LookupEnv(name)
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return "", fmt.Errorf("required dcomp link %q is not configured (%s is empty)", slot, name)
	}
	return target, nil
}
