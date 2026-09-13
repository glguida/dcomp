package runtimecontract

import (
	"fmt"
	"io"
	"strings"
)

// Output streams start with this proxy-owned header, followed by application
// bytes. Input streams have no header. The limit includes the prefix and LF.
const OriginPrefix = "DCOMP/1 "
const MaxOriginHeader = len(OriginPrefix) + 63 + 1 + 63 + 1

func ValidOrigin(origin string) bool {
	component, endpoint, ok := strings.Cut(origin, ".")
	return ok && len(component) <= 63 && len(endpoint) <= 63 &&
		ValidEndpointName(component) && ValidEndpointName(endpoint)
}

func WriteOrigin(writer io.Writer, origin string) error {
	if !ValidOrigin(origin) {
		return fmt.Errorf("invalid DComp connection origin %q", origin)
	}
	header := OriginPrefix + origin + "\n"
	n, err := io.WriteString(writer, header)
	if err == nil && n != len(header) {
		return io.ErrShortWrite
	}
	return err
}

// ReadOrigin consumes exactly the header; it must not buffer application bytes.
func ReadOrigin(reader io.Reader) (string, error) {
	header := make([]byte, 0, 64)
	var value [1]byte
	for len(header) < MaxOriginHeader {
		if _, err := io.ReadFull(reader, value[:]); err != nil {
			return "", err
		}
		header = append(header, value[0])
		if value[0] != '\n' {
			continue
		}
		line := string(header[:len(header)-1])
		origin, ok := strings.CutPrefix(line, OriginPrefix)
		if !ok || !ValidOrigin(origin) {
			return "", fmt.Errorf("invalid DComp connection origin header")
		}
		return origin, nil
	}
	return "", fmt.Errorf("DComp connection origin header exceeds %d bytes", MaxOriginHeader)
}
