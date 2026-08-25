package lifecycle

import (
	"context"
	"errors"
	"sort"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

type ComponentStatus struct {
	Name           string
	ID             string
	Status         string
	Health         engine.Health
	ExitCode       int
	Problem        string
	PublishedPorts []engine.PortBinding
}

type NetworkStatus struct {
	Key      string
	ID       string
	Internal bool
	Problem  string
}

type ProxyStatus struct {
	InstanceID        string
	Digest            string
	PID               int
	Ready             bool
	Inputs            int
	Outputs           int
	ActiveConnections int64
	Links             []proxy.LinkMetrics
	Problem           string
}

type Status struct {
	Name               string
	Desired            bool
	Digest             string
	Operation          string
	Phase              string
	Proxy              ProxyStatus
	Networks           []NetworkStatus
	Components         []ComponentStatus
	RetiringNetworks   []NetworkStatus
	RetiringComponents []ComponentStatus
	// Spec is the resolved system the reported resources belong to: the
	// pending operation's target when one is recorded, otherwise the committed
	// deployment. Nil when the system is absent.
	Spec *composition.ResolvedSpec
}

type ComponentProcess struct {
	System    string
	Operation string
	Phase     string
	ComponentStatus
}

func (status Status) Operational() bool {
	if !status.Desired || status.Operation != "" ||
		len(status.Components) == 0 {
		return false
	}
	if !status.Proxy.Ready || status.Proxy.Problem != "" {
		return false
	}
	for _, network := range status.Networks {
		if network.Problem != "" {
			return false
		}
	}
	for _, component := range status.Components {
		if component.Problem != "" ||
			component.Status != "running" ||
			component.Health != engine.HealthHealthy {
			return false
		}
	}
	return true
}

// Processes observes components across recorded systems. By default it
// returns only containers Docker reports as running; includeAll also returns
// created, exited, missing, and otherwise degraded component records.
func (controller *Controller) Processes(
	ctx context.Context,
	system string,
	includeAll bool,
) ([]ComponentProcess, error) {
	if err := controller.validate(); err != nil {
		return nil, err
	}
	var names []string
	if system != "" {
		names = []string{system}
	} else {
		var err error
		names, err = controller.State.Systems()
		if err != nil {
			return nil, err
		}
	}
	result := make([]ComponentProcess, 0)
	for _, name := range names {
		status, err := controller.Status(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, component := range status.Components {
			if !includeAll && component.Status != "running" {
				continue
			}
			result = append(result, ComponentProcess{
				System:          status.Name,
				Operation:       status.Operation,
				Phase:           status.Phase,
				ComponentStatus: component,
			})
		}
	}
	return result, nil
}

// Status observes only; it never repairs, starts, stops, or deletes anything.
func (controller *Controller) Status(ctx context.Context, name string) (Status, error) {
	if err := controller.validate(); err != nil {
		return Status{}, err
	}
	result := Status{Name: name}
	lock, stateExists, err := controller.State.AcquireShared(ctx, name)
	if err != nil {
		return Status{}, err
	}
	if !stateExists {
		return result, nil
	}
	defer lock.Close()

	operation, operationExists, err := controller.State.ReadOperation(name)
	if err != nil {
		return Status{}, err
	}
	if operationExists {
		if err := controller.verifyEngineBinding(ctx); err != nil {
			return Status{}, err
		}
		result.Operation = operation.Kind
		result.Phase = operation.Phase
		result.Digest = operation.Target.Digest
		result.Spec = &operation.Target
		controller.observeStatusResources(
			ctx,
			&result,
			operation.Target,
			operation.Networks,
			operation.Containers,
			operation.Proxy,
			operation.RuntimeRoot,
			false,
		)
		controller.observeRetiringResources(ctx, &result, operation)
		return result, nil
	}
	desired, desiredExists, err := controller.State.ReadDesired(name)
	if err != nil {
		return Status{}, err
	}
	if !desiredExists {
		return result, nil
	}
	if err := controller.verifyEngineBinding(ctx); err != nil {
		return Status{}, err
	}
	result.Desired = true
	result.Digest = desired.Spec.Digest
	result.Spec = &desired.Spec
	controller.observeStatusResources(
		ctx,
		&result,
		desired.Spec,
		desired.Networks,
		desired.Containers,
		desired.Proxy,
		desired.RuntimeRoot,
		true,
	)
	return result, nil
}

func (controller *Controller) observeRetiringResources(
	ctx context.Context,
	result *Status,
	operation state.Operation,
) {
	if operation.Previous == nil || operation.Kind != kindApply {
		return
	}
	previous := *operation.Previous
	scope := controller.dockerScope(previous.Spec.Name)
	plans, err := resolvedTopology(scope, previous.Spec)
	if err != nil {
		result.RetiringNetworks = append(result.RetiringNetworks, NetworkStatus{
			Key: "topology", Problem: err.Error(),
		})
		return
	}
	for _, component := range previous.Spec.Components {
		resource, exists := previous.Containers[component.Name]
		if !exists || resource.ID == "" {
			continue
		}
		if retained, exists := operation.Containers[component.Name]; exists &&
			retained.ID == resource.ID {
			continue
		}
		status := controller.observeComponent(
			ctx,
			previous.Spec,
			plans,
			previous.Networks,
			component,
			previous.Proxy.RuntimeDir,
			resource,
			true,
		)
		awaitingEndpoint := controller.previousEndpointAwaitsCleanup(
			ctx,
			operation,
			component,
			resource,
			previous.Networks,
		)
		if status.Status != "missing" || awaitingEndpoint {
			if status.Status == "missing" && awaitingEndpoint {
				status.Problem = "recorded container is absent; network endpoint awaits cleanup"
			}
			result.RetiringComponents = append(result.RetiringComponents, status)
		}
	}
	for _, key := range sortedNetworkKeys(plans) {
		resource, exists := previous.Networks[key]
		if !exists || resource.ID == "" {
			continue
		}
		if retained, exists := operation.Networks[key]; exists &&
			retained.ID == resource.ID {
			continue
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		status := NetworkStatus{
			Key: key, ID: resource.ID, Internal: plans[key].Internal,
		}
		if inspectErr != nil {
			status.Problem = inspectErr.Error()
		} else if err := verifyNetwork(
			scope,
			plans[key],
			resource,
			actual,
		); err != nil {
			status.Problem = err.Error()
		}
		result.RetiringNetworks = append(result.RetiringNetworks, status)
	}
}

func (controller *Controller) previousEndpointAwaitsCleanup(
	ctx context.Context,
	operation state.Operation,
	component composition.ResolvedComponent,
	container state.Resource,
	networks map[string]state.Resource,
) bool {
	for _, cleanup := range operation.EndpointCleanups {
		if cleanup.ContainerID == container.ID {
			return true
		}
	}
	if !component.Runtime.ExternalEgress {
		return false
	}
	network, exists := networks[componentNetworkKey(component.Name)]
	if !exists {
		return false
	}
	actual, err := controller.inspectNetwork(ctx, network.ID)
	if errors.Is(err, engine.ErrNotFound) {
		return false
	}
	if err != nil {
		return true
	}
	for _, endpoint := range actual.Endpoints {
		if endpoint.Name == container.Name &&
			endpoint.EndpointID != "" &&
			endpoint.Key == "ep-"+endpoint.EndpointID {
			return true
		}
	}
	return false
}

func (controller *Controller) observeStatusResources(
	ctx context.Context,
	result *Status,
	spec composition.ResolvedSpec,
	networks map[string]state.Resource,
	containers map[string]state.Resource,
	process *proxy.Process,
	runtimeRoot string,
	verifyMembers bool,
) {
	if process == nil {
		result.Proxy.Problem = "not created"
	} else {
		result.Proxy.InstanceID = process.InstanceID
		result.Proxy.PID = process.PID
		status, inspectErr := controller.inspectProxy(ctx, *process)
		if errors.Is(inspectErr, proxy.ErrNotRunning) {
			result.Proxy.Problem = "recorded proxy is absent"
		} else if inspectErr != nil {
			result.Proxy.Problem = inspectErr.Error()
		} else {
			result.Proxy.Digest = status.Digest
			result.Proxy.Ready = status.Ready
			result.Proxy.Inputs = status.Inputs
			result.Proxy.Outputs = status.Outputs
			result.Proxy.ActiveConnections = status.ActiveConnections
			result.Proxy.Links = append([]proxy.LinkMetrics(nil), status.Links...)
			wiring, wiringErr := proxy.NewWiring(spec)
			if wiringErr != nil {
				result.Proxy.Problem = wiringErr.Error()
			} else if expectedDigest, digestErr := wiring.Digest(); digestErr != nil {
				result.Proxy.Problem = digestErr.Error()
			} else if status.Ready && status.Digest != expectedDigest {
				result.Proxy.Problem = "proxy reports unexpected wiring digest"
			}
		}
	}
	expectedRuntimeDir := runtimeDirectory(runtimeRoot, spec.Name)
	if process != nil {
		expectedRuntimeDir = process.RuntimeDir
	}
	scope := controller.dockerScope(spec.Name)
	plans, err := resolvedTopology(scope, spec)
	if err != nil {
		result.Networks = append(result.Networks, NetworkStatus{
			Key: "topology", Problem: err.Error(),
		})
		return
	}
	for _, key := range sortedNetworkKeys(plans) {
		status := NetworkStatus{Key: key, Internal: plans[key].Internal}
		resource, exists := networks[key]
		if !exists || resource.ID == "" {
			status.Problem = "not created"
			result.Networks = append(result.Networks, status)
			continue
		}
		status.ID = resource.ID
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			status.Problem = "recorded network is absent"
		} else if inspectErr != nil {
			status.Problem = inspectErr.Error()
		} else if err := verifyNetwork(
			scope,
			plans[key],
			resource,
			actual,
		); err != nil {
			status.Problem = err.Error()
		} else if verifyMembers {
			if err := verifyNetworkMembers(plans[key], actual, containers); err != nil {
				status.Problem = err.Error()
			}
		}
		result.Networks = append(result.Networks, status)
	}
	for _, component := range spec.Components {
		resource, exists := containers[component.Name]
		result.Components = append(
			result.Components,
			controller.observeComponent(
				ctx,
				spec,
				plans,
				networks,
				component,
				expectedRuntimeDir,
				resource,
				exists,
			),
		)
	}
	sort.Slice(result.Components, func(i, j int) bool {
		return result.Components[i].Name < result.Components[j].Name
	})
}

func (controller *Controller) observeComponent(
	ctx context.Context,
	spec composition.ResolvedSpec,
	plans map[string]networkPlan,
	networks map[string]state.Resource,
	component composition.ResolvedComponent,
	runtimeDir string,
	resource state.Resource,
	exists bool,
) ComponentStatus {
	result := ComponentStatus{Name: component.Name}
	if !exists || resource.ID == "" {
		result.Status = "missing"
		result.Problem = "not created"
		return result
	}
	result.ID = resource.ID
	actual, err := controller.inspectContainer(ctx, resource.ID)
	if errors.Is(err, engine.ErrNotFound) {
		result.Status = "missing"
		result.Problem = "recorded container is absent"
		return result
	}
	if err != nil {
		result.Status = "unknown"
		result.Problem = err.Error()
		return result
	}
	result.Status = actual.Status
	result.Health = actual.Health
	result.ExitCode = actual.ExitCode
	result.PublishedPorts = append(
		[]engine.PortBinding(nil), actual.PublishedPorts...,
	)
	sort.Slice(result.PublishedPorts, func(i, j int) bool {
		left := result.PublishedPorts[i]
		right := result.PublishedPorts[j]
		if left.HostIP != right.HostIP {
			return left.HostIP < right.HostIP
		}
		if left.HostPort != right.HostPort {
			return left.HostPort < right.HostPort
		}
		if left.ContainerPort != right.ContainerPort {
			return left.ContainerPort < right.ContainerPort
		}
		return left.Protocol < right.Protocol
	})
	if err := verifyCurrentContainer(
		controller.dockerScope(spec.Name),
		runtimeDir,
		component,
		resource,
		actual,
	); err != nil {
		result.Problem = err.Error()
		return result
	}
	if err := verifyContainerEnvironment(spec, component, actual); err != nil {
		result.Problem = err.Error()
		return result
	}
	if err := verifyContainerNetworks(component, plans, networks, actual); err != nil {
		result.Problem = err.Error()
	}
	return result
}
