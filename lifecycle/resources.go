package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/internal/runtimecontract"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

const componentPIDsLimit int64 = 2048

var errStandardIOPolicy = errors.New("component standard I/O policy mismatch")

func componentSecurity() engine.ContainerSecurity {
	return engine.ContainerSecurity{
		NoNewPrivileges:     true,
		DroppedCapabilities: []string{"NET_RAW"},
		PIDsLimit:           componentPIDsLimit,
	}
}

func expectedNetworkLabels(
	scope dockerScope,
	plan networkPlan,
	operation string,
) map[string]string {
	labels := map[string]string{
		LabelOwner:       ownerValue,
		LabelNamespace:   scope.Namespace,
		LabelSystem:      scope.System,
		LabelKind:        "network",
		LabelNetworkKey:  plan.Key,
		LabelNetworkSpec: plan.Digest,
	}
	if operation != "" {
		labels[LabelOperation] = operation
	}
	return labels
}

func expectedContainerLabels(
	scope dockerScope,
	component composition.ResolvedComponent,
	operation string,
) map[string]string {
	labels := map[string]string{
		LabelOwner:         ownerValue,
		LabelNamespace:     scope.Namespace,
		LabelSystem:        scope.System,
		LabelKind:          "component",
		LabelComponent:     component.Name,
		LabelComponentSpec: component.Digest,
	}
	if operation != "" {
		labels[LabelOperation] = operation
	}
	return labels
}

func expectedVolumeLabels(
	scope dockerScope,
	component string,
	logical string,
) map[string]string {
	return map[string]string{
		LabelOwner:         ownerValue,
		LabelNamespace:     scope.Namespace,
		LabelSystem:        scope.System,
		LabelKind:          "volume",
		LabelComponent:     component,
		LabelVolume:        "1",
		LabelVolumeLogical: logical,
	}
}

func verifyNetwork(
	scope dockerScope,
	plan networkPlan,
	resource state.Resource,
	actual engine.Network,
) error {
	if actual.ID != resource.ID {
		return fmt.Errorf(
			"network ID changed: expected %s, found %s",
			resource.ID,
			actual.ID,
		)
	}
	if actual.Name != resource.Name || actual.Name != plan.Name {
		return fmt.Errorf(
			"network %s has unexpected name %q",
			actual.ID,
			actual.Name,
		)
	}
	if actual.Driver != "bridge" || actual.Internal != plan.Internal {
		return fmt.Errorf(
			"network %s has wrong policy (driver=%q internal=%t)",
			actual.ID,
			actual.Driver,
			actual.Internal,
		)
	}
	expected := expectedNetworkLabels(scope, plan, "")
	for key, value := range expected {
		if actual.Labels[key] != value {
			return fmt.Errorf(
				"network %s is not the expected %s network",
				actual.ID,
				plan.Key,
			)
		}
	}
	return nil
}

func verifyVolume(
	scope dockerScope,
	component string,
	logical string,
	actual engine.Volume,
) error {
	expectedName := volumeName(scope, component, logical)
	if actual.Name != expectedName || actual.Driver != "local" {
		return fmt.Errorf(
			"volume %q is not the expected local volume",
			expectedName,
		)
	}
	for key, value := range expectedVolumeLabels(scope, component, logical) {
		if actual.Labels[key] != value {
			return fmt.Errorf(
				"volume %q is not owned by %s.%s",
				expectedName,
				component,
				logical,
			)
		}
	}
	return nil
}

func verifyContainerCore(
	scope dockerScope,
	runtimeDir string,
	component composition.ResolvedComponent,
	resource state.Resource,
	actual engine.Container,
) error {
	if actual.ID != resource.ID {
		return fmt.Errorf(
			"%s container ID changed: expected %s, found %s",
			component.Name,
			resource.ID,
			actual.ID,
		)
	}
	if actual.Name != resource.Name ||
		actual.Name != containerName(scope, component.Name) {
		return fmt.Errorf(
			"container %s has unexpected name %q",
			actual.ID,
			actual.Name,
		)
	}
	if actual.ImageID != component.ImageID {
		return fmt.Errorf(
			"%s uses image %s, expected %s",
			component.Name,
			actual.ImageID,
			component.ImageID,
		)
	}
	if component.Runtime.User != "" && actual.User != component.Runtime.User {
		return fmt.Errorf("%s has unexpected user %q, expected %q", component.Name, actual.User, component.Runtime.User)
	}
	expected := expectedContainerLabels(scope, component, "")
	for key, value := range expected {
		if actual.Labels[key] != value {
			return fmt.Errorf(
				"container %s is not the expected owned %s component",
				actual.ID,
				component.Name,
			)
		}
	}
	if !actual.Init || actual.RestartPolicy != "no" {
		return fmt.Errorf(
			"%s has unexpected init or restart policy",
			component.Name,
		)
	}
	if !reflect.DeepEqual(actual.Security, componentSecurity()) {
		return fmt.Errorf("%s has unexpected security policy", component.Name)
	}
	expectedMounts := componentMounts(scope, runtimeDir, component)
	if !equalMounts(actual.Mounts, expectedMounts) {
		return fmt.Errorf("%s has unexpected mounts", component.Name)
	}
	expectedPorts := componentPorts(component)
	if !equalPorts(actual.PortBindings, expectedPorts) {
		return fmt.Errorf("%s has unexpected published ports", component.Name)
	}
	if len(component.Runtime.Args) != 0 &&
		!reflect.DeepEqual(actual.Args, component.Runtime.Args) {
		return fmt.Errorf("%s has unexpected command arguments", component.Name)
	}
	return nil
}

