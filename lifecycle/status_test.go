package lifecycle

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
)

func TestStatusReportsOperationalPrivateTopologyWithoutMutation(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()

	status, err := controller.Status(context.Background(), spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Operational() {
		t.Fatalf("healthy deployment is not operational: %#v", status)
	}
	if got, want := len(status.Networks), 3; got != want {
		t.Fatalf("network status count = %d, want %d", got, want)
	}
	for _, network := range status.Networks {
		if network.ID == "" || network.Problem != "" {
			t.Fatalf("unexpected network status: %#v", network)
		}
	}
	if mutations := fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("Status mutated Docker: %#v", mutations)
	}
}

func TestStatusReportsDynamicPortWithoutChangingConfiguredVerification(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	for index := range spec.Components {
		if spec.Components[index].Name != "consumer" {
			continue
		}
		spec.Components[index].Runtime.ExternalEgress = true
		spec.Components[index].Runtime.Ports = []composition.PublishedPort{{
			Protocol: "tcp", HostIP: "127.0.0.1",
			HostPort: 0, ContainerPort: 8080,
		}}
	}
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	consumerID := deployment.Containers["consumer"].ID
	fake.mu.Lock()
	consumer := fake.containers[consumerID]
	consumer.PublishedPorts = []engine.PortBinding{{
		Protocol: engine.ProtocolTCP, HostIP: "127.0.0.1",
		HostPort: 49152, ContainerPort: 8080,
	}}
	fake.containers[consumerID] = consumer
	fake.mu.Unlock()

	status, err := controller.Status(context.Background(), spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Operational() {
		t.Fatalf("effective dynamic port changed configured verification: %#v", status)
	}
	for _, component := range status.Components {
		if component.Name != "consumer" {
			continue
		}
		if want := consumer.PublishedPorts; !reflect.DeepEqual(component.PublishedPorts, want) {
			t.Fatalf(
				"published ports = %#v, want %#v",
				component.PublishedPorts, want,
			)
		}
		return
	}
	t.Fatal("consumer status is absent")
}

func TestStatusReportsOneMissingNetworkAndFailedComponent(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	linkID := deployment.Networks["link/consumer/upstream"].ID
	consumerID := deployment.Containers["consumer"].ID
	fake.deleteNetworkOutOfBand(linkID)
	fake.mu.Lock()
	consumer := fake.containers[consumerID]
	consumer.Status = "exited"
	consumer.Running = false
	consumer.Health = engine.HealthNone
	consumer.ExitCode = 75
	fake.containers[consumerID] = consumer
	fake.mu.Unlock()
	fake.resetCalls()

	status, err := controller.Status(context.Background(), spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if status.Operational() {
		t.Fatalf("broken deployment is operational: %#v", status)
	}
	var sawLink, sawConsumer bool
	for _, network := range status.Networks {
		if network.Key == "link/consumer/upstream" {
			sawLink = true
			if !strings.Contains(network.Problem, "absent") {
				t.Fatalf("missing link problem = %q", network.Problem)
			}
		} else if network.Problem != "" {
			t.Fatalf("unrelated network %s is degraded: %q", network.Key, network.Problem)
		}
	}
	for _, component := range status.Components {
		if component.Name == "consumer" {
			sawConsumer = true
			if component.Status != "exited" || component.ExitCode != 75 {
				t.Fatalf("failed consumer status: %#v", component)
			}
		}
	}
	if !sawLink || !sawConsumer {
		t.Fatalf("incomplete status: %#v", status)
	}
	if mutations := fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("Status repaired broken resources: %#v", mutations)
	}
}

func TestStatusRejectsChangedContainerIdentityWithoutMutation(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	providerID := deployment.Containers["provider"].ID
	fake.mu.Lock()
	provider := fake.containers[providerID]
	provider.ImageID = "sha256:foreign"
	fake.containers[providerID] = provider
	fake.mu.Unlock()
	fake.resetCalls()

	status, err := controller.Status(context.Background(), spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if status.Operational() {
		t.Fatal("status accepted a changed recorded container")
	}
	for _, component := range status.Components {
		if component.Name == "provider" {
			if !strings.Contains(component.Problem, "uses image sha256:foreign") {
				t.Fatalf("provider identity problem = %q", component.Problem)
			}
			if mutations := fake.mutationCalls(); len(mutations) != 0 {
				t.Fatalf("Status mutated changed container: %#v", mutations)
			}
			return
		}
	}
	t.Fatalf("provider status is absent: %#v", status.Components)
}

func TestStatusRejectsChangedFixedContainerSecurityWithoutMutation(t *testing.T) {
	tests := map[string]func(*engine.ContainerSecurity){
		"no-new-privileges": func(security *engine.ContainerSecurity) {
			security.NoNewPrivileges = false
		},
		"dropped capabilities": func(security *engine.ContainerSecurity) {
			security.DroppedCapabilities = nil
		},
		"PIDs limit": func(security *engine.ContainerSecurity) {
			security.PIDsLimit = 0
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			controller, fake := newControllerHarness(t)
			installImages(
				fake,
				"provider:v1", "consumer:v1",
				"sha256:provider", "sha256:consumer",
			)
			spec := linkedSpec("provider:v1", "consumer:v1")
			if err := controller.Up(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			deployment := requireDesired(t, controller.State, spec.Name)
			providerID := deployment.Containers["provider"].ID
			fake.mu.Lock()
			provider := fake.containers[providerID]
			change(&provider.Security)
			fake.containers[providerID] = provider
			fake.mu.Unlock()
			fake.resetCalls()

			status, err := controller.Status(context.Background(), spec.Name)
			if err != nil {
				t.Fatal(err)
			}
			if status.Operational() {
				t.Fatal("status accepted changed fixed container security")
			}
			for _, component := range status.Components {
				if component.Name != "provider" {
					continue
				}
				if !strings.Contains(component.Problem, "unexpected security policy") {
					t.Fatalf("provider security problem = %q", component.Problem)
				}
				if mutations := fake.mutationCalls(); len(mutations) != 0 {
					t.Fatalf("Status mutated changed container: %#v", mutations)
				}
				return
			}
			t.Fatalf("provider status is absent: %#v", status.Components)
		})
	}
}
