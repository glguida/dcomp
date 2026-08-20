package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
)

type fakeProxyManager struct {
	mu        sync.Mutex
	processes map[string]proxy.Process
	configs   map[string]proxy.Config
	nextPID   int
}

func newFakeProxyManager() *fakeProxyManager {
	return &fakeProxyManager{
		processes: make(map[string]proxy.Process),
		configs:   make(map[string]proxy.Config),
		nextPID:   1000,
	}
}

func (manager *fakeProxyManager) Ensure(_ context.Context, config proxy.Config) (proxy.Process, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if process, exists := manager.processes[config.InstanceID]; exists {
		return process, nil
	}
	manager.nextPID++
	process := proxy.Process{
		InstanceID: config.InstanceID, Digest: config.Digest, PID: manager.nextPID,
		RuntimeDir: config.RuntimeDir,
		Control:    proxy.ControlSocket(config.RuntimeDir),
		Log:        filepath.Join(config.RuntimeDir, proxy.LogFileName),
	}
	manager.processes[process.InstanceID] = process
	manager.configs[process.InstanceID] = config
	return process, nil
}

func (manager *fakeProxyManager) Inspect(_ context.Context, process proxy.Process) (proxy.Status, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	actual, exists := manager.processes[process.InstanceID]
	if !exists {
		return proxy.Status{}, proxy.ErrNotRunning
	}
	if actual != process {
		return proxy.Status{}, fmt.Errorf("proxy process identity mismatch")
	}
	inputs, outputs := 0, 0
	for _, endpoint := range manager.configs[process.InstanceID].Endpoints {
		if endpoint.Direction == proxy.DirectionInput {
			inputs++
		} else {
			outputs++
		}
	}
	return proxy.Status{
		Version: proxy.ConfigVersion, InstanceID: actual.InstanceID,
		Digest: actual.Digest, PID: actual.PID, Ready: true,
		Inputs: inputs, Outputs: outputs,
	}, nil
}

func (manager *fakeProxyManager) Stop(_ context.Context, process proxy.Process) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if _, exists := manager.processes[process.InstanceID]; !exists {
		return proxy.ErrNotRunning
	}
	delete(manager.processes, process.InstanceID)
	delete(manager.configs, process.InstanceID)
	return nil
}

func (manager *fakeProxyManager) Logs(
	_ context.Context,
	_ proxy.Process,
	_ bool,
	_ func(proxy.LogLine) error,
) error {
	return nil
}

type engineCall struct {
	Method string
	Target string
}

// fakeEngine models the Docker facts observed by lifecycle code. Mutating
// calls change those facts before an injected error is returned, which also
// lets tests model a lost response from Docker.
type fakeEngine struct {
	mu sync.Mutex

	identity       string
	identityError  error
	images         map[string]engine.Image
	imageErrors    map[string]error
	networks       map[string]engine.Network
	networkNames   map[string]string
	volumes        map[string]engine.Volume
	containers     map[string]engine.Container
	containerNames map[string]string

	inspectNetworkErrors   map[string][]error
	inspectVolumeErrors    map[string][]error
	inspectContainerErrors map[string][]error
	createNetworkErrors    map[string][]error
	createVolumeErrors     map[string][]error
	createContainerErrors  map[string][]error
	connectErrors          map[string][]error
	connectAnyErrors       []error
	disconnectErrors       map[string][]error
	startFailures          map[string][]error
	restartFailures        map[string][]error

	networkRequests   []engine.NetworkRequest
	volumeRequests    []engine.VolumeRequest
	containerRequests []engine.ContainerRequest
	calls             []engineCall

	healthOnStart map[string]engine.Health
	afterStart    map[string]func()

	nextNetwork   int
	nextContainer int
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		identity:               "fake-engine",
		images:                 make(map[string]engine.Image),
		imageErrors:            make(map[string]error),
		networks:               make(map[string]engine.Network),
		networkNames:           make(map[string]string),
		volumes:                make(map[string]engine.Volume),
		containers:             make(map[string]engine.Container),
		containerNames:         make(map[string]string),
		inspectNetworkErrors:   make(map[string][]error),
		inspectVolumeErrors:    make(map[string][]error),
		inspectContainerErrors: make(map[string][]error),
		createNetworkErrors:    make(map[string][]error),
		createVolumeErrors:     make(map[string][]error),
		createContainerErrors:  make(map[string][]error),
		connectErrors:          make(map[string][]error),
		disconnectErrors:       make(map[string][]error),
		startFailures:          make(map[string][]error),
		restartFailures:        make(map[string][]error),
		healthOnStart:          make(map[string]engine.Health),
		afterStart:             make(map[string]func()),
	}
}

