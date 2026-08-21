// Package proxy implements the per-system DComp 0.2 data-plane proxy and
// the host process used to manage it.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/glguida/dcomp/composition"
)

const ConfigVersion = 2

func DefaultRuntimeRoot(stateRoot string) (string, error) {
	if root := os.Getenv("DCOMP_RUNTIME_ROOT"); root != "" {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return "", fmt.Errorf("DCOMP_RUNTIME_ROOT must be an absolute clean path")
		}
		return root, nil
	}
	if !filepath.IsAbs(stateRoot) {
		return "", fmt.Errorf("state root must be an absolute path")
	}
	return filepath.Join(stateRoot, "run"), nil
}

const (
	ControlSocketName = "proxy.sock"
	ConfigFileName    = "proxy.json"
	PIDFileName       = "proxy.pid"
	LogFileName       = "proxy.log"
	ReadyFileName     = "proxy.ready"
)

type Direction string

const (
	DirectionInput  Direction = "input"
	DirectionOutput Direction = "output"
)

// Endpoint is one host-side Unix socket owned by the proxy.
type Endpoint struct {
	Component string    `json:"component"`
	Name      string    `json:"name"`
	Direction Direction `json:"direction"`
	Socket    string    `json:"socket"`
}

// Link routes one input socket to one output socket.
type Link struct {
	InputComponent  string `json:"input_component"`
	InputEndpoint   string `json:"input_endpoint"`
	OutputComponent string `json:"output_component"`
	OutputEndpoint  string `json:"output_endpoint"`
}

// Config is the complete immutable wiring consumed by dcomp-proxy. InstanceID
// identifies one launched process; Digest identifies the wiring independently
// of that particular process.
type Config struct {
	Version    int        `json:"version"`
	System     string     `json:"system"`
	InstanceID string     `json:"instance_id"`
	RuntimeDir string     `json:"runtime_dir"`
	Digest     string     `json:"digest"`
	Endpoints  []Endpoint `json:"endpoints"`
	Links      []Link     `json:"links"`
}

// NewConfig converts a resolved composition into a deterministic proxy
// configuration rooted at runtimeDir.
func NewConfig(
	spec composition.ResolvedSpec,
	runtimeDir string,
	instanceID string,
) (Config, error) {
	if !composition.ValidName(spec.Name) {
		return Config{}, fmt.Errorf("invalid proxy system name %q", spec.Name)
	}
	if strings.TrimSpace(instanceID) == "" {
		return Config{}, fmt.Errorf("proxy instance ID is missing")
	}
	absolute, err := filepath.Abs(runtimeDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve proxy runtime directory: %w", err)
	}
	if filepath.Clean(runtimeDir) != absolute {
		return Config{}, fmt.Errorf("proxy runtime directory must be an absolute clean path")
	}

	config := Config{
		Version: ConfigVersion, System: spec.Name, InstanceID: instanceID,
		RuntimeDir: absolute,
	}
	for _, component := range spec.Components {
		for _, endpoint := range component.Definition.Inputs {
			config.Endpoints = append(config.Endpoints, Endpoint{
				Component: component.Name,
				Name:      endpoint.Name,
				Direction: DirectionInput,
				Socket:    HostSocket(absolute, DirectionInput, component.Name, endpoint.Name),
			})
		}
		for _, endpoint := range component.Definition.Outputs {
			config.Endpoints = append(config.Endpoints, Endpoint{
				Component: component.Name,
				Name:      endpoint.Name,
				Direction: DirectionOutput,
				Socket:    HostSocket(absolute, DirectionOutput, component.Name, endpoint.Name),
			})
		}
	}
	for _, link := range spec.Links {
		config.Links = append(config.Links, Link{
			InputComponent:  link.Input.Component,
			InputEndpoint:   link.Input.Endpoint,
			OutputComponent: link.Output.Component,
			OutputEndpoint:  link.Output.Endpoint,
		})
	}
	config.canonicalize()
	digest, err := config.computeDigest()
	if err != nil {
		return Config{}, err
	}
	config.Digest = digest
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// LoadConfig decodes a strict proxy configuration.
func LoadConfig(data []byte) (Config, error) {
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode proxy config: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode proxy config: trailing data")
	}
	config.canonicalize()
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) Validate() error {
	// Version 2 changes only the proxy status surface. The wiring schema is
	// unchanged, so version-1 files remain readable for verified cleanup and
	// upgrade recovery. New configurations are always emitted at the current
	// version.
	if config.Version < 1 || config.Version > ConfigVersion {
		return fmt.Errorf("unsupported proxy config version %d", config.Version)
	}
	if !composition.ValidName(config.System) {
		return fmt.Errorf("invalid proxy system name %q", config.System)
	}
	if strings.TrimSpace(config.InstanceID) == "" {
		return fmt.Errorf("proxy instance ID is missing")
	}
	if !filepath.IsAbs(config.RuntimeDir) || filepath.Clean(config.RuntimeDir) != config.RuntimeDir {
		return fmt.Errorf("proxy runtime directory must be an absolute clean path")
	}

	endpoints := make(map[string]Endpoint, len(config.Endpoints))
	for _, endpoint := range config.Endpoints {
		if !composition.ValidName(endpoint.Component) || !composition.ValidName(endpoint.Name) {
			return fmt.Errorf("invalid proxy endpoint %q.%q", endpoint.Component, endpoint.Name)
		}
		if endpoint.Direction != DirectionInput && endpoint.Direction != DirectionOutput {
			return fmt.Errorf("invalid direction %q for %s.%s", endpoint.Direction, endpoint.Component, endpoint.Name)
		}
		key := endpointKey(endpoint.Direction, endpoint.Component, endpoint.Name)
		if _, exists := endpoints[key]; exists {
			return fmt.Errorf("proxy endpoint %s is declared more than once", key)
		}
		wantSocket := HostSocket(config.RuntimeDir, endpoint.Direction, endpoint.Component, endpoint.Name)
		if endpoint.Socket != wantSocket {
			return fmt.Errorf("proxy endpoint %s has unexpected socket %q", key, endpoint.Socket)
		}
		endpoints[key] = endpoint
	}

	linkedInputs := make(map[string]struct{}, len(config.Links))
	for _, link := range config.Links {
		inputKey := endpointKey(DirectionInput, link.InputComponent, link.InputEndpoint)
		outputKey := endpointKey(DirectionOutput, link.OutputComponent, link.OutputEndpoint)
		if _, exists := endpoints[inputKey]; !exists {
			return fmt.Errorf("proxy link names unknown input %s", inputKey)
		}
		if _, exists := endpoints[outputKey]; !exists {
			return fmt.Errorf("proxy link names unknown output %s", outputKey)
		}
		if _, exists := linkedInputs[inputKey]; exists {
			return fmt.Errorf("proxy input %s is linked more than once", inputKey)
		}
		linkedInputs[inputKey] = struct{}{}
	}
	for key, endpoint := range endpoints {
		if endpoint.Direction != DirectionInput {
			continue
		}
		if _, exists := linkedInputs[key]; !exists {
			return fmt.Errorf("proxy input %s is not linked", key)
		}
	}

	digest, err := config.computeDigest()
	if err != nil {
		return err
	}
	if config.Digest != digest {
		return fmt.Errorf("proxy config digest mismatch: expected %s, found %s", digest, config.Digest)
	}
	return nil
}

