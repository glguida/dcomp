package component

import (
	"os"
	"testing"
)

func TestLinkEnv(t *testing.T) {
	t.Parallel()

	got, err := LinkEnv("model-provider")
	if err != nil {
		t.Fatalf("LinkEnv: %v", err)
	}
	if want := "DCOMP_LINK_MODEL_PROVIDER"; got != want {
		t.Fatalf("LinkEnv = %q, want %q", got, want)
	}
}

func TestLinkEnvRejectsAmbiguousNames(t *testing.T) {
	t.Parallel()

	for _, slot := range []string{"", "UPSTREAM", "two_words", "1upstream", "with.dot"} {
		if _, err := LinkEnv(slot); err == nil {
			t.Errorf("LinkEnv(%q) unexpectedly succeeded", slot)
		}
	}
}

func TestLinkTarget(t *testing.T) {
	const (
		env    = "DCOMP_LINK_UPSTREAM"
		target = "dns:///echo:50051"
	)
	old, existed := os.LookupEnv(env)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(env, old)
		} else {
			_ = os.Unsetenv(env)
		}
	})
	if err := os.Setenv(env, target); err != nil {
		t.Fatal(err)
	}

	got, err := LinkTarget("upstream")
	if err != nil {
		t.Fatalf("LinkTarget: %v", err)
	}
	if got != target {
		t.Fatalf("LinkTarget = %q, want %q", got, target)
	}
}

func TestLinkTargetRequiresValue(t *testing.T) {
	const env = "DCOMP_LINK_MISSING"
	old, existed := os.LookupEnv(env)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(env, old)
		}
	})
	_ = os.Unsetenv(env)

	if _, err := LinkTarget("missing"); err == nil {
		t.Fatal("LinkTarget unexpectedly succeeded")
	}
}
