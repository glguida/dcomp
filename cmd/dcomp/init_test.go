package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/glguida/dcomp/state"
)

func TestInitIsIdempotentAndDoesNotContactDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "invalid://must-not-be-used")
	root := filepath.Join(t.TempDir(), "state with ' quotes")
	output, code := captureStdout(t, func() int { return run([]string{"init", root}) })
	if code != 0 || !strings.Contains(output, "export DCOMP_STATE_ROOT=") {
		t.Fatalf("init: %d %s", code, output)
	}
	saved := filepath.Join(root, "keep")
	if err := os.WriteFile(saved, []byte("existing state"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"init", root}); code != 0 {
		t.Fatalf("repeat init: %d", code)
	}
	if data, err := os.ReadFile(saved); err != nil || string(data) != "existing state" {
		t.Fatalf("state changed: %s %v", data, err)
	}
	for _, path := range []string{root, filepath.Join(root, "systems"), filepath.Join(root, "run")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private directory %s: %v %v", path, info, err)
		}
	}
}

func TestInitGroupAppliesToFutureStateWrites(t *testing.T) {
	group, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "shared")
	if code := run([]string{"init", root, "--group", group.Name}); code != 0 {
		t.Fatalf("group init: %d", code)
	}
	store := state.Store{Root: root}
	lock, err := store.Acquire("demo")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := store.BindEngine("test-engine"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "systems"), filepath.Join(root, "systems/demo"), filepath.Join(root, "run")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0770 || info.Mode()&os.ModeSetgid == 0 {
			t.Fatalf("shared directory %s: %v %v", path, info, err)
		}
	}
	for _, path := range []string{filepath.Join(root, "engine.json"), filepath.Join(root, "systems/demo/lock")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0660 {
			t.Fatalf("shared file %s: %v %v", path, info, err)
		}
	}
	if code := run([]string{"init", "--group", group.Name, root}); code != 0 {
		t.Fatalf("repeat group init: %d", code)
	}
}

func TestInitRejectsUnknownGroupBeforeCreatingAnything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if code := run([]string{"init", root, "--group", "dcomp-group-that-does-not-exist"}); code == 0 {
		t.Fatal("unknown group accepted")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("created root: %v", err)
	}
}