func verifyCurrentContainer(
	scope dockerScope,
	runtimeDir string,
	component composition.ResolvedComponent,
	resource state.Resource,
	actual engine.Container,
) error {
	if err := verifyContainerCore(scope, runtimeDir, component, resource, actual); err != nil {
		return err
	}
	if !actual.OpenStdin || actual.StdinOnce || actual.TTY {
		return fmt.Errorf(
			"%w: %s must keep stdin open with StdinOnce=false and Tty=false",
			errStandardIOPolicy,
			component.Name,
		)
	}
	return nil
}

func verifyContainerEnvironment(
	spec composition.ResolvedSpec,
	component composition.ResolvedComponent,
	actual engine.Container,
) error {
	expected, err := componentEnvironment(spec, component)
	if err != nil {
		return err
	}
	for key, value := range expected {
		if actual.Environment[key] != value {
			return fmt.Errorf(
				"%s has unexpected %s",
				component.Name,
				key,
			)
		}
	}
	for key := range actual.Environment {
		if len(key) >= len("DCOMP_") && key[:len("DCOMP_")] == "DCOMP_" {
			if _, exists := expected[key]; !exists {
				return fmt.Errorf(
					"%s has undeclared DComp environment %s",
					component.Name,
					key,
				)
			}
		}
	}
	return nil
}

func verifyContainerNetworks(
	component composition.ResolvedComponent,
	plans map[string]networkPlan,
	resources map[string]state.Resource,
	actual engine.Container,
) error {
	expected := make(map[string]state.Resource)
	for _, key := range componentNetworkKeys(component.Name, plans) {
		resource, exists := resources[key]
		if !exists || resource.ID == "" {
			return fmt.Errorf(
				"%s has no recorded network for %s",
				component.Name,
				key,
			)
		}
		expected[resource.ID] = resource
	}
	if len(actual.Networks) != len(expected) {
		return fmt.Errorf(
			"%s has %d networks, expected %d",
			component.Name,
			len(actual.Networks),
			len(expected),
		)
	}
	for _, resource := range expected {
		_, attachment, exists := findContainerNetwork(actual, resource)
		if !exists {
			return fmt.Errorf(
				"%s is not attached to network %s",
				component.Name,
				resource.Name,
			)
		}
		if attachment.NetworkID != "" &&
			attachment.NetworkID != resource.ID {
			return fmt.Errorf(
				"%s network %s has unexpected ID %s",
				component.Name,
				resource.Name,
				attachment.NetworkID,
			)
		}
		if !containsString(attachment.Aliases, component.Name) {
			return fmt.Errorf(
				"%s has no component-name alias on network %s",
				component.Name,
				resource.Name,
			)
		}
	}
	for name, attachment := range actual.Networks {
		if _, exists := matchContainerNetwork(name, attachment, expected); !exists {
			return fmt.Errorf(
				"%s is attached to undeclared network %s",
				component.Name,
				observedNetworkDescription(name, attachment),
			)
		}
	}
	return nil
}

// verifyNoUnknownContainerNetworks permits missing planned attachments so an
// interrupted or externally damaged deployment can still be repaired, but it
// refuses an attachment outside the recorded deployment before any mutation.
func verifyNoUnknownContainerNetworks(
	component composition.ResolvedComponent,
	resources map[string]state.Resource,
	actual engine.Container,
) error {
	return verifyNoUnknownContainerNetworksWithFallback(
		component,
		resources,
		nil,
		actual,
	)
}

// verifyNoUnknownContainerNetworksWithFallback checks the container's own
// generation first. That matters before first start, when Docker reports a
// configured network name but no network ID and two DComp generations can
// have owned different networks at that same deterministic name.
func verifyNoUnknownContainerNetworksWithFallback(
	component composition.ResolvedComponent,
	primary map[string]state.Resource,
	fallback map[string]state.Resource,
	actual engine.Container,
) error {
	primaryByID := resourcesByID(primary)
	knownByID := resourcesByID(primary)
	for _, resource := range fallback {
		knownByID[resource.ID] = resource
	}
	for name, attachment := range actual.Networks {
		if _, exists := matchContainerNetwork(name, attachment, primaryByID); exists {
			continue
		}
		if _, exists := matchContainerNetwork(name, attachment, knownByID); exists {
			continue
		}
		return fmt.Errorf(
			"%s is attached to undeclared network %s",
			component.Name,
			observedNetworkDescription(name, attachment),
		)
	}
	return nil
}

