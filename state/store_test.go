package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/glguida/dcomp/composition"
)

func TestSystemsListsRecordedNamesWithoutCreatingState(t *testing.T) {
	store := Store{Root: t.TempDir()}
	if got, err := store.Systems(); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("empty systems = %#v", got)
	}
	for _, name := range []string{"zeta", "alpha"} {
		lock, err := store.Acquire(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Systems()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("systems = %#v, want %#v", got, want)
	}
}

func TestSystemsRejectsUnexpectedStateEntries(t *testing.T) {
	store := Store{Root: t.TempDir()}
	directory := filepath.Join(store.Root, "systems")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "foreign"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Systems(); err == nil ||
		!strings.Contains(err.Error(), "invalid entry") {
		t.Fatalf("Systems error = %v", err)
	}
}

func TestEngineBindingIsWriteOnce(t *testing.T) {
	store := Store{Root: t.TempDir()}
	start := make(chan struct{})
	ids := []string{"engine-a", "engine-b"}
	errorsByID := make(map[string]error)
	var mutex sync.Mutex
	var wait sync.WaitGroup
	for _, id := range ids {
		id := id
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			err := store.BindEngine(id)
			mutex.Lock()
			errorsByID[id] = err
			mutex.Unlock()
		}()
	}
	close(start)
	wait.Wait()

	bound, exists, err := store.ReadEngine()
	if err != nil {
		t.Fatal(err)
	}
	if !exists || (bound != "engine-a" && bound != "engine-b") {
		t.Fatalf("engine binding = %q, exists=%t", bound, exists)
	}
	for _, id := range ids {
		err := errorsByID[id]
		if id == bound {
			if err != nil {
				t.Fatalf("winning binding %q failed: %v", id, err)
			}
			if err := store.BindEngine(id); err != nil {
				t.Fatalf("idempotent binding failed: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "current engine") {
			t.Fatalf("competing binding %q error = %v", id, err)
		}
	}
	info, err := os.Stat(filepath.Join(store.Root, "engine.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("engine.json mode = %o", info.Mode().Perm())
	}
}

func TestAtomicStateRoundTrip(t *testing.T) {
	store := Store{Root: t.TempDir()}
	deployment := Deployment{
		Spec: composition.ResolvedSpec{Name: "demo", Digest: "sha256:test"},
		Networks: map[string]Resource{
			"component:echo": {
				ID:   "network-id",
				Name: "dcomp.demo.component.echo",
			},
		},
		Containers: map[string]Resource{
			"echo": {ID: "container-id", Name: "dcomp.demo.container.echo"},
		},
	}
	if err := store.WriteDesired("demo", deployment); err != nil {
		t.Fatal(err)
	}
	read, exists, err := store.ReadDesired("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || read.Networks["component:echo"].ID != "network-id" {
		t.Fatalf("unexpected desired state: exists=%v state=%+v", exists, read)
	}
	mode, err := os.Stat(filepath.Join(store.Root, "systems", "demo", "desired.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode().Perm() != 0600 {
		t.Fatalf("desired.json mode = %o", mode.Mode().Perm())
	}
}

func TestKernelLockCannotBeStolen(t *testing.T) {
	store := Store{Root: t.TempDir()}
	first, err := store.Acquire("demo")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := store.Acquire("demo"); err == nil {
		t.Fatal("second lock unexpectedly succeeded")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Acquire("demo")
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	second.Close()
}

func TestRejectsTrailingOrUnknownState(t *testing.T) {
	store := Store{Root: t.TempDir()}
	directory := filepath.Join(store.Root, "systems", "demo")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "desired.json"),
		[]byte(`{"version":1,"unknown":true}{}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadDesired("demo"); err == nil {
		t.Fatal("invalid state was accepted")
	}
}

func TestRejectsStateStoredUnderAnotherComposition(t *testing.T) {
	store := Store{Root: t.TempDir()}
	deployment := Deployment{
		Spec:       composition.ResolvedSpec{Name: "other", Digest: "sha256:test"},
		Networks:   map[string]Resource{},
		Containers: map[string]Resource{},
	}
	if err := store.WriteDesired("demo", deployment); err == nil {
		t.Fatal("cross-composition desired state was written")
	}

	operation, err := NewOperation("apply", "network", deployment.Spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteOperation("demo", operation); err == nil {
		t.Fatal("cross-composition operation state was written")
	}
}
