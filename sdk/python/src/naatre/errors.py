from __future__ import annotations

from dataclasses import dataclass
from typing import TypeAlias

PathElement: TypeAlias = str | int


class NaatreClientError(Exception):
    """A stable client or protocol failure."""

    def __init__(self, code: str, message: str = "") -> None:
        super().__init__(message or code)
        self.code = code


class TransportError(NaatreClientError):
    """A transport failure with an explicit retry classification."""

    def __init__(self, code: str, message: str = "", *, retryable: bool = False) -> None:
        super().__init__(code, message)
        self.retryable = retryable


@dataclass(frozen=True, slots=True)
class Error:
    code: str
    message: str = ""
    path: tuple[PathElement, ...] = ()
