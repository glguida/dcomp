package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sequenceProviderCount = 3
	sequenceSpokeCount    = 12
	sequenceStepCount     = 128
)

// This is a deterministic model test rather than another single scenario. It
// repeatedly adds, removes, re-adds, and relinks endpoints, checking the live
// filesystem and complete ownership ledger after every transition.
func TestLongResyncSequenceMatchesWiringAndOwnershipModel(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		pathForm := "direct"
		if shortened {
			pathForm = "shortened"
		}
		t.Run(pathForm, func(t *testing.T) {
			routes := make([]int, sequenceSpokeCount)
			initial := sequenceWiring(0, routes)
			config := configForSequenceWiring(
				t,
				filepath.Join(t.TempDir(), "sequence"),
				initial,
			)
			if shortened {
				config = configWithLongRuntime(t, config)
			}
			cancel, result := startTestProxy(t, config)
			stopped := false
			t.Cleanup(func() {
				if stopped {
					return
				}
				cancel()
				<-result
			})

			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			live := captureWiringSocketIdentities(t, config.RuntimeDir, initial)
			current := initial
			random := rand.New(rand.NewSource(0xDCC021))
			additions, removals, relinks := 0, 0, 0

			for step := 0; step < sequenceStepCount; step++ {
				mask, targetRoutes := sequenceTarget(step, random)
				target := sequenceWiring(mask, targetRoutes)
				beforeEndpoints := wiringEndpointSet(current)
				afterEndpoints := wiringEndpointSet(target)
				for key := range afterEndpoints {
					if _, retained := beforeEndpoints[key]; !retained {
						additions++
					}
				}
				for key := range beforeEndpoints {
					if _, retained := afterEndpoints[key]; !retained {
						removals++
					}
				}
				if equalStringSets(beforeEndpoints, afterEndpoints) &&
					!equalLinkSets(current, target) {
					relinks++
				}

				digest, err := target.Digest()
				if err != nil {
					t.Fatal(err)
				}
				status, err := manager.Resync(
					context.Background(), process, target, digest,
				)
				if err != nil {
					t.Fatalf("step %d Resync: %v", step, err)
				}
				if !status.Ready || status.Digest != digest {
					t.Fatalf("step %d status = %#v", step, status)
				}

				next := assertWiringAndOwnershipModel(
					t, config, target, current, live,
				)
				if step%11 == 0 {
					status, err := manager.Resync(
						context.Background(), process, target, digest,
					)
					if err != nil {
						t.Fatalf("step %d idempotent Resync: %v", step, err)
					}
					if !status.Ready || status.Digest != digest {
						t.Fatalf("step %d idempotent status = %#v", step, status)
					}
					assertSocketIdentityMapsEqual(t, next, captureWiringSocketIdentities(
						t, config.RuntimeDir, target,
					))
				}
				current = target
				live = next
			}
			if additions == 0 || removals == 0 || relinks == 0 {
				t.Fatalf(
					"sequence coverage additions=%d removals=%d relinks=%d",
					additions, removals, relinks,
				)
			}

			cancel()
			if err := <-result; err != nil {
				t.Fatalf("proxy Run: %v", err)
			}
			stopped = true
		})
	}
}

func sequenceTarget(step int, random *rand.Rand) (uint16, []int) {
	routes := make([]int, sequenceSpokeCount)
	switch step {
	case 0:
		return (1 << sequenceSpokeCount) - 1, routes
	case 1:
		for index := range routes {
			routes[index] = (index + 1) % sequenceProviderCount
		}
		return (1 << sequenceSpokeCount) - 1, routes
	case 2:
		return 0x555, routes
	case 3:
		for index := range routes {
			routes[index] = index % sequenceProviderCount
		}
		return (1 << sequenceSpokeCount) - 1, routes
	default:
		for index := range routes {
			routes[index] = random.Intn(sequenceProviderCount)
		}
		return uint16(random.Intn(1 << sequenceSpokeCount)), routes
	}
}

