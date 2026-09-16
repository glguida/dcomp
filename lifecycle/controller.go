// Package lifecycle applies resolved component systems to a local Docker
// engine and coordinates their per-system proxy process.
package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

const (
	kindApply   = "apply"
	kindDown    = "down"
	kindRestart = "restart"

	phaseNetworks = "networks"
	phaseResync   = "resync"
	phaseRetire   = "retire"
	phaseCreate   = "create"
	phaseAttach   = "attach"
	phaseStart    = "start"
	phaseCommit   = "commit"

	phaseRestart = "restart"
	phaseDown    = "down"
	phaseAbort   = "abort"
)

type Controller struct {
	Engine engine.Engine
	Proxy  proxy.Manager
	State  state.Store
	// RuntimeRoot contains one directory per running system. It is host-local
	// transient state, separate from the durable lifecycle State root.
	RuntimeRoot string

	RequestTimeout time.Duration
	StopTimeout    time.Duration

	// Report receives human-readable progress, if non-nil.
	Report func(string)
}

func effectiveDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (controller *Controller) requestTimeout() time.Duration {
	return effectiveDuration(controller.RequestTimeout, 30*time.Second)
}

func (controller *Controller) stopTimeout() time.Duration {
	return effectiveDuration(controller.StopTimeout, 10*time.Second)
}

func (controller *Controller) validate() error {
	if controller.Engine == nil {
		return fmt.Errorf("Docker engine is not configured")
	}
	if controller.Proxy == nil {
		return fmt.Errorf("DComp proxy manager is not configured")
	}
	if !filepath.IsAbs(controller.State.Root) {
		return fmt.Errorf("state root is not an absolute path")
	}
	if !filepath.IsAbs(controller.RuntimeRoot) ||
		filepath.Clean(controller.RuntimeRoot) != controller.RuntimeRoot {
		return fmt.Errorf("proxy runtime root is not an absolute clean path")
	}
	return nil
}

// Check resolves images and validates a system without changing state or
// Docker.
func (controller *Controller) Check(
	ctx context.Context,
	spec composition.Spec,
) (composition.ResolvedSpec, error) {
	if err := controller.validate(); err != nil {
		return composition.ResolvedSpec{}, err
	}
	for _, instance := range spec.Components {
		if err := composition.ValidateBindSources(instance.Runtime); err != nil {
			return composition.ResolvedSpec{}, fmt.Errorf("component %s: %w", instance.Name, err)
		}
	}
	return controller.resolve(ctx, spec)
}

// Up applies only the changed portion of a system. Unchanged component
// containers keep their immutable IDs and remain running.
func (controller *Controller) Up(
	ctx context.Context,
	spec composition.Spec,
) error {
	if err := controller.validate(); err != nil {
		return err
	}
	resolved, err := controller.resolve(ctx, spec)
	if err != nil {
		return err
	}
	lock, err := controller.State.Acquire(resolved.Name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := controller.bindEngine(ctx); err != nil {
		return err
	}

	if pending, exists, err := controller.State.ReadOperation(resolved.Name); err != nil {
		return err
	} else if exists {
		if pending.Kind == kindApply &&
			pending.Phase != phaseAbort &&
			pending.Target.Digest == resolved.Digest &&
			pending.RuntimeRoot == controller.RuntimeRoot {
			controller.report("resuming interrupted apply for %s", resolved.Name)
			return controller.execute(ctx, &pending)
		}
		if err := controller.supersedeOperation(
			ctx,
			&pending,
			resolved.Digest,
		); err != nil {
			return err
		}
	}
	return controller.applyResolved(ctx, resolved)
}

func (controller *Controller) applyResolved(
	ctx context.Context,
	resolved composition.ResolvedSpec,
) error {
	previous, exists, err := controller.State.ReadDesired(resolved.Name)
	if err != nil {
		return err
	}
	previousDigests := make(map[string]string)
	for _, component := range previous.Spec.Components {
		previousDigests[component.Name] = component.Digest
	}
	for _, component := range resolved.Components {
		if previousDigests[component.Name] == component.Digest {
			continue
		}
		if err := composition.ValidateBindSources(component.Runtime); err != nil {
			return fmt.Errorf("component %s: %w", component.Name, err)
		}
	}
	if exists {
		matches, err := controller.deploymentMatches(ctx, previous, resolved)
		if err != nil {
			return fmt.Errorf("inspect current %s deployment: %w", resolved.Name, err)
		}
		if matches {
			controller.report("%s is already applied", resolved.Name)
			return nil
		}
	}

	var previousPointer *state.Deployment
	if exists {
		copy := previous
		previousPointer = &copy
	}
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		resolved,
		previousPointer,
		controller.RuntimeRoot,
	)
	if err != nil {
		return err
	}
	if previousPointer != nil {
		if err := controller.selectRetainedResources(ctx, &operation); err != nil {
			return err
		}
	}
	if err := controller.State.WriteOperation(resolved.Name, operation); err != nil {
		return err
	}
	return controller.execute(ctx, &operation)
}

