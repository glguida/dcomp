package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
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
	system string,
	plan networkPlan,
	operation string,
) map[string]string {
	labels := map[string]string{
		LabelOwner:       ownerValue,
		LabelSystem:      system,
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
	system string,
	component composition.ResolvedComponent,
	operation string,
) map[string]string {
	labels := map[string]string{
		LabelOwner:         ownerValue,
		LabelSystem:        system,
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
	system string,
	component string,
	logical string,
) map[string]string {
	return map[string]string{
		LabelOwner:         ownerValue,
		LabelSystem:        system,
		LabelKind:          "volume",
		LabelComponent:     component,
		LabelVolume:        "1",
		LabelVolumeLogical: logical,
	}
}

func verifyNetwork(
	system string,
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
	expected := expectedNetworkLabels(system, plan, "")
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
	system string,
	component string,
	logical string,
	actual engine.Volume,
) error {
	expectedName := volumeName(system, component, logical)
	if actual.Name != expectedName || actual.Driver != "local" {
		return fmt.Errorf(
			"volume %q is not the expected local volume",
			expectedName,
		)
	}
	for key, value := range expectedVolumeLabels(system, component, logical) {
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
	system string,
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
		actual.Name != containerName(system, component.Name) {
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
	expected := expectedContainerLabels(system, component, "")
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
	expectedMounts := componentMounts(system, component)
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
	system string,
	component composition.ResolvedComponent,
	resource state.Resource,
	actual engine.Container,
) error {
	if err := verifyContainerCore(system, component, resource, actual); err != nil {
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
	known := make(map[string]state.Resource, len(resources))
	for _, resource := range resources {
		known[resource.ID] = resource
	}
	for name, attachment := range actual.Networks {
		if _, exists := matchContainerNetwork(name, attachment, known); !exists {
			return fmt.Errorf(
				"%s is attached to undeclared network %s",
				component.Name,
				observedNetworkDescription(name, attachment),
			)
		}
	}
	return nil
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
	actual := append([]string(nil), network.Containers...)
	sort.Strings(expected)
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf(
			"network %s has unexpected members",
			plan.Key,
		)
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
	known := make(map[string]struct{}, len(containers))
	for _, resource := range containers {
		known[resource.ID] = struct{}{}
	}
	for _, id := range network.Containers {
		if _, exists := known[id]; !exists {
			return fmt.Errorf(
				"network %s has undeclared member %s",
				plan.Key,
				id,
			)
		}
	}
	return nil
}

func componentMounts(
	system string,
	component composition.ResolvedComponent,
) []engine.Mount {
	mounts := make(
		[]engine.Mount,
		0,
		len(component.Runtime.Binds)+len(component.Runtime.Volumes),
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
			Source:   volumeName(system, component.Name, volume.Name),
			Target:   volume.Target,
			ReadOnly: volume.ReadOnly,
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
	name := volumeName(system, component, logical)
	volume, err := controller.inspectVolume(ctx, name)
	if err == nil {
		return verifyVolume(system, component, logical, volume)
	}
	if !errors.Is(err, engine.ErrNotFound) {
		return err
	}
	callCtx, cancel := controller.callContext(ctx)
	volume, createErr := controller.Engine.CreateVolume(
		callCtx,
		engine.VolumeRequest{
			Name:   name,
			Labels: expectedVolumeLabels(system, component, logical),
		},
	)
	cancel()
	if createErr != nil {
		return fmt.Errorf("create volume %s: %w", name, createErr)
	}
	if err := verifyVolume(system, component, logical, volume); err != nil {
		return err
	}
	controller.report("created persistent volume %s", name)
	return nil
}