func resourcesByID(resources map[string]state.Resource) map[string]state.Resource {
	byID := make(map[string]state.Resource, len(resources))
	for _, resource := range resources {
		byID[resource.ID] = resource
	}
	return byID
}

func findContainerNetwork(
	container engine.Container,
	resource state.Resource,
) (string, engine.NetworkAttachment, bool) {
	if attachment, exists := container.Networks[resource.Name]; exists {
		if attachment.NetworkID == "" || attachment.NetworkID == resource.ID {
			return resource.Name, attachment, true
		}
		return "", engine.NetworkAttachment{}, false
	}
	for name, attachment := range container.Networks {
		if attachment.NetworkID == resource.ID {
			return name, attachment, true
		}
	}
	return "", engine.NetworkAttachment{}, false
}

func matchContainerNetwork(
	name string,
	attachment engine.NetworkAttachment,
	resources map[string]state.Resource,
) (state.Resource, bool) {
	if attachment.NetworkID != "" {
		resource, exists := resources[attachment.NetworkID]
		if !exists || resource.Name != name {
			return state.Resource{}, false
		}
		return resource, true
	}
	var matched state.Resource
	found := false
	for _, resource := range resources {
		if resource.Name != name {
			continue
		}
		if found {
			return state.Resource{}, false
		}
		matched = resource
		found = true
	}
	return matched, found
}

func observedNetworkDescription(
	name string,
	attachment engine.NetworkAttachment,
) string {
	if attachment.NetworkID == "" {
		return name
	}
	return name + " (" + attachment.NetworkID + ")"
}

func verifyNetworkMembers(
	plan networkPlan,
	network engine.Network,
	containers map[string]state.Resource,
) error {
	expected := make([]string, 0, len(plan.Members))
	for component := range plan.Members {
		resource, exists := containers[component]
		if !exists || resource.ID == "" {
			return fmt.Errorf(
				"network %s has no recorded member %s",
				plan.Key,
				component,
			)
		}
		expected = append(expected, resource.ID)
	}
	actual := make([]string, 0, len(network.Endpoints))
	for _, endpoint := range network.Endpoints {
		actual = append(actual, endpoint.Key)
	}
	sort.Strings(expected)
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf(
			"network %s has unexpected members",
			plan.Key,
		)
	}
	known := make(map[string]state.Resource, len(containers))
	for _, resource := range containers {
		known[resource.ID] = resource
	}
	for _, endpoint := range network.Endpoints {
		if err := verifyNetworkEndpointIdentity(plan, endpoint, known[endpoint.Key]); err != nil {
			return err
		}
	}
	return nil
}

// verifyNoUnknownNetworkMembers is the repair-safe counterpart of
// verifyNetworkMembers: recorded containers may be absent, but foreign
// container IDs on a DComp network are never silently disconnected.
func verifyNoUnknownNetworkMembers(
	plan networkPlan,
	network engine.Network,
	containers map[string]state.Resource,
) error {
	known := make(map[string]state.Resource, len(containers))
	for _, resource := range containers {
		known[resource.ID] = resource
	}
	for _, endpoint := range network.Endpoints {
		resource, exists := known[endpoint.Key]
		if !exists {
			return fmt.Errorf(
				"network %s has undeclared member %s",
				plan.Key,
				endpoint.Key,
			)
		}
		if err := verifyNetworkEndpointIdentity(plan, endpoint, resource); err != nil {
			return err
		}
	}
	return nil
}

func verifyNetworkEndpointIdentity(
	plan networkPlan,
	endpoint engine.NetworkEndpoint,
	container state.Resource,
) error {
	if endpoint.Name != container.Name || endpoint.EndpointID == "" {
		return fmt.Errorf(
			"network %s member %s has unexpected endpoint identity",
			plan.Key,
			endpoint.Key,
		)
	}
	return nil
}

