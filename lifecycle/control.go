package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/state"
)

func (controller *Controller) executeAbort(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Kind != kindApply {
		if err := controller.State.ClearOperation(operation.Target.Name); err != nil {
			return err
		}
		controller.report("aborted %s for %s", operation.Kind, operation.Target.Name)
		return nil
	}
	plans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	previousContainerIDs := make(map[string]struct{})
	previousNetworkIDs := make(map[string]struct{})
	if operation.Previous != nil {
		for _, resource := range operation.Previous.Containers {
			previousContainerIDs[resource.ID] = struct{}{}
		}
		for _, resource := range operation.Previous.Networks {
			previousNetworkIDs[resource.ID] = struct{}{}
		}
	}

	containerCandidates := cloneResources(operation.Containers)
	for _, component := range operation.Target.Components {
		actual, inspectErr := controller.inspectContainer(
			ctx,
			containerName(operation.Target.Name, component.Name),
		)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if actual.Labels[LabelOperation] == operation.ID {
			containerCandidates[component.Name] = state.Resource{
				ID: actual.ID, Name: actual.Name,
			}
		}
	}
	networkCandidates := cloneResources(operation.Networks)
	for _, key := range sortedNetworkKeys(plans) {
		actual, inspectErr := controller.inspectNetwork(ctx, plans[key].Name)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if actual.Labels[LabelOperation] == operation.ID {
			networkCandidates[key] = state.Resource{
				ID: actual.ID, Name: actual.Name,
			}
		}
	}

	// Verify every object before the first destructive call.
	for _, component := range operation.Target.Components {
		resource, exists := containerCandidates[component.Name]
		if !exists {
			continue
		}
		if _, retained := previousContainerIDs[resource.ID]; retained {
			continue
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf(
				"refusing to abort over container %s without operation ownership",
				resource.ID,
			)
		}
		if err := verifyContainerCore(
			operation.Target.Name,
			component,
			resource,
			actual,
		); err != nil {
			return err
		}
	}
	for key, resource := range networkCandidates {
		if _, retained := previousNetworkIDs[resource.ID]; retained {
			continue
		}
		plan, exists := plans[key]
		if !exists {
			return fmt.Errorf("abort found unknown target network %q", key)
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf(
				"refusing to abort over network %s without operation ownership",
				resource.ID,
			)
		}
		if err := verifyNetwork(
			operation.Target.Name,
			plan,
			resource,
			actual,
		); err != nil {
			return err
		}
	}

	for _, component := range operation.Target.Components {
		resource, exists := containerCandidates[component.Name]
		if !exists {
			continue
		}
		if _, retained := previousContainerIDs[resource.ID]; retained {
			continue
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if actual.Running {
			callCtx, cancel := controller.callContext(ctx)
			err := controller.Engine.StopContainer(
				callCtx,
				resource.ID,
				controller.stopTimeout(),
			)
			cancel()
			if err != nil && !errors.Is(err, engine.ErrNotFound) {
				return fmt.Errorf("stop %s during abort: %w", component.Name, err)
			}
		}
		callCtx, cancel := controller.callContext(ctx)
		err = controller.Engine.RemoveContainer(callCtx, resource.ID)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf("remove %s during abort: %w", component.Name, err)
		}
	}

	// A retained producer may have been attached to a new interface network
	// before interruption. Detach only exact operation-owned networks.
	if operation.Previous != nil {
		for _, resource := range operation.Previous.Containers {
			actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
			if errors.Is(inspectErr, engine.ErrNotFound) {
				continue
			}
			if inspectErr != nil {
				return inspectErr
			}
			for _, network := range networkCandidates {
				if _, retained := previousNetworkIDs[network.ID]; retained {
					continue
				}
				if _, _, attached := findContainerNetwork(actual, network); !attached {
					continue
				}
				callCtx, cancel := controller.callContext(ctx)
				err := controller.Engine.DisconnectNetwork(
					callCtx,
					network.ID,
					resource.ID,
				)
				cancel()
				if err != nil && !errors.Is(err, engine.ErrNotFound) {
					return fmt.Errorf(
						"detach retained container during abort: %w",
						err,
					)
				}
			}
		}
	}
	for _, key := range sortedNetworkKeys(plans) {
		resource, exists := networkCandidates[key]
		if !exists {
			continue
		}
		if _, retained := previousNetworkIDs[resource.ID]; retained {
			continue
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if len(actual.Containers) != 0 {
			return fmt.Errorf(
				"cannot abort network %s with attached containers",
				key,
			)
		}
		callCtx, cancel := controller.callContext(ctx)
		err = controller.Engine.RemoveNetwork(callCtx, resource.ID)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf("remove network %s during abort: %w", key, err)
		}
	}
	if operation.Previous != nil {
		if err := controller.State.WriteDesired(
			operation.Target.Name,
			*operation.Previous,
		); err != nil {
			return err
		}
	} else if err := controller.State.ClearDesired(operation.Target.Name); err != nil {
		return err
	}
	if err := controller.State.ClearOperation(operation.Target.Name); err != nil {
		return err
	}
	controller.report("aborted apply for %s", operation.Target.Name)
	return nil
}

