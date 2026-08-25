package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

func (controller *Controller) supersedeOperation(
	ctx context.Context,
	operation *state.Operation,
	targetDigest string,
) error {
	if operation.Kind == kindApply && applyPhaseUsesProxy(operation.Phase) {
		if _, err := controller.recoverMissingTargetProxy(ctx, operation); err != nil {
			return fmt.Errorf(
				"recover interrupted apply before superseding it: %w",
				err,
			)
		}
	}
	if err := controller.resolvePendingCreates(ctx, operation); err != nil {
		return fmt.Errorf(
			"resolve interrupted %s before superseding it: %w",
			operation.Kind,
			err,
		)
	}
	oldKind := operation.Kind
	oldDigest := operation.Target.Digest
	if operation.Phase != phaseAbort {
		if err := controller.setPhase(operation, phaseAbort); err != nil {
			return err
		}
	}
	if err := controller.executeAbort(ctx, operation); err != nil {
		return fmt.Errorf("supersede interrupted %s: %w", oldKind, err)
	}
	controller.report(
		"superseded interrupted %s for %s (%s -> %s)",
		oldKind,
		operation.Target.Name,
		oldDigest,
		targetDigest,
	)
	return nil
}

func (controller *Controller) executeAbort(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Kind != kindApply {
		if operation.Kind == kindDown {
			if err := controller.recoverRetiredPreviousEndpoints(ctx, operation); err != nil {
				return fmt.Errorf("recover network endpoints before abort: %w", err)
			}
		}
		if err := controller.State.ClearOperation(operation.Target.Name); err != nil {
			return err
		}
		controller.report("aborted %s for %s", operation.Kind, operation.Target.Name)
		return nil
	}
	scope := controller.dockerScope(operation.Target.Name)
	plans, err := resolvedTopology(scope, operation.Target)
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
			containerName(scope, component.Name),
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
	for _, component := range operation.Target.Components {
		resource, exists := containerCandidates[component.Name]
		if !exists {
			continue
		}
		if _, retained := previousContainerIDs[resource.ID]; retained {
			continue
		}
		if err := controller.recoverAbsentComponentEndpoint(
			ctx,
			operation,
			operation.Target,
			networkCandidates,
			component,
			resource,
		); err != nil {
			return fmt.Errorf("recover %s endpoint before abort: %w", component.Name, err)
		}
	}
	for _, resource := range containerCandidates {
		if _, retained := previousContainerIDs[resource.ID]; retained {
			continue
		}
		if err := controller.recoverContainerEndpointCleanups(
			ctx,
			operation,
			resource.ID,
		); err != nil {
			return fmt.Errorf("recover endpoint cleanup before abort: %w", err)
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
			scope,
			runtimeDirectory(operation.RuntimeRoot, operation.Target.Name),
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
			scope,
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
		if err := controller.prepareEndpointCleanup(
			ctx,
			operation,
			operation.Target,
			networkCandidates,
			component,
			resource,
		); err != nil {
			return fmt.Errorf(
				"record %s endpoint cleanup during abort: %w",
				component.Name,
				err,
			)
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
		if err := controller.recoverContainerEndpointCleanups(
			ctx,
			operation,
			resource.ID,
		); err != nil {
			return fmt.Errorf(
				"verify %s endpoint cleanup during abort: %w",
				component.Name,
				err,
			)
		}
	}
	// Target and previous generations can reuse a deterministic container
	// name. Remove target-only containers first, then recover any previous
	// orphan addressed by that name.
	if err := controller.recoverRetiredPreviousEndpoints(ctx, operation); err != nil {
		return fmt.Errorf("recover previous network endpoints during abort: %w", err)
	}
	if err := controller.restorePreviousProxyDuringAbort(ctx, operation); err != nil {
		return err
	}

	// A retained component may have been attached to a replacement component
	// network before interruption. Detach only exact operation-owned networks.
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
		if len(actual.Endpoints) != 0 {
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
	if operation.AbortRecreatePrevious && operation.Previous != nil {
		if err := controller.handoffAbortToPreviousApply(ctx, operation); err != nil {
			return err
		}
		controller.report("aborted apply for %s", operation.Target.Name)
		return nil
	}
	if err := controller.State.ClearOperation(operation.Target.Name); err != nil {
		return err
	}
	controller.report("aborted apply for %s", operation.Target.Name)
	return nil
}

// handoffAbortToPreviousApply replaces the durable abort journal with a fresh
// apply journal before recreation begins. There is deliberately no clear-state
// gap: after the write, a crash resumes the replacement apply rather than
// leaving desired state pointing at the fleet the abort fallback removed.
func (controller *Controller) handoffAbortToPreviousApply(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Previous == nil {
		return fmt.Errorf("cannot recreate a missing previous deployment")
	}
	previous := *operation.Previous
	restorer := *controller
	restorer.RuntimeRoot = previous.RuntimeRoot
	replacement, err := state.NewOperation(
		kindApply,
		phaseRetire,
		previous.Spec,
		&previous,
		previous.RuntimeRoot,
	)
	if err != nil {
		return fmt.Errorf("journal previous fleet recreation: %w", err)
	}
	if err := restorer.selectRetainedResources(ctx, &replacement); err != nil {
		return fmt.Errorf("select resources for previous fleet recreation: %w", err)
	}
	if err := restorer.State.WriteOperation(previous.Spec.Name, replacement); err != nil {
		return fmt.Errorf("journal previous fleet recreation: %w", err)
	}
	if err := restorer.executeApply(ctx, &replacement); err != nil {
		return fmt.Errorf("recreate previous fleet during abort: %w", err)
	}
	return nil
}

func (controller *Controller) restorePreviousProxyDuringAbort(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Previous == nil {
		if operation.Proxy != nil {
			if err := controller.stopRecordedProxy(ctx, operation.Proxy); err != nil {
				return fmt.Errorf("stop proxy during abort: %w", err)
			}
			operation.Proxy = nil
			return controller.State.WriteOperation(operation.Target.Name, *operation)
		}
		return nil
	}
	if !operation.AbortRecreatePrevious {
		previousWiring, err := proxy.NewWiring(operation.Previous.Spec)
		if err != nil {
			return fmt.Errorf("derive previous wiring during abort: %w", err)
		}
		previousDigest, err := previousWiring.Digest()
		if err != nil {
			return fmt.Errorf("digest previous wiring during abort: %w", err)
		}
		if proxyProcessMatches(operation.Proxy, operation.Previous.Proxy) {
			status, inspectErr := controller.inspectProxy(ctx, *operation.Proxy)
			if inspectErr != nil && !errors.Is(inspectErr, proxy.ErrNotRunning) {
				return fmt.Errorf("inspect proxy during abort: %w", inspectErr)
			}
			if inspectErr == nil && status.Ready && status.Digest == previousDigest {
				return controller.recordPreviousProxyDigest(operation, previousDigest)
			}
			if inspectErr == nil {
				for attempt := 0; attempt < 2; attempt++ {
					callCtx, cancel := controller.callContext(ctx)
					response, resyncErr := controller.Proxy.Resync(
						callCtx, *operation.Proxy, previousWiring, previousDigest,
					)
					cancel()
					if resyncErr == nil && response.Ready && response.Digest == previousDigest {
						controller.report("restored previous proxy wiring for %s", operation.Target.Name)
						return controller.recordPreviousProxyDigest(operation, previousDigest)
					}
					// Caller cancellation says nothing about whether reverse resync can
					// converge on a later resume. Keep the fallback choice uncommitted.
					if err := ctx.Err(); err != nil {
						return err
					}
					if errors.Is(resyncErr, proxy.ErrIdentityMismatch) {
						return fmt.Errorf("reverse resync proxy during abort: %w", resyncErr)
					}
					if errors.Is(resyncErr, proxy.ErrControlProtocolMismatch) {
						return fmt.Errorf("reverse resync proxy during abort: %w", resyncErr)
					}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// Reverse resync was unavailable or could not converge. Persist the chosen
		// fallback before removing retained containers so resume cannot mistake the
		// operation for a still-reversible abort.
		operation.AbortRecreatePrevious = true
		if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
			return err
		}
	}
	if operation.Proxy != nil {
		if err := controller.stopRecordedProxy(ctx, operation.Proxy); err != nil {
			return fmt.Errorf("stop proxy during abort fallback: %w", err)
		}
		operation.Proxy = nil
		if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
			return err
		}
	}
	if err := controller.removeContainersWithStaleProxyMounts(ctx, operation); err != nil {
		return fmt.Errorf("remove component fleet during abort fallback: %w", err)
	}
	return nil
}

func (controller *Controller) recordPreviousProxyDigest(
	operation *state.Operation,
	digest string,
) error {
	if operation.Proxy == nil || operation.Previous == nil {
		return fmt.Errorf("cannot record previous wiring without both proxy records")
	}
	operation.Proxy.Digest = digest
	operation.Previous.Proxy = cloneProxy(operation.Proxy)
	return controller.State.WriteOperation(operation.Target.Name, *operation)
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
	scope := controller.dockerScope(operation.Target.Name)
	if err := controller.recoverRetiredPreviousEndpoints(ctx, operation); err != nil {
		return fmt.Errorf("recover network endpoints before down: %w", err)
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
			if err := controller.recoverAbsentComponentEndpoint(
				ctx,
				operation,
				operation.Previous.Spec,
				operation.Previous.Networks,
				component,
				resource,
			); err != nil {
				return err
			}
			if err := controller.recoverEndpointCleanups(ctx, operation); err != nil {
				return err
			}
			if err := controller.markComplete(operation, key); err != nil {
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
			resource,
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
			resource,
		); err != nil {
			return fmt.Errorf("record %s endpoint cleanup: %w", component.Name, err)
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
		if err := controller.recoverEndpointCleanups(ctx, operation); err != nil {
			return fmt.Errorf("verify %s endpoint cleanup: %w", component.Name, err)
		}
		if err := controller.markComplete(operation, key); err != nil {
			return err
		}
	}
	if !operation.Completed["proxy"] {
		if err := controller.stopRecordedProxy(ctx, operation.Proxy); err != nil {
			return fmt.Errorf("stop proxy: %w", err)
		}
		if err := controller.markComplete(operation, "proxy"); err != nil {
			return err
		}
		controller.report("stopped proxy for %s", operation.Target.Name)
	}
	plans, err := resolvedTopology(scope, operation.Previous.Spec)
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
			scope,
			plans[networkKey],
			resource,
			actual,
		); err != nil {
			return err
		}
		if len(actual.Endpoints) != 0 {
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
	if deployment.Proxy == nil {
		return fmt.Errorf("deployment has no proxy record")
	}
	if _, err := controller.inspectProxy(ctx, *deployment.Proxy); err != nil &&
		!errors.Is(err, proxy.ErrNotRunning) {
		return fmt.Errorf("inspect proxy before down: %w", err)
	}
	scope := controller.dockerScope(deployment.Spec.Name)
	plans, err := resolvedTopology(scope, deployment.Spec)
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
			scope,
			deployment.Proxy.RuntimeDir,
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
			scope,
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
	if deployment.Proxy == nil {
		return fmt.Errorf("deployment has no proxy record")
	}
	status, err := controller.inspectProxy(ctx, *deployment.Proxy)
	if err != nil {
		return fmt.Errorf("inspect proxy before restart: %w", err)
	}
	if !status.Ready || status.Digest != deployment.Proxy.Digest {
		return fmt.Errorf(
			"proxy is not converged before restart: ready=%t, wiring=%q, expected %q",
			status.Ready,
			status.Digest,
			deployment.Proxy.Digest,
		)
	}
	scope := controller.dockerScope(deployment.Spec.Name)
	plans, err := resolvedTopology(scope, deployment.Spec)
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
			scope,
			deployment.Proxy.RuntimeDir,
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
