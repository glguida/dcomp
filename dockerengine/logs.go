package dockerengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/glguida/dcomp/engine"
)

const (
	dockerLogHeaderSize     = 8
	maxDockerLogRecordBytes = 1024 * 1024
)

// ContainerLogs streams a snapshot, or a snapshot followed by live output,
// from a non-TTY Docker container. The full immutable container ID is required;
// names and abbreviated IDs are deliberately rejected.
func (client *Client) ContainerLogs(
	ctx context.Context,
	containerID string,
	options engine.LogOptions,
	emit func(engine.LogLine) error,
) error {
	if !isFullContainerID(containerID) {
		return fmt.Errorf("container logs require a full immutable Docker container ID")
	}
	if emit == nil {
		return fmt.Errorf("container log receiver is nil")
	}

	version, err := client.apiVersion(ctx)
	if err != nil {
		return err
	}
	query := url.Values{
		"follow":     {boolDigit(options.Follow)},
		"stderr":     {"1"},
		"stdout":     {"1"},
		"tail":       {"all"},
		"timestamps": {"1"},
	}
	path := "/v" + version + "/containers/" + url.PathEscape(containerID) +
		"/logs?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return fmt.Errorf("create Docker log request: %w", err)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("Docker API via %s: %w", client.socketPath, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return dockerResponseError(response)
	}
	if err := decodeDockerLogStream(ctx, response.Body, emit); err != nil {
		return fmt.Errorf("read container %s logs: %w", containerID, err)
	}
	return nil
}

func decodeDockerLogStream(
	ctx context.Context,
	input io.Reader,
	emit func(engine.LogLine) error,
) error {
	assemblers := map[engine.LogStream]*dockerLineAssembler{
		engine.LogStdout: {},
		engine.LogStderr: {},
	}
	var header [dockerLogHeaderSize]byte
	buffer := make([]byte, 32*1024)

	for {
		read, err := io.ReadFull(input, header[:])
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, io.EOF) && read == 0 {
				for _, stream := range []engine.LogStream{engine.LogStdout, engine.LogStderr} {
					if err := assemblers[stream].flush(stream, emit); err != nil {
						return err
					}
				}
				return nil
			}
			return fmt.Errorf("truncated Docker multiplex header: %w", err)
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return fmt.Errorf("malformed Docker multiplex header: reserved bytes are nonzero")
		}
		stream, err := dockerLogStream(header[0])
		if err != nil {
			return err
		}

		remaining := int64(binary.BigEndian.Uint32(header[4:]))
		for remaining > 0 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			chunkSize := int64(len(buffer))
			if remaining < chunkSize {
				chunkSize = remaining
			}
			read, readErr := io.ReadFull(input, buffer[:int(chunkSize)])
			if read > 0 {
				if err := assemblers[stream].write(stream, buffer[:read], emit); err != nil {
					return err
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

type dockerLineAssembler struct {
	pending []byte
}

func (assembler *dockerLineAssembler) write(
	stream engine.LogStream,
	data []byte,
	emit func(engine.LogLine) error,
) error {
	assembler.pending = append(assembler.pending, data...)
	for {
		end := bytes.IndexByte(assembler.pending, '\n')
		if end < 0 {
			if len(assembler.pending) > maxDockerLogRecordBytes {
				return fmt.Errorf(
					"Docker log record exceeds %d bytes",
					maxDockerLogRecordBytes,
				)
			}
			return nil
		}
		if end > maxDockerLogRecordBytes {
			return fmt.Errorf(
				"Docker log record exceeds %d bytes",
				maxDockerLogRecordBytes,
			)
		}
		if err := emitDockerLogLine(stream, assembler.pending[:end], emit); err != nil {
			return err
		}
		assembler.discard(end + 1)
	}
}

func (assembler *dockerLineAssembler) flush(
	stream engine.LogStream,
	emit func(engine.LogLine) error,
) error {
	if len(assembler.pending) == 0 {
		return nil
	}
	if len(assembler.pending) > maxDockerLogRecordBytes {
		return fmt.Errorf(
			"Docker log record exceeds %d bytes",
			maxDockerLogRecordBytes,
		)
	}
	if err := emitDockerLogLine(stream, assembler.pending, emit); err != nil {
		return err
	}
	assembler.pending = nil
	return nil
}

func (assembler *dockerLineAssembler) discard(count int) {
	if count == len(assembler.pending) {
		assembler.pending = assembler.pending[:0]
		return
	}
	copy(assembler.pending, assembler.pending[count:])
	assembler.pending = assembler.pending[:len(assembler.pending)-count]
}

func emitDockerLogLine(
	stream engine.LogStream,
	encoded []byte,
	emit func(engine.LogLine) error,
) error {
	separator := bytes.IndexByte(encoded, ' ')
	if separator <= 0 {
		return fmt.Errorf("Docker log line has no timestamp")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, string(encoded[:separator]))
	if err != nil {
		return fmt.Errorf("parse Docker log timestamp %q: %w", encoded[:separator], err)
	}
	line := engine.LogLine{
		Timestamp: timestamp,
		Stream:    stream,
		Message:   string(encoded[separator+1:]),
	}
	if err := emit(line); err != nil {
		return fmt.Errorf("container log receiver: %w", err)
	}
	return nil
}

func dockerLogStream(value byte) (engine.LogStream, error) {
	switch value {
	case 1:
		return engine.LogStdout, nil
	case 2:
		return engine.LogStderr, nil
	default:
		return "", fmt.Errorf("unsupported Docker multiplex stream %d", value)
	}
}

func isFullContainerID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func boolDigit(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

var _ engine.LogEngine = (*Client)(nil)
