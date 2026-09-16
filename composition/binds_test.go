package composition

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBindSourcesAreCheckedWhenPreparingMounts(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "workspace")
	runtime := Runtime{Binds: []BindMount{{Source: directory, Target: "/workspace"}}}
	if err := ValidateRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBindSources(runtime); err == nil {
		t.Fatal("new mount accepted a missing path")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBindSources(runtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRuntime(runtime); err != nil {
		t.Fatalf("recorded mount became invalid: %v", err)
	}
	if err := os.Symlink(directory+"-moved", directory); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBindSources(runtime); err == nil {
		t.Fatal("new mount accepted a noncanonical path")
	}
}
