package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

const proxyCreateKey = "proxy"

func runtimeDirectory(root, system string) string {
	return filepath.Join(root, system)
}

func proxyConfig(
	spec composition.ResolvedSpec,
	runtimeRoot string,
	instanceID string,
) (proxy.Config, error) {
	return proxy.NewConfig(spec, runtimeDirectory(runtimeRoot, spec.Name), instanceID)
}

func (controller *Controller) inspectProxy(
	ctx context.Context,
	process proxy.Process,
) (proxy.Status, error) {
	callCtx, cancel := controller.callContext(ctx)
	defer cancel()
	return controller.Proxy.Inspect(callCtx, process)
}

func (controller *Controller) ensureTargetProxy(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Proxy == nil {
		return controller.startTargetProxy(ctx, operation)
	}

	status, inspectErr := controller.inspectProxy(ctx, *operation.Proxy)
	if errors.Is(inspectErr, proxy.ErrIdentityMismatch) {
		return inspectErr
	}
	if errors.Is(inspectErr, proxy.ErrNotRunning) {
		return controller.replaceTargetProxy(ctx, operation)
	}
	if inspectErr != nil {
		return fmt.Errorf("inspect proxy before resync: %w", inspectErr)
	}
	if status.Ready && status.Digest == operation.TargetWiringDigest {
		return controller.recordTargetProxyDigest(operation)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		callCtx, cancel := controller.callContext(ctx)
		response, resyncErr := controller.Proxy.Resync(
			callCtx,
			*operation.Proxy,
			operation.TargetWiring,
			operation.TargetWiringDigest,
		)
		cancel()
		if resyncErr == nil && response.Ready &&
			response.Digest == operation.TargetWiringDigest {
			if err := controller.recordTargetProxyDigest(operation); err != nil {
				return err
			}
			controller.report("resynced proxy for %s", operation.Target.Name)
			return nil
		}
		if resyncErr == nil {
			resyncErr = fmt.Errorf(
				"proxy reported ready=%t digest=%q after resync",
				response.Ready, response.Digest,
			)
		}
		lastErr = resyncErr
		if errors.Is(resyncErr, proxy.ErrIdentityMismatch) {
			return resyncErr
		}
		if errors.Is(resyncErr, proxy.ErrNotRunning) {
			return controller.replaceTargetProxy(ctx, operation)
		}
		if errors.Is(resyncErr, proxy.ErrControlProtocolMismatch) {
			return fmt.Errorf("proxy control protocol is incompatible: %w", resyncErr)
		}
	}

	// The process was identity-verified by Inspect. Stop that exact process,
	// remove the complete component fleet, then use the established
	// full-replacement path.
	if err := controller.replaceTargetProxy(ctx, operation); err != nil {
		return errors.Join(fmt.Errorf("proxy could not converge: %w", lastErr), err)
	}
	return nil
}

func (controller *Controller) recordTargetProxyDigest(operation *state.Operation) error {
	if operation.Proxy == nil {
		return fmt.Errorf("cannot record wiring for a missing proxy")
	}
	changed := operation.Proxy.Digest != operation.TargetWiringDigest ||
		operation.PendingCreates[proxyCreateKey]
	operation.Proxy.Digest = operation.TargetWiringDigest
	delete(operation.PendingCreates, proxyCreateKey)
	if !changed {
		return nil
	}
	return controller.State.WriteOperation(operation.Target.Name, *operation)
}

func (controller *Controller) startTargetProxy(
	ctx context.Context,
	operation *state.Operation,
) error {
	config, err := proxyConfig(operation.Target, operation.RuntimeRoot, operation.ID)
	if err != nil {
		return err
	}
	if err := controller.markPendingCreate(operation, proxyCreateKey); err != nil {
		return err
	}
	callCtx, cancel := controller.callContext(ctx)
	process, ensureErr := controller.Proxy.Ensure(callCtx, config)
	cancel()
	if ensureErr != nil {
		return fmt.Errorf("start proxy for %s: %w", operation.Target.Name, ensureErr)
	}
	operation.Proxy = &process
	delete(operation.PendingCreates, proxyCreateKey)
	if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
		return err
	}
	controller.report("started proxy for %s", operation.Target.Name)
	return nil
}

func (controller *Controller) replaceTargetProxy(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Proxy != nil {
		if err := controller.stopRecordedProxy(ctx, operation.Proxy); err != nil {
			return fmt.Errorf("finish recorded proxy cleanup: %w", err)
		}
	}
	if err := controller.removeContainersWithStaleProxyMounts(ctx, operation); err != nil {
		return err
	}
	operation.Proxy = nil
	delete(operation.PendingCreates, proxyCreateKey)
	if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
		return err
	}
	controller.report(
		"replacing proxy and component fleet for %s",
		operation.Target.Name,
	)
	return controller.startTargetProxy(ctx, operation)
}

