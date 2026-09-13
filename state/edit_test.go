package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/proxy"
)

func TestWaitingExclusiveLockCanBeCancelled(t *testing.T) {
	store := Store{Root: t.TempDir()}
	lock, err := store.Acquire("demo")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := store.AcquireContext(ctx, "demo"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := store.Acquire("demo")
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestFormatFourDesiredStateCanBeReadAndUpgraded(t *testing.T) {
	store := Store{Root: t.TempDir()}
	runtime := t.TempDir()
	deployment := Deployment{Version: 4, Spec: composition.ResolvedSpec{Name: "demo"}, RuntimeRoot: runtime,
		Networks: map[string]Resource{}, Containers: map[string]Resource{},
		Proxy: &proxy.Process{InstanceID: "test", Digest: "sha256:test", PID: 1,
			RuntimeDir: filepath.Join(runtime, "demo"), Control: proxy.ControlSocket(filepath.Join(runtime, "demo")), Log: filepath.Join(runtime, "demo", proxy.LogFileName)}}
	directory := filepath.Join(store.Root, "systems", "demo")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "desired.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	read, exists, err := store.ReadDesired("demo")
	if err != nil || !exists || read.Version != 4 {
		t.Fatalf("read old state: %#v %v", read, err)
	}
	read.Spec.Globals = []composition.Global{{Name: "provider_endpoint", Service: "example.Provider"}}
	if err := store.WriteDesired("demo", read); err != nil {
		t.Fatal(err)
	}
	read, _, err = store.ReadDesired("demo")
	if err != nil || read.Version != 5 || len(read.Spec.Globals) != 1 {
		t.Fatalf("upgraded state: %#v %v", read, err)
	}
}
