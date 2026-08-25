package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/state"
)

// recoverEndpointCleanups completes cleanup that was durably recorded before
// a container removal. A record whose container still exists is left for the
// removal path that created it.
func (controller *Controller) recoverEndpointCleanups(
	ctx context.Context,
	operation *state.Operation,
) error {
	return controller.recoverContainerEndpointCleanups(ctx, operation, "")
}

func (controller *Controller) recoverContainerEndpointCleanups(
	ctx context.Context,
	operation *state.Operation,
	containerID string,
) error {
	endpointIDs := make([]string, 0, len(operation.EndpointCleanups))
	for endpointID, cleanup := range operation.EndpointCleanups {
		if containerID != "" && cleanup.ContainerID != containerID {
			continue
		}
		endpointIDs = append(endpointIDs, endpointID)
	}
	sort.Strings(endpointIDs)
	for _, endpointID := range endpointIDs {
		cleanup := operation.EndpointCleanups[endpointID]
		absent, err := controller.containerAbsent(ctx, state.Resource{
			ID: cleanup.ContainerID, Name: cleanup.ContainerName,
		})
		if err != nil {
			return err
		}
		if !absent {
			continue
		}
		if err := controller.reconcileEndpointCleanup(ctx, operation, cleanup); err != nil {
			return err
		}
	}
	return nil
}

// recoverRetiredPreviousEndpoints derives cleanup authority when a recorded
// previous container disappeared out of band before DComp could journal its
// endpoint. Retention is only a plan, so every previous container is observed;
// recoverAbsentComponentEndpoint returns immediately while it still exists.
func (controller *Controller) recoverRetiredPreviousEndpoints(
	ctx context.Context,
	operation *state.Operation,
) error {
	if operation.Previous == nil {
		return controller.recoverEndpointCleanups(ctx, operation)
	}
	if err := controller.recoverEndpointCleanups(ctx, operation); err != nil {
		return err
	}
	for _, component := range operation.Previous.Spec.Components {
		resource, exists := operation.Previous.Containers[component.Name]
		if !exists {
			return fmt.Errorf("previous deployment has no %s container", component.Name)
		}
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
	}
	return controller.recoverEndpointCleanups(ctx, operation)
}

func (controller *Controller) recoverAbsentComponentEndpoint(
	ctx context.Context,
	operation *state.Operation,
	spec composition.ResolvedSpec,
	networks map[string]state.Resource,
	component composition.ResolvedComponent,
	container state.Resource,
) error {
	if !component.Runtime.ExternalEgress {
		return nil
	}
	scope := controller.dockerScope(spec.Name)
	if err := verifyEndpointContainerResource(scope, component, container); err != nil {
		return err
	}
	_, inspectErr := controller.inspectContainer(ctx, container.ID)
	if inspectErr == nil {
		return nil
	}
	if !errors.Is(inspectErr, engine.ErrNotFound) {
		return inspectErr
	}
	plan, network, err := componentEndpointNetwork(scope, spec, networks, component)
	if err != nil {
		return err
	}
	actual, inspectErr := controller.inspectNetwork(ctx, network.ID)
	if errors.Is(inspectErr, engine.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return inspectErr
	}
	if err := verifyNetwork(scope, plan, network, actual); err != nil {
		return err
	}
	endpoint, exists, err := orphanEndpointForContainer(actual, container)
	if err != nil || !exists {
		return err
	}
	absent, err := controller.containerAbsent(ctx, container)
	if err != nil || !absent {
		return err
	}
	return controller.recordEndpointCleanup(
		operation,
		newEndpointCleanup(container, plan, network, endpoint),
	)
}

// recoverAbsentComponentEndpointCleanup is the removal boundary for a
// recorded container that is already absent. It preserves the container
// identity until any orphaned Docker endpoint has been durably recorded and
// proven absent.
func (controller *Controller) recoverAbsentComponentEndpointCleanup(
	ctx context.Context,
	operation *state.Operation,
	spec composition.ResolvedSpec,
	networks map[string]state.Resource,
	component composition.ResolvedComponent,
	container state.Resource,
) error {
	if err := controller.recoverAbsentComponentEndpoint(
		ctx,
		operation,
		spec,
		networks,
		component,
		container,
	); err != nil {
		return err
	}
	return controller.recoverContainerEndpointCleanups(
		ctx,
		operation,
		container.ID,
	)
}