func applyPhaseUsesProxy(phase string) bool {
	switch phase {
	case phaseCreate, phaseAttach, phaseStart, phaseCommit:
		return true
	default:
		return false
	}
}

// recoverMissingTargetProxy returns a post-proxy apply to proxy setup. Unix
// socket bind mounts retain the inode that existed when Docker created the
// container. The fallback deliberately recreates the complete component fleet,
// including components without endpoints, before a replacement proxy publishes
// new sockets.
func (controller *Controller) recoverMissingTargetProxy(
	ctx context.Context,
	operation *state.Operation,
) (bool, error) {
	if operation.Proxy != nil {
		status, err := controller.inspectProxy(ctx, *operation.Proxy)
		if err == nil {
			if status.Ready && status.Digest == operation.TargetWiringDigest {
				if err := controller.recordTargetProxyDigest(operation); err != nil {
					return false, err
				}
				return false, nil
			}
			if err := controller.setPhase(operation, phaseResync); err != nil {
				return false, err
			}
			return true, nil
		}
		if errors.Is(err, proxy.ErrIdentityMismatch) {
			return false, fmt.Errorf("inspect target proxy: %w", err)
		}
		if !errors.Is(err, proxy.ErrNotRunning) {
			return false, fmt.Errorf("inspect target proxy: %w", err)
		}
	}
	if err := controller.removeContainersWithStaleProxyMounts(ctx, operation); err != nil {
		return false, err
	}
	if operation.Proxy != nil {
		if err := controller.stopRecordedProxy(ctx, operation.Proxy); err != nil {
			return false, fmt.Errorf("finish missing proxy cleanup: %w", err)
		}
	}
	operation.Proxy = nil
	delete(operation.PendingCreates, proxyCreateKey)
	if err := controller.setPhase(operation, phaseResync); err != nil {
		return false, err
	}
	controller.report("replacing missing proxy and component fleet for %s", operation.Target.Name)
	return true, nil
}

