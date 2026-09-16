// Package dockerengine implements engine.Engine against a local Docker Engine
// Unix socket. It deliberately rejects remote daemons.
package dockerengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/glguida/dcomp/engine"
)

const (
	minAPIVersion = "1.44"
	maxAPIVersion = "1.47"
)

type Client struct {
	httpClient *http.Client
	socketPath string

	versionMu sync.Mutex
	version   string
}

type APIError struct {
	StatusCode int
	Message    string
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func (err *APIError) Error() string {
	if err.Message == "" {
		return fmt.Sprintf("Docker API returned HTTP %d", err.StatusCode)
	}
	return fmt.Sprintf("Docker API returned HTTP %d: %s", err.StatusCode, err.Message)
}

func NewFromEnvironment() (*Client, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	if !strings.HasPrefix(host, "unix://") {
		return nil, fmt.Errorf("dcomp requires a local Docker Unix socket; DOCKER_HOST=%q", host)
	}
	path := strings.TrimPrefix(host, "unix://")
	if path == "" || path[0] != '/' {
		return nil, fmt.Errorf("invalid Docker Unix socket %q", path)
	}

	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		},
	}
	return &Client{
		httpClient: &http.Client{Transport: transport},
		socketPath: path,
	}, nil
}

func (client *Client) Identity(ctx context.Context) (string, error) {
	var raw struct {
		ID string `json:"ID"`
	}
	if err := client.do(ctx, http.MethodGet, "/info", nil, &raw); err != nil {
		return "", err
	}
	if raw.ID == "" {
		return "", fmt.Errorf("Docker /info returned an empty engine ID")
	}
	return raw.ID, nil
}

func (client *Client) ResolveImage(ctx context.Context, reference string) (engine.Image, error) {
	var raw struct {
		ID     string `json:"Id"`
		Config struct {
			Volumes     map[string]json.RawMessage `json:"Volumes"`
			Healthcheck *struct {
				Test []string `json:"Test"`
			} `json:"Healthcheck"`
		} `json:"Config"`
	}
	if err := client.do(ctx, http.MethodGet, "/images/"+url.PathEscape(reference)+"/json", nil, &raw); err != nil {
		return engine.Image{}, err
	}
	hasHealthcheck := raw.Config.Healthcheck != nil &&
		len(raw.Config.Healthcheck.Test) > 0 &&
		!(len(raw.Config.Healthcheck.Test) == 1 && raw.Config.Healthcheck.Test[0] == "NONE")
	volumeTargets := make([]string, 0, len(raw.Config.Volumes))
	for target := range raw.Config.Volumes {
		volumeTargets = append(volumeTargets, target)
	}
	sort.Strings(volumeTargets)
	return engine.Image{
		ID: raw.ID, HasHealthcheck: hasHealthcheck, DeclaredVolumes: volumeTargets,
	}, nil
}

func (client *Client) InspectNetwork(ctx context.Context, idOrName string) (engine.Network, error) {
	type endpoint struct {
		Name       string `json:"Name"`
		EndpointID string `json:"EndpointID"`
	}
	var raw struct {
		ID         string              `json:"Id"`
		Name       string              `json:"Name"`
		Driver     string              `json:"Driver"`
		Internal   bool                `json:"Internal"`
		Labels     map[string]string   `json:"Labels"`
		Containers map[string]endpoint `json:"Containers"`
	}
	if err := client.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(idOrName), nil, &raw); err != nil {
		return engine.Network{}, err
	}
	endpoints := make([]engine.NetworkEndpoint, 0, len(raw.Containers))
	for key, rawEndpoint := range raw.Containers {
		endpoints = append(endpoints, engine.NetworkEndpoint{
			Key:        key,
			Name:       rawEndpoint.Name,
			EndpointID: rawEndpoint.EndpointID,
		})
	}
	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].Key < endpoints[j].Key
	})
	return engine.Network{
		ID: raw.ID, Name: raw.Name, Driver: raw.Driver, Internal: raw.Internal,
		Labels: cloneMap(raw.Labels), Endpoints: endpoints,
	}, nil
}

