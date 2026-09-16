package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

func (controller *Controller) deploymentMatches(
	ctx context.Context,
	deployment state.Deployment,
	target composition.ResolvedSpec,
) (bool, error) {
	scope := controller.dockerScope(target.Name)
	if deployment.Spec.Digest != target.Digest {
		return false, nil
	}
	if deployment.RuntimeRoot != controller.RuntimeRoot || deployment.Proxy == nil {
		return false, nil
	}
	targetProxy, err := proxyConfig(target, controller.RuntimeRoot, deployment.Proxy.InstanceID)
	if err != nil {
		return false, err
	}
	if deployment.Proxy.Digest != targetProxy.Digest ||
		deployment.Proxy.RuntimeDir != targetProxy.RuntimeDir {
		return false, nil
	}
	status, inspectErr := controller.inspectProxy(ctx, *deployment.Proxy)
	if errors.Is(inspectErr, proxy.ErrNotRunning) {
		return false, nil
	} else if inspectErr != nil {
		return false, inspectErr
	}
	if !status.Ready || status.Digest != targetProxy.Digest {
		return false, nil
	}
	plans, err := resolvedTopology(scope, target)
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
		if err := verifyNetwork(scope, plans[key], resource, actual); err != nil {
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
		if err := verifyCurrentContainer(
			scope, deployment.Proxy.RuntimeDir, component, resource, actual,
		); err != nil {
			if errors.Is(err, errStandardIOPolicy) {
				return false, nil
			}
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
				volumeName(scope, component.Name, volume.Name),
			)
			if errors.Is(inspectErr, engine.ErrNotFound) {
				return false, nil
			}
			if inspectErr != nil {
				return false, inspectErr
			}
			if err := verifyVolume(
				scope,
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
	scope := controller.dockerScope(operation.Target.Name)
	targetPlans, err := resolvedTopology(scope, operation.Target)
	if err != nil {
		return err
	}
	previousPlans, err := resolvedTopology(scope, operation.Previous.Spec)
	if err != nil {
		return err
	}
	retainComponents := false
	if operation.Previous.Proxy != nil &&
		operation.Previous.RuntimeRoot == operation.RuntimeRoot {
		// Preserve the durable process identity even when its control socket is
		// unavailable. Full replacement may still need the recorded PID to prove
		// that pathname cleanup has completed before a new proxy is launched.
		operation.Proxy = cloneProxy(operation.Previous.Proxy)
		if _, inspectErr := controller.inspectProxy(ctx, *operation.Previous.Proxy); inspectErr == nil {
			// Wiring is mutable observed state. A live, identity-verified proxy
			// remains the mount owner even when its digest differs from the
			// target; resync reconciles it after retirement.
			retainComponents = true
		} else if !errors.Is(inspectErr, proxy.ErrNotRunning) {
			return inspectErr
		}
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
			scope,
			targetPlan,
			resource,
			actual,
		); err != nil {
			return err
		}
		operation.Networks[key] = resource
	}
	if !retainComponents {
		return nil
	}
	for _, component := range operation.Target.Components {
		previousComponent, exists := operation.Previous.Spec.Component(component.Name)
		if !exists || previousComponent.Digest != component.Digest {
			continue
		}
		if component.Runtime.ExternalEgress {
			key := componentNetworkKey(component.Name)
			retainedNetwork, retained := operation.Networks[key]
			previousNetwork, recorded := operation.Previous.Networks[key]
			if !retained || !recorded || retainedNetwork.ID != previousNetwork.ID {
				continue
			}
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
		if err := verifyCurrentContainer(
			scope,
			operation.Previous.Proxy.RuntimeDir,
			component,
			resource,
			actual,
		); err != nil {
			if errors.Is(err, errStandardIOPolicy) {
				continue
			}
			return err
		}
		if err := verifyContainerEnvironment(
			operation.Target,
			component,
			actual,
		); err != nil {
			return err
		}
		operation.Containers[component.Name] = resource
	}
	return nil
}

func (controller *Controller) executeApply(
	ctx context.Context,
	operation *state.Operation,
) error {
	// This runs before the active phase's strict member preflight so a recorded
	// container that disappeared out of band can have its orphaned ep-* member
	// recovered before unknown-member validation.
	if err := controller.recoverRetiredPreviousEndpoints(ctx, operation); err != nil {
		return fmt.Errorf("recover retired network endpoints: %w", err)
	}
	for {
		if applyPhaseUsesProxy(operation.Phase) {
			recovered, err := controller.recoverMissingTargetProxy(ctx, operation)
			if err != nil {
				return err
			}
			if recovered {
				continue
			}
		}
		recovered, err := controller.recoverMissingApplyPrerequisite(ctx, operation)
		if err != nil {
			return err
		}
		if recovered {
			continue
		}
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
			if err := controller.setPhase(operation, phaseResync); err != nil {
				return err
			}
		case phaseResync:
			if err := controller.ensureTargetProxy(ctx, operation); err != nil {
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
			if err := controller.ensureTargetContainersRunning(ctx, operation); err != nil {
				return err
			}
			if err := controller.setPhase(operation, phaseCommit); err != nil {
				return err
			}
		case phaseCommit:
			if len(operation.EndpointCleanups) != 0 {
				return fmt.Errorf(
					"cannot commit %s with pending endpoint cleanup",
					operation.Target.Name,
				)
			}
			if operation.Proxy == nil {
				return fmt.Errorf("cannot commit %s without a proxy", operation.Target.Name)
			}
			deployment := state.Deployment{
				Spec:        operation.Target,
				RuntimeRoot: operation.RuntimeRoot,
				Proxy:       cloneProxy(operation.Proxy),
				Networks:    cloneResources(operation.Networks),
				Containers:  cloneResources(operation.Containers),
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

// recoverMissingApplyPrerequisite makes phase advancement conditional on the
// durable resources produced by earlier phases still existing. A rewind is
// persisted only after any container tied to a missing network generation has
// been safely removed, preserving the publication-before-mount ordering.
func (controller *Controller) recoverMissingApplyPrerequisite(
	ctx context.Context,
	operation *state.Operation,
) (bool, error) {
	if applyPhaseRequiresTargetNetworks(operation.Phase) {
		recovered, err := controller.recoverMissingTargetNetwork(ctx, operation)
		if err != nil || recovered {
			return recovered, err
		}
	}
	if applyPhaseRequiresTargetContainers(operation.Phase) {
		return controller.recoverMissingTargetContainer(ctx, operation)
	}
	return false, nil
}

func applyPhaseRequiresTargetNetworks(phase string) bool {
	switch phase {
	case phaseResync, phaseCreate, phaseAttach, phaseStart, phaseCommit:
		return true
	default:
		return false
	}
}

func applyPhaseRequiresTargetContainers(phase string) bool {
	switch phase {
	case phaseAttach, phaseStart, phaseCommit:
		return true
	default:
		return false
	}
}

func (controller *Controller) recoverMissingTargetNetwork(
	ctx context.Context,
	operation *state.Operation,
) (bool, error) {
	scope := controller.dockerScope(operation.Target.Name)
	plans, err := resolvedTopology(scope, operation.Target)
	if err != nil {
		return false, err
	}
	for _, key := range sortedNetworkKeys(plans) {
		resource, exists := operation.Networks[key]
		if !exists {
			if len(plans[key].Members) != 0 {
				for component := range plans[key].Members {
					if _, recorded := operation.Containers[component]; recorded {
						return false, fmt.Errorf(
							"apply phase %s has %s container but no recorded network %s",
							operation.Phase,
							component,
							key,
						)
					}
				}
			}
			if err := controller.setPhase(operation, phaseNetworks); err != nil {
				return false, err
			}
			return true, nil
		}
		actual, inspectErr := controller.inspectNetwork(ctx, resource.ID)
		if inspectErr == nil {
			if err := verifyNetwork(scope, plans[key], resource, actual); err != nil {
				return false, err
			}
			continue
		}
		if !errors.Is(inspectErr, engine.ErrNotFound) {
			return false, inspectErr
		}
		occupant, nameErr := controller.inspectNetwork(ctx, plans[key].Name)
		if nameErr == nil {
			return false, fmt.Errorf(
				"network name %q no longer identifies recorded network %s; found %s",
				plans[key].Name,
				resource.ID,
				occupant.ID,
			)
		}
		if !errors.Is(nameErr, engine.ErrNotFound) {
			return false, nameErr
		}

		if err := controller.removeContainersPinnedToNetworkGeneration(
			ctx,
			operation,
			plans[key],
		); err != nil {
			return false, err
		}
		delete(operation.Networks, key)
		delete(operation.PendingCreates, networkCreateKey(key))
		if err := controller.setPhase(operation, phaseNetworks); err != nil {
			return false, err
		}
		controller.report("recreating missing network %s", key)
		return true, nil
	}
	return false, nil
}

func (controller *Controller) removeContainersPinnedToNetworkGeneration(
	ctx context.Context,
	operation *state.Operation,
	plan networkPlan,
) error {
	members := make([]string, 0, len(plan.Members))
	for component := range plan.Members {
		members = append(members, component)
	}
	sort.Strings(members)
	for _, name := range members {
		component, declared := operation.Target.Component(name)
		if !declared {
			return fmt.Errorf("network %s names unknown component %s", plan.Key, name)
		}
		if _, recorded := operation.Containers[name]; !recorded {
			continue
		}
		if err := controller.removeTargetContainerForRecreate(
			ctx,
			operation,
			component,
		); err != nil {
			return fmt.Errorf(
				"remove %s after network %s disappeared: %w",
				name,
				plan.Key,
				err,
			)
		}
	}
	return nil
}

func (controller *Controller) recoverMissingTargetContainer(
	ctx context.Context,
	operation *state.Operation,
) (bool, error) {
	for _, component := range operation.Target.Components {
		resource, exists := operation.Containers[component.Name]
		if !exists {
			if err := controller.setPhase(operation, phaseCreate); err != nil {
				return false, err
			}
			return true, nil
		}
		_, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if inspectErr == nil {
			continue
		}
		if !errors.Is(inspectErr, engine.ErrNotFound) {
			return false, inspectErr
		}
		if err := controller.removeTargetContainerForRecreate(
			ctx,
			operation,
			component,
		); err != nil {
			return false, fmt.Errorf(
				"recover missing %s container: %w",
				component.Name,
				err,
			)
		}
		if err := controller.setPhase(operation, phaseCreate); err != nil {
			return false, err
		}
		controller.report("recreating missing component %s", component.Name)
		return true, nil
	}
	return false, nil
}

func (controller *Controller) removeTargetContainerForRecreate(
	ctx context.Context,
	operation *state.Operation,
	component composition.ResolvedComponent,
) error {
	resource, exists := operation.Containers[component.Name]
	if !exists {
		return nil
	}
	actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
	if errors.Is(inspectErr, engine.ErrNotFound) {
		cleanupSpec := operation.Target
		cleanupNetworks := operation.Networks
		cleanupComponent := component
		if operation.Previous != nil {
			previousResource, previousRecorded := operation.Previous.Containers[component.Name]
			previousComponent, previousDeclared := operation.Previous.Spec.Component(component.Name)
			if previousRecorded && previousResource.ID == resource.ID {
				if !previousDeclared {
					return fmt.Errorf("recorded previous container %s is not declared", component.Name)
				}
				cleanupSpec = operation.Previous.Spec
				cleanupNetworks = operation.Previous.Networks
				cleanupComponent = previousComponent
			}
		}
		if err := controller.recoverAbsentComponentEndpointCleanup(
			ctx,
			operation,
			cleanupSpec,
			cleanupNetworks,
			cleanupComponent,
			resource,
		); err != nil {
			return err
		}
		delete(operation.Containers, component.Name)
		delete(operation.Completed, component.Name)
		delete(operation.PendingCreates, containerCreateKey(component.Name))
		return nil
	}
	if inspectErr != nil {
		return inspectErr
	}
	runtimeDir := runtimeDirectory(operation.RuntimeRoot, operation.Target.Name)
	if operation.Proxy != nil {
		runtimeDir = operation.Proxy.RuntimeDir
	}
	scope := controller.dockerScope(operation.Target.Name)
	if err := verifyCurrentContainer(
		scope,
		runtimeDir,
		component,
		resource,
		actual,
	); err != nil {
		return err
	}
	if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
		return err
	}
	if err := verifyNoUnknownContainerNetworksWithFallback(
		component,
		operation.Networks,
		previousNetworks(operation),
		actual,
	); err != nil {
		return err
	}
	cleanupNetworks := targetContainerEndpointNetworks(operation, component, actual)
	if err := controller.prepareEndpointCleanup(
		ctx,
		operation,
		operation.Target,
		cleanupNetworks,
		component,
		resource,
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
			return err
		}
	}
	callCtx, cancel := controller.callContext(ctx)
	removeErr := controller.Engine.RemoveContainer(callCtx, resource.ID)
	cancel()
	if removeErr != nil && !errors.Is(removeErr, engine.ErrNotFound) {
		return removeErr
	}
	if err := controller.recoverContainerEndpointCleanups(
		ctx,
		operation,
		resource.ID,
	); err != nil {
		return err
	}
	delete(operation.Containers, component.Name)
	delete(operation.Completed, component.Name)
	delete(operation.PendingCreates, containerCreateKey(component.Name))
	return nil
}

func targetContainerEndpointNetworks(
	operation *state.Operation,
	component composition.ResolvedComponent,
	actual engine.Container,
) map[string]state.Resource {
	if !component.Runtime.ExternalEgress || operation.Previous == nil {
		return operation.Networks
	}
	key := componentNetworkKey(component.Name)
	target, hasTarget := operation.Networks[key]
	previous, hasPrevious := operation.Previous.Networks[key]
	if !hasTarget || !hasPrevious || target.ID == previous.ID {
		return operation.Networks
	}
	_, attachment, attached := findContainerNetwork(actual, previous)
	if attached && attachment.NetworkID == previous.ID {
		return operation.Previous.Networks
	}
	return operation.Networks
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
	scope := controller.dockerScope(operation.Target.Name)
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
			if err := controller.recoverAbsentComponentEndpoint(
				ctx,
				operation,
				operation.Previous.Spec,
				operation.Previous.Networks,
				component,
				previousResource,
			); err != nil {
				return err
			}
			if err := controller.recoverEndpointCleanups(ctx, operation); err != nil {
				return err
			}
			if err := controller.markComplete(operation, progressKey); err != nil {
				return err
			}
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyContainerCore(
			scope,
			operation.Previous.Proxy.RuntimeDir,
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
		if err := controller.prepareEndpointCleanup(
			ctx,
			operation,
			operation.Previous.Spec,
			operation.Previous.Networks,
			component,
			previousResource,
		); err != nil {
			return fmt.Errorf("record %s endpoint cleanup: %w", component.Name, err)
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
		if err := controller.recoverEndpointCleanups(ctx, operation); err != nil {
			return fmt.Errorf("verify %s endpoint cleanup: %w", component.Name, err)
		}
		if err := controller.markComplete(operation, progressKey); err != nil {
			return err
		}
		controller.report("retired component %s", component.Name)
	}
	if !proxyProcessMatches(operation.Proxy, operation.Previous.Proxy) &&
		!operation.Completed["retire/proxy"] {
		if err := controller.stopRecordedProxy(ctx, operation.Previous.Proxy); err != nil {
			return fmt.Errorf("stop replaced proxy: %w", err)
		}
		if err := controller.markComplete(operation, "retire/proxy"); err != nil {
			return err
		}
		controller.report("stopped replaced proxy for %s", operation.Target.Name)
	}
	return nil
}

func (controller *Controller) preflightApply(
	ctx context.Context,
	operation *state.Operation,
) error {
	scope := controller.dockerScope(operation.Target.Name)
	targetPlans, err := resolvedTopology(scope, operation.Target)
	if err != nil {
		return err
	}
	var previousPlans map[string]networkPlan
	if operation.Previous != nil {
		previousPlans, err = resolvedTopology(scope, operation.Previous.Spec)
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
				scope,
				operation.Previous.Proxy.RuntimeDir,
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
				scope,
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
			containerName(scope, component.Name),
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
				volumeName(scope, component.Name, volume.Name),
			)
			if errors.Is(inspectErr, engine.ErrNotFound) {
				continue
			}
			if inspectErr != nil {
				return inspectErr
			}
			if err := verifyVolume(
				scope,
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
	scope := controller.dockerScope(operation.Target.Name)
	plans, err := resolvedTopology(scope, operation.Target)
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
	scope := controller.dockerScope(operation.Target.Name)
	target, targetExists := operation.Networks[plan.Key]
	previousPlans, err := resolvedTopology(scope, operation.Previous.Spec)
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
			scope,
			previousPlan,
			resource,
			actual,
		); err != nil {
			return err
		}
		if len(actual.Endpoints) != 0 {
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
	scope := controller.dockerScope(operation.Target.Name)
	pendingKey := networkCreateKey(plan.Key)
	if resource, exists := operation.Networks[plan.Key]; exists {
		actual, err := controller.inspectNetwork(ctx, resource.ID)
		if err == nil {
			if err := verifyNetwork(scope, plan, resource, actual); err != nil {
				return err
			}
			return controller.clearPendingCreate(operation, pendingKey)
		}
		if !errors.Is(err, engine.ErrNotFound) {
			return err
		}
		occupant, nameErr := controller.inspectNetwork(ctx, plan.Name)
		if nameErr == nil {
			return fmt.Errorf(
				"network name %q no longer identifies recorded network %s; found %s",
				plan.Name,
				resource.ID,
				occupant.ID,
			)
		}
		if !errors.Is(nameErr, engine.ErrNotFound) {
			return nameErr
		}
		if err := controller.removeContainersPinnedToNetworkGeneration(
			ctx,
			operation,
			plan,
		); err != nil {
			return err
		}
		delete(operation.Networks, plan.Key)
		delete(operation.PendingCreates, pendingKey)
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
			scope,
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
				scope,
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
		scope,
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
	scope := controller.dockerScope(operation.Target.Name)
	plans, err := resolvedTopology(scope, operation.Target)
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
	if operation.Proxy == nil {
		return fmt.Errorf("cannot create %s before the proxy is ready", component.Name)
	}
	scope := controller.dockerScope(operation.Target.Name)
	runtimeDir := operation.Proxy.RuntimeDir
	pendingKey := containerCreateKey(component.Name)
	if resource, exists := operation.Containers[component.Name]; exists {
		actual, err := controller.inspectContainer(ctx, resource.ID)
		if err == nil {
			if err := verifyCurrentContainer(
				scope,
				runtimeDir,
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
		if err := controller.removeTargetContainerForRecreate(
			ctx,
			operation,
			component,
		); err != nil {
			return fmt.Errorf("recover missing %s container: %w", component.Name, err)
		}
		if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
			return err
		}
	}

	name := containerName(scope, component.Name)
	if actual, err := controller.inspectContainer(ctx, name); err == nil {
		if actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf("container name %q is occupied", name)
		}
		resource := state.Resource{ID: actual.ID, Name: actual.Name}
		if err := verifyCurrentContainer(
			scope,
			runtimeDir,
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

	var networkID string
	var networkAliases []string
	if component.Runtime.ExternalEgress {
		base, exists := operation.Networks[componentNetworkKey(component.Name)]
		if !exists {
			return fmt.Errorf("%s has no egress network", component.Name)
		}
		networkID = base.ID
		networkAliases = []string{component.Name}
	}
	environment, err := componentEnvironment(operation.Target, component)
	if err != nil {
		return err
	}
	if err := composition.ValidateBindSources(component.Runtime); err != nil {
		return fmt.Errorf("create component %s: %w", component.Name, err)
	}
	if err := controller.markPendingCreate(operation, pendingKey); err != nil {
		return err
	}
	callCtx, cancel := controller.callContext(ctx)
	actual, createErr := controller.Engine.CreateContainer(
		callCtx,
		engine.ContainerRequest{
			User:           component.Runtime.User,
			Name:           name,
			ImageID:        component.ImageID,
			NetworkID:      networkID,
			NetworkAliases: networkAliases,
			Labels: expectedContainerLabels(
				scope,
				component,
				operation.ID,
			),
			Environment: environment,
			Mounts: componentMounts(
				scope, runtimeDir, component,
			),
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
	if err := verifyCurrentContainer(
		scope,
		runtimeDir,
		component,
		resource,
		actual,
	); err != nil {
		return err
	}
	if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
		return err
	}
	if component.Runtime.ExternalEgress {
		base := operation.Networks[componentNetworkKey(component.Name)]
		if _, _, attached := findContainerNetwork(actual, base); !attached {
			return fmt.Errorf("%s was not created on its egress network", component.Name)
		}
	} else if len(actual.Networks) != 0 {
		return fmt.Errorf(
			"%s was created with %d networks, expected network mode none",
			component.Name,
			len(actual.Networks),
		)
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
	scope := controller.dockerScope(operation.Target.Name)
	plans, err := resolvedTopology(scope, operation.Target)
	if err != nil {
		return err
	}
	if operation.PendingCreates[proxyCreateKey] {
		if err := controller.ensureTargetProxy(ctx, operation); err != nil {
			return err
		}
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
		if key == proxyCreateKey {
			continue
		}
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
	scope := controller.dockerScope(operation.Target.Name)
	plans, err := resolvedTopology(scope, operation.Target)
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
		if err := verifyCurrentContainer(
			scope,
			operation.Proxy.RuntimeDir,
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
			if _, wanted := matchContainerNetwork(name, attachment, expected); wanted {
				continue
			}
			observed, owned := matchContainerNetwork(name, attachment, known)
			if !owned {
				return fmt.Errorf(
					"%s is attached to undeclared network %s",
					component.Name,
					observedNetworkDescription(name, attachment),
				)
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
		controller.report("attached %s to its egress network", change.component)
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
	scope := controller.dockerScope(operation.Target.Name)
	previousPlans, err := resolvedTopology(scope, operation.Previous.Spec)
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
			scope,
			previousPlans[key],
			resource,
			actual,
		); err != nil {
			return err
		}
		if len(actual.Endpoints) != 0 {
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

func (controller *Controller) ensureTargetContainersRunning(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Proxy == nil {
		return fmt.Errorf("cannot start components without a proxy")
	}
	if _, err := controller.inspectProxy(ctx, *operation.Proxy); err != nil {
		return fmt.Errorf("inspect proxy before starting components: %w", err)
	}
	scope := controller.dockerScope(operation.Target.Name)
	plans, err := resolvedTopology(scope, operation.Target)
	if err != nil {
		return err
	}
	for _, component := range operation.Target.Components {
		resource, exists := operation.Containers[component.Name]
		if !exists {
			return fmt.Errorf("target has no %s container", component.Name)
		}
		actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyCurrentContainer(
			scope,
			operation.Proxy.RuntimeDir,
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
		if !operation.Completed[component.Name] {
			if err := controller.markComplete(operation, component.Name); err != nil {
				return err
			}
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