func (controller *Controller) removeContainersWithStaleProxyMounts(
	ctx context.Context,
	operation *state.Operation,
) error {
	scope := controller.dockerScope(operation.Target.Name)
	// A deterministic name may still hold either deployment generation. Carry
	// the facts that own the exact container ID through verification and cleanup.
	type candidate struct {
		resource   state.Resource
		component  composition.ResolvedComponent
		spec       composition.ResolvedSpec
		networks   map[string]state.Resource
		runtimeDir string
	}
	names := make([]string, 0, len(operation.Target.Components))
	seen := make(map[string]struct{})
	for _, component := range operation.Target.Components {
		names = append(names, component.Name)
		seen[component.Name] = struct{}{}
	}
	if operation.Previous != nil {
		for _, component := range operation.Previous.Spec.Components {
			if _, exists := seen[component.Name]; exists {
				continue
			}
			names = append(names, component.Name)
			seen[component.Name] = struct{}{}
		}
	}
	candidates := make(map[string]candidate, len(names))
	for _, name := range names {
		actual, inspectErr := controller.inspectContainer(
			ctx,
			containerName(scope, name),
		)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			if _, recorded := operation.Containers[name]; recorded {
				component, declared := operation.Target.Component(name)
				if !declared {
					return fmt.Errorf("recorded target container %s is not declared", name)
				}
				if err := controller.removeTargetContainerForRecreate(
					ctx,
					operation,
					component,
				); err != nil {
					return fmt.Errorf(
						"recover %s endpoint after proxy loss: %w",
						name,
						err,
					)
				}
			}
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		targetResource, recorded := operation.Containers[name]
		targetComponent, declared := operation.Target.Component(name)
		if recorded && declared && targetResource.ID == actual.ID {
			usePrevious := false
			if targetComponent.Runtime.ExternalEgress {
				_, targetHasNetwork := operation.Networks[componentNetworkKey(name)]
				if !targetHasNetwork && operation.Previous != nil {
					// Selection omits a network observed missing. The retained
					// container's previous record remains cleanup authority.
					previousResource, previousRecorded := operation.Previous.Containers[name]
					_, previousDeclared := operation.Previous.Spec.Component(name)
					usePrevious = previousRecorded && previousDeclared &&
						previousResource.ID == actual.ID
				}
			}
			if !usePrevious {
				candidates[name] = candidate{
					resource:   targetResource,
					component:  targetComponent,
					spec:       operation.Target,
					networks:   operation.Networks,
					runtimeDir: runtimeDirectory(operation.RuntimeRoot, operation.Target.Name),
				}
				continue
			}
		}
		if operation.Previous != nil {
			previousResource, previousRecorded := operation.Previous.Containers[name]
			previousComponent, previousDeclared := operation.Previous.Spec.Component(name)
			if previousRecorded && previousDeclared && previousResource.ID == actual.ID {
				candidates[name] = candidate{
					resource:   previousResource,
					component:  previousComponent,
					spec:       operation.Previous.Spec,
					networks:   operation.Previous.Networks,
					runtimeDir: runtimeDirectory(operation.Previous.RuntimeRoot, operation.Target.Name),
				}
				continue
			}
		}
		if recorded {
			return fmt.Errorf(
				"container name %q no longer identifies recorded container %s",
				actual.Name,
				targetResource.ID,
			)
		}
		if actual.Labels[LabelOperation] == operation.ID && declared {
			candidates[name] = candidate{
				resource:   state.Resource{ID: actual.ID, Name: actual.Name},
				component:  targetComponent,
				spec:       operation.Target,
				networks:   operation.Networks,
				runtimeDir: runtimeDirectory(operation.RuntimeRoot, operation.Target.Name),
			}
			continue
		}
		if operation.Previous != nil {
			if previousResource, exists := operation.Previous.Containers[name]; exists {
				return fmt.Errorf(
					"container name %q no longer identifies recorded container %s",
					actual.Name,
					previousResource.ID,
				)
			}
		}
		if actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf("container name %q is occupied", actual.Name)
		}
		return fmt.Errorf("container name %q has no target component", actual.Name)
	}

	knownNetworks := make(map[string]state.Resource)
	for _, resource := range operation.Networks {
		knownNetworks[resource.ID] = resource
	}
	if operation.Previous != nil {
		for _, resource := range operation.Previous.Networks {
			knownNetworks[resource.ID] = resource
		}
	}
	for _, name := range names {
		candidate, exists := candidates[name]
		if !exists {
			continue
		}
		actual, inspectErr := controller.inspectContainer(ctx, candidate.resource.ID)
		if errors.Is(inspectErr, engine.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			return inspectErr
		}
		if err := verifyCurrentContainer(
			scope,
			candidate.runtimeDir,
			candidate.component,
			candidate.resource,
			actual,
		); err != nil {
			return err
		}
		if err := verifyContainerEnvironment(
			candidate.spec,
			candidate.component,
			actual,
		); err != nil {
			return err
		}
		if err := verifyNoUnknownContainerNetworksWithFallback(
			candidate.component,
			candidate.networks,
			knownNetworks,
			actual,
		); err != nil {
			return err
		}
	}

	for _, name := range names {
		candidate, exists := candidates[name]
		if exists {
			actual, inspectErr := controller.inspectContainer(ctx, candidate.resource.ID)
			if inspectErr != nil && !errors.Is(inspectErr, engine.ErrNotFound) {
				return inspectErr
			}
			if inspectErr == nil {
				if err := controller.prepareEndpointCleanup(
					ctx,
					operation,
					candidate.spec,
					candidate.networks,
					candidate.component,
					candidate.resource,
				); err != nil {
					return fmt.Errorf(
						"record %s endpoint cleanup after proxy loss: %w",
						name,
						err,
					)
				}
				if actual.Running {
					callCtx, cancel := controller.callContext(ctx)
					err := controller.Engine.StopContainer(
						callCtx,
						candidate.resource.ID,
						controller.stopTimeout(),
					)
					cancel()
					if err != nil && !errors.Is(err, engine.ErrNotFound) {
						return fmt.Errorf(
							"stop %s after proxy loss: %w",
							name,
							err,
						)
					}
				}
				callCtx, cancel := controller.callContext(ctx)
				err := controller.Engine.RemoveContainer(callCtx, candidate.resource.ID)
				cancel()
				if err != nil && !errors.Is(err, engine.ErrNotFound) {
					return fmt.Errorf(
						"remove %s after proxy loss: %w",
						name,
						err,
					)
				}
				if err := controller.recoverEndpointCleanups(ctx, operation); err != nil {
					return fmt.Errorf(
						"verify %s endpoint cleanup after proxy loss: %w",
						name,
						err,
					)
				}
			}
		}
		delete(operation.Containers, name)
		delete(operation.Completed, name)
		delete(operation.PendingCreates, containerCreateKey(name))
		if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
			return err
		}
	}
	return nil
}

func (controller *Controller) stopRecordedProxy(
	ctx context.Context,
	process *proxy.Process,
) error {
	if process == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, controller.stopTimeout())
	defer cancel()
	err := controller.Proxy.Stop(callCtx, *process)
	if err != nil && !errors.Is(err, proxy.ErrNotRunning) {
		return err
	}
	return nil
}

func proxyProcessMatches(left, right *proxy.Process) bool {
	if left == nil || right == nil {
		return false
	}
	return left.InstanceID == right.InstanceID &&
		left.PID == right.PID && left.RuntimeDir == right.RuntimeDir &&
		left.Control == right.Control && left.Log == right.Log
}
