# DComp examples

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
host exposure for that non-DComp service. Docker
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

## Build the pipeline incrementally

After building the binaries and images above, run these commands from the
repository root. This uses a separate system named `incremental-demo`:

```sh
bin/dcomp add-component incremental-demo echo examples/echo
bin/dcomp assign-global incremental-demo provider_endpoint echo.echo
bin/dcomp add-component --link upstream=@provider_endpoint \
  --arg=--repeat --arg 750ms --arg Hello \
  incremental-demo caller examples/caller
bin/dcomp view incremental-demo
```

The caller receives `Hello`. Insert the uppercase stage, with its upstream
wired directly to the original output, then reassign the global:

```sh
bin/dcomp add-component --link upstream=echo.echo \
  incremental-demo uppercase examples/uppercase
bin/dcomp assign-global incremental-demo provider_endpoint uppercase.echo
bin/dcomp view incremental-demo
```

New calls receive `HELLO`. The caller's wire remains `@provider_endpoint`;
its resolved output is now `uppercase.echo`. No auxiliary HTTP port is
published in this flow, so every component uses network mode `none`.

```sh
bin/dcomp rm-component incremental-demo uppercase
bin/dcomp view incremental-demo
bin/dcomp assign-global incremental-demo provider_endpoint echo.echo
bin/dcomp down incremental-demo
```

Removing `uppercase` leaves the global unbound and its consumers connected to
that name. Calls fail until reassignment restores the original provider.
`make integration` also runs `tools/incremental-test`, which exercises this
flow, direct-link isolation, single-wire edits, concurrent component additions,
and reuse of an empty system.

## Preview clickable global names

No containers or image builds are needed for this preview. From the repository
root, run:

```sh
bin/dcomp dashboard examples/global-system.dcomp
```

Open <http://127.0.0.1:8199>. Click `@provider_endpoint` in the sidebar to
highlight `uppercase.echo`, `caller.upstream`, and their symbolic wire. The
direct upstream wires remain unselected. Click `@pending_endpoint` to see an
unbound global: its `waiting.upstream` consumer is highlighted without an
output route. Click the name again or press Escape to clear the selection.

This is a file preview, so it shows topology without live traffic. To run the
same composition after `make examples`, use `bin/dcomp up
examples/global-system.dcomp`, then `bin/dcomp dashboard globals-demo`.
