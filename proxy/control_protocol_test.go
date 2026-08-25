package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type controlLimitProbe struct {
	Payload string `json:"payload"`
}

func TestControlMessageSizeBoundaryIsSymmetric(t *testing.T) {
	empty, err := json.Marshal(controlLimitProbe{})
	if err != nil {
		t.Fatal(err)
	}
	payloadLength := maxControlMessage - 1 - len(empty)
	exact := controlLimitProbe{Payload: strings.Repeat("x", payloadLength)}
	encoded, err := json.Marshal(exact)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(encoded) + 1; got != maxControlMessage {
		t.Fatalf("exact boundary fixture is %d bytes, want %d", got, maxControlMessage)
	}

	var wire bytes.Buffer
	if err := writeControlMessage(&wire, exact); err != nil {
		t.Fatalf("write exact-limit message: %v", err)
	}
	if wire.Len() != maxControlMessage {
		t.Fatalf("wire message is %d bytes, want %d", wire.Len(), maxControlMessage)
	}
	var decoded controlLimitProbe
	if err := readControlMessage(&wire, &decoded); err != nil {
		t.Fatalf("read exact-limit message: %v", err)
	}
	if decoded.Payload != exact.Payload {
		t.Fatal("exact-limit message changed during round trip")
	}

	over := controlLimitProbe{Payload: exact.Payload + "x"}
	wire.Reset()
	if err := writeControlMessage(&wire, over); err == nil {
		t.Fatal("writer accepted a message one byte over the limit")
	}
	if wire.Len() != 0 {
		t.Fatalf("rejected oversized write emitted %d bytes", wire.Len())
	}
	overEncoded, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	overWire := append(overEncoded, '\n')
	if err := readControlMessage(bytes.NewReader(overWire), &decoded); err == nil {
		t.Fatal("reader accepted a message one byte over the limit")
	}
	if err := readControlMessage(bytes.NewReader(encoded), &decoded); err == nil {
		t.Fatal("reader accepted an exact-limit message without newline framing")
	}
}
