package runtimecontract

import "testing"

func TestInterfaceContract(t *testing.T) {
	if !ValidEndpointName("model-provider") {
		t.Fatal("valid endpoint name was rejected")
	}
	for _, invalid := range []string{"", "Model", "1model", "model_provider"} {
		if ValidEndpointName(invalid) {
			t.Fatalf("invalid endpoint name %q was accepted", invalid)
		}
	}

	if got, want := InputEnvironment("model-provider"), "DCOMP_IN_MODEL_PROVIDER"; got != want {
		t.Fatalf("input environment = %q, want %q", got, want)
	}
	if got, want := OutputEnvironment("model-provider"), "DCOMP_OUT_MODEL_PROVIDER"; got != want {
		t.Fatalf("output environment = %q, want %q", got, want)
	}
	if got, want := InputSocket("model-provider"), "/run/dcomp/in/model-provider"; got != want {
		t.Fatalf("input socket = %q, want %q", got, want)
	}
	if got, want := OutputSocket("model-provider"), "/run/dcomp/out/model-provider"; got != want {
		t.Fatalf("output socket = %q, want %q", got, want)
	}
	if got, want := InputURI("model-provider"), "unix:///run/dcomp/in/model-provider"; got != want {
		t.Fatalf("input URI = %q, want %q", got, want)
	}
	if got, want := OutputURI("model-provider"), "unix:///run/dcomp/out/model-provider"; got != want {
		t.Fatalf("output URI = %q, want %q", got, want)
	}
}
