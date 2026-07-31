package dockerengine

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
)

const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestContainerLogsDecodesSnapshotLinesAndQuery(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet ||
			request.URL.Path != "/v1.47/containers/"+testContainerID+"/logs" {
			t.Errorf("unexpected log request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		wantQuery := url.Values{
			"follow": {"0"}, "stderr": {"1"}, "stdout": {"1"},
			"tail": {"all"}, "timestamps": {"1"},
		}
		if !reflect.DeepEqual(request.URL.Query(), wantQuery) {
			t.Errorf("log query = %#v, want %#v", request.URL.Query(), wantQuery)
		}
		writer.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		writeDockerLogFrame(t, writer, 1,
			"2026-07-30T08:00:00.000000001Z first\n"+
				"2026-07-30T08:00:01Z second\n")
		writeDockerLogFrame(t, writer, 2,
			"2026-07-30T08:00:02.5Z error\n")
		writeDockerLogFrame(t, writer, 1,
			"2026-07-30T08:00:03Z third\n")
	}))

	var got []engine.LogLine
	err := client.ContainerLogs(
		context.Background(),
		testContainerID,
		engine.LogOptions{},
		func(line engine.LogLine) error {
			got = append(got, line)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.LogLine{
		{
			Timestamp: mustParseTime(t, "2026-07-30T08:00:00.000000001Z"),
			Stream:    engine.LogStdout,
			Message:   "first",
		},
		{
			Timestamp: mustParseTime(t, "2026-07-30T08:00:01Z"),
			Stream:    engine.LogStdout,
			Message:   "second",
		},
		{
			Timestamp: mustParseTime(t, "2026-07-30T08:00:02.5Z"),
			Stream:    engine.LogStderr,
			Message:   "error",
		},
		{
			Timestamp: mustParseTime(t, "2026-07-30T08:00:03Z"),
			Stream:    engine.LogStdout,
			Message:   "third",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded log lines:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestContainerLogsAssemblesInterleavedFragmentsPerStream(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
		writeDockerLogFrame(t, writer, 1, "2026-07-30T08:00:00Z out-")
		writeDockerLogFrame(t, writer, 2, "2026-07-30T08:00:01Z err-")
		writeDockerLogFrame(t, writer, 1, "complete\n")
		writeDockerLogFrame(t, writer, 2, "complete\n")
		writeDockerLogFrame(t, writer, 1, "2026-07-")
		writeDockerLogFrame(t, writer, 1, "30T08:00:02Z split-timestamp\n")
	}))

	var got []engine.LogLine
	err := client.ContainerLogs(
		context.Background(), testContainerID, engine.LogOptions{},
		func(line engine.LogLine) error {
			got = append(got, line)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.LogLine{
		{
			Timestamp: mustParseTime(t, "2026-07-30T08:00:00Z"),
			Stream:    engine.LogStdout,
			Message:   "out-complete",
		},
		{
			Timestamp: mustParseTime(t, "2026-07-30T08:00:01Z"),
			Stream:    engine.LogStderr,
			Message:   "err-complete",
		},
		{
			Timestamp: mustParseTime(t, "2026-07-30T08:00:02Z"),
			Stream:    engine.LogStdout,
			Message:   "split-timestamp",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interleaved fragmented lines:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestContainerLogsFlushesUnterminatedFinalLine(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
		writeDockerLogFrame(t, writer, 2, "2026-07-30T08:00:00Z final")
	}))
	var got []engine.LogLine
	err := client.ContainerLogs(
		context.Background(), testContainerID, engine.LogOptions{},
		func(line engine.LogLine) error {
			got = append(got, line)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Stream != engine.LogStderr || got[0].Message != "final" {
		t.Fatalf("unterminated line = %#v", got)
	}
}

func TestContainerLogsHandlesLongLinesWithoutScannerLimit(t *testing.T) {
	const length = 512 * 1024
	message := strings.Repeat("x", length)
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
		writeDockerLogFrame(t, writer, 1, "2026-07-30T08:00:00Z "+message+"\n")
	}))
	var got engine.LogLine
	err := client.ContainerLogs(
		context.Background(), testContainerID, engine.LogOptions{},
		func(line engine.LogLine) error {
			got = line
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stream != engine.LogStdout || got.Message != message {
		t.Fatalf("long line decoded as stream=%q length=%d, want stdout length=%d",
			got.Stream, len(got.Message), length)
	}
}

func TestContainerLogsRejectsOversizedRecord(t *testing.T) {
	payload := "2026-07-30T08:00:00Z " +
		strings.Repeat("x", maxDockerLogRecordBytes)
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
		writeDockerLogFrame(t, writer, 1, payload)
	}))
	err := client.ContainerLogs(
		context.Background(), testContainerID, engine.LogOptions{},
		func(engine.LogLine) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "log record exceeds") {
		t.Fatalf("oversized log error = %v", err)
	}
}

func TestContainerLogsFollowStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("follow") != "1" {
			t.Errorf("follow query = %q, want 1", request.URL.Query().Get("follow"))
		}
		writeDockerLogFrame(t, writer, 1, "2026-07-30T08:00:00Z live\n")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Error("fake Docker response cannot flush")
			return
		}
		flusher.Flush()
		<-request.Context().Done()
	}))

	lines := 0
	err := client.ContainerLogs(
		ctx, testContainerID, engine.LogOptions{Follow: true},
		func(engine.LogLine) error {
			lines++
			cancel()
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("follow error = %v, want context.Canceled", err)
	}
	if lines != 1 {
		t.Fatalf("follow emitted %d lines, want 1", lines)
	}
}