func (fake *fakeEngine) Identity(ctx context.Context) (string, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("engine-identity", "")
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if fake.identityError != nil {
		return "", fake.identityError
	}
	return fake.identity, nil
}

func (fake *fakeEngine) ResolveImage(ctx context.Context, reference string) (engine.Image, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("resolve-image", reference)
	if err := ctx.Err(); err != nil {
		return engine.Image{}, err
	}
	if err := fake.imageErrors[reference]; err != nil {
		return engine.Image{}, err
	}
	image, exists := fake.images[reference]
	if !exists {
		return engine.Image{}, fmt.Errorf("%w: image %s", engine.ErrNotFound, reference)
	}
	return cloneImage(image), nil
}

func (fake *fakeEngine) InspectNetwork(ctx context.Context, idOrName string) (engine.Network, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("inspect-network", idOrName)
	if err := ctx.Err(); err != nil {
		return engine.Network{}, err
	}
	if err := popFailure(fake.inspectNetworkErrors, idOrName); err != nil {
		return engine.Network{}, err
	}
	id := idOrName
	if namedID, exists := fake.networkNames[idOrName]; exists {
		id = namedID
	}
	network, exists := fake.networks[id]
	if !exists {
		return engine.Network{}, engine.ErrNotFound
	}
	return cloneNetwork(network), nil
}

func (fake *fakeEngine) CreateNetwork(ctx context.Context, request engine.NetworkRequest) (engine.Network, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("create-network", request.Name)
	if err := ctx.Err(); err != nil {
		return engine.Network{}, err
	}
	if _, exists := fake.networkNames[request.Name]; exists {
		return engine.Network{}, fmt.Errorf("network name %q already exists", request.Name)
	}
	fake.nextNetwork++
	id := fmt.Sprintf("network-%d", fake.nextNetwork)
	network := engine.Network{
		ID:       id,
		Name:     request.Name,
		Driver:   "bridge",
		Internal: request.Internal,
		Labels:   cloneStrings(request.Labels),
	}
	fake.networks[id] = network
	fake.networkNames[request.Name] = id
	fake.networkRequests = append(fake.networkRequests, cloneNetworkRequest(request))
	if err := popFailure(fake.createNetworkErrors, request.Name); err != nil {
		return engine.Network{}, err
	}
	return cloneNetwork(network), nil
}

func (fake *fakeEngine) RemoveNetwork(ctx context.Context, id string) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("remove-network", id)
	if err := ctx.Err(); err != nil {
		return err
	}
	network, exists := fake.networks[id]
	if !exists {
		return engine.ErrNotFound
	}
	if len(network.Containers) != 0 {
		return fmt.Errorf("network %s still has attached containers", id)
	}
	delete(fake.networks, id)
	delete(fake.networkNames, network.Name)
	return nil
}

func (fake *fakeEngine) InspectVolume(ctx context.Context, name string) (engine.Volume, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("inspect-volume", name)
	if err := ctx.Err(); err != nil {
		return engine.Volume{}, err
	}
	if err := popFailure(fake.inspectVolumeErrors, name); err != nil {
		return engine.Volume{}, err
	}
	volume, exists := fake.volumes[name]
	if !exists {
		return engine.Volume{}, engine.ErrNotFound
	}
	return cloneVolume(volume), nil
}

func (fake *fakeEngine) CreateVolume(ctx context.Context, request engine.VolumeRequest) (engine.Volume, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("create-volume", request.Name)
	if err := ctx.Err(); err != nil {
		return engine.Volume{}, err
	}
	if _, exists := fake.volumes[request.Name]; exists {
		return engine.Volume{}, fmt.Errorf("volume name %q already exists", request.Name)
	}
	volume := engine.Volume{
		Name:       request.Name,
		Driver:     "local",
		Mountpoint: "/var/lib/docker/volumes/" + request.Name + "/_data",
		Labels:     cloneStrings(request.Labels),
	}
	fake.volumes[request.Name] = volume
	fake.volumeRequests = append(fake.volumeRequests, cloneVolumeRequest(request))
	if err := popFailure(fake.createVolumeErrors, request.Name); err != nil {
		return engine.Volume{}, err
	}
	return cloneVolume(volume), nil
}

