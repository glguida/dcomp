"""Client-only component socket helpers for DComp 0.2."""

from .endpoints import (
    input_env,
    input_path,
    input_target,
    output_env,
    output_path,
    output_target,
    unix_path,
)
from .listener import DialListener, connect_input, connect_output

__all__ = [
    "DialListener",
    "connect_input",
    "connect_output",
    "input_env",
    "input_path",
    "input_target",
    "output_env",
    "output_path",
    "output_target",
    "unix_path",
]

__version__ = "0.2.0"