// prepareEndpointCleanup journals the exact live endpoint before its
// container can be removed. Components using Docker network mode none have no
// endpoint and take no durable cleanup entry.
func (controller *Controller) prepareEndpointCleanup(
	ctx context.Context,
	operation *state.Operation,
	spec composition.ResolvedSpec,
	networks map[string]state.Resource,
	component composition.ResolvedComponent,
	container state.Resource,
) error {
	if !component.Runtime.ExternalEgress {
		return nil
	}
	scope := controller.dockerScope(spec.Name)
	if err := verifyEndpointContainerResource(scope, component, container); err != nil {
		return err
	}
	plan, network, err := componentEndpointNetwork(scope, spec, networks, component)
	if err != nil {
		return err
	}
	actual, inspectErr := controller.inspectNetwork(ctx, network.ID)
	if errors.Is(inspectErr, engine.ErrNotFound) {
		return nil
	}
	if inspectErr != nil {
		return inspectErr
	}
	if err := verifyNetwork(scope, plan, network, actual); err != nil {
		return err
	}
	endpoint, exists, err := liveEndpointForContainer(actual, container)
	if err != nil || !exists {
		return err
	}
	return controller.recordEndpointCleanup(
		operation,
		newEndpointCleanup(container, plan, network, endpoint),
	)
}

func (controller *Controller) recordEndpointCleanup(
	operation *state.Operation,
	cleanup state.EndpointCleanup,
) error {
	if operation.EndpointCleanups == nil {
		operation.EndpointCleanups = make(map[string]state.EndpointCleanup)
	}
	if existing, exists := operation.EndpointCleanups[cleanup.EndpointID]; exists {
		if existing != cleanup {
			return fmt.Errorf(
				"endpoint %s cleanup identity changed",
				cleanup.EndpointID,
			)
		}
		return nil
	}
	operation.EndpointCleanups[cleanup.EndpointID] = cleanup
	if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
		delete(operation.EndpointCleanups, cleanup.EndpointID)
		return err
	}
	return nil
}

func (controller *Controller) reconcileEndpointCleanup(
	ctx context.Context,
	operation *state.Operation,
	cleanup state.EndpointCleanup,
) error {
	scope := controller.dockerScope(operation.Target.Name)
	absent, err := controller.containerAbsent(ctx, state.Resource{
		ID: cleanup.ContainerID, Name: cleanup.ContainerName,
	})
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf(
			"refusing to remove endpoint %s while container %s exists",
			cleanup.EndpointID,
			cleanup.ContainerID,
		)
	}
	actual, inspectErr := controller.inspectNetwork(ctx, cleanup.NetworkID)
	if errors.Is(inspectErr, engine.ErrNotFound) {
		return controller.clearEndpointCleanup(operation, cleanup.EndpointID)
	}
	if inspectErr != nil {
		return inspectErr
	}
	if err := verifyEndpointCleanupNetwork(scope, operation, cleanup, actual); err != nil {
		return err
	}
	endpoint, exists, err := pendingEndpoint(actual, cleanup)
	if err != nil {
		return err
	}
	if !exists {
		return controller.clearEndpointCleanup(operation, cleanup.EndpointID)
	}

	callCtx, cancel := controller.callContext(ctx)
	forceErr := controller.Engine.ForceDisconnectNetworkEndpoint(
		callCtx,
		cleanup.NetworkID,
		endpoint.Name,
	)
	cancel()

	actual, inspectErr = controller.inspectNetwork(ctx, cleanup.NetworkID)
	if errors.Is(inspectErr, engine.ErrNotFound) {
		return controller.clearEndpointCleanup(operation, cleanup.EndpointID)
	}
	if inspectErr != nil {
		return inspectErr
	}
	if err := verifyEndpointCleanupNetwork(scope, operation, cleanup, actual); err != nil {
		return err
	}
	_, exists, identityErr := pendingEndpoint(actual, cleanup)
	if identityErr != nil {
		return identityErr
	}
	if exists {
		if forceErr != nil {
			return fmt.Errorf(
				"force-disconnect endpoint %s: %w",
				cleanup.EndpointID,
				forceErr,
			)
		}
		return fmt.Errorf(
			"Docker left endpoint %s attached to network %s",
			cleanup.EndpointID,
			cleanup.NetworkID,
		)
	}
	return controller.clearEndpointCleanup(operation, cleanup.EndpointID)
}