func (client *Client) CreateNetwork(ctx context.Context, request engine.NetworkRequest) (engine.Network, error) {
	body := struct {
		Name     string            `json:"Name"`
		Driver   string            `json:"Driver"`
		Internal bool              `json:"Internal"`
		Labels   map[string]string `json:"Labels"`
	}{
		Name: request.Name, Driver: "bridge", Internal: request.Internal, Labels: request.Labels,
	}
	var response struct {
		ID      string `json:"Id"`
		Warning string `json:"Warning"`
	}
	if err := client.do(ctx, http.MethodPost, "/networks/create", body, &response); err != nil {
		return engine.Network{}, err
	}
	if response.ID == "" {
		return engine.Network{}, fmt.Errorf("Docker created network %q without returning an ID", request.Name)
	}
	return client.InspectNetwork(ctx, response.ID)
}

func (client *Client) RemoveNetwork(ctx context.Context, id string) error {
	return client.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(id), nil, nil)
}

func (client *Client) InspectVolume(ctx context.Context, name string) (engine.Volume, error) {
	var raw struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		Mountpoint string            `json:"Mountpoint"`
		Labels     map[string]string `json:"Labels"`
	}
	if err := client.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &raw); err != nil {
		return engine.Volume{}, err
	}
	return engine.Volume{
		Name: raw.Name, Driver: raw.Driver, Mountpoint: raw.Mountpoint,
		Labels: cloneMap(raw.Labels),
	}, nil
}

func (client *Client) CreateVolume(ctx context.Context, request engine.VolumeRequest) (engine.Volume, error) {
	body := struct {
		Name   string            `json:"Name"`
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}{
		Name: request.Name, Driver: "local", Labels: request.Labels,
	}
	var raw struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		Mountpoint string            `json:"Mountpoint"`
		Labels     map[string]string `json:"Labels"`
	}
	if err := client.do(ctx, http.MethodPost, "/volumes/create", body, &raw); err != nil {
		return engine.Volume{}, err
	}
	if raw.Name == "" {
		return engine.Volume{}, fmt.Errorf("Docker created volume %q without returning a name", request.Name)
	}
	return engine.Volume{
		Name: raw.Name, Driver: raw.Driver, Mountpoint: raw.Mountpoint,
		Labels: cloneMap(raw.Labels),
	}, nil
}

func (client *Client) InspectContainer(ctx context.Context, idOrName string) (engine.Container, error) {
	var raw struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			User      string            `json:"User"`
			Labels    map[string]string `json:"Labels"`
			Cmd       []string          `json:"Cmd"`
			Env       []string          `json:"Env"`
			OpenStdin bool              `json:"OpenStdin"`
			StdinOnce bool              `json:"StdinOnce"`
			Tty       bool              `json:"Tty"`
		} `json:"Config"`
		HostConfig struct {
			Init          *bool `json:"Init"`
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
			PortBindings map[string][]dockerPortBinding `json:"PortBindings"`
			SecurityOpt  []string                       `json:"SecurityOpt"`
			CapDrop      []string                       `json:"CapDrop"`
			PidsLimit    int64                          `json:"PidsLimit"`
		} `json:"HostConfig"`
		Mounts []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} `json:"Mounts"`
		State struct {
			Status   string `json:"Status"`
			Running  bool   `json:"Running"`
			ExitCode int    `json:"ExitCode"`
			Error    string `json:"Error"`
			Health   *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				NetworkID string   `json:"NetworkID"`
				Aliases   []string `json:"Aliases"`
			} `json:"Networks"`
			Ports map[string][]dockerPortBinding `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := client.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(idOrName)+"/json", nil, &raw); err != nil {
		return engine.Container{}, err
	}
	health := engine.HealthNone
	if raw.State.Health != nil {
		health = engine.Health(raw.State.Health.Status)
	}
	networks := make(map[string]engine.NetworkAttachment, len(raw.NetworkSettings.Networks))
	for name, attachment := range raw.NetworkSettings.Networks {
		// Docker reports its built-in "none" network in NetworkSettings even
		// though NetworkMode=none has no attachable network resource.
		if name == "none" {
			continue
		}
		networks[name] = engine.NetworkAttachment{
			NetworkID: attachment.NetworkID,
			Aliases:   cloneSlice(attachment.Aliases),
		}
	}
	mounts := make([]engine.Mount, 0, len(raw.Mounts))
	for _, rawMount := range raw.Mounts {
		source := rawMount.Source
		if rawMount.Type == string(engine.MountVolume) {
			source = rawMount.Name
		}
		mounts = append(mounts, engine.Mount{
			Type: engine.MountType(rawMount.Type), Source: source,
			Target: rawMount.Destination, ReadOnly: !rawMount.RW,
		})
	}
	sortMounts(mounts)
	portBindings, err := parsePortBindings(raw.HostConfig.PortBindings)
	if err != nil {
		return engine.Container{}, fmt.Errorf("inspect container %s port bindings: %w", raw.ID, err)
	}
	publishedPorts, err := parsePortBindings(raw.NetworkSettings.Ports)
	if err != nil {
		return engine.Container{}, fmt.Errorf("inspect container %s published ports: %w", raw.ID, err)
	}
	initProcess := raw.HostConfig.Init != nil && *raw.HostConfig.Init
	droppedCapabilities := cloneSlice(raw.HostConfig.CapDrop)
	sort.Strings(droppedCapabilities)
	args := cloneSlice(raw.Config.Cmd)
	environment := make(map[string]string, len(raw.Config.Env))
	for _, assignment := range raw.Config.Env {
		key, value, found := strings.Cut(assignment, "=")
		if !found {
			value = ""
		}
		environment[key] = value
	}
	return engine.Container{
		User: raw.Config.User,
		ID:   raw.ID, Name: strings.TrimPrefix(raw.Name, "/"), ImageID: raw.Image,
		Labels: cloneMap(raw.Config.Labels), Status: raw.State.Status,
		Running: raw.State.Running, ExitCode: raw.State.ExitCode,
		Error: raw.State.Error, Health: health, Networks: networks,
		Environment: environment, Mounts: mounts, Args: args,
		PortBindings:   portBindings,
		PublishedPorts: publishedPorts,
		Init:           initProcess, RestartPolicy: raw.HostConfig.RestartPolicy.Name,
		OpenStdin: raw.Config.OpenStdin, StdinOnce: raw.Config.StdinOnce,
		TTY: raw.Config.Tty,
		Security: engine.ContainerSecurity{
			NoNewPrivileges:     hasString(raw.HostConfig.SecurityOpt, "no-new-privileges"),
			DroppedCapabilities: droppedCapabilities,
			PIDsLimit:           raw.HostConfig.PidsLimit,
		},
	}, nil
}

func (client *Client) CreateContainer(ctx context.Context, request engine.ContainerRequest) (engine.Container, error) {
	type endpointSettings struct {
		Aliases []string `json:"Aliases"`
	}
	type mount struct {
		Type     string `json:"Type"`
		Source   string `json:"Source"`
		Target   string `json:"Target"`
		ReadOnly bool   `json:"ReadOnly"`
	}
	stopSeconds := durationSeconds(request.StopTimeout)
	initProcess := true
	environment := make([]string, 0, len(request.Environment))
	for key, value := range request.Environment {
		environment = append(environment, key+"="+value)
	}
	sort.Strings(environment)
	droppedCapabilities := cloneSlice(request.Security.DroppedCapabilities)
	sort.Strings(droppedCapabilities)
	var securityOptions []string
	if request.Security.NoNewPrivileges {
		securityOptions = []string{"no-new-privileges"}
	}
	mounts := make([]mount, 0, len(request.Mounts))
	for _, requestedMount := range request.Mounts {
		if err := validateMount(requestedMount); err != nil {
			return engine.Container{}, err
		}
		mounts = append(mounts, mount{
			Type: string(requestedMount.Type), Source: requestedMount.Source,
			Target: requestedMount.Target, ReadOnly: requestedMount.ReadOnly,
		})
	}
	exposedPorts := make(map[string]struct{})
	portBindings := make(map[string][]dockerPortBinding)
	for _, requestedPort := range request.PortBindings {
		key, err := portKey(requestedPort.ContainerPort, requestedPort.Protocol)
		if err != nil {
			return engine.Container{}, err
		}
		if requestedPort.HostPort < 0 || requestedPort.HostPort > 65535 {
			return engine.Container{}, fmt.Errorf("invalid host port %d", requestedPort.HostPort)
		}
		hostPort := ""
		if requestedPort.HostPort != 0 {
			hostPort = strconv.Itoa(requestedPort.HostPort)
		}
		exposedPorts[key] = struct{}{}
		portBindings[key] = append(portBindings[key], dockerPortBinding{
			HostIP: requestedPort.HostIP, HostPort: hostPort,
		})
	}

	body := struct {
		User         string              `json:"User,omitempty"`
		Image        string              `json:"Image"`
		Env          []string            `json:"Env,omitempty"`
		Cmd          *[]string           `json:"Cmd,omitempty"`
		OpenStdin    bool                `json:"OpenStdin"`
		StdinOnce    bool                `json:"StdinOnce"`
		Tty          bool                `json:"Tty"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
		Labels       map[string]string   `json:"Labels"`
		StopTimeout  *int                `json:"StopTimeout"`
		HostConfig   struct {
			NetworkMode   string `json:"NetworkMode"`
			Init          *bool  `json:"Init"`
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
			Mounts       []mount                        `json:"Mounts,omitempty"`
			PortBindings map[string][]dockerPortBinding `json:"PortBindings,omitempty"`
			SecurityOpt  []string                       `json:"SecurityOpt,omitempty"`
			CapDrop      []string                       `json:"CapDrop,omitempty"`
			PidsLimit    int64                          `json:"PidsLimit,omitempty"`
		} `json:"HostConfig"`
		NetworkingConfig *struct {
			EndpointsConfig map[string]endpointSettings `json:"EndpointsConfig"`
		} `json:"NetworkingConfig,omitempty"`
	}{
		Image: request.ImageID, User: request.User, Env: environment,
		OpenStdin: true, StdinOnce: false, Tty: false,
		ExposedPorts: exposedPorts, Labels: request.Labels, StopTimeout: &stopSeconds,
	}
	if request.Args != nil {
		args := cloneSlice(request.Args)
		body.Cmd = &args
	}
	body.HostConfig.NetworkMode = "none"
	body.HostConfig.Init = &initProcess
	body.HostConfig.RestartPolicy.Name = "no"
	body.HostConfig.Mounts = mounts
	body.HostConfig.PortBindings = portBindings
	body.HostConfig.SecurityOpt = securityOptions
	body.HostConfig.CapDrop = droppedCapabilities
	body.HostConfig.PidsLimit = request.Security.PIDsLimit
	if request.NetworkID != "" {
		body.HostConfig.NetworkMode = request.NetworkID
		body.NetworkingConfig = &struct {
			EndpointsConfig map[string]endpointSettings `json:"EndpointsConfig"`
		}{
			EndpointsConfig: map[string]endpointSettings{
				request.NetworkID: {Aliases: cloneSlice(request.NetworkAliases)},
			},
		}
	}

	path := "/containers/create?name=" + url.QueryEscape(request.Name)
	var response struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := client.do(ctx, http.MethodPost, path, body, &response); err != nil {
		return engine.Container{}, err
	}
	if response.ID == "" {
		return engine.Container{}, fmt.Errorf("Docker created container %q without returning an ID", request.Name)
	}
	return client.InspectContainer(ctx, response.ID)
}

