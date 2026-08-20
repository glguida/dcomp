# DComp 0.2 examples

The example system demonstrates a two-hop gRPC pipeline carried only by
orchestrator-owned Unix sockets:

```text
caller.upstream -> uppercase.echo
uppercase.upstream -> echo.echo
```

`echo` returns its request. `uppercase` calls `echo`, uppercases the response,
and serves it on its own output. `caller --wait` is the system-managed sink
used to declare the final input; the same caller image can make a one-shot RPC
when its input socket is mounted into a test container.

The descriptors keep the ordinary syntax:

```text
system demo

component echo echo
component uppercase uppercase
component caller caller

args caller --wait
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

The final input is deliberately not published as TCP. For a manual call, find
the selected runtime root (default `/var/run/dcomp`) and bind-mount the exact
caller input socket into a one-shot caller container:

```sh
docker run --rm --network none \
  --mount type=bind,src=/var/run/dcomp/demo/in/caller.upstream,dst=/run/dcomp/in/upstream \
  -e DCOMP_IN_UPSTREAM=unix:///run/dcomp/in/upstream \
  dcomp-example-caller:dev 'hello components'
```

Expected output:

```text
HELLO COMPONENTS
```

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

`make integration` automates this flow and additionally checks network mode
`none` for components without egress, the caller's single egress bridge, the
absence of the old `DCOMP_LINK_*` environment, proxy survival across a
component restart, and complete proxy/container/network cleanup during down.
