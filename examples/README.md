# Example system

The managed system contains an `uppercase` component whose `upstream` input is
linked directly to the `echo` component's output.

These examples choose protobuf/gRPC and implement
`dcomp.example.v1.Echo`. They use the optional Go component package for gRPC
multiplexing, standard health, reflection, link lookup, and bounded shutdown.
That protocol stack is an example convention, not a requirement of DComp's
substrate.

The component descriptors are:

```text
# echo/component.dcomp
docker dcomp-example-echo:dev
output dcomp.example.v1.Echo echo
```

```text
# uppercase/component.dcomp
docker dcomp-example-uppercase:dev
input dcomp.example.v1.Echo upstream
output dcomp.example.v1.Echo echo
```

`system.dcomp` creates one instance of each and links the input directly:

```text
system demo
component echo echo
component uppercase uppercase
publish uppercase tcp 127.0.0.1 50052 50051
egress uppercase
link uppercase.upstream echo.echo
```

The explicit `publish` makes the final service available only at
`127.0.0.1:50052`. Docker cannot publish from an internal-only bridge, so the
separate `egress uppercase` permission is required. DComp still gives the link
a private internal network containing only `echo` and `uppercase`.

DComp injects this target into `uppercase`:

```text
DCOMP_LINK_UPSTREAM=dns:///echo:50051
```

DComp matches the two declared interface identifiers nominally and injects the
address. It does not load the example `.proto`, inspect reflection, or decode
the RPC payload.

Build each example image with the repository root as its Docker build context:

```sh
docker build -f examples/echo/Dockerfile -t dcomp-example-echo:dev .
docker build -f examples/uppercase/Dockerfile -t dcomp-example-uppercase:dev .
docker build -f examples/caller/Dockerfile -t dcomp-example-caller:dev .
```

DComp does not perform these builds. The caller image is a one-shot test client,
not a component in `system.dcomp`.

Run and inspect the system:

```sh
bin/dcomp check examples/system.dcomp
bin/dcomp up examples/system.dcomp
bin/dcomp status demo
bin/dcomp logs demo
```

Exercise the explicitly published output:

```sh
docker run --rm --network host \
  -e DCOMP_LINK_UPSTREAM=dns:///127.0.0.1:50052 \
  dcomp-example-caller:dev "hello components"
```

The expected response is `HELLO COMPONENTS`. Remove the system with:

```sh
bin/dcomp down demo
```

Other systems may add instance policy after a `component` declaration:

```text
bind worker ./config /etc/worker ro
volume worker state /var/lib/worker rw
args worker serve --mode production
egress worker
```

Bind sources are relative to `system.dcomp`; named volumes persist across
replacement and `down`. Arguments are whitespace-delimited, and external
network access is denied unless `egress` is declared.