func (client *Client) ConnectNetwork(
	ctx context.Context, networkID, containerID string, aliases []string,
) error {
	body := struct {
		Container      string `json:"Container"`
		EndpointConfig struct {
			Aliases []string `json:"Aliases"`
		} `json:"EndpointConfig"`
	}{
		Container: containerID,
	}
	body.EndpointConfig.Aliases = cloneSlice(aliases)
	return client.do(
		ctx, http.MethodPost, "/networks/"+url.PathEscape(networkID)+"/connect", body, nil,
	)
}

func (client *Client) DisconnectNetwork(
	ctx context.Context, networkID, containerID string,
) error {
	return client.disconnectNetwork(ctx, networkID, containerID, false)
}

func (client *Client) ForceDisconnectNetworkEndpoint(
	ctx context.Context, networkID, endpointName string,
) error {
	return client.disconnectNetwork(ctx, networkID, endpointName, true)
}

func (client *Client) disconnectNetwork(
	ctx context.Context, networkID, target string, force bool,
) error {
	body := struct {
		Container string `json:"Container"`
		Force     bool   `json:"Force"`
	}{
		Container: target, Force: force,
	}
	return client.do(
		ctx, http.MethodPost, "/networks/"+url.PathEscape(networkID)+"/disconnect", body, nil,
	)
}

