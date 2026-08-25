package proxy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRequiresCurrentVersion(t *testing.T) {
	for _, version := range []int{1, ConfigVersion + 1} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			config := testConfig(t, false)
			config.Version = version
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(encoded); err == nil ||
				!strings.Contains(err.Error(), "unsupported proxy config version") {
				t.Fatalf("LoadConfig version %d error = %v", version, err)
			}
		})
	}
}

func TestWiringDigestExcludesProcessAndRuntimeIdentity(t *testing.T) {
	first := testConfig(t, true)
	second := first
	second.System = "another"
	second.InstanceID = "another-instance"
	second.RuntimeDir = filepath.Join(t.TempDir(), "another")
	second.Endpoints = append([]Endpoint(nil), first.Endpoints...)
	for index := range second.Endpoints {
		endpoint := &second.Endpoints[index]
		endpoint.Socket = HostSocket(
			second.RuntimeDir, endpoint.Direction, endpoint.Component, endpoint.Name,
		)
	}
	second.Digest = ""
	digest, err := second.computeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != first.Digest {
		t.Fatalf("runtime identity changed wiring digest: %s != %s", digest, first.Digest)
	}
}

func TestWiringDigestCanonicalizesSetOrder(t *testing.T) {
	config := testConfig(t, true)
	wiring := config.Wiring()
	for left, right := 0, len(wiring.Endpoints)-1; left < right; left, right = left+1, right-1 {
		wiring.Endpoints[left], wiring.Endpoints[right] = wiring.Endpoints[right], wiring.Endpoints[left]
	}
	for left, right := 0, len(wiring.Links)-1; left < right; left, right = left+1, right-1 {
		wiring.Links[left], wiring.Links[right] = wiring.Links[right], wiring.Links[left]
	}
	digest, err := wiring.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != config.Digest {
		t.Fatalf("declaration order changed wiring digest: %s != %s", digest, config.Digest)
	}
}