func (config *Config) canonicalize() {
	sort.Slice(config.Endpoints, func(i, j int) bool {
		left, right := config.Endpoints[i], config.Endpoints[j]
		return endpointKey(left.Direction, left.Component, left.Name) <
			endpointKey(right.Direction, right.Component, right.Name)
	})
	sort.Slice(config.Links, func(i, j int) bool {
		left, right := config.Links[i], config.Links[j]
		leftInput := endpointKey(DirectionInput, left.InputComponent, left.InputEndpoint)
		rightInput := endpointKey(DirectionInput, right.InputComponent, right.InputEndpoint)
		if leftInput != rightInput {
			return leftInput < rightInput
		}
		return endpointKey(DirectionOutput, left.OutputComponent, left.OutputEndpoint) <
			endpointKey(DirectionOutput, right.OutputComponent, right.OutputEndpoint)
	})
}

func (config Config) computeDigest() (string, error) {
	copy := config
	copy.InstanceID = ""
	copy.Digest = ""
	copy.canonicalize()
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("encode proxy identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func HostSocket(runtimeDir string, direction Direction, component, endpoint string) string {
	directory := "in"
	if direction == DirectionOutput {
		directory = "out"
	}
	return filepath.Join(runtimeDir, directory, component+"."+endpoint)
}

func ControlSocket(runtimeDir string) string {
	return socketListenPath(filepath.Join(runtimeDir, ControlSocketName))
}

func controlSocketFile(runtimeDir string) string {
	return filepath.Join(runtimeDir, ControlSocketName)
}

func socketListenPath(exposedPath string) string {
	if len(exposedPath) < 104 {
		return exposedPath
	}
	sum := sha256.Sum256([]byte(exposedPath))
	directory := filepath.Dir(exposedPath)
	// Endpoint sockets are one directory below the per-system runtime
	// directory; the control socket is directly inside it. Start at the common
	// runtime root so the shortened socket and its exposed hard link stay on
	// the same filesystem (notably when /var/run is tmpfs and /tmp is not).
	if base := filepath.Base(directory); base == "in" || base == "out" {
		directory = filepath.Dir(directory)
	}
	root := filepath.Dir(directory)
	for {
		candidate := filepath.Join(
			root,
			fmt.Sprintf(".dcomp-proxy-%d", os.Getuid()),
			hex.EncodeToString(sum[:16]),
		)
		if len(candidate) < 104 {
			return candidate
		}
		parent := filepath.Dir(root)
		if parent == root {
			return candidate
		}
		root = parent
	}
}

func endpointKey(direction Direction, component, endpoint string) string {
	return string(direction) + "/" + component + "/" + endpoint
}