func (fake *fakeEngine) InspectContainer(ctx context.Context, idOrName string) (engine.Container, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("inspect-container", idOrName)
	if err := ctx.Err(); err != nil {
		return engine.Container{}, err
	}
	if err := popFailure(fake.inspectContainerErrors, idOrName); err != nil {
		return engine.Container{}, err
	}
	id := idOrName
	if namedID, exists := fake.containerNames[idOrName]; exists {
		id = namedID
	}
	container, exists := fake.containers[id]
	if !exists {
		return engine.Container{}, engine.ErrNotFound
	}
	return cloneContainer(container), nil
}

func (fake *fakeEngine) CreateContainer(
	ctx context.Context,
	request engine.ContainerRequest,
) (engine.Container, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("create-container", request.Name)
	if err := ctx.Err(); err != nil {
		return engine.Container{}, err
	}
	if _, exists := fake.containerNames[request.Name]; exists {
		return engine.Container{}, fmt.Errorf("container name %q already exists", request.Name)
	}
	networks := make(map[string]engine.NetworkAttachment)
	if request.NetworkID != "" {
		network, exists := fake.networks[request.NetworkID]
		if !exists {
			return engine.Container{}, fmt.Errorf("%w: network %s", engine.ErrNotFound, request.NetworkID)
		}
		networks[network.Name] = engine.NetworkAttachment{
			// Docker leaves NetworkID empty until first start while
			// retaining the configured network name and aliases.
			NetworkID: "",
			Aliases:   append([]string(nil), request.NetworkAliases...),
		}
	}
	fake.nextContainer++
	id := fmt.Sprintf("container-%d", fake.nextContainer)
	container := engine.Container{
		ID:            id,
		Name:          request.Name,
		ImageID:       request.ImageID,
		Labels:        cloneStrings(request.Labels),
		Status:        "created",
		Health:        engine.HealthStarting,
		Environment:   cloneStrings(request.Environment),
		Mounts:        append([]engine.Mount(nil), request.Mounts...),
		Args:          append([]string(nil), request.Args...),
		PortBindings:  append([]engine.PortBinding(nil), request.PortBindings...),
		Init:          true,
		RestartPolicy: "no",
		OpenStdin:     true,
		StdinOnce:     false,
		TTY:           false,
		Security:      cloneContainerSecurity(request.Security),
		Networks:      networks,
	}
	fake.containers[id] = container
	fake.containerNames[request.Name] = id
	fake.containerRequests = append(fake.containerRequests, cloneContainerRequest(request))
	if err := popFailure(fake.createContainerErrors, request.Name); err != nil {
		return engine.Container{}, err
	}
	return cloneContainer(container), nil
}

func (fake *fakeEngine) ConnectNetwork(
	ctx context.Context,
	networkID string,
	containerID string,
	aliases []string,
) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("connect-network", networkID+"->"+containerID)
	if err := ctx.Err(); err != nil {
		return err
	}
	network, networkExists := fake.networks[networkID]
	container, containerExists := fake.containers[containerID]
	if !networkExists || !containerExists {
		return engine.ErrNotFound
	}
	if _, exists := container.Networks[network.Name]; exists {
		return fmt.Errorf("container %s is already attached to %s", containerID, networkID)
	}
	observedID := ""
	if container.Running {
		observedID = networkID
	}
	container.Networks[network.Name] = engine.NetworkAttachment{
		NetworkID: observedID,
		Aliases:   append([]string(nil), aliases...),
	}
	fake.containers[containerID] = container
	if container.Running {
		fake.attach(networkID, containerID)
	}
	if len(fake.connectAnyErrors) != 0 {
		err := fake.connectAnyErrors[0]
		fake.connectAnyErrors = fake.connectAnyErrors[1:]
		return err
	}
	if err := popFailure(fake.connectErrors, networkID+"->"+containerID); err != nil {
		return err
	}
	return nil
}

func (fake *fakeEngine) DisconnectNetwork(
	ctx context.Context,
	networkID string,
	containerID string,
) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("disconnect-network", networkID+"->"+containerID)
	if err := ctx.Err(); err != nil {
		return err
	}
	network, networkExists := fake.networks[networkID]
	container, containerExists := fake.containers[containerID]
	if !networkExists || !containerExists {
		return engine.ErrNotFound
	}
	attached := false
	for name, attachment := range container.Networks {
		if name == network.Name || attachment.NetworkID == networkID {
			delete(container.Networks, name)
			attached = true
		}
	}
	if !attached {
		return engine.ErrNotFound
	}
	fake.containers[containerID] = container
	fake.detach(networkID, containerID)
	if err := popFailure(fake.disconnectErrors, networkID+"->"+containerID); err != nil {
		return err
	}
	return nil
}

