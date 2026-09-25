package proxy

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestPendingDisconnectRemovesConnection(t *testing.T) {
	for _, direction := range []Direction{DirectionInput, DirectionOutput} {
		for _, buffered := range []bool{false, true} {
			t.Run(string(direction)+map[bool]string{false: "/idle", true: "/buffered"}[buffered], func(t *testing.T) {
				config := testConfig(t, false)
				cancel, result := startTestProxy(t, config)
				defer func() {
					cancel()
					if err := <-result; err != nil {
						t.Fatal(err)
					}
				}()
				manager := &ProcessManager{}
				process := processForConfig(config, os.Getpid())
				component := "source"
				wait := waitForPendingOutputs
				if direction == DirectionInput {
					component, wait = "filter", waitForPendingInputs
				}
				connection := dialUnix(t, HostSocket(config.RuntimeDir, direction, component, "documents"))
				wait(t, manager, process, 1)
				if buffered {
					if _, err := connection.Write([]byte("cancelled data")); err != nil {
						t.Fatal(err)
					}
				}
				connection.Close()
				// Cleanup must happen without another client arriving to drain the queue.
				wait(t, manager, process, 0)
				producer := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
				consumer := dialUnix(t, HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents"))
				defer producer.Close()
				defer consumer.Close()
				assertOrigin(t, producer, "filter.documents")
				writeAndRead(t, consumer, producer, "first live request")
				writeAndRead(t, producer, consumer, "first live response")
			})
		}
	}
}

func TestPendingHalfClosePreservesDataAndReply(t *testing.T) {
	for _, test := range []struct {
		direction Direction
		payload   string
	}{
		{DirectionInput, "queued bytes"}, {DirectionOutput, "queued bytes"},
		{DirectionInput, ""}, {DirectionOutput, ""},
	} {
		t.Run(string(test.direction)+"/"+test.payload, func(t *testing.T) {
			direction := test.direction
			config := testConfig(t, false)
			cancel, result := startTestProxy(t, config)
			defer func() {
				cancel()
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}()
			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			component, peer, opposite := "source", "filter", DirectionInput
			wait := waitForPendingOutputs
			if direction == DirectionInput {
				component, peer, opposite, wait = "filter", "source", DirectionOutput, waitForPendingInputs
			}
			waiting := dialUnix(t, HostSocket(config.RuntimeDir, direction, component, "documents"))
			defer waiting.Close()
			wait(t, manager, process, 1)
			if _, err := waiting.Write([]byte(test.payload)); err != nil {
				t.Fatal(err)
			}
			if err := waiting.(*net.UnixConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			// Allow the disconnect watcher to run before pairing this half-closed peer.
			time.Sleep(20 * time.Millisecond)
			other := dialUnix(t, HostSocket(config.RuntimeDir, opposite, peer, "documents"))
			defer other.Close()
			producer := waiting
			if direction == DirectionInput {
				producer = other
			}
			assertOrigin(t, producer, "filter.documents")
			other.SetReadDeadline(time.Now().Add(time.Second))
			got, err := io.ReadAll(other)
			if err != nil || string(got) != test.payload {
				t.Fatalf("queued payload = %q, %v", got, err)
			}
			writeAndRead(t, other, waiting, "reply after EOF")
			if err := other.(*net.UnixConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			waiting.SetReadDeadline(time.Now().Add(time.Second))
			if rest, err := io.ReadAll(waiting); err != nil || len(rest) != 0 {
				t.Fatalf("reply EOF = %q, %v", rest, err)
			}
			wait(t, manager, process, 0)
		})
	}
}

func TestPendingReplacementServesFirstClient(t *testing.T) {
	for _, direction := range []Direction{DirectionInput, DirectionOutput} {
		t.Run(string(direction), func(t *testing.T) {
			config := testConfig(t, false)
			cancel, result := startTestProxy(t, config)
			defer func() {
				cancel()
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}()
			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			component, peer, opposite := "source", "filter", DirectionInput
			wait := waitForPendingOutputs
			if direction == DirectionInput {
				component, peer, opposite, wait = "filter", "source", DirectionOutput, waitForPendingInputs
			}
			for round := 0; round < 30; round++ {
				stale := dialUnix(t, HostSocket(config.RuntimeDir, direction, component, "documents"))
				wait(t, manager, process, 1)
				stale.Close()
				// Pair immediately, without waiting for the close watcher to catch up.
				replacement := dialUnix(t, HostSocket(config.RuntimeDir, direction, component, "documents"))
				other := dialUnix(t, HostSocket(config.RuntimeDir, opposite, peer, "documents"))
				producer := replacement
				if direction == DirectionInput {
					producer = other
				}
				assertOrigin(t, producer, "filter.documents")
				writeAndRead(t, replacement, other, "first request after reconnect")
				writeAndRead(t, other, replacement, "first reply after reconnect")
				replacement.Close()
				other.Close()
				wait(t, manager, process, 0)
			}
		})
	}
}

func TestPendingShutdownReleasesWatchers(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer cancel()
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	for i := 0; i < 8; i++ {
		producer := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
		defer producer.Close()
	}
	waitForPendingOutputs(t, manager, process, 8)
	if err := manager.Stop(context.Background(), process); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left pending connection watchers running")
	}
}
