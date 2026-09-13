"""Socket connection helpers for DComp inputs and outputs."""

from __future__ import annotations

import errno
import math
import re
import socket
import threading
from collections.abc import Mapping
from types import TracebackType

from .endpoints import input_path, output_path

_MAX_ORIGIN_HEADER = 8 + 63 + 1 + 63 + 1  # Prefix, two names, dot, LF.
_ORIGIN_HEADER = re.compile(rb"DCOMP/1 ([a-z][a-z0-9-]{0,62}\.[a-z][a-z0-9-]{0,62})\n")


class DialListener:
    """Present claimed output connections through Python's ``accept`` shape.

    ``accept()`` connects to the proxy rather than binding an interface path.
    It consumes the proxy's origin header before returning, so one output
    connection corresponds to one real consumer connection. Application bytes
    remain unchanged; the peer address is ``component.endpoint``.
    """

    def __init__(
        self,
        output: str,
        *,
        environ: Mapping[str, str] | None = None,
        retry_delay: float = 0.025,
    ) -> None:
        if isinstance(retry_delay, bool) or not isinstance(
            retry_delay,
            (int, float),
        ):
            raise TypeError("retry_delay must be a non-negative number")
        if retry_delay < 0 or not math.isfinite(retry_delay):
            raise ValueError("retry_delay must be a non-negative number")
        self._path = output_path(output, environ)
        self._retry_delay = float(retry_delay)
        self._closed = threading.Event()
        self._lock = threading.Lock()
        self._pending: set[socket.socket] = set()

    def accept(self) -> tuple[socket.socket, str]:
        """Return the next connection and its originating component.endpoint."""

        while not self._closed.is_set():
            connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            connection.settimeout(0.1)
            if not self._track(connection):
                connection.close()
                break
            try:
                connection.connect(self._path)
                header = bytearray()
                while not self._closed.is_set():
                    try:
                        value = connection.recv(1)
                    except TimeoutError:
                        continue
                    if not value:
                        break
                    header.extend(value)
                    if value == b"\n":
                        match = _ORIGIN_HEADER.fullmatch(header)
                        if match is None:
                            break
                        connection.settimeout(None)
                        with self._lock:
                            if self._closed.is_set():
                                break
                            self._pending.discard(connection)
                        return connection, match[1].decode("ascii")
                    if len(header) >= _MAX_ORIGIN_HEADER:
                        break
            except OSError:
                pass
            finally:
                with self._lock:
                    pending = connection in self._pending
                    self._pending.discard(connection)
                if pending:
                    connection.close()
            if self._closed.wait(self._retry_delay):
                break
        raise OSError(errno.EBADF, "DComp output listener is closed")

    def close(self) -> None:
        """Stop future accepts and wake accepts currently waiting for a peer."""

        with self._lock:
            if self._closed.is_set():
                return
            self._closed.set()
            pending = tuple(self._pending)
            self._pending.clear()
        for connection in pending:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            connection.close()

    def __enter__(self) -> DialListener:
        return self

    def __exit__(
        self,
        _exception_type: type[BaseException] | None,
        _exception: BaseException | None,
        _traceback: TracebackType | None,
    ) -> None:
        self.close()

    def _track(self, connection: socket.socket) -> bool:
        with self._lock:
            if self._closed.is_set():
                return False
            self._pending.add(connection)
            return True


def connect_input(
    name: str,
    *,
    environ: Mapping[str, str] | None = None,
    timeout: float | None = None,
) -> socket.socket:
    """Connect a raw Unix stream to a declared input."""

    return _connect(input_path(name, environ), timeout)


def connect_output(
    name: str,
    *,
    environ: Mapping[str, str] | None = None,
    timeout: float | None = None,
) -> socket.socket:
    """Connect a raw output stream, including the proxy's origin header.

    Prefer DialListener to consume the header and obtain the peer address.
    """

    return _connect(output_path(name, environ), timeout)


def _connect(path: str, timeout: float | None) -> socket.socket:
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        connection.settimeout(timeout)
        connection.connect(path)
    except BaseException:
        connection.close()
        raise
    return connection
