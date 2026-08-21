"""Validation and lookup for the DComp component environment contract."""

from __future__ import annotations

import os
import posixpath
import re
from collections.abc import Mapping
from urllib.parse import urlsplit


_ENDPOINT_NAME = re.compile(r"^[a-z][a-z0-9-]*$")


def input_env(name: str) -> str:
    """Return the environment variable for a declared input."""

    return _endpoint_env("DCOMP_IN_", name)


def output_env(name: str) -> str:
    """Return the environment variable for a declared output."""

    return _endpoint_env("DCOMP_OUT_", name)


def input_target(
    name: str,
    environ: Mapping[str, str] | None = None,
) -> str:
    """Return and validate the ``unix:///`` target for an input."""

    return _endpoint_target(input_env(name), "input", name, environ)


def output_target(
    name: str,
    environ: Mapping[str, str] | None = None,
) -> str:
    """Return and validate the ``unix:///`` target for an output."""

    return _endpoint_target(output_env(name), "output", name, environ)


def input_path(
    name: str,
    environ: Mapping[str, str] | None = None,
) -> str:
    """Return the Unix socket path for a declared input."""

    return unix_path(input_target(name, environ))


def output_path(
    name: str,
    environ: Mapping[str, str] | None = None,
) -> str:
    """Return the Unix socket path for a declared output."""

    return unix_path(output_target(name, environ))


def unix_path(target: str) -> str:
    """Validate a canonical DComp Unix URI and return its socket path."""

    if not isinstance(target, str) or not target or target.strip() != target:
        raise ValueError("DComp target must be a non-empty canonical string")
    try:
        parsed = urlsplit(target)
    except ValueError as exc:
        raise ValueError("DComp target must contain an absolute unix:/// path") from exc
    if (
        not target.startswith("unix:///")
        or parsed.scheme != "unix"
        or parsed.netloc
        or target != f"unix://{parsed.path}"
        or not parsed.path.startswith("/")
        or parsed.path.startswith("//")
        or posixpath.normpath(parsed.path) != parsed.path
        or parsed.query
        or parsed.fragment
        or not parsed.path.isascii()
        or "%" in parsed.path
        or "\x00" in parsed.path
    ):
        raise ValueError("DComp target must contain an absolute unix:/// path")
    return parsed.path


def _endpoint_env(prefix: str, name: str) -> str:
    if not isinstance(name, str) or _ENDPOINT_NAME.fullmatch(name) is None:
        raise ValueError(
            f"invalid DComp interface name {name!r}: "
            "use lower-case letters, digits, and hyphens"
        )
    return prefix + name.replace("-", "_").upper()


def _endpoint_target(
    variable: str,
    direction: str,
    name: str,
    environ: Mapping[str, str] | None,
) -> str:
    source = os.environ if environ is None else environ
    target = source.get(variable)
    if not isinstance(target, str) or not target.strip():
        raise ValueError(
            f"required DComp {direction} {name!r} is not configured "
            f"({variable} is empty)"
        )
    try:
        unix_path(target)
    except ValueError as exc:
        raise ValueError(f"{variable} must contain an absolute unix:/// path") from exc
    return target