func TestContainerLogsRejectsMalformedOrTruncatedMultiplexData(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "truncated header",
			payload: []byte{1, 0, 0},
			want:    "truncated Docker multiplex header",
		},
		{
			name: "reserved header bytes",
			payload: []byte{
				1, 1, 0, 0, 0, 0, 0, 0,
			},
			want: "reserved bytes are nonzero",
		},
		{
			name: "invalid stream",
			payload: []byte{
				3, 0, 0, 0, 0, 0, 0, 0,
			},
			want: "unsupported Docker multiplex stream 3",
		},
		{
			name: "truncated payload",
			payload: []byte{
				1, 0, 0, 0, 0, 0, 0, 5, 'a', 'b',
			},
			want: "truncated Docker multiplex payload",
		},
		{
			name:    "missing timestamp",
			payload: dockerLogFrame(1, []byte("message-only\n")),
			want:    "has no timestamp",
		},
		{
			name:    "invalid timestamp",
			payload: dockerLogFrame(1, []byte("yesterday message\n")),
			want:    "parse Docker log timestamp",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write(test.payload)
			}))
			err := client.ContainerLogs(
				context.Background(), testContainerID, engine.LogOptions{},
				func(engine.LogLine) error { return nil },
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ContainerLogs error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestContainerLogsPropagatesReceiverError(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
		writeDockerLogFrame(t, writer, 1, "2026-07-30T08:00:00Z line\n")
	}))
	injected := errors.New("receiver stopped")
	err := client.ContainerLogs(
		context.Background(), testContainerID, engine.LogOptions{},
		func(engine.LogLine) error { return injected },
	)
	if !errors.Is(err, injected) {
		t.Fatalf("ContainerLogs error = %v, want injected receiver error", err)
	}
}

func TestContainerLogsRejectsNamesAbbreviationsAndNilReceiverBeforeRequest(t *testing.T) {
	requests := 0
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	}))
	for _, id := range []string{"worker", "0123456789ab", strings.Repeat("A", 64)} {
		err := client.ContainerLogs(
			context.Background(), id, engine.LogOptions{},
			func(engine.LogLine) error { return nil },
		)
		if err == nil || !strings.Contains(err.Error(), "full immutable") {
			t.Fatalf("ContainerLogs(%q) error = %v", id, err)
		}
	}
	err := client.ContainerLogs(context.Background(), testContainerID, engine.LogOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "receiver is nil") {
		t.Fatalf("nil receiver error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("invalid calls made %d Docker requests, want none", requests)
	}
}

func TestContainerLogsPreservesDockerResponseErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{
			name:   "not found",
			status: http.StatusNotFound,
			check:  func(err error) bool { return errors.Is(err, engine.ErrNotFound) },
		},
		{
			name:   "server error",
			status: http.StatusServiceUnavailable,
			check: func(err error) bool {
				var apiError *APIError
				return errors.As(err, &apiError) &&
					apiError.StatusCode == http.StatusServiceUnavailable &&
					apiError.Message == "daemon unavailable"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
				writeJSON(t, writer, test.status, map[string]string{"message": "daemon unavailable"})
			}))
			err := client.ContainerLogs(
				context.Background(), testContainerID, engine.LogOptions{},
				func(engine.LogLine) error { return nil },
			)
			if !test.check(err) {
				t.Fatalf("ContainerLogs error = %T %v", err, err)
			}
		})
	}
}

func writeDockerLogFrame(t *testing.T, writer http.ResponseWriter, stream byte, payload string) {
	t.Helper()
	if _, err := writer.Write(dockerLogFrame(stream, []byte(payload))); err != nil {
		t.Errorf("write Docker log frame: %v", err)
	}
}

func dockerLogFrame(stream byte, payload []byte) []byte {
	frame := make([]byte, dockerLogHeaderSize+len(payload))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[dockerLogHeaderSize:], payload)
	return frame
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
