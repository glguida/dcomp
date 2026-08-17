package dockerengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
)

func TestContainerAttachPreservesThreeStandardStreams(t *testing.T) {
	inputReceived := make(chan string, 1)
	client := newUnixDockerClient(t, dockerHandler(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/v1.47/containers/"+testContainerID+"/attach" {
			t.Errorf("unexpected attach request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		wantQuery := url.Values{
			"logs": {"0"}, "stream": {"1"}, "stdin": {"1"},
			"stdout": {"1"}, "stderr": {"1"},
		}
		if !reflect.DeepEqual(request.URL.Query(), wantQuery) {
			t.Errorf("attach query = %#v, want %#v", request.URL.Query(), wantQuery)
		}
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack fake Docker connection: %v", err)
			return
		}
		defer connection.Close()
		if _, err := fmt.Fprint(
			buffered,
			"HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n",
		); err != nil {
			t.Errorf("write attach response: %v", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Errorf("flush attach response: %v", err)
			return
		}
		rawInput := make([]byte, len("host input\n"))
		if _, err := io.ReadFull(buffered, rawInput); err != nil {
			t.Errorf("read attached stdin: %v", err)
			return
		}
		inputReceived <- string(rawInput)
		if _, err := buffered.Write(dockerLogFrame(1, []byte("component stdout\n"))); err != nil {
			t.Errorf("write attached stdout: %v", err)
			return
		}
		if _, err := buffered.Write(dockerLogFrame(2, []byte("component stderr\n"))); err != nil {
			t.Errorf("write attached stderr: %v", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Errorf("flush attached output: %v", err)
		}
	}))

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	readyCalls := 0
	err := client.ContainerAttach(
		context.Background(),
		testContainerID,
		engine.AttachOptions{
			Stdin:  strings.NewReader("host input\n"),
			Stdout: &stdout,
			Stderr: &stderr,
			Ready: func() error {
				readyCalls++
				return nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-inputReceived; got != "host input\n" {
		t.Fatalf("attached stdin = %q", got)
	}
	if stdout.String() != "component stdout\n" {
		t.Fatalf("attached stdout = %q", stdout.String())
	}
	if stderr.String() != "component stderr\n" {
		t.Fatalf("attached stderr = %q", stderr.String())
	}
	if readyCalls != 1 {
		t.Fatalf("ready callback called %d times", readyCalls)
	}
}

func TestContainerAttachCancellationInterruptsUpgradeHandshake(t *testing.T) {
	requestReceived := make(chan struct{})
	releaseHandler := make(chan struct{})
	defer close(releaseHandler)
	client := newUnixDockerClient(t, dockerHandler(func(
		_writer http.ResponseWriter,
		_request *http.Request,
	) {
		close(requestReceived)
		<-releaseHandler
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- client.ContainerAttach(
			ctx,
			testContainerID,
			engine.AttachOptions{Stdout: io.Discard},
		)
	}()
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("fake Docker did not receive the attach request")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("attach handshake cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("attach handshake did not stop after context cancellation")
	}
}

func TestContainerAttachCancellationClosesHijackedConnection(t *testing.T) {
	closed := make(chan struct{})
	client := newUnixDockerClient(t, dockerHandler(func(
		writer http.ResponseWriter,
		_request *http.Request,
	) {
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack fake Docker connection: %v", err)
			return
		}
		defer connection.Close()
		_, _ = fmt.Fprint(
			buffered,
			"HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n",
		)
		_ = buffered.Flush()
		_, _ = buffered.ReadByte()
		close(closed)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	err := client.ContainerAttach(
		ctx,
		testContainerID,
		engine.AttachOptions{
			Stdout: io.Discard,
			Ready: func() error {
				cancel()
				return nil
			},
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("attach cancellation error = %v", err)
	}
	<-closed
}

func TestContainerAttachKeepsOutputOpenAfterInputEOF(t *testing.T) {
	serverSawInput := make(chan struct{})
	allowOutput := make(chan struct{})
	client := newUnixDockerClient(t, dockerHandler(func(
		writer http.ResponseWriter,
		_request *http.Request,
	) {
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack fake Docker connection: %v", err)
			return
		}
		defer connection.Close()
		_, _ = fmt.Fprint(
			buffered,
			"HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n",
		)
		_ = buffered.Flush()
		input := make([]byte, len("done\n"))
		if _, err := io.ReadFull(buffered, input); err != nil {
			t.Errorf("read attached stdin: %v", err)
			return
		}
		close(serverSawInput)
		<-allowOutput
		_, _ = buffered.Write(dockerLogFrame(1, []byte("still attached\n")))
		_ = buffered.Flush()
	}))

	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- client.ContainerAttach(
			context.Background(),
			testContainerID,
			engine.AttachOptions{
				Stdin: strings.NewReader("done\n"), Stdout: &stdout,
			},
		)
	}()
	<-serverSawInput
	select {
	case err := <-done:
		t.Fatalf("attachment ended with stdin: %v", err)
	default:
	}
	close(allowOutput)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "still attached\n" {
		t.Fatalf("attached stdout after input EOF = %q", stdout.String())
	}
}

func TestContainerAttachRejectsInvalidRequestsBeforeDockerAccess(t *testing.T) {
	requests := 0
	client := newUnixDockerClient(t, dockerHandler(func(
		writer http.ResponseWriter,
		_request *http.Request,
	) {
		requests++
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	}))

	tests := []struct {
		name    string
		id      string
		options engine.AttachOptions
	}{
		{name: "short ID", id: "abc", options: engine.AttachOptions{Stdout: io.Discard}},
		{name: "no streams", id: testContainerID, options: engine.AttachOptions{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := client.ContainerAttach(
				context.Background(), test.id, test.options,
			); err == nil {
				t.Fatal("invalid attachment unexpectedly succeeded")
			}
		})
	}
	if requests != 0 {
		t.Fatalf("invalid attachments made %d Docker requests", requests)
	}
}
