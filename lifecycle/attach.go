package lifecycle

import (
	"context"
	"fmt"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
)

// Attach connects a caller to one running component's global fd 0, 1, and 2.
// It holds a shared lifecycle lock for the complete attachment, preventing the
// recorded container from being replaced, and serializes writable attachment
// to that component across DComp processes.
func (controller *Controller) Attach(
	ctx context.Context,
	system string,
	componentName string,
	options engine.AttachOptions,
) error {
	if err := controller.validate(); err != nil {
		return err
	}
	if !composition.ValidName(system) {
		return fmt.Errorf("invalid system name %q", system)
	}
	if !composition.ValidName(componentName) {
		return fmt.Errorf("invalid component name %q", componentName)
	}
	attachEngine, ok := controller.Engine.(engine.AttachEngine)
	if !ok {
		return fmt.Errorf("configured container engine does not support standard I/O attachment")
	}

	systemLock, stateExists, err := controller.State.AcquireShared(ctx, system)
	if err != nil {
		return err
	}
	if !stateExists {
		return fmt.Errorf("system %q is absent", system)
	}
	defer systemLock.Close()
	if operation, exists, err := controller.State.ReadOperation(system); err != nil {
		return err
	} else if exists {
		return fmt.Errorf(
			"system %q has an interrupted %s operation in phase %s",
			system,
			operation.Kind,
			operation.Phase,
		)
	}
	deployment, exists, err := controller.State.ReadDesired(system)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("system %q is absent", system)
	}
	component, exists := deployment.Spec.Component(componentName)
	if !exists {
		return fmt.Errorf(
			"component %q is not present in recorded system %q",
			componentName,
			system,
		)
	}
	resource, exists := deployment.Containers[componentName]
	if !exists || resource.ID == "" {
		return fmt.Errorf("system %q has no recorded %s container", system, componentName)
	}
	if err := controller.verifyEngineBinding(ctx); err != nil {
		return err
	}
	if deployment.Proxy == nil {
		return fmt.Errorf("system %q has no recorded proxy", system)
	}
	if _, err := controller.inspectProxy(ctx, *deployment.Proxy); err != nil {
		return fmt.Errorf("inspect proxy before attachment: %w", err)
	}

	attachmentLock, err := controller.State.AcquireAttachment(
		ctx,
		system,
		componentName,
	)
	if err != nil {
		return err
	}
	defer attachmentLock.Close()

	scope := controller.dockerScope(deployment.Spec.Name)
	plans, err := resolvedTopology(scope, deployment.Spec)
	if err != nil {
		return err
	}
	container, err := controller.inspectContainer(ctx, resource.ID)
	if err != nil {
		return fmt.Errorf("inspect %s before standard I/O attachment: %w", componentName, err)
	}
	if err := verifyCurrentContainer(
		scope,
		deployment.Proxy.RuntimeDir,
		component,
		resource,
		container,
	); err != nil {
		return err
	}
	if err := verifyContainerEnvironment(deployment.Spec, component, container); err != nil {
		return err
	}
	if err := verifyContainerNetworks(
		component,
		plans,
		deployment.Networks,
		container,
	); err != nil {
		return err
	}
	if !container.Running {
		return fmt.Errorf("component %s.%s is not running", system, componentName)
	}
	if err := attachEngine.ContainerAttach(ctx, resource.ID, options); err != nil {
		return fmt.Errorf("attach %s.%s standard I/O: %w", system, componentName, err)
	}
	return nil
}