// Resume continues the exact durable operation using observed Docker facts.
func (controller *Controller) Resume(ctx context.Context, name string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	lock, err := controller.State.Acquire(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := controller.bindEngine(ctx); err != nil {
		return err
	}
	operation, exists, err := controller.State.ReadOperation(name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("system %q has no interrupted operation", name)
	}
	return controller.execute(ctx, &operation)
}

// Abort stops an operation without treating retained components as new
// resources. For an interrupted apply it removes target-only objects and
// restores the previous wiring. If reverse resync cannot converge, abort
// durably hands off to a fresh apply of the previous deployment.
func (controller *Controller) Abort(ctx context.Context, name string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	lock, err := controller.State.Acquire(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := controller.bindEngine(ctx); err != nil {
		return err
	}
	operation, exists, err := controller.State.ReadOperation(name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("system %q has no interrupted operation", name)
	}
	if len(operation.PendingCreates) != 0 {
		keys := make([]string, 0, len(operation.PendingCreates))
		for key := range operation.PendingCreates {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return fmt.Errorf(
			"cannot abort system %q with unresolved creates %v; run dcomp resume %s first",
			name,
			keys,
			name,
		)
	}
	if operation.Phase != phaseAbort {
		operation.Phase = phaseAbort
		operation.Completed = make(map[string]bool)
		if err := controller.State.WriteOperation(name, operation); err != nil {
			return err
		}
	}
	return controller.executeAbort(ctx, &operation)
}

// Down removes every exact component container and transient network while
// preserving explicitly declared persistent volumes.
func (controller *Controller) Down(ctx context.Context, name string) error {
	if err := controller.validate(); err != nil {
		return err
	}
	lock, err := controller.State.Acquire(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := controller.bindEngine(ctx); err != nil {
		return err
	}
	if pending, exists, err := controller.State.ReadOperation(name); err != nil {
		return err
	} else if exists {
		return pendingError(pending)
	}
	desired, exists, err := controller.State.ReadDesired(name)
	if err != nil {
		return err
	}
	if !exists {
		controller.report("%s is already absent", name)
		return nil
	}
	operation, err := state.NewOperation(
		kindDown,
		phaseDown,
		desired.Spec,
		&desired,
		desired.RuntimeRoot,
	)
	if err != nil {
		return err
	}
	operation.Networks = cloneResources(desired.Networks)
	operation.Containers = cloneResources(desired.Containers)
	operation.Proxy = cloneProxy(desired.Proxy)
	if err := controller.State.WriteOperation(name, operation); err != nil {
		return err
	}
	return controller.executeDown(ctx, &operation)
}

// Restart applies Docker's restart operation to selected committed component
// IDs. With no component names it restarts the complete system.
func (controller *Controller) Restart(
	ctx context.Context,
	name string,
	components ...string,
) error {
	if err := controller.validate(); err != nil {
		return err
	}
	lock, err := controller.State.Acquire(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := controller.bindEngine(ctx); err != nil {
		return err
	}
	if pending, exists, err := controller.State.ReadOperation(name); err != nil {
		return err
	} else if exists {
		return pendingError(pending)
	}
	desired, exists, err := controller.State.ReadDesired(name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("system %q is absent", name)
	}
	selected, err := selectedComponents(desired.Spec, components)
	if err != nil {
		return err
	}
	operation, err := state.NewOperation(
		kindRestart,
		phaseRestart,
		desired.Spec,
		&desired,
		desired.RuntimeRoot,
	)
	if err != nil {
		return err
	}
	operation.Networks = cloneResources(desired.Networks)
	operation.Containers = cloneResources(desired.Containers)
	operation.Proxy = cloneProxy(desired.Proxy)
	operation.Components = selected
	if err := controller.State.WriteOperation(name, operation); err != nil {
		return err
	}
	return controller.executeRestart(ctx, &operation)
}

func selectedComponents(
	spec composition.ResolvedSpec,
	requested []string,
) ([]string, error) {
	if len(requested) == 0 {
		result := make([]string, 0, len(spec.Components))
		for _, component := range spec.Components {
			result = append(result, component.Name)
		}
		return result, nil
	}
	seen := make(map[string]struct{}, len(requested))
	result := make([]string, 0, len(requested))
	for _, name := range requested {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("component %q was selected more than once", name)
		}
		if _, exists := spec.Component(name); !exists {
			return nil, fmt.Errorf("system %q has no component %q", spec.Name, name)
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (controller *Controller) resolve(
	ctx context.Context,
	spec composition.Spec,
) (composition.ResolvedSpec, error) {
	if err := composition.Validate(spec); err != nil {
		return composition.ResolvedSpec{}, err
	}
	images := make(map[string]composition.ResolvedImage, len(spec.Components))
	cache := make(map[string]engine.Image)
	for _, instance := range spec.Components {
		imageRef := instance.Component.Image
		image, exists := cache[imageRef]
		if !exists {
			callCtx, cancel := controller.callContext(ctx)
			var err error
			image, err = controller.Engine.ResolveImage(callCtx, imageRef)
			cancel()
			if err != nil {
				return composition.ResolvedSpec{}, fmt.Errorf(
					"resolve image for %s (%s): %w",
					instance.Name,
					imageRef,
					err,
				)
			}
			cache[imageRef] = image
		}
		images[instance.Name] = composition.ResolvedImage{
			ID:              image.ID,
			HasHealthcheck:  image.HasHealthcheck,
			DeclaredVolumes: append([]string(nil), image.DeclaredVolumes...),
		}
	}
	return composition.Resolve(spec, images)
}

func (controller *Controller) execute(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Phase == phaseAbort {
		return controller.executeAbort(ctx, operation)
	}
	switch operation.Kind {
	case kindApply:
		return controller.executeApply(ctx, operation)
	case kindDown:
		return controller.executeDown(ctx, operation)
	case kindRestart:
		return controller.executeRestart(ctx, operation)
	default:
		return fmt.Errorf("unknown operation kind %q", operation.Kind)
	}
}

func (controller *Controller) setPhase(
	operation *state.Operation,
	phase string,
) error {
	if len(operation.EndpointCleanups) != 0 {
		return fmt.Errorf(
			"cannot advance to %s with pending endpoint cleanup",
			phase,
		)
	}
	operation.Phase = phase
	operation.Completed = make(map[string]bool)
	return controller.State.WriteOperation(operation.Target.Name, *operation)
}

func (controller *Controller) markComplete(
	operation *state.Operation,
	key string,
) error {
	if operation.Completed == nil {
		operation.Completed = make(map[string]bool)
	}
	operation.Completed[key] = true
	return controller.State.WriteOperation(operation.Target.Name, *operation)
}

func (controller *Controller) callContext(
	parent context.Context,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, controller.requestTimeout())
}

func (controller *Controller) engineIdentity(ctx context.Context) (string, error) {
	callCtx, cancel := controller.callContext(ctx)
	defer cancel()
	id, err := controller.Engine.Identity(callCtx)
	if err != nil {
		return "", fmt.Errorf("identify Docker engine: %w", err)
	}
	if id == "" {
		return "", fmt.Errorf("identify Docker engine: empty engine ID")
	}
	return id, nil
}

func (controller *Controller) bindEngine(ctx context.Context) error {
	id, err := controller.engineIdentity(ctx)
	if err != nil {
		return err
	}
	if err := controller.State.BindEngine(id); err != nil {
		return fmt.Errorf("bind lifecycle state: %w", err)
	}
	return nil
}

func (controller *Controller) verifyEngineBinding(ctx context.Context) error {
	bound, exists, err := controller.State.ReadEngine()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("lifecycle state is not bound to a Docker engine")
	}
	current, err := controller.engineIdentity(ctx)
	if err != nil {
		return err
	}
	if bound != current {
		return fmt.Errorf(
			"state root is bound to Docker engine %q, current engine is %q",
			bound,
			current,
		)
	}
	return nil
}

func (controller *Controller) report(format string, values ...interface{}) {
	if controller.Report != nil {
		controller.Report(fmt.Sprintf(format, values...))
	}
}

func cloneResources(input map[string]state.Resource) map[string]state.Resource {
	output := make(map[string]state.Resource, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneProxy(input *proxy.Process) *proxy.Process {
	if input == nil {
		return nil
	}
	copy := *input
	return &copy
}

func pendingError(operation state.Operation) error {
	return fmt.Errorf(
		"system %q has an interrupted %s operation at phase %s; "+
			"run dcomp resume %s or dcomp abort %s",
		operation.Target.Name,
		operation.Kind,
		operation.Phase,
		operation.Target.Name,
		operation.Target.Name,
	)
}
