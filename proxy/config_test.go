package proxy

import (
	"encoding/json"
	"testing"
)

func TestLoadConfigAcceptsVersionOneForUpgradeCleanup(t *testing.T) {
	config := testConfig(t, false)
	config.Version = 1
	digest, err := config.computeDigest()
	if err != nil {
		t.Fatal(err)
	}
	config.Digest = digest
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(encoded)
	if err != nil {
		t.Fatalf("load version-1 config: %v", err)
	}
	if loaded.Version != 1 || loaded.Digest != digest {
		t.Fatalf("loaded config = %#v", loaded)
	}
}