func componentMounts(
	scope dockerScope,
	runtimeDir string,
	component composition.ResolvedComponent,
) []engine.Mount {
	mounts := make(
		[]engine.Mount,
		0,
		len(component.Runtime.Binds)+len(component.Runtime.Volumes)+
			len(component.Definition.Inputs)+len(component.Definition.Outputs),
	)
	for _, bind := range component.Runtime.Binds {
		mounts = append(mounts, engine.Mount{
			Type:     engine.MountBind,
			Source:   bind.Source,
			Target:   bind.Target,
			ReadOnly: bind.ReadOnly,
		})
	}
	for _, volume := range component.Runtime.Volumes {
		mounts = append(mounts, engine.Mount{
			Type:     engine.MountVolume,
			Source:   volumeName(scope, component.Name, volume.Name),
			Target:   volume.Target,
			ReadOnly: volume.ReadOnly,
		})
	}
	for _, endpoint := range component.Definition.Inputs {
		mounts = append(mounts, engine.Mount{
			Type:     engine.MountBind,
			ReadOnly: true,
			Source: proxy.HostSocket(
				runtimeDir, proxy.DirectionInput, component.Name, endpoint.Name,
			),
			Target: runtimecontract.InputSocket(endpoint.Name),
		})
	}
	for _, endpoint := range component.Definition.Outputs {
		mounts = append(mounts, engine.Mount{
			Type:     engine.MountBind,
			ReadOnly: true,
			Source: proxy.HostSocket(
				runtimeDir, proxy.DirectionOutput, component.Name, endpoint.Name,
			),
			Target: runtimecontract.OutputSocket(endpoint.Name),
		})
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].Target != mounts[j].Target {
			return mounts[i].Target < mounts[j].Target
		}
		if mounts[i].Type != mounts[j].Type {
			return mounts[i].Type < mounts[j].Type
		}
		return mounts[i].Source < mounts[j].Source
	})
	return mounts
}

func componentPorts(
	component composition.ResolvedComponent,
) []engine.PortBinding {
	ports := make([]engine.PortBinding, 0, len(component.Runtime.Ports))
	for _, port := range component.Runtime.Ports {
		ports = append(ports, engine.PortBinding{
			ContainerPort: port.ContainerPort,
			Protocol:      engine.PortProtocol(port.Protocol),
			HostIP:        port.HostIP,
			HostPort:      port.HostPort,
		})
	}
	sortPorts(ports)
	return ports
}

func equalMounts(left, right []engine.Mount) bool {
	leftCopy := append([]engine.Mount(nil), left...)
	rightCopy := append([]engine.Mount(nil), right...)
	sort.Slice(leftCopy, func(i, j int) bool {
		return leftCopy[i].Target < leftCopy[j].Target
	})
	sort.Slice(rightCopy, func(i, j int) bool {
		return rightCopy[i].Target < rightCopy[j].Target
	})
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func equalPorts(left, right []engine.PortBinding) bool {
	leftCopy := append([]engine.PortBinding(nil), left...)
	rightCopy := append([]engine.PortBinding(nil), right...)
	sortPorts(leftCopy)
	sortPorts(rightCopy)
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func sortPorts(ports []engine.PortBinding) {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Protocol != ports[j].Protocol {
			return ports[i].Protocol < ports[j].Protocol
		}
		if ports[i].HostIP != ports[j].HostIP {
			return ports[i].HostIP < ports[j].HostIP
		}
		if ports[i].HostPort != ports[j].HostPort {
			return ports[i].HostPort < ports[j].HostPort
		}
		return ports[i].ContainerPort < ports[j].ContainerPort
	})
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (controller *Controller) inspectNetwork(
	ctx context.Context,
	idOrName string,
) (engine.Network, error) {
	callCtx, cancel := controller.callContext(ctx)
	defer cancel()
	return controller.Engine.InspectNetwork(callCtx, idOrName)
}

func (controller *Controller) inspectContainer(
	ctx context.Context,
	idOrName string,
) (engine.Container, error) {
	callCtx, cancel := controller.callContext(ctx)
	defer cancel()
	return controller.Engine.InspectContainer(callCtx, idOrName)
}

func (controller *Controller) inspectVolume(
	ctx context.Context,
	name string,
) (engine.Volume, error) {
	callCtx, cancel := controller.callContext(ctx)
	defer cancel()
	return controller.Engine.InspectVolume(callCtx, name)
}

func (controller *Controller) ensureVolume(
	ctx context.Context,
	system string,
	component string,
	logical string,
) error {
	scope := controller.dockerScope(system)
	name := volumeName(scope, component, logical)
	volume, err := controller.inspectVolume(ctx, name)
	if err == nil {
		return verifyVolume(scope, component, logical, volume)
	}
	if !errors.Is(err, engine.ErrNotFound) {
		return err
	}
	callCtx, cancel := controller.callContext(ctx)
	volume, createErr := controller.Engine.CreateVolume(
		callCtx,
		engine.VolumeRequest{
			Name:   name,
			Labels: expectedVolumeLabels(scope, component, logical),
		},
	)
	cancel()
	if createErr != nil {
		return fmt.Errorf("create volume %s: %w", name, createErr)
	}
	if err := verifyVolume(scope, component, logical, volume); err != nil {
		return err
	}
	controller.report("created persistent volume %s", name)
	return nil
}