func (client *Client) StartContainer(ctx context.Context, id string) error {
	return client.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil)
}

func (client *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	path := "/containers/" + url.PathEscape(id) + "/stop?t=" + strconv.Itoa(durationSeconds(timeout))
	return client.do(ctx, http.MethodPost, path, nil, nil)
}

func (client *Client) RestartContainer(ctx context.Context, id string, timeout time.Duration) error {
	path := "/containers/" + url.PathEscape(id) + "/restart?t=" + strconv.Itoa(durationSeconds(timeout))
	return client.do(ctx, http.MethodPost, path, nil, nil)
}

func (client *Client) RemoveContainer(ctx context.Context, id string) error {
	return client.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?v=0&force=0", nil, nil)
}

func (client *Client) do(ctx context.Context, method, path string, input, output interface{}) error {
	version, err := client.apiVersion(ctx)
	if err != nil {
		return err
	}
	return client.doUnversioned(ctx, method, "/v"+version+path, input, output)
}

func (client *Client) apiVersion(ctx context.Context) (string, error) {
	client.versionMu.Lock()
	defer client.versionMu.Unlock()
	if client.version != "" {
		return client.version, nil
	}
	var response struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := client.doUnversioned(ctx, http.MethodGet, "/version", nil, &response); err != nil {
		// A canceled or transient first request must not poison the client.
		return "", err
	}
	if compareVersions(response.APIVersion, minAPIVersion) < 0 {
		return "", fmt.Errorf(
			"Docker API %s is too old; dcomp requires at least %s",
			response.APIVersion, minAPIVersion,
		)
	}
	client.version = response.APIVersion
	if compareVersions(client.version, maxAPIVersion) > 0 {
		client.version = maxAPIVersion
	}
	return client.version, nil
}

func (client *Client) doUnversioned(ctx context.Context, method, path string, input, output interface{}) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Docker request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Docker API via %s: %w", client.socketPath, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return dockerResponseError(response)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16*1024*1024)).Decode(output); err != nil {
		return fmt.Errorf("decode Docker response: %w", err)
	}
	return nil
}

func dockerResponseError(response *http.Response) error {
	var apiResponse struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&apiResponse)
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", engine.ErrNotFound, apiResponse.Message)
	}
	return &APIError{StatusCode: response.StatusCode, Message: apiResponse.Message}
}

