package lifecycle

import (
	"context"
	"errors"
	"sort"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
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

type Status struct {
	Name       string
	Desired    bool
	Digest     string
	Operation  string
	Phase      string
	Networks   []NetworkStatus
	Components []ComponentStatus
}

func (status Status) Operational() bool {
	if !status.Desired || status.Operation != "" ||
		len(status.Networks) == 0 || len(status.Components) == 0 {
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
		controller.observeStatusResources(
			ctx,
			&result,
			operation.Target,
			operation.Networks,
			operation.Containers,
			false,
		)
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
	controller.observeStatusResources(
		ctx,
		&result,
		desired.Spec,
		desired.Networks,
		desired.Containers,
		true,
	)
	return result, nil
}

func (controller *Controller) observeStatusResources(
	ctx context.Context,
	result *Status,
	spec composition.ResolvedSpec,
	networks map[string]state.Resource,
	containers map[string]state.Resource,
	verifyMembers bool,
) {
	plans, err := resolvedTopology(spec)
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
			spec.Name,
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
	if err := verifyContainerCore(spec.Name, component, resource, actual); err != nil {
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
