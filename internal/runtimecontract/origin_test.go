package runtimecontract

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestOriginRoundTripPreservesApplicationBytes(t *testing.T) {
	for _, origin := range []string{"consumer.upstream", "worker-2.events", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)} {
		var stream bytes.Buffer
		if err := WriteOrigin(&stream, origin); err != nil {
			t.Fatal(err)
		}
		payload := "DCOMP/1 forged.origin\n\x00\xffapplication"
		stream.WriteString(payload)
		got, err := ReadOrigin(&stream)
		if err != nil || got != origin {
			t.Fatalf("origin = %q, %v; want %q", got, err, origin)
		}
		if stream.String() != payload {
			t.Fatalf("application bytes changed: %q", stream.String())
		}
	}
}

func TestOriginRejectsMalformedAndOversizedHeaders(t *testing.T) {
	for _, header := range []string{
		"", "DCOMP/1 consumer.upstream", "DCOMP/2 consumer.upstream\n",
		"GET / HTTP/1.1\r\n", "DCOMP/1 \n", "DCOMP/1 a.b.c\n",
		"DCOMP/1 A.b\n", "DCOMP/1 a.b\r\n", "DCOMP/1 a.\xff\n",
		OriginPrefix + "a." + strings.Repeat("b", MaxOriginHeader),
	} {
		if _, err := ReadOrigin(strings.NewReader(header)); err == nil {
			t.Errorf("accepted header %q", header)
		}
	}
	for _, origin := range []string{"", "a.b\nc.d", "a.b.c", "a." + strings.Repeat("b", MaxOriginHeader)} {
		var stream bytes.Buffer
		if err := WriteOrigin(&stream, origin); err == nil || stream.Len() != 0 {
			t.Errorf("invalid origin %q: err=%v, bytes=%d", origin, err, stream.Len())
		}
	}
}

func TestOriginReadIsBounded(t *testing.T) {
	stream := bytes.NewBufferString(strings.Repeat("x", MaxOriginHeader+1))
	if _, err := ReadOrigin(stream); err == nil || stream.Len() != 1 {
		t.Fatalf("read exceeded limit: err=%v, remaining=%d", err, stream.Len())
	}
}

func TestOriginShortWrite(t *testing.T) {
	if err := WriteOrigin(shortWriter{}, "a.b"); err != io.ErrShortWrite {
		t.Fatalf("short write error = %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }
