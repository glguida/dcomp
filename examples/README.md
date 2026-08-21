# DComp 0.2 examples

The example system demonstrates a two-hop gRPC pipeline carried only by
orchestrator-owned Unix sockets, plus one explicitly published auxiliary HTTP
service:

```text
caller.upstream -> uppercase.echo
uppercase.upstream -> echo.echo
```

`echo` returns its request. `uppercase` calls `echo`, uppercases the response,
and serves it on its own output. The same image owns an ordinary HTTP admin
listener on container port 8080 with `/healthz` and `/metrics`; DComp publishes
that separate listener on host loopback port 8080. The managed `caller` sends
one request every 750 milliseconds so the dashboard has real traffic to show;
the same image can make a one-shot RPC when its input socket is mounted into a
test container.

The descriptors keep the ordinary syntax:

```text
system demo

component echo echo
component uppercase uppercase
component caller caller

publish uppercase tcp 127.0.0.1 8080 8080
egress uppercase

args caller --repeat 750ms dcomp-demo
link uppercase.upstream echo.echo
link caller.upstream uppercase.echo
```

DComp injects addresses such as:

```text
DCOMP_IN_UPSTREAM=unix:///run/dcomp/in/upstream
DCOMP_OUT_ECHO=unix:///run/dcomp/out/echo
```

The programs use `component.InputTarget` for gRPC clients and
`component.NewServer(component.WithOutput("echo"))` for client-only gRPC
outputs. None binds or listens on a DComp interface socket.

The admin server is intentionally different: it calls `listen()` on an
ordinary container TCP port supplied by the image. `publish` is the explicit
host exposure for that non-DComp service. In the current 0.2 policy, Docker
port publication requires the same component to declare `egress`, so
`uppercase` receives its own externally routed bridge; `echo` and `caller`
remain in Docker network mode `none`.

Build the three images and DComp binaries:

```sh
make build
make examples
```

Apply the system:

```sh
bin/dcomp up examples/system.dcomp
bin/dcomp status demo
```

Check the published auxiliary service:

```sh
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/metrics
```

The first command prints `ok`. The metric
`dcomp_example_uppercase_requests_total` increases when the gRPC pipeline is
called, so it rises continuously in this demo. This is an application-specific
metric. Independently, `dcomp view --json demo` reports the proxy's generic
per-link active gauge and directional byte counters. Neither HTTP route is a
DComp input or output, and the DComp proxy does not carry the HTTP scrape.

The final input is deliberately not published as TCP. For a manual call, find
the selected runtime root (by default the selected state root followed by
`run`) and bind-mount the exact caller input socket into a one-shot caller
container. With the standard state-root default and no XDG override:

```sh
docker run --rm --network none \
  --mount type=bind,src="$HOME/.local/state/dcomp/run/demo/in/caller.upstream",dst=/run/dcomp/in/upstream \
  -e DCOMP_IN_UPSTREAM=unix:///run/dcomp/in/upstream \
  dcomp-example-caller:dev 'hello components'
```

Expected output:

```text
HELLO COMPONENTS
```

That one-shot call increases the application counter by one in addition to the
managed caller's continuing requests.

This extra container is only a demonstration client. Managed component
containers receive their endpoint mounts automatically and individually.

Observe both component and proxy records:

```sh
bin/dcomp logs demo
bin/dcomp logs demo @proxy
```

Restart one component without replacing the proxy:

```sh
bin/dcomp restart demo uppercase
```

Remove containers, the per-system proxy, endpoint sockets, and transient
egress networks:

```sh
bin/dcomp down demo
```

`make integration` automates this flow and additionally checks the published
health and metrics routes, network mode `none` for components without egress,
the uppercase component's single egress bridge, the absence of the old
`DCOMP_LINK_*` environment, proxy survival across a component restart, and
complete proxy/container/network cleanup during down.
