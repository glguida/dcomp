package proxy

import (
	"path/filepath"
	"testing"
)

func TestDefaultRuntimeRootIsInsideStateRoot(t *testing.T) {
	t.Setenv("DCOMP_RUNTIME_ROOT", "")
	stateRoot := filepath.Join(t.TempDir(), "state")

	got, err := DefaultRuntimeRoot(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateRoot, "run"); got != want {
		t.Fatalf("runtime root = %q, want %q", got, want)
	}
}

func TestDefaultRuntimeRootHonoursEnvironmentOverride(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	override := filepath.Join(t.TempDir(), "proxy")
	t.Setenv("DCOMP_RUNTIME_ROOT", override)

	got, err := DefaultRuntimeRoot(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != override {
		t.Fatalf("runtime root = %q, want override %q", got, override)
	}
}

func TestDefaultRuntimeRootRejectsInvalidRoots(t *testing.T) {
	t.Setenv("DCOMP_RUNTIME_ROOT", "")
	if _, err := DefaultRuntimeRoot("relative"); err == nil {
		t.Fatal("relative state root was accepted")
	}

	t.Setenv("DCOMP_RUNTIME_ROOT", "relative")
	if _, err := DefaultRuntimeRoot(t.TempDir()); err == nil {
		t.Fatal("relative runtime-root override was accepted")
	}
}