func (controller *Controller) clearEndpointCleanup(
	operation *state.Operation,
	endpointID string,
) error {
	cleanup, exists := operation.EndpointCleanups[endpointID]
	if !exists {
		return nil
	}
	delete(operation.EndpointCleanups, endpointID)
	if err := controller.State.WriteOperation(operation.Target.Name, *operation); err != nil {
		operation.EndpointCleanups[endpointID] = cleanup
		return err
	}
	return nil
}

func (controller *Controller) containerAbsent(
	ctx context.Context,
	container state.Resource,
) (bool, error) {
	actual, inspectErr := controller.inspectContainer(ctx, container.ID)
	if inspectErr == nil {
		if actual.ID != container.ID || actual.Name != container.Name {
			return false, fmt.Errorf(
				"container %s no longer has recorded identity %q",
				container.ID,
				container.Name,
			)
		}
		return false, nil
	}
	if !errors.Is(inspectErr, engine.ErrNotFound) {
		return false, inspectErr
	}
	actual, inspectErr = controller.inspectContainer(ctx, container.Name)
	if errors.Is(inspectErr, engine.ErrNotFound) {
		return true, nil
	}
	if inspectErr != nil {
		return false, inspectErr
	}
	return false, fmt.Errorf(
		"refusing endpoint cleanup: container name %q belongs to %s",
		container.Name,
		actual.ID,
	)
}

func componentEndpointNetwork(
	scope dockerScope,
	spec composition.ResolvedSpec,
	networks map[string]state.Resource,
	component composition.ResolvedComponent,
) (networkPlan, state.Resource, error) {
	plans, err := resolvedTopology(scope, spec)
	if err != nil {
		return networkPlan{}, state.Resource{}, err
	}
	key := componentNetworkKey(component.Name)
	plan, exists := plans[key]
	if !exists {
		return networkPlan{}, state.Resource{}, fmt.Errorf(
			"%s has no egress network plan",
			component.Name,
		)
	}
	network, exists := networks[key]
	if !exists || network.ID == "" || network.Name == "" {
		return networkPlan{}, state.Resource{}, fmt.Errorf(
			"%s has no recorded egress network",
			component.Name,
		)
	}
	return plan, network, nil
}

func verifyEndpointContainerResource(
	scope dockerScope,
	component composition.ResolvedComponent,
	container state.Resource,
) error {
	expectedName := containerName(scope, component.Name)
	if container.ID == "" || container.Name != expectedName {
		return fmt.Errorf(
			"%s has unexpected endpoint cleanup container identity",
			component.Name,
		)
	}
	return nil
}

func newEndpointCleanup(
	container state.Resource,
	plan networkPlan,
	network state.Resource,
	endpoint engine.NetworkEndpoint,
) state.EndpointCleanup {
	return state.EndpointCleanup{
		ContainerID:   container.ID,
		ContainerName: container.Name,
		NetworkKey:    plan.Key,
		NetworkID:     network.ID,
		NetworkName:   network.Name,
		EndpointID:    endpoint.EndpointID,
		EndpointName:  endpoint.Name,
	}
}

func liveEndpointForContainer(
	network engine.Network,
	container state.Resource,
) (engine.NetworkEndpoint, bool, error) {
	var found engine.NetworkEndpoint
	for _, endpoint := range network.Endpoints {
		if endpoint.Key != container.ID {
			continue
		}
		if found.Key != "" {
			return engine.NetworkEndpoint{}, false, fmt.Errorf(
				"network %s has multiple endpoints for container %s",
				network.ID,
				container.ID,
			)
		}
		found = endpoint
	}
	if found.Key == "" {
		return engine.NetworkEndpoint{}, false, nil
	}
	if found.Name != container.Name || found.EndpointID == "" {
		return engine.NetworkEndpoint{}, false, fmt.Errorf(
			"network %s endpoint for container %s has unexpected identity",
			network.ID,
			container.ID,
		)
	}
	return found, true, nil
}

