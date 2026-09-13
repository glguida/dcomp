package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSDKOutputOriginThroughProxy(t *testing.T) {
	for _, language := range []string{"python3", "node"} {
		t.Run(language, func(t *testing.T) {
			binary, err := exec.LookPath(language)
			if err != nil {
				t.Skipf("%s is not installed", language)
			}
			config := testConfig(t, true)
			cancel, result := startTestProxy(t, config)
			defer func() {
				cancel()
				if err := <-result; err != nil {
					t.Errorf("proxy: %v", err)
				}
			}()
			script := "testdata/origin-output.py"
			if language == "node" {
				script = "testdata/origin-output.mjs"
			}
			ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			command := exec.CommandContext(ctx, binary, script)
			pythonPath, err := filepath.Abs("../sdk/python/src")
			if err != nil {
				t.Fatal(err)
			}
			command.Env = append(os.Environ(),
				"PYTHONDONTWRITEBYTECODE=1", "PYTHONPATH="+pythonPath,
				"DCOMP_OUT_DOCUMENTS=unix://"+HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"),
			)
			var logs bytes.Buffer
			command.Stdout, command.Stderr = &logs, &logs
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				stop()
				if command.ProcessState == nil {
					_ = command.Wait()
				}
			}()
			for _, component := range []string{"filter", "archive"} {
				connection := dialUnix(t, HostSocket(config.RuntimeDir, DirectionInput, component, "documents"))
				defer connection.Close()
				reader := bufio.NewReader(connection)
				// The SDK can reply before any client application bytes arrive.
				origin, err := reader.ReadString('\n')
				if err != nil || origin != component+".documents\n" {
					t.Fatalf("SDK origin = %q, %v", origin, err)
				}
				// A header-shaped payload cannot replace the proxy-supplied origin.
				payload := "DCOMP/1 forged.identity\n\x00\xffpayload"
				if _, err := io.WriteString(connection, payload); err != nil {
					t.Fatal(err)
				}
				if err := connection.(*net.UnixConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(reader)
				if err != nil || string(got) != payload {
					t.Fatalf("SDK payload = %q, %v", got, err)
				}
			}
			if err := command.Wait(); err != nil {
				t.Fatalf("SDK server: %v\n%s", err, &logs)
			}
		})
	}
}