func (fake *fakeEngine) StartContainer(ctx context.Context, id string) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("start-container", id)
	if err := ctx.Err(); err != nil {
		return err
	}
	container, exists := fake.containers[id]
	if !exists {
		return engine.ErrNotFound
	}
	component := container.Labels[LabelComponent]
	if err := popFailure(fake.startFailures, component); err != nil {
		return err
	}
	fake.setContainerRunning(id, component, container)
	return nil
}

func (fake *fakeEngine) setContainerRunning(
	id string,
	component string,
	container engine.Container,
) {
	health := fake.healthOnStart[component]
	if health == "" {
		health = engine.HealthHealthy
	}
	container.Status = "running"
	container.Running = true
	container.ExitCode = 0
	container.Error = ""
	container.Health = health
	for name, attachment := range container.Networks {
		networkID, exists := fake.networkNames[name]
		if !exists {
			continue
		}
		attachment.NetworkID = networkID
		container.Networks[name] = attachment
		fake.attach(networkID, id)
	}
	fake.containers[id] = container
	if hook := fake.afterStart[component]; hook != nil {
		delete(fake.afterStart, component)
		hook()
	}
}

func (fake *fakeEngine) StopContainer(ctx context.Context, id string, _ time.Duration) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("stop-container", id)
	if err := ctx.Err(); err != nil {
		return err
	}
	container, exists := fake.containers[id]
	if !exists {
		return engine.ErrNotFound
	}
	container.Status = "exited"
	container.Running = false
	container.Health = engine.HealthNone
	for name, attachment := range container.Networks {
		networkID, exists := fake.networkNames[name]
		if !exists {
			continue
		}
		fake.detach(networkID, id)
		attachment.NetworkID = ""
		container.Networks[name] = attachment
	}
	fake.containers[id] = container
	return nil
}

func (fake *fakeEngine) RestartContainer(ctx context.Context, id string, _ time.Duration) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("restart-container", id)
	if err := ctx.Err(); err != nil {
		return err
	}
	container, exists := fake.containers[id]
	if !exists {
		return engine.ErrNotFound
	}
	component := container.Labels[LabelComponent]
	fake.setContainerRunning(id, component, container)
	if err := popFailure(fake.restartFailures, component); err != nil {
		return err
	}
	return nil
}

func (fake *fakeEngine) RemoveContainer(ctx context.Context, id string) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("remove-container", id)
	if err := ctx.Err(); err != nil {
		return err
	}
	container, exists := fake.containers[id]
	if !exists {
		return engine.ErrNotFound
	}
	if container.Running {
		return fmt.Errorf("container %s is running", id)
	}
	for name, attachment := range container.Networks {
		networkID := attachment.NetworkID
		if networkID == "" {
			networkID = fake.networkNames[name]
		}
		fake.detach(networkID, id)
	}
	delete(fake.containers, id)
	delete(fake.containerNames, container.Name)
	return nil
}

func (fake *fakeEngine) attach(networkID, containerID string) {
	network := fake.networks[networkID]
	for _, existing := range network.Containers {
		if existing == containerID {
			return
		}
	}
	network.Containers = append(network.Containers, containerID)
	sort.Strings(network.Containers)
	fake.networks[networkID] = network
}

func (fake *fakeEngine) detach(networkID, containerID string) {
	network, exists := fake.networks[networkID]
	if !exists {
		return
	}
	filtered := network.Containers[:0]
	for _, existing := range network.Containers {
		if existing != containerID {
			filtered = append(filtered, existing)
		}
	}
	network.Containers = append([]string(nil), filtered...)
	fake.networks[networkID] = network
}

func popFailure(failures map[string][]error, key string) error {
	items := failures[key]
	if len(items) == 0 {
		return nil
	}
	err := items[0]
	failures[key] = items[1:]
	return err
}

func (fake *fakeEngine) record(method, target string) {
	fake.calls = append(fake.calls, engineCall{Method: method, Target: target})
}

func (fake *fakeEngine) resetCalls() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = nil
	fake.networkRequests = nil
	fake.volumeRequests = nil
	fake.containerRequests = nil
}

func (fake *fakeEngine) callsFor(method string) []engineCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var result []engineCall
	for _, call := range fake.calls {
		if call.Method == method {
			result = append(result, call)
		}
	}
	return result
}

