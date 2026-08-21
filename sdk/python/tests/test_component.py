from __future__ import annotations

import errno
import socket
import tempfile
import threading
import time
import unittest
from pathlib import Path

from dcomp_component import (
    DialListener,
    connect_input,
    input_env,
    input_path,
    input_target,
    output_env,
    output_path,
    output_target,
    unix_path,
)


class EndpointTests(unittest.TestCase):
    def test_environment_names_and_targets(self) -> None:
        environment = {
            "DCOMP_IN_MODEL_PROVIDER": "unix:///run/dcomp/in/model-provider",
            "DCOMP_OUT_FILTERED": "unix:///run/dcomp/out/filtered",
        }
        self.assertEqual(input_env("model-provider"), "DCOMP_IN_MODEL_PROVIDER")
        self.assertEqual(output_env("filtered"), "DCOMP_OUT_FILTERED")
        self.assertEqual(
            input_target("model-provider", environment),
            environment["DCOMP_IN_MODEL_PROVIDER"],
        )
        self.assertEqual(
            input_path("model-provider", environment),
            "/run/dcomp/in/model-provider",
        )
        self.assertEqual(
            output_target("filtered", environment),
            environment["DCOMP_OUT_FILTERED"],
        )
        self.assertEqual(
            output_path("filtered", environment),
            "/run/dcomp/out/filtered",
        )

    def test_invalid_names_are_rejected(self) -> None:
        for name in ("", "UPSTREAM", "two_words", "1upstream", "with.dot", None):
            with self.subTest(name=name), self.assertRaises(ValueError):
                input_env(name)  # type: ignore[arg-type]

    def test_target_is_required_and_canonical(self) -> None:
        with self.assertRaisesRegex(ValueError, "DCOMP_IN_UPSTREAM is empty"):
            input_target("upstream", {})
        for target in (
            " dns:///old:50051",
            "dns:///old:50051",
            "unix:relative.sock",
            "unix:/run/dcomp/in/upstream",
            "unix:////run/dcomp/in/upstream",
            "unix://host/run/dcomp/in/upstream",
            "unix:///run/dcomp/in/../upstream",
            "unix:///run/dcomp/in/%75pstream",
            "unix:///run/dcomp/in/upstream?query=yes",
            "unix:///run/dcomp/in/upstream#fragment",
        ):
            with self.subTest(target=target), self.assertRaises(ValueError):
                unix_path(target)


@unittest.skipUnless(hasattr(socket, "AF_UNIX"), "Unix sockets are required")
class ConnectionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="dcomp-python-sdk-")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def socket_path(self, name: str) -> str:
        return str(Path(self.temporary.name) / name)

    def test_connect_input_returns_a_raw_bidirectional_stream(self) -> None:
        path = self.socket_path("input.sock")
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(server.close)
        server.bind(path)
        server.listen()

        client = connect_input(
            "upstream",
            environ={"DCOMP_IN_UPSTREAM": f"unix://{path}"},
        )
        self.addCleanup(client.close)
        peer, _address = server.accept()
        self.addCleanup(peer.close)

        client.sendall(b"request")
        self.assertEqual(peer.recv(7), b"request")
        peer.sendall(b"response")
        self.assertEqual(client.recv(8), b"response")

    def test_dial_listener_waits_for_and_preserves_first_byte(self) -> None:
        path = self.socket_path("output.sock")
        proxy = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(proxy.close)
        proxy.bind(path)
        proxy.listen()
        listener = DialListener(
            "filtered",
            environ={"DCOMP_OUT_FILTERED": f"unix://{path}"},
        )
        self.addCleanup(listener.close)

        accepted: list[tuple[socket.socket, str] | BaseException] = []

        def accept() -> None:
            try:
                accepted.append(listener.accept())
            except BaseException as exc:  # pragma: no cover - assertion reports it
                accepted.append(exc)

        thread = threading.Thread(target=accept)
        thread.start()
        producer, _address = proxy.accept()
        self.addCleanup(producer.close)
        time.sleep(0.05)
        self.assertTrue(thread.is_alive(), "unclaimed output was returned")

        producer.sendall(b"request")
        thread.join(timeout=2)
        self.assertFalse(thread.is_alive(), "claimed output was not returned")
        self.assertEqual(len(accepted), 1)
        self.assertIsInstance(accepted[0], tuple)
        connection, address = accepted[0]  # type: ignore[misc]
        self.addCleanup(connection.close)
        self.assertEqual(address, "")
        self.assertEqual(connection.recv(7), b"request")
        connection.sendall(b"response")
        self.assertEqual(producer.recv(8), b"response")

    def test_dial_listener_reconnects_when_unclaimed_stream_closes(self) -> None:
        path = self.socket_path("reconnect.sock")
        proxy = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(proxy.close)
        proxy.bind(path)
        proxy.listen()
        listener = DialListener(
            "result",
            environ={"DCOMP_OUT_RESULT": f"unix://{path}"},
        )
        self.addCleanup(listener.close)

        result: list[tuple[socket.socket, str]] = []
        thread = threading.Thread(target=lambda: result.append(listener.accept()))
        thread.start()
        abandoned, _address = proxy.accept()
        abandoned.close()
        claimed, _address = proxy.accept()
        self.addCleanup(claimed.close)
        claimed.sendall(b"x")

        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(result[0][0].recv(1), b"x")
        result[0][0].close()

    def test_dial_listener_retries_until_proxy_socket_appears(self) -> None:
        path = self.socket_path("late.sock")
        listener = DialListener(
            "result",
            environ={"DCOMP_OUT_RESULT": f"unix://{path}"},
        )
        self.addCleanup(listener.close)
        result: list[tuple[socket.socket, str]] = []
        thread = threading.Thread(target=lambda: result.append(listener.accept()))
        thread.start()
        time.sleep(0.05)

        proxy = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(proxy.close)
        proxy.bind(path)
        proxy.listen()
        claimed, _address = proxy.accept()
        self.addCleanup(claimed.close)
        claimed.sendall(b"x")

        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(result[0][0].recv(1), b"x")
        result[0][0].close()

    def test_close_wakes_accept(self) -> None:
        path = self.socket_path("close.sock")
        proxy = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(proxy.close)
        proxy.bind(path)
        proxy.listen()
        listener = DialListener(
            "result",
            environ={"DCOMP_OUT_RESULT": f"unix://{path}"},
        )

        error: list[OSError] = []

        def accept() -> None:
            try:
                listener.accept()
            except OSError as exc:
                error.append(exc)

        thread = threading.Thread(target=accept)
        thread.start()
        pending, _address = proxy.accept()
        self.addCleanup(pending.close)
        listener.close()
        thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(error[0].errno, errno.EBADF)


if __name__ == "__main__":
    unittest.main()