func orphanEndpointForContainer(
	network engine.Network,
	container state.Resource,
) (engine.NetworkEndpoint, bool, error) {
	var found engine.NetworkEndpoint
	for _, endpoint := range network.Endpoints {
		if endpoint.Name != container.Name ||
			endpoint.EndpointID == "" ||
			endpoint.Key != "ep-"+endpoint.EndpointID {
			continue
		}
		if found.Key != "" {
			return engine.NetworkEndpoint{}, false, fmt.Errorf(
				"network %s has multiple endpoints named %q",
				network.ID,
				container.Name,
			)
		}
		found = endpoint
	}
	if found.Key == "" {
		return engine.NetworkEndpoint{}, false, nil
	}
	return found, true, nil
}

// pendingEndpoint distinguishes a removed endpoint from a same-name endpoint
// with a different immutable identity. The latter is never force-disconnected.
func pendingEndpoint(
	network engine.Network,
	cleanup state.EndpointCleanup,
) (engine.NetworkEndpoint, bool, error) {
	var exact engine.NetworkEndpoint
	for _, endpoint := range network.Endpoints {
		if endpoint.EndpointID == cleanup.EndpointID {
			if exact.EndpointID != "" {
				return engine.NetworkEndpoint{}, false, fmt.Errorf(
					"network %s repeats endpoint ID %s",
					network.ID,
					cleanup.EndpointID,
				)
			}
			exact = endpoint
		}
	}
	if exact.EndpointID != "" {
		if exact.Name != cleanup.EndpointName {
			return engine.NetworkEndpoint{}, false, fmt.Errorf(
				"endpoint %s name changed from %q to %q",
				cleanup.EndpointID,
				cleanup.EndpointName,
				exact.Name,
			)
		}
		return exact, true, nil
	}
	for _, endpoint := range network.Endpoints {
		if endpoint.Name == cleanup.EndpointName {
			return engine.NetworkEndpoint{}, false, fmt.Errorf(
				"endpoint %q identity changed from %s to %s",
				cleanup.EndpointName,
				cleanup.EndpointID,
				endpoint.EndpointID,
			)
		}
	}
	return engine.NetworkEndpoint{}, false, nil
}

func verifyEndpointCleanupNetwork(
	scope dockerScope,
	operation *state.Operation,
	cleanup state.EndpointCleanup,
	actual engine.Network,
) error {
	resource := state.Resource{ID: cleanup.NetworkID, Name: cleanup.NetworkName}
	var verificationErrors []error
	for _, spec := range cleanupSpecs(operation) {
		plans, err := resolvedTopology(scope, spec)
		if err != nil {
			return err
		}
		plan, exists := plans[cleanup.NetworkKey]
		if !exists || plan.Name != cleanup.NetworkName {
			continue
		}
		if len(plan.Members) != 1 {
			return fmt.Errorf(
				"endpoint cleanup network %s has ambiguous membership",
				cleanup.NetworkKey,
			)
		}
		identityMatches := true
		for component := range plan.Members {
			if cleanup.ContainerName != containerName(scope, component) {
				identityMatches = false
				verificationErrors = append(verificationErrors, fmt.Errorf(
					"endpoint cleanup container name %q does not match %s",
					cleanup.ContainerName,
					cleanup.NetworkKey,
				))
				continue
			}
		}
		if !identityMatches {
			continue
		}
		if err := verifyNetwork(scope, plan, resource, actual); err == nil {
			return nil
		} else {
			verificationErrors = append(verificationErrors, err)
		}
	}
	if len(verificationErrors) != 0 {
		return verificationErrors[0]
	}
	return fmt.Errorf(
		"endpoint cleanup references unknown network %s",
		cleanup.NetworkKey,
	)
}

func cleanupSpecs(operation *state.Operation) []composition.ResolvedSpec {
	result := []composition.ResolvedSpec{operation.Target}
	if operation.Previous != nil &&
		operation.Previous.Spec.Digest != operation.Target.Digest {
		result = append(result, operation.Previous.Spec)
	}
	return result
}