func (fake *fakeEngine) mutationCalls() []engineCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var result []engineCall
	for _, call := range fake.calls {
		switch call.Method {
		case "create-network", "remove-network",
			"create-volume", "create-container", "remove-container",
			"connect-network", "disconnect-network",
			"start-container", "stop-container", "restart-container":
			result = append(result, call)
		}
	}
	return result
}

func (fake *fakeEngine) resources() (networkIDs, containerIDs []string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for id := range fake.networks {
		networkIDs = append(networkIDs, id)
	}
	for id := range fake.containers {
		containerIDs = append(containerIDs, id)
	}
	sort.Strings(networkIDs)
	sort.Strings(containerIDs)
	return networkIDs, containerIDs
}

func (fake *fakeEngine) setHealth(id string, health engine.Health) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	container := fake.containers[id]
	container.Health = health
	fake.containers[id] = container
}

func (fake *fakeEngine) replaceContainer(id string, replacement engine.Container) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.containers[id] = cloneContainer(replacement)
}

func (fake *fakeEngine) deleteNetworkOutOfBand(id string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	network, exists := fake.networks[id]
	if !exists {
		return
	}
	delete(fake.networks, id)
	delete(fake.networkNames, network.Name)
	for containerID, container := range fake.containers {
		for name, attachment := range container.Networks {
			if name == network.Name || attachment.NetworkID == id {
				delete(container.Networks, name)
			}
		}
		fake.containers[containerID] = container
	}
}

func (fake *fakeEngine) deleteContainerOutOfBand(id string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	container, exists := fake.containers[id]
	if !exists {
		return
	}
	for name, attachment := range container.Networks {
		networkID := attachment.NetworkID
		if networkID == "" {
			networkID = fake.networkNames[name]
		}
		fake.detach(networkID, id)
	}
	delete(fake.containers, id)
	delete(fake.containerNames, container.Name)
}

func cloneImage(input engine.Image) engine.Image {
	input.DeclaredVolumes = append([]string(nil), input.DeclaredVolumes...)
	return input
}

func cloneNetwork(input engine.Network) engine.Network {
	input.Labels = cloneStrings(input.Labels)
	input.Containers = append([]string(nil), input.Containers...)
	return input
}

func cloneVolume(input engine.Volume) engine.Volume {
	input.Labels = cloneStrings(input.Labels)
	return input
}

func cloneContainer(input engine.Container) engine.Container {
	input.Labels = cloneStrings(input.Labels)
	input.Environment = cloneStrings(input.Environment)
	input.Mounts = append([]engine.Mount(nil), input.Mounts...)
	input.Args = append([]string(nil), input.Args...)
	input.PortBindings = append([]engine.PortBinding(nil), input.PortBindings...)
	input.PublishedPorts = append([]engine.PortBinding(nil), input.PublishedPorts...)
	input.Security = cloneContainerSecurity(input.Security)
	input.Networks = cloneAttachments(input.Networks)
	return input
}

func cloneAttachments(input map[string]engine.NetworkAttachment) map[string]engine.NetworkAttachment {
	output := make(map[string]engine.NetworkAttachment, len(input))
	for name, attachment := range input {
		attachment.Aliases = append([]string(nil), attachment.Aliases...)
		output[name] = attachment
	}
	return output
}

func cloneNetworkRequest(input engine.NetworkRequest) engine.NetworkRequest {
	input.Labels = cloneStrings(input.Labels)
	return input
}

func cloneVolumeRequest(input engine.VolumeRequest) engine.VolumeRequest {
	input.Labels = cloneStrings(input.Labels)
	return input
}

func cloneContainerRequest(input engine.ContainerRequest) engine.ContainerRequest {
	input.NetworkAliases = append([]string(nil), input.NetworkAliases...)
	input.Labels = cloneStrings(input.Labels)
	input.Environment = cloneStrings(input.Environment)
	input.Mounts = append([]engine.Mount(nil), input.Mounts...)
	input.Args = append([]string(nil), input.Args...)
	input.PortBindings = append([]engine.PortBinding(nil), input.PortBindings...)
	input.Security = cloneContainerSecurity(input.Security)
	return input
}

func cloneContainerSecurity(input engine.ContainerSecurity) engine.ContainerSecurity {
	input.DroppedCapabilities = append([]string(nil), input.DroppedCapabilities...)
	return input
}

func cloneStrings(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
