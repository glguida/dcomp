package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/state"
)

func (controller *Controller) deploymentMatches(
	ctx context.Context,
	deployment state.Deployment,
	target composition.ResolvedSpec,
) (bool, error) {
	if deployment.Spec.Digest != target.Digest {
		return false, nil
	}
	plans, err := resolvedTopology(target)
	if err != nil {
		return false, err
	}
	if len(deployment.Networks) != len(plans) ||
		len(deployment.Containers) != len(target.Components) {
		return false, nil
	}
	for _, key := range sortedNetworkKeys(plans) {
		resource, exists := deployment.Networks[key]
		if !exists {
			return false, nil
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			return false, nil
		}
		if inspectErr != nil {
			return false, inspectErr
		}
		if err := verifyNetwork(target.Name, plans[key], resource, actual); err != nil {
			return false, err
		}
	}
	for _, component := range target.Components {
		resource, exists := deployment.Containers[component.Name]
		if !exists {
			return false, nil
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			return false, nil
		}
		if inspectErr != nil {
			return false, inspectErr
		}
		if err := verifyContainerCore(target.Name, component, resource, actual); err != nil {
			return false, err
		}
		if err := verifyContainerEnvironment(target, component, actual); err != nil {
			return false, err
		}
		if err := verifyNoUnknownContainerNetworks(
			component,
			deployment.Networks,
			actual,
		); err != nil {
			return false, err
		}
		if err := verifyContainerNetworks(component, plans, deployment.Networks, actual); err != nil {
			return false, nil
		}
		if !actual.Running {
			return false, nil
		}
		for _, volume := range component.Runtime.Volumes {
			actualVolume, inspectErr := controller.inspectVolume(
				ctx,
				volumeName(target.Name, component.Name, volume.Name),
			)
			if errors.Is(inspectErr, engine.ErrNotFound) {
				return false, nil
			}
			if inspectErr != nil {
				return false, inspectErr
			}
			if err := verifyVolume(
				target.Name,
				component.Name,
				volume.Name,
				actualVolume,
			); err != nil {
				return false, err
			}
		}
	}
	for _, key := range sortedNetworkKeys(plans) {
		actual, err := controller.inspectNetwork(ctx, deployment.Networks[key].ID)
		if err != nil {
			return false, err
		}
		if err := verifyNoUnknownNetworkMembers(
			plans[key],
			actual,
			deployment.Containers,
		); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (controller *Controller) selectRetainedResources(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Previous == nil {
		return nil
	}
	targetPlans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	previousPlans, err := resolvedTopology(operation.Previous.Spec)
	if err != nil {
		return err
	}
	for _, key := range sortedNetworkKeys(targetPlans) {
		targetPlan := targetPlans[key]
		previousPlan, exists := previousPlans[key]
		if !exists || previousPlan.Digest != targetPlan.Digest {
			continue
		}
		resource, exists := operation.Previous.Networks[key]
		if !exists {
			continue
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyNetwork(
			operation.Target.Name,
			targetPlan,
			resource,
			actual,
		); err != nil {
			return err
		}
		operation.Networks[key] = resource
	}
	for _, component := range operation.Target.Components {
		previousComponent, exists := operation.Previous.Spec.Component(component.Name)
		if !exists || previousComponent.Digest != component.Digest {
			continue
		}
		resource, exists := operation.Previous.Containers[component.Name]
		if !exists {
			continue
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
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
		if err := verifyContainerEnvironment(
			operation.Target,
			component,
			actual,
		); err != nil {
			return err
		}
		if !actual.Running {
			continue
		}
		operation.Containers[component.Name] = resource
	}
	return nil
}

func (controller *Controller) executeApply(
	ctx context.Context,
	operation *state.Operation,
) error {
	for {
		switch operation.Phase {
		case phaseRetire:
			if err := controller.retireChangedContainers(ctx, operation); err != nil {
				return err
			}
			if err := controller.setPhase(operation, phaseNetworks); err != nil {
				return err
			}
		case phaseNetworks:
			if err := controller.ensureTargetNetworks(ctx, operation); err != nil {
				return err
			}
			if err := controller.setPhase(operation, phaseCreate); err != nil {
				return err
			}
		case phaseCreate:
			if err := controller.ensureTargetContainers(ctx, operation); err != nil {
				return err
			}
			if err := controller.setPhase(operation, phaseAttach); err != nil {
				return err
			}
		case phaseAttach:
			if err := controller.reconcileAttachments(ctx, operation); err != nil {
				return err
			}
			if err := controller.setPhase(operation, phaseStart); err != nil {
				return err
			}
		case phaseStart:
			// A failed Docker start can tear down one of a container's
			// endpoints even though attachment reconciliation already
			// completed durably. Reconcile again so both an immediate retry
			// and a resumed start repair Docker's partial side effects.
			if err := controller.reconcileAttachments(ctx, operation); err != nil {
				return err
			}
			if err := controller.startNewContainers(ctx, operation); err != nil {
				return err
			}
			if err := controller.setPhase(operation, phaseCommit); err != nil {
				return err
			}
		case phaseCommit:
			deployment := state.Deployment{
				Spec:       operation.Target,
				Networks:   cloneResources(operation.Networks),
				Containers: cloneResources(operation.Containers),
			}
			if err := controller.State.WriteDesired(operation.Target.Name, deployment); err != nil {
				return err
			}
			if err := controller.State.ClearOperation(operation.Target.Name); err != nil {
				return err
			}
			controller.report("applied %s", operation.Target.Name)
			return nil
		default:
			return fmt.Errorf("unknown apply phase %q", operation.Phase)
		}
	}
}

func (controller *Controller) retireChangedContainers(
	ctx context.Context,
	operation *state.Operation,
) error {
	if err := controller.preflightApply(ctx, operation); err != nil {
		return err
	}
	if operation.Previous == nil {
		return nil
	}
	for _, component := range operation.Previous.Spec.Components {
		previousResource, exists := operation.Previous.Containers[component.Name]
		if !exists {
			return fmt.Errorf("previous deployment has no %s container", component.Name)
		}
		if retained, exists := operation.Containers[component.Name]; exists &&
			retained.ID == previousResource.ID {
			continue
		}
		progressKey := "retire/" + component.Name
		if operation.Completed[progressKey] {
			continue
		}
		actual, inspectErr := controller.inspectContainer(ctx, previousResource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			if err := controller.markComplete(operation, progressKey); err != nil {
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
			previousResource,
			actual,
		); err != nil {
			return err
		}
		if err := verifyContainerEnvironment(
			operation.Previous.Spec,
			component,
			actual,
		); err != nil {
			return err
		}
		if err := verifyNoUnknownContainerNetworks(
			component,
			operation.Previous.Networks,
			actual,
		); err != nil {
			return err
		}
		if actual.Running {
			callCtx, cancel := controller.callContext(ctx)
			err := controller.Engine.StopContainer(
				callCtx,
				previousResource.ID,
				controller.stopTimeout(),
			)
			cancel()
			if err != nil && !errors.Is(err, engine.ErrNotFound) {
				return fmt.Errorf("stop %s: %w", component.Name, err)
			}
		}
		callCtx, cancel := controller.callContext(ctx)
		removeErr := controller.Engine.RemoveContainer(callCtx, previousResource.ID)
		cancel()
		if removeErr != nil && !errors.Is(removeErr, engine.ErrNotFound) {
			return fmt.Errorf("remove %s: %w", component.Name, removeErr)
		}
		if err := controller.markComplete(operation, progressKey); err != nil {
			return err
		}
		controller.report("retired component %s", component.Name)
	}
	return nil
}

func (controller *Controller) preflightApply(
	ctx context.Context,
	operation *state.Operation,
) error {
	targetPlans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	var previousPlans map[string]networkPlan
	if operation.Previous != nil {
		previousPlans, err = resolvedTopology(operation.Previous.Spec)
		if err != nil {
			return err
		}
		for _, component := range operation.Previous.Spec.Components {
			resource, exists := operation.Previous.Containers[component.Name]
			if !exists {
				return fmt.Errorf("previous deployment has no %s container", component.Name)
			}
			actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
			if errors.Is(inspectErr, engine.ErrNotFound) {
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
			if err := verifyContainerEnvironment(
				operation.Previous.Spec,
				component,
				actual,
			); err != nil {
				return err
			}
			if err := verifyNoUnknownContainerNetworks(
				component,
				operation.Previous.Networks,
				actual,
			); err != nil {
				return err
			}
		}
		if len(operation.Previous.Networks) != len(previousPlans) {
			return fmt.Errorf("previous deployment has an incomplete network map")
		}
		for _, key := range sortedNetworkKeys(previousPlans) {
			resource, exists := operation.Previous.Networks[key]
			if !exists {
				return fmt.Errorf("previous deployment has no network %q", key)
			}
			actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
			if errors.Is(inspectErr, engine.ErrNotFound) {
				continue
			}
			if inspectErr != nil {
				return inspectErr
			}
			if err := verifyNetwork(
				operation.Target.Name,
				previousPlans[key],
				resource,
				actual,
			); err != nil {
				return err
			}
			if err := verifyNoUnknownNetworkMembers(
				previousPlans[key],
				actual,
				operation.Previous.Containers,
			); err != nil {
				return err
			}
		}
	}

	for _, component := range operation.Target.Components {
		actual, inspectErr := controller.inspectContainer(
			ctx,
			containerName(operation.Target.Name, component.Name),
		)
		if inspectErr == nil {
			if !controller.allowedContainerAtTargetName(operation, component.Name, actual.ID) {
				return fmt.Errorf(
					"container name %q is occupied by an unrelated object",
					actual.Name,
				)
			}
		} else if !errors.Is(inspectErr, engine.ErrNotFound) {
			return inspectErr
		}
		for _, volume := range component.Runtime.Volumes {
			actualVolume, inspectErr := controller.inspectVolume(
				ctx,
				volumeName(operation.Target.Name, component.Name, volume.Name),
			)
			if errors.Is(inspectErr, engine.ErrNotFound) {
				continue
			}
			if inspectErr != nil {
				return inspectErr
			}
			if err := verifyVolume(
				operation.Target.Name,
				component.Name,
				volume.Name,
				actualVolume,
			); err != nil {
				return err
			}
		}
	}
	for _, key := range sortedNetworkKeys(targetPlans) {
		plan := targetPlans[key]
		actual, inspectErr := controller.inspectNetwork(ctx, plan.Name)
		if inspectErr == nil {
			if !controller.allowedNetworkAtTargetName(operation, actual.ID) {
				return fmt.Errorf(
					"network name %q is occupied by an unrelated object",
					plan.Name,
				)
			}
		} else if !errors.Is(inspectErr, engine.ErrNotFound) {
			return inspectErr
		}
	}
	return nil
}

func (controller *Controller) allowedContainerAtTargetName(
	operation *state.Operation,
	component string,
	actualID string,
) bool {
	if resource, exists := operation.Containers[component]; exists &&
		resource.ID == actualID {
		return true
	}
	if operation.Previous != nil {
		if resource, exists := operation.Previous.Containers[component]; exists &&
			resource.ID == actualID {
			return true
		}
	}
	return false
}

func (controller *Controller) allowedNetworkAtTargetName(
	operation *state.Operation,
	actualID string,
) bool {
	for _, resources := range []map[string]state.Resource{
		operation.Networks,
		previousNetworks(operation),
	} {
		for _, resource := range resources {
			if resource.ID == actualID {
				return true
			}
		}
	}
	return false
}

func previousNetworks(operation *state.Operation) map[string]state.Resource {
	if operation.Previous == nil {
		return nil
	}
	return operation.Previous.Networks
}

func (controller *Controller) ensureTargetNetworks(
	ctx context.Context,
	operation *state.Operation,
) error {
	plans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	for _, key := range sortedNetworkKeys(plans) {
		if err := controller.removeConflictingPreviousNetwork(
			ctx,
			operation,
			plans[key],
		); err != nil {
			return err
		}
		if err := controller.ensureNetwork(ctx, operation, plans[key]); err != nil {
			return err
		}
	}
	return nil
}

func (controller *Controller) removeConflictingPreviousNetwork(
	ctx context.Context,
	operation *state.Operation,
	plan networkPlan,
) error {
	if operation.Previous == nil {
		return nil
	}
	target, targetExists := operation.Networks[plan.Key]
	previousPlans, err := resolvedTopology(operation.Previous.Spec)
	if err != nil {
		return err
	}
	for key, resource := range operation.Previous.Networks {
		if resource.Name != plan.Name ||
			(targetExists && target.ID == resource.ID) {
			continue
		}
		previousPlan, exists := previousPlans[key]
		if !exists {
			return fmt.Errorf("previous deployment has unknown network %q", key)
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			return nil
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyNetwork(
			operation.Target.Name,
			previousPlan,
			resource,
			actual,
		); err != nil {
			return err
		}
		if len(actual.Containers) != 0 {
			return fmt.Errorf(
				"cannot replace network %s while it still has attached containers",
				previousPlan.Key,
			)
		}
		callCtx, cancel := controller.callContext(ctx)
		err = controller.Engine.RemoveNetwork(callCtx, resource.ID)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf("remove replaced network %s: %w", key, err)
		}
	}
	return nil
}

func (controller *Controller) ensureNetwork(
	ctx context.Context,
	operation *state.Operation,
	plan networkPlan,
) error {
	pendingKey := networkCreateKey(plan.Key)
	if resource, exists := operation.Networks[plan.Key]; exists {
		actual, err := controller.inspectNetwork(ctx, resource.ID)
		if err == nil {
			if err := verifyNetwork(operation.Target.Name, plan, resource, actual); err != nil {
				return err
			}
			return controller.clearPendingCreate(operation, pendingKey)
		}
		if !errors.Is(err, engine.ErrNotFound) {
			return err
		}
		delete(operation.Networks, plan.Key)
		if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
			return err
		}
	}

	if actual, err := controller.inspectNetwork(ctx, plan.Name); err == nil {
		if actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf("network name %q is occupied", plan.Name)
		}
		resource := state.Resource{ID: actual.ID, Name: actual.Name}
		if err := verifyNetwork(
			operation.Target.Name,
			plan,
			resource,
			actual,
		); err != nil {
			return err
		}
		return controller.recordNetwork(operation, plan.Key, resource)
	} else if !errors.Is(err, engine.ErrNotFound) {
		return err
	}

	if err := controller.markPendingCreate(operation, pendingKey); err != nil {
		return err
	}
	callCtx, cancel := controller.callContext(ctx)
	actual, createErr := controller.Engine.CreateNetwork(
		callCtx,
		engine.NetworkRequest{
			Name:     plan.Name,
			Internal: plan.Internal,
			Labels: expectedNetworkLabels(
				operation.Target.Name,
				plan,
				operation.ID,
			),
		},
	)
	cancel()
	if createErr != nil {
		recovered, inspectErr := controller.inspectNetwork(ctx, plan.Name)
		if inspectErr != nil || recovered.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf("create network %s: %w", plan.Key, createErr)
		}
		actual = recovered
	}
	resource := state.Resource{ID: actual.ID, Name: actual.Name}
	if err := verifyNetwork(
		operation.Target.Name,
		plan,
		resource,
		actual,
	); err != nil {
		return err
	}
	if err := controller.recordNetwork(operation, plan.Key, resource); err != nil {
		return err
	}
	controller.report("created network %s", plan.Key)
	return nil
}

func (controller *Controller) ensureTargetContainers(
	ctx context.Context,
	operation *state.Operation,
) error {
	plans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	for _, component := range operation.Target.Components {
		for _, volume := range component.Runtime.Volumes {
			if err := controller.ensureVolume(
				ctx,
				operation.Target.Name,
				component.Name,
				volume.Name,
			); err != nil {
				return err
			}
		}
		if err := controller.ensureContainer(ctx, operation, plans, component); err != nil {
			return err
		}
	}
	return nil
}

func (controller *Controller) ensureContainer(
	ctx context.Context,
	operation *state.Operation,
	plans map[string]networkPlan,
	component composition.ResolvedComponent,
) error {
	pendingKey := containerCreateKey(component.Name)
	if resource, exists := operation.Containers[component.Name]; exists {
		actual, err := controller.inspectContainer(ctx, resource.ID)
		if err == nil {
			if err := verifyContainerCore(
				operation.Target.Name,
				component,
				resource,
				actual,
			); err != nil {
				return err
			}
			if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
				return err
			}
			return controller.clearPendingCreate(operation, pendingKey)
		}
		if !errors.Is(err, engine.ErrNotFound) {
			return err
		}
		delete(operation.Containers, component.Name)
		if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
			return err
		}
	}

	name := containerName(operation.Target.Name, component.Name)
	if actual, err := controller.inspectContainer(ctx, name); err == nil {
		if actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf("container name %q is occupied", name)
		}
		resource := state.Resource{ID: actual.ID, Name: actual.Name}
		if err := verifyContainerCore(
			operation.Target.Name,
			component,
			resource,
			actual,
		); err != nil {
			return err
		}
		if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
			return err
		}
		return controller.recordContainer(operation, component.Name, resource)
	} else if !errors.Is(err, engine.ErrNotFound) {
		return err
	}

	base, exists := operation.Networks[componentNetworkKey(component.Name)]
	if !exists {
		return fmt.Errorf("%s has no base network", component.Name)
	}
	environment, err := componentEnvironment(operation.Target, component)
	if err != nil {
		return err
	}
	if err := controller.markPendingCreate(operation, pendingKey); err != nil {
		return err
	}
	callCtx, cancel := controller.callContext(ctx)
	actual, createErr := controller.Engine.CreateContainer(
		callCtx,
		engine.ContainerRequest{
			Name:           name,
			ImageID:        component.ImageID,
			NetworkID:      base.ID,
			NetworkAliases: []string{component.Name},
			Labels: expectedContainerLabels(
				operation.Target.Name,
				component,
				operation.ID,
			),
			Environment:  environment,
			Mounts:       componentMounts(operation.Target.Name, component),
			Args:         append([]string(nil), component.Runtime.Args...),
			PortBindings: componentPorts(component),
			StopTimeout:  controller.stopTimeout(),
			Security:     componentSecurity(),
		},
	)
	cancel()
	if createErr != nil {
		recovered, inspectErr := controller.inspectContainer(ctx, name)
		if inspectErr != nil || recovered.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf("create component %s: %w", component.Name, createErr)
		}
		actual = recovered
	}
	resource := state.Resource{ID: actual.ID, Name: actual.Name}
	if err := verifyContainerCore(
		operation.Target.Name,
		component,
		resource,
		actual,
	); err != nil {
		return err
	}
	if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
		return err
	}
	if _, _, attached := findContainerNetwork(actual, base); !attached {
		return fmt.Errorf("%s was not created on its base network", component.Name)
	}
	if err := controller.recordContainer(operation, component.Name, resource); err != nil {
		return err
	}
	controller.report("created component %s", component.Name)
	return nil
}

