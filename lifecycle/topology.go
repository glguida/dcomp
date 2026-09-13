package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/internal/runtimecontract"
)

const (
	LabelOwner          = "io.dcomp.owner"
	LabelNamespace      = "io.dcomp.namespace"
	LabelSystem         = "io.dcomp.system"
	LabelComponent      = "io.dcomp.component"
	LabelKind           = "io.dcomp.kind"
	LabelOperation      = "io.dcomp.operation"
	LabelComponentSpec  = "io.dcomp.component-spec"
	LabelNetworkKey     = "io.dcomp.network-key"
	LabelNetworkSpec    = "io.dcomp.network-spec"
	LabelVolume         = "io.dcomp.volume"
	LabelVolumeLogical  = "io.dcomp.volume-logical"
	ownerValue          = "dcomp"
	componentNetworkTag = "component"
	dockerNamespaceSize = 16
)

// dockerScope is the complete identity of one lifecycle namespace on one
// Docker engine. The namespace is derived from the cleaned state-root path;
// the human-readable system name remains a separate ownership coordinate.
type dockerScope struct {
	Namespace string
	System    string
}

func newDockerScope(stateRoot, system string) dockerScope {
	sum := sha256.Sum256([]byte(filepath.Clean(stateRoot)))
	return dockerScope{
		Namespace: hex.EncodeToString(sum[:dockerNamespaceSize]),
		System:    system,
	}
}

func (controller *Controller) dockerScope(system string) dockerScope {
	return newDockerScope(controller.State.Root, system)
}

type networkPlan struct {
	Key      string
	Name     string
	Internal bool
	Members  map[string]struct{}
	Digest   string
}

func resolvedTopology(
	scope dockerScope,
	spec composition.ResolvedSpec,
) (map[string]networkPlan, error) {
	if scope.System != spec.Name {
		return nil, fmt.Errorf(
			"Docker scope belongs to system %q, not %q",
			scope.System,
			spec.Name,
		)
	}
	plans := make(map[string]networkPlan, len(spec.Components))
	for _, component := range spec.Components {
		if !component.Runtime.ExternalEgress {
			continue
		}
		key := componentNetworkKey(component.Name)
		plan := networkPlan{
			Key:      key,
			Name:     componentNetworkName(scope, component.Name),
			Internal: false,
			Members: map[string]struct{}{
				component.Name: {},
			},
		}
		digest, err := networkDigest(plan)
		if err != nil {
			return nil, err
		}
		plan.Digest = digest
		plans[key] = plan
	}
	return plans, nil
}

func networkDigest(plan networkPlan) (string, error) {
	encoded, err := json.Marshal(struct {
		Key      string `json:"key"`
		Internal bool   `json:"internal"`
	}{
		Key:      plan.Key,
		Internal: plan.Internal,
	})
	if err != nil {
		return "", fmt.Errorf("encode network identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func componentNetworkKeys(
	component string,
	plans map[string]networkPlan,
) []string {
	keys := make([]string, 0)
	for key, plan := range plans {
		if _, exists := plan.Members[component]; exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func componentEnvironment(
	spec composition.ResolvedSpec,
	component composition.ResolvedComponent,
) (map[string]string, error) {
	environment := map[string]string{
		"DCOMP_COMPONENT_NAME": component.Name,
	}
	for _, input := range component.Definition.Inputs {
		environment[runtimecontract.InputEnvironment(input.Name)] =
			runtimecontract.InputURI(input.Name)
	}
	for _, output := range component.Definition.Outputs {
		environment[runtimecontract.OutputEnvironment(output.Name)] =
			runtimecontract.OutputURI(output.Name)
	}
	return environment, nil
}

func componentNetworkKey(component string) string {
	return componentNetworkTag + "/" + component
}

func componentNetworkName(scope dockerScope, component string) string {
	return "dcomp." + scope.Namespace + "." + scope.System +
		".component." + component
}

func containerName(scope dockerScope, component string) string {
	return "dcomp." + scope.Namespace + "." + scope.System +
		".container." + component
}

func volumeName(scope dockerScope, component, logical string) string {
	return "dcomp." + scope.Namespace + "." + scope.System +
		".volume." + component + "." + logical
}
