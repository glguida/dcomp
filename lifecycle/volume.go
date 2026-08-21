package lifecycle

import (
	"context"
	"fmt"

	"github.com/glguida/dcomp/composition"
)

// PersistentVolume identifies one verified DComp-owned Docker volume. It is
// independent of committed deployment state because persistent volumes remain
// after a system is brought down.
type PersistentVolume struct {
	System      string
	Component   string
	LogicalName string
	Name        string
}

// InspectPersistentVolume returns the deterministic Docker volume name only
// after the existing volume's local driver and complete DComp ownership labels
// have been verified.
func (controller *Controller) InspectPersistentVolume(
	ctx context.Context,
	system string,
	component string,
	logical string,
) (PersistentVolume, error) {
	for _, coordinate := range []struct {
		kind  string
		value string
	}{
		{kind: "system", value: system},
		{kind: "component", value: component},
		{kind: "logical volume", value: logical},
	} {
		if !composition.ValidName(coordinate.value) {
			return PersistentVolume{}, fmt.Errorf(
				"invalid %s name %q", coordinate.kind, coordinate.value,
			)
		}
	}
	if err := controller.validate(); err != nil {
		return PersistentVolume{}, err
	}
	lock, stateExists, err := controller.State.AcquireShared(ctx, system)
	if err != nil {
		return PersistentVolume{}, err
	}
	if !stateExists {
		return PersistentVolume{}, fmt.Errorf("system %q has no lifecycle state", system)
	}
	defer lock.Close()
	if err := controller.verifyEngineBinding(ctx); err != nil {
		return PersistentVolume{}, err
	}

	scope := controller.dockerScope(system)
	name := volumeName(scope, component, logical)
	volume, err := controller.inspectVolume(ctx, name)
	if err != nil {
		return PersistentVolume{}, fmt.Errorf(
			"inspect persistent volume %q: %w", name, err,
		)
	}
	if err := verifyVolume(scope, component, logical, volume); err != nil {
		return PersistentVolume{}, err
	}
	return PersistentVolume{
		System: system, Component: component, LogicalName: logical, Name: name,
	}, nil
}
