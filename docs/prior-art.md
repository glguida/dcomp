# Prior art

DComp deliberately reuses existing standards where they fit and stays smaller
than systems solving broader problems.

## Docker Compose

[Docker Compose](https://docs.docker.com/compose/) defines multi-container
applications, creates a default user-defined network, and resolves services by
name. Its
[networking model](https://docs.docker.com/compose/how-tos/networking/) and
[startup ordering with health conditions](https://docs.docker.com/compose/how-tos/startup-order/)
cover much of the container wiring needed by a single-host application.

Compose is the closest operational baseline. DComp differs in purpose:

- downstream projects keep small `component.dcomp` and `system.dcomp` files
  beside their source;
- links bind named input endpoints directly to named output endpoints and
  nominally match fully qualified protobuf service names before start;
- one per-system proxy owns interface Unix sockets and components only connect;
- endpoint socket mounts are isolated per component, while mounts, command
  arguments, published ports, and external egress remain explicit typed
  instance policy;
- the library exposes lifecycle operations to another Go program, rather than
  requiring generated Compose configuration; and
- interrupted operations retain a small, resumable intent record and mutate
  only verified immutable IDs.

DComp does not interpret links as `depends_on`. It applies and health-checks
only new or changed components while retaining unaffected containers; cyclic
links remain valid.

DComp uses the same Docker primitives rather than recreating networking or
process supervision.

## Dapr

[Dapr service invocation](https://docs.dapr.io/developing-applications/building-blocks/service-invocation/service-invocation-overview/)
provides service-to-service calls with name resolution, retries, tracing,
security features, and HTTP or gRPC APIs. Dapr supports multiple hosting
environments and normally introduces a sidecar beside each application.

Dapr is appropriate when those distributed-system facilities are desired.
DComp intentionally has no sidecars, placement service, distributed discovery,
security layer, protocol translation, or multi-host routing. Its narrow
per-system proxy only pairs declared local Unix byte streams.

## HashiCorp go-plugin

The [`go-plugin` package](https://pkg.go.dev/github.com/hashicorp/go-plugin)
uses RPC, including gRPC, to connect a host process with separately launched
plugin processes. It demonstrates a useful separation between a stable
interface and independently implemented extensions.

Its lifecycle and transport model is different: plugins are child processes
managed by a host, with process handshakes and stdio-based launch coordination.
DComp components are OCI images. They connect as clients to individually
mounted Unix sockets owned by one independently launched per-system proxy.

## Testcontainers

[Testcontainers](https://testcontainers.com/) libraries create disposable
containers and networks from test code and provide waiting strategies for
readiness. They are excellent for integration-test fixtures and are relevant to
DComp's own acceptance testing.

Their primary abstraction is an ephemeral test dependency, not a project-owned
protobuf component contract or a resumable application lifecycle. DComp may be
tested with Testcontainers, but it does not need to embed Testcontainers in its
runtime.

## Docker managed plugins

[Docker Engine managed plugins](https://docs.docker.com/engine/extend/)
package extensions to the Docker Engine for infrastructure functions such as
volumes, networks, authorization, and logging. Docker manages their install,
enable, disable, and upgrade lifecycle.

DComp components are ordinary application containers and do not extend the
Docker daemon. Using normal OCI images avoids elevated plugin privileges,
special root filesystems, and daemon plugin APIs.

## gRPC standards

DComp uses the established gRPC protocols instead of defining a custom control
RPC:

- [Protocol Buffers and gRPC services](https://grpc.io/docs/what-is-grpc/core-concepts/)
  define arbitrary language-neutral application interfaces.
- [gRPC health checking](https://grpc.io/docs/guides/health-checking/) supplies
  the standard `grpc.health.v1.Health` service and serving states.
- [gRPC server reflection](https://grpc.io/docs/guides/reflection/) lets generic
  clients discover services and descriptors exposed by a server.
- [Graceful server shutdown](https://grpc.io/docs/guides/server-graceful-stop/)
  defines bounded draining behavior for in-flight RPCs.
- [Deadlines](https://grpc.io/docs/guides/deadlines/) bound health probes and
  application calls.

Application service definitions remain owned by component authors. DComp
standardizes only how interfaces are named in component descriptors, linked,
addressed, observed, and stopped.

## Scope conclusion

DComp chooses a deliberately narrow combination: project-owned nominal
interface declarations, opaque streams paired by one single-host proxy,
fine-grained Unix socket mounts, explicit per-instance runtime policy, and a
resumable host library with no global control-plane daemon.

The useful existing pieces are already standardized:

- Docker supplies isolation, health execution, signals, and object
  lifecycle;
- gRPC supplies the application transport, health, reflection, deadlines, and
  graceful shutdown; and
- ordinary project files supply component and system configuration.

DComp remains a thin system and lifecycle layer over those facilities. It adds
one bounded per-system data-plane process, but no global daemon, registry,
sidecar, or image-build system.