func networkCreateKey(key string) string {
	return "network/" + key
}

func containerCreateKey(name string) string {
	return "container/" + name
}

// resolvePendingCreates turns ambiguous create requests back into exact,
// operation-owned resources before a superseding target is allowed to abort
// them. It performs no attachment or start work for the stale target.
func (controller *Controller) resolvePendingCreates(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Kind != kindApply {
		if len(operation.PendingCreates) != 0 {
			return fmt.Errorf(
				"%s operation has unresolved creates",
				operation.Kind,
			)
		}
		return nil
	}
	plans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(operation.PendingCreates))
	for key, pending := range operation.PendingCreates {
		if !pending {
			return fmt.Errorf("pending create %q is not marked pending", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch {
		case strings.HasPrefix(key, "network/"):
			planKey := strings.TrimPrefix(key, "network/")
			plan, exists := plans[planKey]
			if !exists {
				return fmt.Errorf("pending create names unknown network %q", planKey)
			}
			if err := controller.ensureNetwork(ctx, operation, plan); err != nil {
				return err
			}
		case strings.HasPrefix(key, "container/"):
			name := strings.TrimPrefix(key, "container/")
			component, exists := operation.Target.Component(name)
			if !exists {
				return fmt.Errorf("pending create names unknown component %q", name)
			}
			for _, volume := range component.Runtime.Volumes {
				if err := controller.ensureVolume(
					ctx,
					operation.Target.Name,
					component.Name,
					volume.Name,
				); err != nil {
					return err
				}
			}
			if err := controller.ensureContainer(
				ctx,
				operation,
				plans,
				component,
			); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown pending create %q", key)
		}
	}
	if len(operation.PendingCreates) != 0 {
		return fmt.Errorf("unresolved creates remain after reconciliation")
	}
	return nil
}

func (controller *Controller) markPendingCreate(
	operation *state.Operation,
	key string,
) error {
	if operation.PendingCreates == nil {
		operation.PendingCreates = make(map[string]bool)
	}
	if operation.PendingCreates[key] {
		return nil
	}
	operation.PendingCreates[key] = true
	return controller.State.WriteOperation(operation.Target.Name, *operation)
}

func (controller *Controller) clearPendingCreate(
	operation *state.Operation,
	key string,
) error {
	if !operation.PendingCreates[key] {
		return nil
	}
	delete(operation.PendingCreates, key)
	return controller.State.WriteOperation(operation.Target.Name, *operation)
}

func (controller *Controller) recordNetwork(
	operation *state.Operation,
	key string,
	resource state.Resource,
) error {
	operation.Networks[key] = resource
	delete(operation.PendingCreates, networkCreateKey(key))
	return controller.State.WriteOperation(operation.Target.Name, *operation)
}

func (controller *Controller) recordContainer(
	operation *state.Operation,
	name string,
	resource state.Resource,
) error {
	operation.Containers[name] = resource
	delete(operation.PendingCreates, containerCreateKey(name))
	return controller.State.WriteOperation(operation.Target.Name, *operation)
}

type networkChange struct {
	component string
	container string
	network   string
	aliases   []string
}

func (controller *Controller) reconcileAttachments(
	ctx context.Context,
	operation *state.Operation,
) error {
	plans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	known := make(map[string]state.Resource)
	for _, resource := range operation.Networks {
		known[resource.ID] = resource
	}
	if operation.Previous != nil {
		for _, resource := range operation.Previous.Networks {
			known[resource.ID] = resource
		}
	}

	var connects, reconnects, disconnects []networkChange
	for _, component := range operation.Target.Components {
		resource, exists := operation.Containers[component.Name]
		if !exists {
			return fmt.Errorf("target has no %s container", component.Name)
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
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
		if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
			return err
		}
		expected := make(map[string]state.Resource)
		for _, key := range componentNetworkKeys(component.Name, plans) {
			network := operation.Networks[key]
			expected[network.ID] = network
		}
		for name, attachment := range actual.Networks {
			observed, owned := matchContainerNetwork(name, attachment, known)
			if !owned {
				return fmt.Errorf(
					"%s is attached to undeclared network %s",
					component.Name,
					observedNetworkDescription(name, attachment),
				)
			}
			if _, wanted := expected[observed.ID]; wanted {
				continue
			}
			disconnects = append(disconnects, networkChange{
				component: component.Name,
				container: resource.ID,
				network:   observed.ID,
			})
		}
		for _, network := range expected {
			if _, _, attached := findContainerNetwork(actual, network); !attached {
				connects = append(connects, networkChange{
					component: component.Name,
					container: resource.ID,
					network:   network.ID,
					aliases:   []string{component.Name},
				})
			} else if !containerNetworkHasAlias(
				actual,
				network,
				component.Name,
			) {
				reconnects = append(reconnects, networkChange{
					component: component.Name,
					container: resource.ID,
					network:   network.ID,
					aliases:   []string{component.Name},
				})
			}
		}
	}
	sortNetworkChanges(connects)
	sortNetworkChanges(reconnects)
	sortNetworkChanges(disconnects)
	for _, change := range connects {
		callCtx, cancel := controller.callContext(ctx)
		err := controller.Engine.ConnectNetwork(
			callCtx,
			change.network,
			change.container,
			change.aliases,
		)
		cancel()
		if err != nil {
			return fmt.Errorf(
				"attach %s to network %s: %w",
				change.component,
				change.network,
				err,
			)
		}
		controller.report("attached %s to an interface network", change.component)
	}
	for _, change := range reconnects {
		callCtx, cancel := controller.callContext(ctx)
		err := controller.Engine.DisconnectNetwork(
			callCtx,
			change.network,
			change.container,
		)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf(
				"detach %s to restore its network alias: %w",
				change.component,
				err,
			)
		}
		callCtx, cancel = controller.callContext(ctx)
		err = controller.Engine.ConnectNetwork(
			callCtx,
			change.network,
			change.container,
			change.aliases,
		)
		cancel()
		if err != nil {
			return fmt.Errorf(
				"reattach %s to restore its network alias: %w",
				change.component,
				err,
			)
		}
	}
	for _, change := range disconnects {
		callCtx, cancel := controller.callContext(ctx)
		err := controller.Engine.DisconnectNetwork(
			callCtx,
			change.network,
			change.container,
		)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf(
				"detach %s from network %s: %w",
				change.component,
				change.network,
				err,
			)
		}
	}
	if err := controller.removeObsoleteNetworks(ctx, operation); err != nil {
		return err
	}

	for _, component := range operation.Target.Components {
		resource := operation.Containers[component.Name]
		actual, err := controller.inspectContainer(ctx, resource.ID)
		if err != nil {
			return err
		}
		if err := verifyContainerNetworks(component, plans, operation.Networks, actual); err != nil {
			return err
		}
	}
	for _, key := range sortedNetworkKeys(plans) {
		actual, err := controller.inspectNetwork(ctx, operation.Networks[key].ID)
		if err != nil {
			return err
		}
		if err := verifyNoUnknownNetworkMembers(
			plans[key],
			actual,
			operation.Containers,
		); err != nil {
			return err
		}
	}
	return nil
}

