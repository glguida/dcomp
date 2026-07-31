package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/glguida/dcomp/composition"
)

const (
	LabelOwner          = "io.dcomp.owner"
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
	linkNetworkTag      = "link"
)

type networkPlan struct {
	Key      string
	Name     string
	Internal bool
	Members  map[string]struct{}
	Digest   string
}

func resolvedTopology(spec composition.ResolvedSpec) (map[string]networkPlan, error) {
	plans := make(map[string]networkPlan, len(spec.Components)+len(spec.Links))
	for _, component := range spec.Components {
		key := componentNetworkKey(component.Name)
		plan := networkPlan{
			Key:      key,
			Name:     componentNetworkName(spec.Name, component.Name),
			Internal: !component.Runtime.ExternalEgress,
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
	for _, link := range spec.Links {
		key := linkNetworkKey(link.Input.Component, link.Input.Endpoint)
		members := map[string]struct{}{
			link.Input.Component:  {},
			link.Output.Component: {},
		}
		if link.Input.Component == link.Output.Component {
			members = map[string]struct{}{
				link.Input.Component: {},
			}
		}
		plan := networkPlan{
			Key: key,
			Name: linkNetworkName(
				spec.Name,
				link.Input.Component,
				link.Input.Endpoint,
			),
			Internal: true,
			Members:  members,
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
		provider, _, linked := spec.LinkTarget(component.Name, input.Name)
		if !linked {
			return nil, fmt.Errorf(
				"%s.%s has no resolved link",
				component.Name,
				input.Name,
			)
		}
		environment[linkEnvironment(input.Name)] = fmt.Sprintf(
			"dns:///%s:%d",
			provider.Name,
			provider.Port,
		)
	}
	return environment, nil
}

func componentNetworkKey(component string) string {
	return componentNetworkTag + "/" + component
}

func linkNetworkKey(component, input string) string {
	return linkNetworkTag + "/" + component + "/" + input
}

func componentNetworkName(system, component string) string {
	return "dcomp." + system + ".component." + component
}

func linkNetworkName(system, component, input string) string {
	return "dcomp." + system + ".link." + component + "." + input
}

func containerName(system, component string) string {
	return "dcomp." + system + ".container." + component
}

func volumeName(system, component, logical string) string {
	return "dcomp." + system + ".volume." + component + "." + logical
}

func linkEnvironment(input string) string {
	return "DCOMP_LINK_" + strings.ToUpper(strings.ReplaceAll(input, "-", "_"))
}
