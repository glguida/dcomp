# dcomp-component for Python

This dependency-free package implements DComp 0.2's component-facing Unix
socket contract. It does not depend on protobuf, gRPC, ConnectRPC, or a
particular server framework.

Install it from a DComp checkout with `pip install ./sdk/python`. The directory
is also a normal wheel/sdist project for release packaging. Python 3.10 or
newer is required.

## Inputs

An input is a normal client connection:

```python
from dcomp_component import connect_input

with connect_input("upstream") as connection:
    connection.sendall(request)
    response = connection.recv(4096)
```

`connect_input()` performs one connection attempt and returns a standard
blocking `socket.socket`. Its optional `timeout=` is passed to the socket.
Application-level retry, request framing, deadlines, and reconnect policy stay
with the component.

Libraries that accept a Unix URI can use `input_target("upstream")`; libraries
that require a filesystem path can use `input_path("upstream")`.

## Outputs

For a provided interface, `DialListener` supplies Python's conventional
`(socket, address)` accept result without binding the DComp path:

```python
import errno
import signal
import threading

from dcomp_component import DialListener


def serve_connection(connection):
    with connection:
        request = connection.recv(4096)
        connection.sendall(handle(request))


listener = DialListener("filtered")
stopping = threading.Event()


def stop(_signal, _frame):
    stopping.set()
    listener.close()


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)

with listener:
    try:
        while not stopping.is_set():
            connection, _address = listener.accept()
            threading.Thread(
                target=serve_connection,
                args=(connection,),
            ).start()
    except OSError as error:
        if error.errno != errno.EBADF or not stopping.is_set():
            raise
```

Each `accept()` call connects to `DCOMP_OUT_FILTERED`, retries while the proxy
is unavailable, and waits until the proxy pairs the stream with a real
consumer. It returns the connection with the consumer's first byte untouched.
One returned socket represents one consumer connection; fan-out and reconnects
therefore produce additional accepts.

Calling `listener.close()` stops future acquisition and wakes an `accept()`
currently waiting for a consumer. Sockets already returned by `accept()`
belong to the application and are not closed by the listener. Track active
workers when the application needs a bounded graceful drain before exit.

Use one accept loop per output. Several outputs can share an application
handler, but their connections remain independently acquired:

```python
listeners = {
    name: DialListener(name)
    for name in ("component", "provider")
}
```

## Asyncio integration

`DialListener` is blocking, but asyncio can adopt each connected socket with
the public `loop.connect_accepted_socket()` API:

```python
import asyncio

from dcomp_component import DialListener


class Protocol(asyncio.Protocol):
    def connection_made(self, transport):
        self.transport = transport

    def data_received(self, data):
        self.transport.write(handle(data))


async def serve_output(name):
    loop = asyncio.get_running_loop()
    listener = DialListener(name)
    try:
        while True:
            connection, _address = await asyncio.to_thread(listener.accept)
            await loop.connect_accepted_socket(Protocol, connection)
    finally:
        listener.close()
```

After `connect_accepted_socket()` succeeds, the asyncio transport owns the
socket.

## Framework boundary

`DialListener` is an accept-shaped connection source, not a listening
`socket.socket`: it has no bound address or listening file descriptor. It works
directly with code that consumes an already-connected socket and with
frameworks that expose an equivalent accepted-socket hook, such as asyncio's
`connect_accepted_socket()`.

APIs that insist on creating or receiving a listening socket are not direct
consumers. This includes `asyncio.start_server()`, ordinary WSGI/ASGI server
launchers, and grpcio's public `add_insecure_port()` server setup. Do not pass a
DComp output path to those APIs: they would try to bind it. A framework-specific
adapter must feed it the connected sockets instead; this package does not claim
such an adapter for grpcio or an ASGI server.

The listener adapter is intended for client-first protocols, where the
consumer sends the first byte, including HTTP-like request/response protocols.
A server-first protocol must use `connect_output()` directly and manage its
desired number of producer connections explicitly.

## API summary

- `input_env(name)` / `output_env(name)` return the contract environment name.
- `input_target(name)` / `output_target(name)` return a validated `unix:///`
  target.
- `input_path(name)` / `output_path(name)` return the native socket path.
- `unix_path(target)` validates a target and returns its path.
- `connect_input(name, timeout=...)` performs one raw input connection.
- `connect_output(name, timeout=...)` performs one raw output connection.
- `DialListener(name, retry_delay=...)` repeatedly acquires claimed output
  connections.

Endpoint names and targets are validated before connection. Tests can pass an
explicit `environ=` mapping to target and listener helpers instead of mutating
`os.environ`.

Run the package tests from the DComp repository root:

```sh
PYTHONPATH=sdk/python/src python3 -m unittest discover -s sdk/python/tests
```