func (controller *Controller) removeObsoleteNetworks(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Previous == nil {
		return nil
	}
	previousPlans, err := resolvedTopology(operation.Previous.Spec)
	if err != nil {
		return err
	}
	targetIDs := make(map[string]struct{}, len(operation.Networks))
	for _, resource := range operation.Networks {
		targetIDs[resource.ID] = struct{}{}
	}
	for _, key := range sortedNetworkKeys(previousPlans) {
		resource, exists := operation.Previous.Networks[key]
		if !exists {
			return fmt.Errorf("previous deployment has no network %q", key)
		}
		if _, retained := targetIDs[resource.ID]; retained {
			continue
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyNetwork(
			operation.Target.Name,
			previousPlans[key],
			resource,
			actual,
		); err != nil {
			return err
		}
		if len(actual.Containers) != 0 {
			return fmt.Errorf("obsolete network %s still has attached containers", key)
		}
		callCtx, cancel := controller.callContext(ctx)
		err = controller.Engine.RemoveNetwork(callCtx, resource.ID)
		cancel()
		if err != nil && !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf("remove obsolete network %s: %w", key, err)
		}
		controller.report("removed network %s", key)
	}
	return nil
}

func (controller *Controller) startNewContainers(
	ctx context.Context,
	operation *state.Operation,
) error {
	plans, err := resolvedTopology(operation.Target)
	if err != nil {
		return err
	}
	for _, component := range operation.Target.Components {
		resource := operation.Containers[component.Name]
		if operation.Completed[component.Name] {
			continue
		}
		if operation.Previous != nil {
			if previous, exists := operation.Previous.Containers[component.Name]; exists &&
				previous.ID == resource.ID {
				if err := controller.markComplete(operation, component.Name); err != nil {
					return err
				}
				continue
			}
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
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
		if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
			return err
		}
		if err := verifyContainerNetworks(component, plans, operation.Networks, actual); err != nil {
			return err
		}
		if !actual.Running {
			callCtx, cancel := controller.callContext(ctx)
			err := controller.Engine.StartContainer(callCtx, resource.ID)
			cancel()
			if err != nil {
				return fmt.Errorf("start %s: %w", component.Name, err)
			}
			controller.report("started component %s", component.Name)
		}
		if err := controller.markComplete(operation, component.Name); err != nil {
			return err
		}
	}
	return nil
}

func sortedNetworkKeys(plans map[string]networkPlan) []string {
	keys := make([]string, 0, len(plans))
	for key := range plans {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containerNetworkHasAlias(
	container engine.Container,
	network state.Resource,
	alias string,
) bool {
	_, attachment, exists := findContainerNetwork(container, network)
	return exists && containsString(attachment.Aliases, alias)
}

func sortNetworkChanges(changes []networkChange) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].component != changes[j].component {
			return changes[i].component < changes[j].component
		}
		return changes[i].network < changes[j].network
	})
}
