package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/glguida/dcomp/engine"
)

func TestInspectPersistentVolumeReturnsOnlyVerifiedOwnedVolume(t *testing.T) {
	controller, fake := newControllerHarness(t)
	preparePersistentVolumeInspection(t, controller, fake, "demo")
	name := volumeName("demo", "worker", "data")
	fake.volumes[name] = engine.Volume{
		Name: name, Driver: "local",
		Mountpoint: "/var/lib/docker/volumes/" + name + "/_data",
		Labels:     expectedVolumeLabels("demo", "worker", "data"),
	}

	got, err := controller.InspectPersistentVolume(
		context.Background(), "demo", "worker", "data",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := PersistentVolume{
		System: "demo", Component: "worker", LogicalName: "data", Name: name,
	}
	if got != want {
		t.Fatalf("persistent volume = %#v, want %#v", got, want)
	}
}

func TestInspectPersistentVolumeRejectsInvalidCoordinatesBeforeDocker(t *testing.T) {
	for _, test := range []struct {
		name      string
		system    string
		component string
		logical   string
		message   string
	}{
		{
			name: "system", system: "Bad", component: "worker", logical: "data",
			message: "invalid system name",
		},
		{
			name: "component", system: "demo", component: "../worker", logical: "data",
			message: "invalid component name",
		},
		{
			name: "logical", system: "demo", component: "worker", logical: "",
			message: "invalid logical volume name",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, fake := newControllerHarness(t)
			_, err := controller.InspectPersistentVolume(
				context.Background(), test.system, test.component, test.logical,
			)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("invalid coordinate reached Docker: %#v", fake.calls)
			}
		})
	}
}

func TestInspectPersistentVolumeRejectsMissingAndForeignVolumes(t *testing.T) {
	controller, fake := newControllerHarness(t)
	preparePersistentVolumeInspection(t, controller, fake, "demo")
	name := volumeName("demo", "worker", "data")

	_, err := controller.InspectPersistentVolume(
		context.Background(), "demo", "worker", "data",
	)
	if !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("missing volume error = %v, want engine.ErrNotFound", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*engine.Volume)
	}{
		{
			name: "foreign owner",
			mutate: func(volume *engine.Volume) {
				volume.Labels[LabelOwner] = "someone-else"
			},
		},
		{
			name: "wrong system",
			mutate: func(volume *engine.Volume) {
				volume.Labels[LabelSystem] = "other"
			},
		},
		{
			name: "wrong component",
			mutate: func(volume *engine.Volume) {
				volume.Labels[LabelComponent] = "other"
			},
		},
		{
			name: "wrong logical name",
			mutate: func(volume *engine.Volume) {
				volume.Labels[LabelVolumeLogical] = "other"
			},
		},
		{
			name: "wrong kind",
			mutate: func(volume *engine.Volume) {
				volume.Labels[LabelKind] = "component"
			},
		},
		{
			name: "missing volume marker",
			mutate: func(volume *engine.Volume) {
				delete(volume.Labels, LabelVolume)
			},
		},
		{
			name: "wrong physical name",
			mutate: func(volume *engine.Volume) {
				volume.Name = "dcomp.other.volume.worker.data"
			},
		},
		{
			name: "wrong driver",
			mutate: func(volume *engine.Volume) {
				volume.Driver = "remote"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			volume := engine.Volume{
				Name: name, Driver: "local",
				Labels: expectedVolumeLabels("demo", "worker", "data"),
			}
			test.mutate(&volume)
			fake.volumes[name] = volume
			if _, err := controller.InspectPersistentVolume(
				context.Background(), "demo", "worker", "data",
			); err == nil {
				t.Fatal("foreign or malformed volume was accepted")
			}
		})
	}
}

func TestInspectPersistentVolumeRequiresMatchingBoundEngine(t *testing.T) {
	controller, fake := newControllerHarness(t)
	preparePersistentVolumeInspection(t, controller, fake, "demo")
	fake.identity = "different-engine"

	_, err := controller.InspectPersistentVolume(
		context.Background(), "demo", "worker", "data",
	)
	if err == nil || !strings.Contains(err.Error(), "current engine") {
		t.Fatalf("engine mismatch error = %v", err)
	}
	for _, call := range fake.calls {
		if call.Method == "inspect-volume" {
			t.Fatalf("engine mismatch inspected a volume: %#v", fake.calls)
		}
	}
}

func TestInspectPersistentVolumeRequiresExistingSystemState(t *testing.T) {
	controller, fake := newControllerHarness(t)
	_, err := controller.InspectPersistentVolume(
		context.Background(), "demo", "worker", "data",
	)
	if err == nil || !strings.Contains(err.Error(), "has no lifecycle state") {
		t.Fatalf("absent state error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("absent state reached Docker: %#v", fake.calls)
	}
}

func preparePersistentVolumeInspection(
	t *testing.T,
	controller *Controller,
	fake *fakeEngine,
	system string,
) {
	t.Helper()
	lock, err := controller.State.Acquire(system)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := controller.State.BindEngine(fake.identity); err != nil {
		t.Fatal(err)
	}
}