func sequenceWiring(mask uint16, routes []int) Wiring {
	wiring := Wiring{}
	for provider := 0; provider < sequenceProviderCount; provider++ {
		wiring.Endpoints = append(wiring.Endpoints, EndpointIdentity{
			Component: fmt.Sprintf("provider-%d", provider),
			Name:      "stream",
			Direction: DirectionOutput,
		})
	}
	for spoke := 0; spoke < sequenceSpokeCount; spoke++ {
		if mask&(1<<spoke) == 0 {
			continue
		}
		component := fmt.Sprintf("spoke-%02d", spoke)
		wiring.Endpoints = append(wiring.Endpoints, EndpointIdentity{
			Component: component,
			Name:      "stream",
			Direction: DirectionInput,
		})
		wiring.Links = append(wiring.Links, Link{
			InputComponent: component,
			InputEndpoint:  "stream",
			OutputComponent: fmt.Sprintf(
				"provider-%d", routes[spoke]%sequenceProviderCount,
			),
			OutputEndpoint: "stream",
		})
	}
	wiring.canonicalize()
	return wiring
}

func configForSequenceWiring(
	t *testing.T,
	runtimeDir string,
	wiring Wiring,
) Config {
	t.Helper()
	digest, err := wiring.Digest()
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Version: ConfigVersion, System: "sequence", InstanceID: "sequence-instance",
		RuntimeDir: runtimeDir, Digest: digest,
		Links: append([]Link(nil), wiring.Links...),
	}
	for _, endpoint := range wiring.Endpoints {
		config.Endpoints = append(config.Endpoints, Endpoint{
			Component: endpoint.Component,
			Name:      endpoint.Name,
			Direction: endpoint.Direction,
			Socket: HostSocket(
				runtimeDir, endpoint.Direction, endpoint.Component, endpoint.Name,
			),
		})
	}
	config.canonicalize()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

func assertWiringAndOwnershipModel(
	t *testing.T,
	config Config,
	target Wiring,
	previous Wiring,
	previousIdentities map[string]artifactIdentity,
) map[string]artifactIdentity {
	t.Helper()
	expected := wiringSocketPaths(config.RuntimeDir, target)
	previousPaths := wiringSocketPaths(config.RuntimeDir, previous)
	next := make(map[string]artifactIdentity, len(expected))
	for key, path := range expected {
		identity, err := artifactIdentityAt(path)
		if err != nil {
			t.Fatalf("stat target endpoint %s: %v", key, err)
		}
		if before, retained := previousIdentities[key]; retained && before != identity {
			t.Fatalf("retained endpoint %s changed inode: %#v -> %#v", key, before, identity)
		}
		next[key] = identity
	}
	for key, path := range previousPaths {
		if _, retained := expected[key]; retained {
			continue
		}
		record := ownershipRecordForFinal(
			config.RuntimeDir, config.InstanceID, path, socketAnchored,
		)
		for _, artifact := range socketOwnershipAllPaths(record) {
			if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf(
					"removed endpoint %s left artifact at %s: %v",
					key, artifact, err,
				)
			}
		}
	}

	ledger, err := loadSocketOwnershipLedger(
		filepath.Join(config.RuntimeDir, socketOwnershipFileName),
		config.RuntimeDir,
		config.InstanceID,
	)
	if err != nil {
		t.Fatal(err)
	}
	expectedFinals := make(map[string]struct{}, len(expected)+1)
	expectedFinals[controlSocketFile(config.RuntimeDir)] = struct{}{}
	for _, path := range expected {
		expectedFinals[path] = struct{}{}
	}
	if len(ledger.records) != len(expectedFinals) {
		t.Fatalf("ledger records = %d, want %d", len(ledger.records), len(expectedFinals))
	}
	expectedAnchors := make(map[string]struct{}, len(expectedFinals))
	for final := range expectedFinals {
		record, exists := ledger.records[final]
		if !exists {
			t.Fatalf("ledger has no claim for live socket %s", final)
		}
		if record.State != socketAnchored {
			t.Fatalf("live socket %s state = %q", final, record.State)
		}
		owned, err := socketOwnershipOwnsPath(record, final)
		if err != nil {
			t.Fatal(err)
		}
		if !owned {
			t.Fatalf("live socket %s does not match its anchor", final)
		}
		actualOwned, err := socketOwnershipOwnsPath(record, record.Actual)
		if err != nil {
			t.Fatal(err)
		}
		if !actualOwned {
			t.Fatalf("live socket %s actual path does not match its anchor", final)
		}
		if _, err := os.Lstat(record.Temporary); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("live socket %s retains temporary path %s: %v", final, record.Temporary, err)
		}
		expectedAnchors[record.Anchor] = struct{}{}
	}
	entries, err := os.ReadDir(config.RuntimeDir)
	if err != nil {
		t.Fatal(err)
	}
	seenAnchors := 0
	for _, entry := range entries {
		path := filepath.Join(config.RuntimeDir, entry.Name())
		if strings.HasPrefix(entry.Name(), ".a-") {
			seenAnchors++
			if _, expected := expectedAnchors[path]; !expected {
				t.Fatalf("unledgered ownership anchor remains at %s", path)
			}
		}
		if strings.HasPrefix(entry.Name(), ".proxy-tmp-") {
			t.Fatalf("temporary metadata remains at %s", path)
		}
	}
	if seenAnchors != len(expectedAnchors) {
		t.Fatalf("ownership anchors = %d, want %d", seenAnchors, len(expectedAnchors))
	}
	for _, directory := range []string{"in", "out"} {
		entries, err := os.ReadDir(filepath.Join(config.RuntimeDir, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".p-") {
				t.Fatalf("temporary socket publication remains in %s", directory)
			}
		}
	}
	return next
}

