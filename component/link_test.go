package component

import (
	"os"
	"testing"
)

func TestEndpointEnv(t *testing.T) {
	t.Parallel()

	input, err := InputEnv("model-provider")
	if err != nil {
		t.Fatalf("InputEnv: %v", err)
	}
	if want := "DCOMP_IN_MODEL_PROVIDER"; input != want {
		t.Fatalf("InputEnv = %q, want %q", input, want)
	}
	output, err := OutputEnv("model-provider")
	if err != nil {
		t.Fatalf("OutputEnv: %v", err)
	}
	if want := "DCOMP_OUT_MODEL_PROVIDER"; output != want {
		t.Fatalf("OutputEnv = %q, want %q", output, want)
	}
}

func TestEndpointEnvRejectsAmbiguousNames(t *testing.T) {
	t.Parallel()

	for _, slot := range []string{"", "UPSTREAM", "two_words", "1upstream", "with.dot"} {
		if _, err := InputEnv(slot); err == nil {
			t.Errorf("InputEnv(%q) unexpectedly succeeded", slot)
		}
	}
}

func TestEndpointTargets(t *testing.T) {
	const target = "unix:///run/dcomp/in/upstream"
	t.Setenv("DCOMP_IN_UPSTREAM", target)

	got, err := InputTarget("upstream")
	if err != nil {
		t.Fatalf("InputTarget: %v", err)
	}
	if got != target {
		t.Fatalf("InputTarget = %q, want %q", got, target)
	}
}

func TestEndpointTargetRequiresUnixValue(t *testing.T) {
	_ = os.Unsetenv("DCOMP_OUT_MISSING")
	if _, err := OutputTarget("missing"); err == nil {
		t.Fatal("OutputTarget unexpectedly succeeded")
	}
	for _, target := range []string{
		" unix:///run/dcomp/out/missing",
		"dns:///old:50051",
		"unix:relative.sock",
		"unix:/run/dcomp/out/missing",
		"unix:////run/dcomp/out/missing",
		"unix://host/run/dcomp/out/missing",
		"unix:///run/dcomp/out/../missing",
		"unix:///run/dcomp/out/%6dissing",
		"unix:///run/dcomp/out/missing?query=yes",
		"unix:///run/dcomp/out/missing#fragment",
	} {
		t.Setenv("DCOMP_OUT_MISSING", target)
		if _, err := OutputTarget("missing"); err == nil {
			t.Errorf("OutputTarget accepted invalid target %q", target)
		}
	}
}
