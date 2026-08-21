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
	config, err := proxyConfig(operation.Target, operation.RuntimeRoot, operation.ID)
	if err != nil {
		return err
	}
	if operation.Proxy != nil {
		status, inspectErr := controller.inspectProxy(ctx, *operation.Proxy)
		if inspectErr == nil {
			if operation.Proxy.Digest != config.Digest || status.Digest != config.Digest {
				return fmt.Errorf("recorded proxy has unexpected wiring digest")
			}
			if operation.PendingCreates[proxyCreateKey] {
				delete(operation.PendingCreates, proxyCreateKey)
				return controller.State.WriteOperation(operation.Target.Name, *operation)
			}
			return nil
		}
		if !errors.Is(inspectErr, proxy.ErrNotRunning) {
			return inspectErr
		}
		operation.Proxy = nil
		if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
			return err
		}
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
// container, so every target container must be recreated before a replacement
// proxy can safely publish new sockets at the same paths.
func (controller *Controller) recoverMissingTargetProxy(
	ctx context.Context,
	operation *state.Operation,
) (bool, error) {
	if operation.Proxy != nil {
		if _, err := controller.inspectProxy(ctx, *operation.Proxy); err == nil {
			return false, nil
		} else if !errors.Is(err, proxy.ErrNotRunning) {
			return false, fmt.Errorf("inspect target proxy: %w", err)
		}
	}
	if err := controller.removeContainersWithStaleProxyMounts(ctx, operation); err != nil {
		return false, err
	}
	operation.Proxy = nil
	delete(operation.PendingCreates, proxyCreateKey)
	if err := controller.setPhase(operation, phaseProxy); err != nil {
		return false, err
	}
	controller.report("replacing missing proxy and socket-mounted components for %s", operation.Target.Name)
	return true, nil
}

func (controller *Controller) removeContainersWithStaleProxyMounts(
	ctx context.Context,
	operation *state.Operation,
) error {
	candidates := cloneResources(operation.Containers)
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
		if recorded, exists := candidates[component.Name]; exists {
			if recorded.ID != actual.ID {
				return fmt.Errorf(
					"container name %q no longer identifies recorded container %s",
					actual.Name,
					recorded.ID,
				)
			}
			continue
		}
		if actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf("container name %q is occupied", actual.Name)
		}
		candidates[component.Name] = state.Resource{ID: actual.ID, Name: actual.Name}
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
	previousContainers := make(map[string]struct{})
	if operation.Previous != nil {
		for _, resource := range operation.Previous.Containers {
			previousContainers[resource.ID] = struct{}{}
		}
	}
	for _, component := range operation.Target.Components {
		resource, exists := candidates[component.Name]
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
		if _, retained := previousContainers[resource.ID]; !retained &&
			actual.Labels[LabelOperation] != operation.ID {
			return fmt.Errorf(
				"refusing to replace container %s without operation ownership",
				resource.ID,
			)
		}
		if err := verifyCurrentContainer(
			operation.Target.Name,
			runtimeDirectory(operation.RuntimeRoot, operation.Target.Name),
			component,
			resource,
			actual,
		); err != nil {
			return err
		}
		if err := verifyContainerEnvironment(operation.Target, component, actual); err != nil {
			return err
		}
		if err := verifyNoUnknownContainerNetworks(component, knownNetworks, actual); err != nil {
			return err
		}
	}

	for _, component := range operation.Target.Components {
		resource, exists := candidates[component.Name]
		if exists {
			actual, inspectErr := controller.inspectContainer(ctx, resource.ID)
			if inspectErr != nil && !errors.Is(inspectErr, engine.ErrNotFound) {
				return inspectErr
			}
			if inspectErr == nil {
				if err := controller.prepareEndpointCleanup(
					ctx,
					operation,
					operation.Target,
					operation.Networks,
					component,
					resource,
				); err != nil {
					return fmt.Errorf(
						"record %s endpoint cleanup after proxy loss: %w",
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
						return fmt.Errorf(
							"stop %s after proxy loss: %w",
							component.Name,
							err,
						)
					}
				}
				callCtx, cancel := controller.callContext(ctx)
				err := controller.Engine.RemoveContainer(callCtx, resource.ID)
				cancel()
				if err != nil && !errors.Is(err, engine.ErrNotFound) {
					return fmt.Errorf(
						"remove %s after proxy loss: %w",
						component.Name,
						err,
					)
				}
				if err := controller.recoverEndpointCleanups(ctx, operation); err != nil {
					return fmt.Errorf(
						"verify %s endpoint cleanup after proxy loss: %w",
						component.Name,
						err,
					)
				}
			}
		}
		delete(operation.Containers, component.Name)
		delete(operation.Completed, component.Name)
		delete(operation.PendingCreates, containerCreateKey(component.Name))
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
	return left.InstanceID == right.InstanceID && left.Digest == right.Digest &&
		left.PID == right.PID && left.RuntimeDir == right.RuntimeDir &&
		left.Control == right.Control && left.Log == right.Log
}
