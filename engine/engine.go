// Package engine defines the narrow container-engine boundary used by dcomp.
//
// The interface uses immutable object IDs for every mutation. Names are only
// used to discover objects after an interrupted create.
package engine

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("docker object not found")

type Image struct {
	ID             string
	HasHealthcheck bool
	// DeclaredVolumes are the absolute container paths declared by the image's
	// OCI Config.Volumes map. The engine does not create anonymous volumes for
	// them; callers must supply explicit persistent volume mounts.
	DeclaredVolumes []string
}

type Network struct {
	ID        string
	Name      string
	Driver    string
	Internal  bool
	Labels    map[string]string
	Endpoints []NetworkEndpoint
}

// NetworkEndpoint preserves both Docker's Containers map key and the
// endpoint's immutable identity. The key normally equals the container ID,
// but Docker exposes a stranded endpoint as "ep-" followed by EndpointID.
type NetworkEndpoint struct {
	Key        string
	Name       string
	EndpointID string
}

type Health string

const (
	HealthNone      Health = "none"
	HealthStarting  Health = "starting"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

type Container struct {
	User     string
	ID       string
	Name     string
	ImageID  string
	Labels   map[string]string
	Status   string
	Running  bool
	ExitCode int
	Error    string
	Health   Health
	// Networks is keyed by attachable Docker network name; Docker's built-in
	// "none" pseudo-network is omitted. Before first start Docker may leave
	// NetworkID empty, so lifecycle verification uses the exact recorded name
	// and then verifies a non-empty ID whenever Docker supplies one.
	Networks    map[string]NetworkAttachment
	Environment map[string]string
	Mounts      []Mount
	Args        []string
	// PortBindings is Docker's configured binding policy from HostConfig.
	// A zero HostPort remains zero when Docker was asked to allocate it.
	PortBindings []PortBinding
	// PublishedPorts is the effective host binding observed in
	// NetworkSettings. It is observational and is never used to verify the
	// configured binding policy.
	PublishedPorts []PortBinding
	Init           bool
	RestartPolicy  string
	OpenStdin      bool
	StdinOnce      bool
	TTY            bool
	Security       ContainerSecurity
}

// ContainerSecurity is the security policy configured on a container. It is
// part of the immutable launch contract: lifecycle code supplies it on create
// and verifies the same values on every later inspection.
type ContainerSecurity struct {
	NoNewPrivileges     bool
	DroppedCapabilities []string
	PIDsLimit           int64
}

type NetworkAttachment struct {
	NetworkID string
	Aliases   []string
}

type NetworkRequest struct {
	Name     string
	Internal bool
	Labels   map[string]string
}

type MountType string

const (
	MountBind   MountType = "bind"
	MountVolume MountType = "volume"
)

// Mount describes either a host bind mount or a named persistent volume. For
// a bind Source is the host path; for a volume it is the Docker volume name.
type Mount struct {
	Type     MountType
	Source   string
	Target   string
	ReadOnly bool
}

type PortProtocol string

const (
	ProtocolTCP PortProtocol = "tcp"
	ProtocolUDP PortProtocol = "udp"
)

// PortBinding publishes one container TCP or UDP port on the host. HostPort
// zero asks Docker to allocate a port.
type PortBinding struct {
	ContainerPort int
	Protocol      PortProtocol
	HostIP        string
	HostPort      int
}

type Volume struct {
	Name       string
	Driver     string
	Mountpoint string
	Labels     map[string]string
}

type VolumeRequest struct {
	Name   string
	Labels map[string]string
}

type ContainerRequest struct {
	User           string
	Name           string
	ImageID        string
	NetworkID      string
	NetworkAliases []string
	Labels         map[string]string
	Environment    map[string]string
	Mounts         []Mount
	Args           []string
	PortBindings   []PortBinding
	StopTimeout    time.Duration
	Security       ContainerSecurity
}

type LogStream string

const (
	LogStdout LogStream = "stdout"
	LogStderr LogStream = "stderr"
)

// LogLine is one timestamped line from a container's stdout or stderr. Message
// excludes the terminating newline.
type LogLine struct {
	Timestamp time.Time
	Stream    LogStream
	Message   string
}

type LogOptions struct {
	// Follow keeps the Docker response open for new output. When false,
	// ContainerLogs returns after the current log snapshot.
	Follow bool
}

// LogEngine is separate from Engine because observing output is not part of
// component lifecycle mutation. containerID must be Docker's full immutable ID.
// emit is called synchronously as complete lines become available and may stop
// streaming by returning an error. Stdout and stderr fragments are assembled
// independently.
type LogEngine interface {
	ContainerLogs(
		context.Context,
		string,
		LogOptions,
		func(LogLine) error,
	) error
}

// AttachOptions maps one caller's standard streams to the component's global
// file descriptors. Ready is called after Docker has established the attach
// transport and before any bytes are copied.
type AttachOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Ready  func() error
}

// AttachEngine is separate from Engine because attaching standard I/O is a
// scoped communication operation, not a lifecycle mutation.
type AttachEngine interface {
	ContainerAttach(context.Context, string, AttachOptions) error
}

// Engine is intentionally smaller than the Docker API. Implementations must
// distinguish not-found with errors.Is(err, ErrNotFound).
type Engine interface {
	// Identity returns the stable ID of the local container engine. Lifecycle
	// state is bound to this value before its resource IDs may be mutated.
	Identity(context.Context) (string, error)

	ResolveImage(context.Context, string) (Image, error)

	InspectNetwork(context.Context, string) (Network, error)
	CreateNetwork(context.Context, NetworkRequest) (Network, error)
	RemoveNetwork(context.Context, string) error

	InspectVolume(context.Context, string) (Volume, error)
	CreateVolume(context.Context, VolumeRequest) (Volume, error)

	InspectContainer(context.Context, string) (Container, error)
	CreateContainer(context.Context, ContainerRequest) (Container, error)
	ConnectNetwork(context.Context, string, string, []string) error
	DisconnectNetwork(context.Context, string, string) error
	// ForceDisconnectNetworkEndpoint removes an endpoint by the exact name
	// returned from InspectNetwork. It is reserved for recovery after the
	// owning container has been verified absent.
	ForceDisconnectNetworkEndpoint(context.Context, string, string) error
	StartContainer(context.Context, string) error
	StopContainer(context.Context, string, time.Duration) error
	// RestartContainer must use the engine's single-container restart
	// operation. Lifecycle retries this same operation after an ambiguous
	// result; implementations must not expand it into separate stop and start
	// calls.
	RestartContainer(context.Context, string, time.Duration) error
	RemoveContainer(context.Context, string) error
}
