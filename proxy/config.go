// Package proxy implements the per-system DComp data-plane proxy and
// the host process used to manage it.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/glguida/dcomp/hostfs"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/glguida/dcomp/composition"
)

// Version 4 adds symbolic global interfaces and permits unconnected inputs.
// The output origin header introduced in 0.2.2 is unchanged.
const ConfigVersion = 4

func DefaultRuntimeRoot(stateRoot string) (string, error) {
	if root := os.Getenv("DCOMP_RUNTIME_ROOT"); root != "" {
		if !filepath.IsAbs(root) {
			return "", fmt.Errorf("DCOMP_RUNTIME_ROOT must be an absolute path")
		}
		return filepath.Clean(root), nil
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

// EndpointIdentity is the filesystem-independent identity of one endpoint.
// Its host socket path is derived only after a Wiring is attached to a fixed
// proxy runtime directory.
type EndpointIdentity struct {
	Component string    `json:"component"`
	Name      string    `json:"name"`
	Direction Direction `json:"direction"`
}

// Link routes one input socket to one output socket.
type Link struct {
	InputComponent  string `json:"input_component"`
	InputEndpoint   string `json:"input_endpoint"`
	OutputComponent string `json:"output_component"`
	OutputEndpoint  string `json:"output_endpoint"`
	Global          string `json:"global,omitempty"`
}

// Wiring is the canonical, process-independent data-plane content served by a
// proxy. Its digest excludes process identity, system name, runtime paths, and
// schema versions.
type Wiring struct {
	Endpoints []EndpointIdentity `json:"endpoints"`
	Links     []Link             `json:"links"`
	Globals   []Global           `json:"globals,omitempty"`
}

// Config is a complete wiring snapshot consumed by dcomp-proxy. InstanceID
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
	Globals    []Global   `json:"globals,omitempty"`
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

	wiring, err := NewWiring(spec)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		Version: ConfigVersion, System: spec.Name, InstanceID: instanceID,
		RuntimeDir: absolute,
	}
	for _, endpoint := range wiring.Endpoints {
		config.Endpoints = append(config.Endpoints, Endpoint{
			Component: endpoint.Component,
			Name:      endpoint.Name,
			Direction: endpoint.Direction,
			Socket:    HostSocket(absolute, endpoint.Direction, endpoint.Component, endpoint.Name),
		})
	}
	config.Links = append(config.Links, wiring.Links...)
	config.Globals = append([]Global(nil), wiring.Globals...)
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

// NewWiring derives the complete canonical data-plane wiring from a resolved
// system without including any process or host-path identity.
func NewWiring(spec composition.ResolvedSpec) (Wiring, error) {
	wiring := Wiring{
		Endpoints: make([]EndpointIdentity, 0),
		Links:     make([]Link, 0, len(spec.Links)),
	}
	for _, component := range spec.Components {
		for _, endpoint := range component.Definition.Inputs {
			wiring.Endpoints = append(wiring.Endpoints, EndpointIdentity{
				Component: component.Name, Name: endpoint.Name, Direction: DirectionInput,
			})
		}
		for _, endpoint := range component.Definition.Outputs {
			wiring.Endpoints = append(wiring.Endpoints, EndpointIdentity{
				Component: component.Name, Name: endpoint.Name, Direction: DirectionOutput,
			})
		}
	}
	for _, link := range spec.Links {
		wiring.Links = append(wiring.Links, Link{
			InputComponent: link.Input.Component, InputEndpoint: link.Input.Endpoint,
			OutputComponent: link.Output.Component, OutputEndpoint: link.Output.Endpoint, Global: link.Output.Global,
		})
	}
	for _, global := range spec.Globals {
		wiring.Globals = append(wiring.Globals, Global{Name: global.Name, OutputComponent: global.Target.Component, OutputEndpoint: global.Target.Endpoint})
	}
	wiring.canonicalize()
	if err := wiring.Validate(); err != nil {
		return Wiring{}, err
	}
	return wiring, nil
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
	if config.Version != ConfigVersion {
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

	if err := config.Wiring().Validate(); err != nil {
		return err
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

// Wiring returns the path-independent content carried by a Config.
func (config Config) Wiring() Wiring {
	wiring := Wiring{
		Endpoints: make([]EndpointIdentity, 0, len(config.Endpoints)),
		Links:     append([]Link(nil), config.Links...),
		Globals:   append([]Global(nil), config.Globals...),
	}
	for _, endpoint := range config.Endpoints {
		wiring.Endpoints = append(wiring.Endpoints, EndpointIdentity{
			Component: endpoint.Component, Name: endpoint.Name, Direction: endpoint.Direction,
		})
	}
	wiring.canonicalize()
	return wiring
}

// Validate verifies endpoints, globals, and direct or symbolic links. Inputs
// have at most one target; outputs may be unused or shared by many inputs.
func (wiring Wiring) Validate() error {
	endpoints := make(map[string]EndpointIdentity, len(wiring.Endpoints))
	for _, endpoint := range wiring.Endpoints {
		if !composition.ValidName(endpoint.Component) || !composition.ValidName(endpoint.Name) {
			return fmt.Errorf("invalid proxy endpoint %q.%q", endpoint.Component, endpoint.Name)
		}
		if endpoint.Direction != DirectionInput && endpoint.Direction != DirectionOutput {
			return fmt.Errorf(
				"invalid direction %q for %s.%s",
				endpoint.Direction, endpoint.Component, endpoint.Name,
			)
		}
		key := endpointIdentityKey(endpoint)
		if _, exists := endpoints[key]; exists {
			return fmt.Errorf("proxy endpoint %s is declared more than once", key)
		}
		endpoints[key] = endpoint
	}
	globals := make(map[string]Global, len(wiring.Globals))
	for _, global := range wiring.Globals {
		if !composition.ValidGlobalName(global.Name) {
			return fmt.Errorf("invalid global interface name %q", global.Name)
		}
		if _, exists := globals[global.Name]; exists {
			return fmt.Errorf("global %s is declared more than once", global.Name)
		}
		if global.OutputComponent != "" || global.OutputEndpoint != "" {
			if _, exists := endpoints[endpointKey(DirectionOutput, global.OutputComponent, global.OutputEndpoint)]; !exists {
				return fmt.Errorf("global %s names unknown output", global.Name)
			}
		}
		globals[global.Name] = global
	}
	linkedInputs := make(map[string]struct{}, len(wiring.Links))
	for _, link := range wiring.Links {
		inputKey := linkInputKey(link)
		outputKey := linkOutputKey(link)
		if _, exists := endpoints[inputKey]; !exists {
			return fmt.Errorf("proxy link names unknown input %s", inputKey)
		}
		if link.Global != "" {
			if link.OutputComponent != "" || link.OutputEndpoint != "" {
				return fmt.Errorf("proxy link mixes global and direct targets")
			}
			if _, exists := globals[link.Global]; !exists {
				return fmt.Errorf("proxy link names unknown global %s", link.Global)
			}
		} else if _, exists := endpoints[outputKey]; !exists {
			return fmt.Errorf("proxy link names unknown output %s", outputKey)
		}
		if _, exists := linkedInputs[inputKey]; exists {
			return fmt.Errorf("proxy input %s is linked more than once", inputKey)
		}
		linkedInputs[inputKey] = struct{}{}
	}

	return nil
}

// Digest returns SHA-256 over only the canonical endpoint and link sets.
func (wiring Wiring) Digest() (string, error) {
	copy := wiring
	copy.Endpoints = append([]EndpointIdentity(nil), wiring.Endpoints...)
	copy.Links = append([]Link(nil), wiring.Links...)
	copy.Globals = append([]Global(nil), wiring.Globals...)
	copy.canonicalize()
	if err := copy.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("encode proxy wiring: %w", err)
	}
	return digestBytes(encoded), nil
}

func (wiring *Wiring) canonicalize() {
	sort.Slice(wiring.Globals, func(i, j int) bool { return wiring.Globals[i].Name < wiring.Globals[j].Name })
	if wiring.Endpoints == nil {
		wiring.Endpoints = make([]EndpointIdentity, 0)
	}
	if wiring.Links == nil {
		wiring.Links = make([]Link, 0)
	}
	sort.Slice(wiring.Endpoints, func(i, j int) bool {
		return endpointIdentityKey(wiring.Endpoints[i]) < endpointIdentityKey(wiring.Endpoints[j])
	})
	sort.Slice(wiring.Links, func(i, j int) bool {
		leftInput, rightInput := linkInputKey(wiring.Links[i]), linkInputKey(wiring.Links[j])
		if leftInput != rightInput {
			return leftInput < rightInput
		}
		return linkOutputKey(wiring.Links[i]) < linkOutputKey(wiring.Links[j])
	})
}

func (config *Config) canonicalize() {
	sort.Slice(config.Globals, func(i, j int) bool { return config.Globals[i].Name < config.Globals[j].Name })
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
	return config.Wiring().Digest()
}

func digestBytes(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
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
	return shortenedSocketPath(exposedPath)
}

// shortenedSocketPath returns the hidden same-filesystem socket path even for
// an exposed path that would itself fit the kernel limit. Publication uses
// this when there is room for the final path but not for a temporary bind name
// in the same directory.
func shortenedSocketPath(exposedPath string) string {
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
	anchor := fmt.Sprintf(".dcomp-proxy-%d", os.Getuid())
	if gid, shared := hostfs.Group(directory); shared {
		anchor = fmt.Sprintf(".dcomp-proxy-g%d", gid)
	}
	for {
		candidate := filepath.Join(
			root,
			anchor,
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

func endpointIdentityKey(endpoint EndpointIdentity) string {
	return endpointKey(endpoint.Direction, endpoint.Component, endpoint.Name)
}

func linkInputKey(link Link) string {
	return endpointKey(DirectionInput, link.InputComponent, link.InputEndpoint)
}

func linkOutputKey(link Link) string {
	return endpointKey(DirectionOutput, link.OutputComponent, link.OutputEndpoint)
}

func linkKey(link Link) string {
	return linkInputKey(link) + "\x00" + linkOutputKey(link)
}
