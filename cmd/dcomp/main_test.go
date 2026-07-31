package main

import "testing"

func TestHelpSucceeds(t *testing.T) {
	t.Setenv("DCOMP_STATE_ROOT", "invalid-relative-default")
	if code := run([]string{"-h"}); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
}

func TestRelativeStateRootIsRejectedBeforeDockerAccess(t *testing.T) {
	if code := run([]string{"--state-root", "relative", "status", "demo"}); code != 2 {
		t.Fatalf("relative state-root exit code = %d, want 2", code)
	}
}