func validateMount(mount engine.Mount) error {
	if mount.Type != engine.MountBind && mount.Type != engine.MountVolume {
		return fmt.Errorf("unsupported mount type %q", mount.Type)
	}
	if mount.Source == "" {
		return fmt.Errorf("%s mount source is empty", mount.Type)
	}
	if mount.Target == "" {
		return fmt.Errorf("%s mount target is empty", mount.Type)
	}
	return nil
}

func portKey(containerPort int, protocol engine.PortProtocol) (string, error) {
	if containerPort < 1 || containerPort > 65535 {
		return "", fmt.Errorf("invalid container port %d", containerPort)
	}
	if protocol != engine.ProtocolTCP && protocol != engine.ProtocolUDP {
		return "", fmt.Errorf("unsupported port protocol %q", protocol)
	}
	return strconv.Itoa(containerPort) + "/" + string(protocol), nil
}

func parsePortBindings(raw map[string][]dockerPortBinding) ([]engine.PortBinding, error) {
	result := make([]engine.PortBinding, 0)
	for key, bindings := range raw {
		portText, protocolText, found := strings.Cut(key, "/")
		if !found {
			return nil, fmt.Errorf("invalid container port key %q", key)
		}
		containerPort, err := strconv.Atoi(portText)
		if err != nil {
			return nil, fmt.Errorf("invalid container port key %q", key)
		}
		protocol := engine.PortProtocol(protocolText)
		if _, err := portKey(containerPort, protocol); err != nil {
			return nil, fmt.Errorf("invalid container port key %q: %w", key, err)
		}
		for _, binding := range bindings {
			hostPort := 0
			if binding.HostPort != "" {
				hostPort, err = strconv.Atoi(binding.HostPort)
				if err != nil || hostPort < 0 || hostPort > 65535 {
					return nil, fmt.Errorf(
						"invalid host port %q for %s", binding.HostPort, key,
					)
				}
			}
			result = append(result, engine.PortBinding{
				ContainerPort: containerPort, Protocol: protocol,
				HostIP: binding.HostIP, HostPort: hostPort,
			})
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].ContainerPort != result[right].ContainerPort {
			return result[left].ContainerPort < result[right].ContainerPort
		}
		if result[left].Protocol != result[right].Protocol {
			return result[left].Protocol < result[right].Protocol
		}
		if result[left].HostIP != result[right].HostIP {
			return result[left].HostIP < result[right].HostIP
		}
		return result[left].HostPort < result[right].HostPort
	})
	return result, nil
}

func sortMounts(mounts []engine.Mount) {
	sort.Slice(mounts, func(left, right int) bool {
		if mounts[left].Target != mounts[right].Target {
			return mounts[left].Target < mounts[right].Target
		}
		if mounts[left].Type != mounts[right].Type {
			return mounts[left].Type < mounts[right].Type
		}
		if mounts[left].Source != mounts[right].Source {
			return mounts[left].Source < mounts[right].Source
		}
		return !mounts[left].ReadOnly && mounts[right].ReadOnly
	})
}

func durationSeconds(duration time.Duration) int {
	if duration <= 0 {
		return 10
	}
	seconds := int(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

func compareVersions(left, right string) int {
	parse := func(value string) (int, int) {
		var major, minor int
		_, _ = fmt.Sscanf(value, "%d.%d", &major, &minor)
		return major, minor
	}
	leftMajor, leftMinor := parse(left)
	rightMajor, rightMinor := parse(right)
	if leftMajor != rightMajor {
		if leftMajor < rightMajor {
			return -1
		}
		return 1
	}
	if leftMinor < rightMinor {
		return -1
	}
	if leftMinor > rightMinor {
		return 1
	}
	return 0
}

func cloneMap(input map[string]string) map[string]string {
	if input == nil {
		return map[string]string{}
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneSlice(input []string) []string {
	if input == nil {
		return nil
	}
	output := make([]string, len(input))
	copy(output, input)
	return output
}

func hasString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

var _ engine.Engine = (*Client)(nil)