func captureWiringSocketIdentities(
	t *testing.T,
	runtimeDir string,
	wiring Wiring,
) map[string]artifactIdentity {
	t.Helper()
	result := make(map[string]artifactIdentity, len(wiring.Endpoints))
	for key, path := range wiringSocketPaths(runtimeDir, wiring) {
		identity, err := artifactIdentityAt(path)
		if err != nil {
			t.Fatalf("stat endpoint %s: %v", key, err)
		}
		result[key] = identity
	}
	return result
}

func wiringSocketPaths(runtimeDir string, wiring Wiring) map[string]string {
	result := make(map[string]string, len(wiring.Endpoints))
	for _, endpoint := range wiring.Endpoints {
		result[endpointIdentityKey(endpoint)] = HostSocket(
			runtimeDir, endpoint.Direction, endpoint.Component, endpoint.Name,
		)
	}
	return result
}

func wiringEndpointSet(wiring Wiring) map[string]struct{} {
	result := make(map[string]struct{}, len(wiring.Endpoints))
	for _, endpoint := range wiring.Endpoints {
		result[endpointIdentityKey(endpoint)] = struct{}{}
	}
	return result
}

func equalStringSets(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, exists := right[key]; !exists {
			return false
		}
	}
	return true
}

func equalLinkSets(left, right Wiring) bool {
	if len(left.Links) != len(right.Links) {
		return false
	}
	leftKeys := make(map[string]struct{}, len(left.Links))
	for _, link := range left.Links {
		leftKeys[linkKey(link)] = struct{}{}
	}
	for _, link := range right.Links {
		if _, exists := leftKeys[linkKey(link)]; !exists {
			return false
		}
	}
	return true
}

func assertSocketIdentityMapsEqual(
	t *testing.T,
	want,
	got map[string]artifactIdentity,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("socket identity count = %d, want %d", len(got), len(want))
	}
	for key, identity := range want {
		if got[key] != identity {
			t.Fatalf("socket identity %s = %#v, want %#v", key, got[key], identity)
		}
	}
}
