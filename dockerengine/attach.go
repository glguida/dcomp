package dockerengine

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/glguida/dcomp/engine"
)

// ContainerAttach maps a caller's streams to the global standard streams of
// one non-TTY component container. The full immutable ID is required.
func (client *Client) ContainerAttach(
	ctx context.Context,
	containerID string,
	options engine.AttachOptions,
) error {
	if !isFullContainerID(containerID) {
		return fmt.Errorf("container attach requires a full immutable Docker container ID")
	}
	if options.Stdin == nil && options.Stdout == nil && options.Stderr == nil {
		return fmt.Errorf("container attach requires at least one standard stream")
	}
	version, err := client.apiVersion(ctx)
	if err != nil {
		return err
	}
	query := url.Values{
		"logs":   {"0"},
		"stream": {"1"},
		"stdin":  {boolDigit(options.Stdin != nil)},
		"stdout": {boolDigit(options.Stdout != nil)},
		"stderr": {boolDigit(options.Stderr != nil)},
	}
	path := "/v" + version + "/containers/" + url.PathEscape(containerID) +
		"/attach?" + query.Encode()

	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", client.socketPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("Docker API via %s: %w", client.socketPath, err)
	}
	defer connection.Close()
	stopCancelClose := context.AfterFunc(ctx, func() {
		_ = connection.Close()
	})
	defer stopCancelClose()

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://docker"+path,
		nil,
	)
	if err != nil {
		return fmt.Errorf("create Docker attach request: %w", err)
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "tcp")
	if err := request.Write(connection); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("write Docker attach request: %w", err)
	}

	buffered := bufio.NewReader(connection)
	response, err := http.ReadResponse(buffered, request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("read Docker attach response: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols &&
		response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return dockerResponseError(response)
	}

	var output io.Reader = buffered
	if response.StatusCode == http.StatusOK {
		defer response.Body.Close()
		output = response.Body
	}
	if options.Ready != nil {
		if err := options.Ready(); err != nil {
			return fmt.Errorf("announce component attachment: %w", err)
		}
	}

	outputErrors := make(chan error, 1)
	go func() {
		outputErrors <- decodeDockerAttachStream(
			ctx,
			output,
			options.Stdout,
			options.Stderr,
		)
	}()

	var inputErrors <-chan error
	if options.Stdin != nil {
		channel := make(chan error, 1)
		inputErrors = channel
		go func() {
			_, copyErr := io.Copy(connection, options.Stdin)
			channel <- copyErr
		}()
	}

	for {
		select {
		case outputErr := <-outputErrors:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if outputErr != nil {
				return fmt.Errorf("read container %s standard output: %w", containerID, outputErr)
			}
			return nil
		case inputErr := <-inputErrors:
			inputErrors = nil
			if inputErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return fmt.Errorf("write container %s standard input: %w", containerID, inputErr)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func decodeDockerAttachStream(
	ctx context.Context,
	input io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	var header [dockerLogHeaderSize]byte
	buffer := make([]byte, 32*1024)
	for {
		read, err := io.ReadFull(input, header[:])
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, io.EOF) && read == 0 {
				return nil
			}
			return fmt.Errorf("truncated Docker multiplex header: %w", err)
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return fmt.Errorf("malformed Docker multiplex header: reserved bytes are nonzero")
		}
		var destination io.Writer
		switch header[0] {
		case 1:
			destination = stdout
		case 2:
			destination = stderr
		default:
			return fmt.Errorf("unsupported Docker multiplex stream %d", header[0])
		}
		if destination == nil {
			destination = io.Discard
		}
		remaining := int64(binary.BigEndian.Uint32(header[4:]))
		for remaining > 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			chunk := int64(len(buffer))
			if remaining < chunk {
				chunk = remaining
			}
			read, readErr := io.ReadFull(input, buffer[:int(chunk)])
			if read > 0 {
				written, writeErr := destination.Write(buffer[:read])
				if writeErr != nil {
					return writeErr
				}
				if written != read {
					return io.ErrShortWrite
				}
				remaining -= int64(read)
			}
			if readErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return fmt.Errorf("truncated Docker multiplex payload: %w", readErr)
			}
		}
	}
}

var _ engine.AttachEngine = (*Client)(nil)