func (controller *Controller) executeDown(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Phase != phaseDown {
		return fmt.Errorf("unknown down phase %q", operation.Phase)
	}
	if operation.Previous == nil {
		return fmt.Errorf("down operation has no committed deployment")
	}
	if len(operation.Completed) == 0 {
		if err := controller.preflightCommittedDeployment(
			ctx,
			*operation.Previous,
		); err != nil {
			return err
		}
	}
	for _, component := range operation.Previous.Spec.Components {
		key := "container/" + component.Name
		if operation.Completed[key] {
			continue
		}
		resource := operation.Containers[component.Name]
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			if err := controller.markComplete(operation, key); err != nil {
				return err
			}
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyContainerCore(
			operation.Target.Name,
			component,
			resource,
			actual,
		); err != nil {
			return err
		}
		if actual.Running {
			callCtx, cancel := controller.callContext(ctx)
			err := controller.Engine.StopContainer(
				callCtx,
				resource.ID,
				controller.stopTimeout(),
			)
			cancel()
			if err != nil && !errors.Is(err, engine.ErrNotFound) {
				return fmt.Errorf("stop %s: %w", component.Name, err)
			}
		}
		callCtx, cancel := controller.callContext(ctx)
		err := controller.Engine.RemoveContainer(callCtx, resource.ID)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf("remove %s: %w", component.Name, err)
		}
		if err := controller.markComplete(operation, key); err != nil {
			return err
		}
	}
	plans, err := resolvedTopology(operation.Previous.Spec)
	if err != nil {
		return err
	}
	for _, networkKey := range sortedNetworkKeys(plans) {
		key := "network/" + networkKey
		if operation.Completed[key] {
			continue
		}
		resource := operation.Networks[networkKey]
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			if err := controller.markComplete(operation, key); err != nil {
				return err
			}
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyNetwork(
			operation.Target.Name,
			plans[networkKey],
			resource,
			actual,
		); err != nil {
			return err
		}
		if len(actual.Containers) != 0 {
			return fmt.Errorf("network %s still has attached containers", networkKey)
		}
		callCtx, cancel := controller.callContext(ctx)
		err = controller.Engine.RemoveNetwork(callCtx, resource.ID)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf("remove network %s: %w", networkKey, err)
		}
		if err := controller.markComplete(operation, key); err != nil {
			return err
		}
	}
	if err := controller.State.ClearDesired(operation.Target.Name); err != nil {
		return err
	}
	if err := controller.State.ClearOperation(operation.Target.Name); err != nil {
		return err
	}
	controller.report("removed %s; persistent volumes were preserved", operation.Target.Name)
	return nil
}

func (controller *Controller) executeRestart(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Previous == nil {
		return fmt.Errorf("restart operation has no committed deployment")
	}
	if operation.Phase != phaseRestart {
		return fmt.Errorf("unknown restart phase %q", operation.Phase)
	}
	if err := controller.preflightSelectedContainers(
		ctx,
		*operation.Previous,
		operation.Components,
	); err != nil {
		return err
	}
	for _, name := range operation.Components {
		if operation.Completed[name] {
			continue
		}
		resource := operation.Containers[name]
		callCtx, cancel := controller.callContext(ctx)
		err := controller.Engine.RestartContainer(
			callCtx,
			resource.ID,
			controller.stopTimeout(),
		)
		cancel()
		if err != nil {
			return fmt.Errorf("restart %s: %w", name, err)
		}
		if err := controller.markComplete(operation, name); err != nil {
			return err
		}
	}
	if err := controller.State.ClearOperation(operation.Target.Name); err != nil {
		return err
	}
	controller.report("restarted selected components in %s", operation.Target.Name)
	return nil
}

func (controller *Controller) preflightCommittedDeployment(
	ctx context.Context,
	deployment state.Deployment,
) error {
	plans, err := resolvedTopology(deployment.Spec)
	if err != nil {
		return err
	}
	for _, component := range deployment.Spec.Components {
		resource, exists := deployment.Containers[component.Name]
		if !exists {
			return fmt.Errorf("deployment has no %s container", component.Name)
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyContainerCore(
			deployment.Spec.Name,
			component,
			resource,
			actual,
		); err != nil {
			return err
		}
		if err := verifyContainerEnvironment(deployment.Spec, component, actual); err != nil {
			return err
		}
		if err := verifyNoUnknownContainerNetworks(
			component,
			deployment.Networks,
			actual,
		); err != nil {
			return err
		}
	}
	for _, key := range sortedNetworkKeys(plans) {
		resource, exists := deployment.Networks[key]
		if !exists {
			return fmt.Errorf("deployment has no network %q", key)
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyNetwork(
			deployment.Spec.Name,
			plans[key],
			resource,
			actual,
		); err != nil {
			return err
		}
		if err := verifyNoUnknownNetworkMembers(
			plans[key],
			actual,
			deployment.Containers,
		); err != nil {
			return err
		}
	}
	return nil
}

func (controller *Controller) preflightSelectedContainers(
	ctx context.Context,
	deployment state.Deployment,
	selected []string,
) error {
	plans, err := resolvedTopology(deployment.Spec)
	if err != nil {
		return err
	}
	for _, name := range selected {
		component, exists := deployment.Spec.Component(name)
		if !exists {
			return fmt.Errorf("deployment has no component %q", name)
		}
		resource, exists := deployment.Containers[name]
		if !exists {
			return fmt.Errorf("deployment has no %s container", name)
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyContainerCore(
			deployment.Spec.Name,
			component,
			resource,
			actual,
		); err != nil {
			return err
		}
		if err := verifyContainerEnvironment(deployment.Spec, component, actual); err != nil {
			return err
		}
		if err := verifyContainerNetworks(
			component,
			plans,
			deployment.Networks,
			actual,
		); err != nil {
			return err
		}
	}
	return nil
}
