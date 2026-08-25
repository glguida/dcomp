package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
)

func TestFakeEnginePreservesUnstartedContainerNetworkGeneration(t *testing.T) {
	ctx := context.Background()
	fake := newFakeEngine()
	first, err := fake.CreateNetwork(ctx, engine.NetworkRequest{Name: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	container, err := fake.CreateContainer(ctx, engine.ContainerRequest{
		Name:           "worker",
		ImageID:        "sha256:worker",
		NetworkID:      first.ID,
		NetworkAliases: []string{"worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.deleteNetworkOutOfBand(first.ID)
	replacement, err := fake.CreateNetwork(ctx, engine.NetworkRequest{Name: first.Name})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == first.ID {
		t.Fatalf("replacement network reused ID %q", first.ID)
	}

	err = fake.StartContainer(ctx, container.ID)
	if !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("StartContainer error = %v, want missing create-time network", err)
	}
	actual, err := fake.InspectContainer(ctx, container.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Running {
		t.Fatal("container started on a same-name replacement network")
	}
	actualReplacement, err := fake.InspectNetwork(ctx, replacement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actualReplacement.Endpoints) != 0 {
		t.Fatalf(
			"replacement network gained endpoints: %#v",
			actualReplacement.Endpoints,
		)
	}
}

type fakeProxyManager struct {
	mu              sync.Mutex
	processes       map[string]proxy.Process
	configs         map[string]proxy.Config
	nextPID         int
	resyncs         int
	resyncAttempts  int
	ensures         int
	stops           int
	stopAttempts    int
	beforeResync    func(proxy.Wiring) error
	inspectErrors   []error
	resyncErrors    []error
	stopErrors      []error
	stopAfterErrors []error
	dieOnResync     bool
	ready           map[string]bool
	controlVersions map[string]int
}

func newFakeProxyManager() *fakeProxyManager {
	return &fakeProxyManager{
		processes:       make(map[string]proxy.Process),
		configs:         make(map[string]proxy.Config),
		ready:           make(map[string]bool),
		controlVersions: make(map[string]int),
		nextPID:         1000,
	}
}

func (manager *fakeProxyManager) Ensure(ctx context.Context, config proxy.Config) (proxy.Process, error) {
	if err := ctx.Err(); err != nil {
		return proxy.Process{}, err
	}
	if err := config.Validate(); err != nil {
		return proxy.Process{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	control := proxy.ControlSocket(config.RuntimeDir)
	if process, exists := manager.processAtControlLocked(control); exists {
		if process.InstanceID != config.InstanceID {
			return proxy.Process{}, fmt.Errorf(
				"fake proxy runtime %s already contains proxy instance %s",
				config.RuntimeDir,
				process.InstanceID,
			)
		}
		if !manager.ready[process.InstanceID] || process.Digest != config.Digest {
			return proxy.Process{}, fmt.Errorf(
				"fake proxy instance %s is live with ready=%t wiring %q, expected %q",
				process.InstanceID,
				manager.ready[process.InstanceID],
				process.Digest,
				config.Digest,
			)
		}
		return process, nil
	}
	if process, exists := manager.processes[config.InstanceID]; exists {
		return proxy.Process{}, fmt.Errorf(
			"fake proxy instance %s is already live at %s",
			process.InstanceID,
			process.Control,
		)
	}
	manager.ensures++
	manager.nextPID++
	process := proxy.Process{
		InstanceID: config.InstanceID, Digest: config.Digest, PID: manager.nextPID,
		RuntimeDir: config.RuntimeDir,
		Control:    proxy.ControlSocket(config.RuntimeDir),
		Log:        filepath.Join(config.RuntimeDir, proxy.LogFileName),
	}
	manager.processes[process.InstanceID] = process
	manager.configs[process.InstanceID] = config
	manager.ready[process.InstanceID] = true
	manager.controlVersions[process.InstanceID] = proxy.ControlProtocolVersion
	return process, nil
}

func (manager *fakeProxyManager) Inspect(ctx context.Context, process proxy.Process) (proxy.Status, error) {
	if err := ctx.Err(); err != nil {
		return proxy.Status{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.inspectErrors) != 0 {
		err := manager.inspectErrors[0]
		manager.inspectErrors = manager.inspectErrors[1:]
		return proxy.Status{}, err
	}
	actual, exists := manager.processAtControlLocked(process.Control)
	if !exists {
		return proxy.Status{}, proxy.ErrNotRunning
	}
	if actual.InstanceID != process.InstanceID || actual.PID != process.PID ||
		actual.RuntimeDir != process.RuntimeDir || actual.Control != process.Control ||
		actual.Log != process.Log {
		return proxy.Status{}, fmt.Errorf("%w: fake proxy process identity mismatch", proxy.ErrIdentityMismatch)
	}
	inputs, outputs := 0, 0
	for _, endpoint := range manager.configs[actual.InstanceID].Endpoints {
		if endpoint.Direction == proxy.DirectionInput {
			inputs++
		} else {
			outputs++
		}
	}
	status := proxy.Status{
		Version:                proxy.ConfigVersion,
		ControlProtocolVersion: manager.controlVersions[actual.InstanceID],
		InstanceID:             actual.InstanceID,
		Digest:                 actual.Digest, PID: actual.PID, Ready: manager.ready[actual.InstanceID],
		Inputs: inputs, Outputs: outputs,
	}
	if status.ControlProtocolVersion != proxy.ControlProtocolVersion {
		return status, proxy.ErrControlProtocolMismatch
	}
	return status, nil
}

func (manager *fakeProxyManager) Resync(
	ctx context.Context,
	process proxy.Process,
	wiring proxy.Wiring,
	digest string,
) (proxy.Status, error) {
	if err := ctx.Err(); err != nil {
		return proxy.Status{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	actual, exists := manager.processAtControlLocked(process.Control)
	if !exists {
		return proxy.Status{}, proxy.ErrNotRunning
	}
	if actual.InstanceID != process.InstanceID || actual.PID != process.PID ||
		actual.RuntimeDir != process.RuntimeDir || actual.Control != process.Control ||
		actual.Log != process.Log {
		return proxy.Status{}, proxy.ErrIdentityMismatch
	}
	manager.resyncAttempts++
	if manager.dieOnResync {
		manager.dieOnResync = false
		delete(manager.processes, actual.InstanceID)
		delete(manager.configs, actual.InstanceID)
		delete(manager.ready, actual.InstanceID)
		delete(manager.controlVersions, actual.InstanceID)
		return proxy.Status{}, proxy.ErrNotRunning
	}
	if manager.controlVersions[actual.InstanceID] != proxy.ControlProtocolVersion {
		return proxy.Status{
			Version:                proxy.ConfigVersion,
			ControlProtocolVersion: manager.controlVersions[actual.InstanceID],
			InstanceID:             actual.InstanceID, Digest: actual.Digest,
			PID: actual.PID, Ready: manager.ready[actual.InstanceID],
		}, proxy.ErrControlProtocolMismatch
	}
	if len(manager.resyncErrors) != 0 {
		err := manager.resyncErrors[0]
		manager.resyncErrors = manager.resyncErrors[1:]
		return proxy.Status{
			Version:                proxy.ConfigVersion,
			ControlProtocolVersion: manager.controlVersions[actual.InstanceID],
			InstanceID:             actual.InstanceID, Digest: actual.Digest,
			PID: actual.PID, Ready: manager.ready[actual.InstanceID],
		}, err
	}
	computed, err := wiring.Digest()
	if err != nil {
		return proxy.Status{}, err
	}
	if computed != digest {
		return proxy.Status{}, fmt.Errorf("fake proxy wiring digest mismatch")
	}
	if manager.beforeResync != nil {
		if err := manager.beforeResync(wiring); err != nil {
			return proxy.Status{}, err
		}
	}
	config := manager.configs[actual.InstanceID]
	config.Version = proxy.ConfigVersion
	config.Digest = digest
	config.Endpoints = nil
	for _, endpoint := range wiring.Endpoints {
		config.Endpoints = append(config.Endpoints, proxy.Endpoint{
			Component: endpoint.Component,
			Name:      endpoint.Name,
			Direction: endpoint.Direction,
			Socket: proxy.HostSocket(
				config.RuntimeDir, endpoint.Direction, endpoint.Component, endpoint.Name,
			),
		})
	}
	config.Links = append([]proxy.Link(nil), wiring.Links...)
	actual.Digest = digest
	manager.processes[actual.InstanceID] = actual
	manager.configs[actual.InstanceID] = config
	manager.ready[actual.InstanceID] = true
	manager.resyncs++
	inputs, outputs := 0, 0
	for _, endpoint := range wiring.Endpoints {
		if endpoint.Direction == proxy.DirectionInput {
			inputs++
		} else {
			outputs++
		}
	}
	return proxy.Status{
		Version:                proxy.ConfigVersion,
		ControlProtocolVersion: manager.controlVersions[actual.InstanceID],
		InstanceID:             actual.InstanceID, Digest: digest, PID: actual.PID, Ready: true,
		Inputs: inputs, Outputs: outputs,
	}, nil
}

func (manager *fakeProxyManager) Stop(ctx context.Context, process proxy.Process) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopAttempts++
	if len(manager.stopErrors) != 0 {
		err := manager.stopErrors[0]
		manager.stopErrors = manager.stopErrors[1:]
		return err
	}
	actual, exists := manager.processAtControlLocked(process.Control)
	if !exists {
		return proxy.ErrNotRunning
	}
	if actual.InstanceID != process.InstanceID || actual.PID != process.PID ||
		actual.RuntimeDir != process.RuntimeDir || actual.Control != process.Control ||
		actual.Log != process.Log {
		return proxy.ErrIdentityMismatch
	}
	delete(manager.processes, actual.InstanceID)
	delete(manager.configs, actual.InstanceID)
	delete(manager.ready, actual.InstanceID)
	delete(manager.controlVersions, actual.InstanceID)
	manager.stops++
	if len(manager.stopAfterErrors) != 0 {
		err := manager.stopAfterErrors[0]
		manager.stopAfterErrors = manager.stopAfterErrors[1:]
		return err
	}
	return nil
}

func (manager *fakeProxyManager) processAtControlLocked(control string) (proxy.Process, bool) {
	for _, process := range manager.processes {
		if process.Control == control {
			return process, true
		}
	}
	return proxy.Process{}, false
}

func (manager *fakeProxyManager) Logs(
	ctx context.Context,
	_ proxy.Process,
	_ bool,
	_ func(proxy.LogLine) error,
) error {
	return ctx.Err()
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
	// Docker can omit the network ID from inspect until first start, but the
	// create/connect request still pins one exact network generation.
	configuredNetworks map[string]map[string]engine.NetworkAttachment

	inspectNetworkErrors   map[string][]error
	inspectVolumeErrors    map[string][]error
	inspectContainerErrors map[string][]error
	createNetworkErrors    map[string][]error
	createVolumeErrors     map[string][]error
	createContainerErrors  map[string][]error
	connectErrors          map[string][]error
	connectAnyErrors       []error
	disconnectErrors       map[string][]error
	forceDisconnectErrors  map[string][]error
	orphanOnRemove         map[string]bool
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
	nextEndpoint  int
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
		configuredNetworks:     make(map[string]map[string]engine.NetworkAttachment),
		inspectNetworkErrors:   make(map[string][]error),
		inspectVolumeErrors:    make(map[string][]error),
		inspectContainerErrors: make(map[string][]error),
		createNetworkErrors:    make(map[string][]error),
		createVolumeErrors:     make(map[string][]error),
		createContainerErrors:  make(map[string][]error),
		connectErrors:          make(map[string][]error),
		disconnectErrors:       make(map[string][]error),
		forceDisconnectErrors:  make(map[string][]error),
		orphanOnRemove:         make(map[string]bool),
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
	if len(network.Endpoints) != 0 {
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
	configuredNetworks := make(map[string]engine.NetworkAttachment)
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
		configuredNetworks[network.Name] = engine.NetworkAttachment{
			NetworkID: request.NetworkID,
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
	fake.configuredNetworks[id] = configuredNetworks
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
	if _, exists := fake.configuredNetworks[containerID][network.Name]; exists {
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
	if fake.configuredNetworks[containerID] == nil {
		fake.configuredNetworks[containerID] = make(map[string]engine.NetworkAttachment)
	}
	fake.configuredNetworks[containerID][network.Name] = engine.NetworkAttachment{
		NetworkID: networkID,
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
			delete(fake.configuredNetworks[containerID], name)
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

func (fake *fakeEngine) ForceDisconnectNetworkEndpoint(
	ctx context.Context,
	networkID string,
	endpointName string,
) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.record("force-disconnect-network", networkID+"->"+endpointName)
	if err := ctx.Err(); err != nil {
		return err
	}
	network, exists := fake.networks[networkID]
	if !exists {
		return engine.ErrNotFound
	}
	if err := popFailure(
		fake.forceDisconnectErrors,
		networkID+"->"+endpointName,
	); err != nil {
		return err
	}
	matched := -1
	for index, endpoint := range network.Endpoints {
		if endpoint.Name != endpointName {
			continue
		}
		if matched >= 0 {
			return fmt.Errorf("multiple endpoints named %s", endpointName)
		}
		matched = index
	}
	if matched < 0 {
		return engine.ErrNotFound
	}
	network.Endpoints = append(
		append([]engine.NetworkEndpoint(nil), network.Endpoints[:matched]...),
		network.Endpoints[matched+1:]...,
	)
	fake.networks[networkID] = network
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
	return fake.setContainerRunning(id, component, container)
}

func (fake *fakeEngine) setContainerRunning(
	id string,
	component string,
	container engine.Container,
) error {
	configured := fake.configuredNetworks[id]
	for name, attachment := range configured {
		network, exists := fake.networks[attachment.NetworkID]
		if !exists || network.Name != name {
			return fmt.Errorf(
				"%w: configured network %s (%s)",
				engine.ErrNotFound,
				name,
				attachment.NetworkID,
			)
		}
	}
	health := fake.healthOnStart[component]
	if health == "" {
		health = engine.HealthHealthy
	}
	container.Status = "running"
	container.Running = true
	container.ExitCode = 0
	container.Error = ""
	container.Health = health
	for name, attachment := range configured {
		networkID := attachment.NetworkID
		container.Networks[name] = attachment
		fake.attach(networkID, id)
	}
	fake.containers[id] = container
	if hook := fake.afterStart[component]; hook != nil {
		delete(fake.afterStart, component)
		hook()
	}
	return nil
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
	if err := fake.setContainerRunning(id, component, container); err != nil {
		return err
	}
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
	for _, attachment := range fake.configuredNetworks[id] {
		networkID := attachment.NetworkID
		if fake.orphanOnRemove[id] {
			fake.orphan(networkID, id)
		} else {
			fake.detach(networkID, id)
		}
	}
	delete(fake.containers, id)
	delete(fake.containerNames, container.Name)
	delete(fake.configuredNetworks, id)
	return nil
}

func (fake *fakeEngine) attach(networkID, containerID string) {
	network := fake.networks[networkID]
	for _, existing := range network.Endpoints {
		if existing.Key == containerID {
			return
		}
	}
	fake.nextEndpoint++
	network.Endpoints = append(network.Endpoints, engine.NetworkEndpoint{
		Key:        containerID,
		Name:       fake.containers[containerID].Name,
		EndpointID: fmt.Sprintf("endpoint-%d", fake.nextEndpoint),
	})
	sort.Slice(network.Endpoints, func(i, j int) bool {
		return network.Endpoints[i].Key < network.Endpoints[j].Key
	})
	fake.networks[networkID] = network
}

func (fake *fakeEngine) detach(networkID, containerID string) {
	network, exists := fake.networks[networkID]
	if !exists {
		return
	}
	filtered := network.Endpoints[:0]
	for _, existing := range network.Endpoints {
		if existing.Key != containerID {
			filtered = append(filtered, existing)
		}
	}
	network.Endpoints = append([]engine.NetworkEndpoint(nil), filtered...)
	fake.networks[networkID] = network
}

func (fake *fakeEngine) orphan(networkID, containerID string) {
	network, exists := fake.networks[networkID]
	if !exists {
		return
	}
	for index, endpoint := range network.Endpoints {
		if endpoint.Key == containerID {
			network.Endpoints[index].Key = "ep-" + endpoint.EndpointID
		}
	}
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
			"connect-network", "disconnect-network", "force-disconnect-network",
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
	for _, attachment := range fake.configuredNetworks[id] {
		networkID := attachment.NetworkID
		fake.detach(networkID, id)
	}
	delete(fake.containers, id)
	delete(fake.containerNames, container.Name)
	delete(fake.configuredNetworks, id)
}

func (fake *fakeEngine) exitContainerOutOfBand(id string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	container, exists := fake.containers[id]
	if !exists {
		return
	}
	container.Status = "exited"
	container.Running = false
	container.Health = engine.HealthNone
	fake.containers[id] = container
}

func (fake *fakeEngine) orphanContainerOutOfBand(id string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	container, exists := fake.containers[id]
	if !exists {
		return
	}
	for _, attachment := range fake.configuredNetworks[id] {
		networkID := attachment.NetworkID
		fake.orphan(networkID, id)
	}
	delete(fake.containers, id)
	delete(fake.containerNames, container.Name)
	delete(fake.configuredNetworks, id)
}

func cloneImage(input engine.Image) engine.Image {
	input.DeclaredVolumes = append([]string(nil), input.DeclaredVolumes...)
	return input
}

func cloneNetwork(input engine.Network) engine.Network {
	input.Labels = cloneStrings(input.Labels)
	input.Endpoints = append([]engine.NetworkEndpoint(nil), input.Endpoints...)
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
