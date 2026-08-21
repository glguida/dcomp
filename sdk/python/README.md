# dcomp-component for Python

This dependency-free package implements DComp 0.2's component-facing Unix
socket contract. It does not depend on protobuf, gRPC, ConnectRPC, or a
particular server framework.

Install it from a DComp checkout with `pip install ./sdk/python`; it is also a
normal wheel/sdist project for release packaging.

An input is a normal client connection:

```python
from dcomp_component import connect_input

connection = connect_input("upstream")
connection.sendall(request)
response = connection.recv(4096)
```

Libraries that accept a Unix URI can use `input_target("upstream")`; libraries
that want a filesystem path can use `input_path("upstream")`.

For a provided interface, `DialListener` supplies Python's conventional
`(socket, address)` accept shape without binding the DComp path:

```python
from dcomp_component import DialListener

with DialListener("filtered") as listener:
    while True:
        connection, _address = listener.accept()
        serve_protocol(connection)
```

Each `accept()` call connects to `DCOMP_OUT_FILTERED`, waits until the proxy
pairs that stream with a real consumer, and returns the connection with its
first byte untouched. Protocol integrations can put their own worker or task
policy around the returned sockets. This listener shape is intended for
client-first protocols such as HTTP and gRPC. A server-first protocol can use
`connect_output()` directly and manage its output connections explicitly.

Run the package tests from the DComp repository root:

```sh
PYTHONPATH=sdk/python/src python3 -m unittest discover -s sdk/python/tests
```
