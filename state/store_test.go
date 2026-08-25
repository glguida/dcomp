package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/proxy"
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
	runtimeRoot := t.TempDir()
	deployment := Deployment{
		Spec:        composition.ResolvedSpec{Name: "demo", Digest: "sha256:test"},
		RuntimeRoot: runtimeRoot,
		Proxy: &proxy.Process{
			InstanceID: "proxy-instance", Digest: "sha256:proxy", PID: 100,
			RuntimeDir: filepath.Join(runtimeRoot, "demo"),
			Control:    filepath.Join(runtimeRoot, "demo", proxy.ControlSocketName),
			Log:        filepath.Join(runtimeRoot, "demo", proxy.LogFileName),
		},
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

func TestWriteDesiredRejectsProxyOutsideRuntimeRoot(t *testing.T) {
	store := Store{Root: t.TempDir()}
	runtimeRoot := t.TempDir()
	runtimeDir := filepath.Join(runtimeRoot, "other-system")
	deployment := Deployment{
		Spec:        composition.ResolvedSpec{Name: "demo", Digest: "sha256:test"},
		RuntimeRoot: runtimeRoot,
		Proxy: &proxy.Process{
			InstanceID: "proxy-instance", Digest: "sha256:proxy", PID: 100,
			RuntimeDir: runtimeDir,
			Control:    proxy.ControlSocket(runtimeDir),
			Log:        filepath.Join(runtimeDir, proxy.LogFileName),
		},
		Networks: map[string]Resource{}, Containers: map[string]Resource{},
	}
	if err := store.WriteDesired("demo", deployment); err == nil ||
		!strings.Contains(err.Error(), "outside its runtime root") {
		t.Fatalf("WriteDesired error = %v", err)
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

	operation, err := NewOperation("apply", "network", deployment.Spec, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteOperation("demo", operation); err == nil {
		t.Fatal("cross-composition operation state was written")
	}
}

func TestNewOperationRequiresValidRuntimeRoot(t *testing.T) {
	target := composition.ResolvedSpec{Name: "demo", Digest: "sha256:test"}
	for _, root := range []string{"", "relative", "/tmp/../tmp"} {
		if _, err := NewOperation("apply", "network", target, nil, root); err == nil {
			t.Fatalf("NewOperation accepted invalid runtime root %q", root)
		}
	}
	root := t.TempDir()
	operation, err := NewOperation("apply", "network", target, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if operation.RuntimeRoot != root {
		t.Fatalf("runtime root = %q, want %q", operation.RuntimeRoot, root)
	}
}

func TestOperationRoundTripsEndpointCleanupJournal(t *testing.T) {
	store := Store{Root: t.TempDir()}
	operation, err := NewOperation(
		"apply",
		"retire",
		composition.ResolvedSpec{Name: "demo", Digest: "sha256:test"},
		nil,
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := EndpointCleanup{
		ContainerID: "container-id", ContainerName: "dcomp.demo.container.worker",
		NetworkKey: "component/worker", NetworkID: "network-id",
		NetworkName: "dcomp.demo.component.worker", EndpointID: "endpoint-id",
		EndpointName: "dcomp.demo.container.worker",
	}
	operation.EndpointCleanups[cleanup.EndpointID] = cleanup
	if err := store.WriteOperation("demo", operation); err != nil {
		t.Fatal(err)
	}
	read, exists, err := store.ReadOperation("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !reflect.DeepEqual(read.EndpointCleanups, operation.EndpointCleanups) {
		t.Fatalf("endpoint cleanup round trip = %#v", read.EndpointCleanups)
	}
}

func TestOperationJournalsAndValidatesCompleteTargetWiring(t *testing.T) {
	store := Store{Root: t.TempDir()}
	target := composition.ResolvedSpec{
		Name: "demo", Digest: "sha256:system",
		Components: []composition.ResolvedComponent{
			{Name: "source", Definition: composition.Definition{Outputs: []composition.Endpoint{{Name: "stream"}}}},
			{Name: "sink", Definition: composition.Definition{Inputs: []composition.Endpoint{{Name: "stream"}}}},
		},
		Links: []composition.Link{{
			Input:  composition.EndpointRef{Component: "sink", Endpoint: "stream"},
			Output: composition.EndpointRef{Component: "source", Endpoint: "stream"},
		}},
	}
	operation, err := NewOperation("apply", "retire", target, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(operation.TargetWiring.Endpoints) != 2 ||
		len(operation.TargetWiring.Links) != 1 ||
		operation.TargetWiringDigest == "" {
		t.Fatalf("operation did not journal complete wiring: %#v", operation)
	}
	if err := store.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}

	tampered := operation
	tampered.TargetWiring.Links = append([]proxy.Link(nil), operation.TargetWiring.Links...)
	tampered.TargetWiring.Links[0].OutputComponent = "intruder"
	if err := store.WriteOperation(target.Name, tampered); err == nil ||
		!strings.Contains(err.Error(), "target wiring") {
		t.Fatalf("tampered target wiring error = %v", err)
	}
}

func TestNonApplyOperationOmitsTargetWiring(t *testing.T) {
	target := composition.ResolvedSpec{
		Name: "demo", Digest: "sha256:system",
		Components: []composition.ResolvedComponent{{
			Name:       "source",
			Definition: composition.Definition{Outputs: []composition.Endpoint{{Name: "stream"}}},
		}},
	}
	for _, kind := range []string{"down", "restart"} {
		t.Run(kind, func(t *testing.T) {
			store := Store{Root: t.TempDir()}
			operation, err := NewOperation(kind, kind, target, nil, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if operation.TargetWiringDigest != "" ||
				len(operation.TargetWiring.Endpoints) != 0 ||
				len(operation.TargetWiring.Links) != 0 {
				t.Fatalf("%s operation contains target wiring: %#v", kind, operation.TargetWiring)
			}
			if err := store.WriteOperation(target.Name, operation); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(store.Root, "systems", target.Name, "operation.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"target_wiring":`) ||
				strings.Contains(string(data), `"target_wiring_digest":`) {
				t.Fatalf("%s journal contains target wiring: %s", kind, data)
			}
			read, exists, err := store.ReadOperation(target.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !exists || read.TargetWiringDigest != "" ||
				len(read.TargetWiring.Endpoints) != 0 || len(read.TargetWiring.Links) != 0 {
				t.Fatalf("read %s operation contains target wiring: %#v", kind, read)
			}
		})
	}
}

func TestApplyOperationRequiresCompleteTargetWiring(t *testing.T) {
	store := Store{Root: t.TempDir()}
	target := composition.ResolvedSpec{Name: "demo", Digest: "sha256:system"}
	operation, err := NewOperation("apply", "resync", target, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	operation.TargetWiring = proxy.Wiring{}
	operation.TargetWiringDigest = ""
	if err := store.WriteOperation(target.Name, operation); err == nil ||
		!strings.Contains(err.Error(), "target wiring") {
		t.Fatalf("WriteOperation missing target wiring error = %v", err)
	}
}

func TestRejectsPreviousReleaseStateVersions(t *testing.T) {
	t.Run("engine-binding", func(t *testing.T) {
		store := Store{Root: t.TempDir()}
		const previous = "{\"version\":1,\"id\":\"engine-id\"}\n"
		if err := os.WriteFile(
			filepath.Join(store.Root, "engine.json"),
			[]byte(previous),
			0600,
		); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ReadEngine(); err == nil ||
			!strings.Contains(err.Error(), "unsupported engine binding version 1") {
			t.Fatalf("ReadEngine previous version error = %v", err)
		}
		if err := store.BindEngine("engine-id"); err == nil ||
			!strings.Contains(err.Error(), "unsupported engine binding version 1") {
			t.Fatalf("BindEngine previous version error = %v", err)
		}
		data, err := os.ReadFile(filepath.Join(store.Root, "engine.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != previous {
			t.Fatalf("BindEngine rewrote previous binding: %s", data)
		}
	})

	for _, filename := range []string{"desired.json", "operation.json"} {
		t.Run(filename, func(t *testing.T) {
			store := Store{Root: t.TempDir()}
			directory := filepath.Join(store.Root, "systems", "demo")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(directory, filename), []byte("{\"version\":3}\n"), 0600,
			); err != nil {
				t.Fatal(err)
			}
			var err error
			if filename == "desired.json" {
				_, _, err = store.ReadDesired("demo")
			} else {
				_, _, err = store.ReadOperation("demo")
			}
			if err == nil || !strings.Contains(err.Error(), "unsupported") ||
				!strings.Contains(err.Error(), "version 3") {
				t.Fatalf("read %s previous version error = %v", filename, err)
			}
		})
	}
}

func TestOversizedStateWriteDoesNotReplaceReadableJournal(t *testing.T) {
	const documentLimit = 16 * 1024 * 1024
	store := Store{Root: t.TempDir()}
	target := composition.ResolvedSpec{
		Name: "demo", Digest: "sha256:small",
		Components: []composition.ResolvedComponent{{
			Name: "worker", ImageRef: "worker:v1", ImageID: "sha256:worker",
			Digest: "sha256:worker-definition",
		}},
	}
	operation, err := NewOperation("apply", "retire", target, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exact := operation
	exact.Target.Digest = "sha256:exact-limit"
	exact.Target.Components = append(
		[]composition.ResolvedComponent(nil), operation.Target.Components...,
	)
	exact.Target.Components[0].ImageRef = ""
	encoded, err := json.MarshalIndent(exact, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	payloadLength := documentLimit - len(encoded) - 1 // Encoder.Encode appends '\n'.
	if payloadLength <= 0 {
		t.Fatalf("operation fixture without payload is already %d bytes", len(encoded)+1)
	}
	exact.Target.Components[0].ImageRef = strings.Repeat("x", payloadLength)
	if err := store.WriteOperation(target.Name, exact); err != nil {
		t.Fatalf("write exact-limit operation: %v", err)
	}
	path := filepath.Join(store.Root, "systems", target.Name, "operation.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != documentLimit {
		t.Fatalf("exact-limit journal size = %d, want %d", info.Size(), documentLimit)
	}

	oversized := exact
	oversized.Target.Digest = "sha256:overx-limit"
	oversized.Target.Components = append(
		[]composition.ResolvedComponent(nil), exact.Target.Components...,
	)
	oversized.Target.Components[0].ImageRef += "x"
	if err := store.WriteOperation(target.Name, oversized); err == nil {
		t.Error("oversized operation replaced the durable journal")
	}

	read, exists, err := store.ReadOperation(target.Name)
	if err != nil {
		t.Fatalf("read journal after rejected oversized write: %v", err)
	}
	if !exists || read.Target.Digest != exact.Target.Digest {
		t.Fatalf(
			"journal after oversized write = exists %t digest %q, want %q",
			exists, read.Target.Digest, exact.Target.Digest,
		)
	}
}

func TestStateDocumentReadBoundary(t *testing.T) {
	const documentLimit = 16 * 1024 * 1024
	for _, test := range []struct {
		name      string
		extraByte int
		wantError bool
	}{
		{name: "exact-limit", extraByte: 0, wantError: false},
		{name: "one-byte-over", extraByte: 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := Store{Root: t.TempDir()}
			runtimeRoot := t.TempDir()
			runtimeDir := filepath.Join(runtimeRoot, "demo")
			deployment := Deployment{
				Spec: composition.ResolvedSpec{
					Name: "demo", Digest: "sha256:system",
				},
				RuntimeRoot: runtimeRoot,
				Proxy: &proxy.Process{
					InstanceID: "proxy-instance", Digest: "sha256:wiring", PID: 100,
					RuntimeDir: runtimeDir, Control: proxy.ControlSocket(runtimeDir),
					Log: filepath.Join(runtimeDir, proxy.LogFileName),
				},
				Networks: map[string]Resource{}, Containers: map[string]Resource{},
			}
			if err := store.WriteDesired("demo", deployment); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Root, "systems", "demo", "desired.json")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			padding := documentLimit + test.extraByte - int(info.Size())
			if padding < 0 {
				t.Fatalf("desired-state fixture is already %d bytes", info.Size())
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(strings.Repeat(" ", padding)); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}

			_, _, err = store.ReadDesired("demo")
			if test.wantError && err == nil {
				t.Fatal("state reader accepted a document one byte over the limit")
			}
			if !test.wantError && err != nil {
				t.Fatalf("read exact-limit state document: %v", err)
			}
		})
	}
}
