from __future__ import annotations

from collections.abc import AsyncGenerator, AsyncIterator, Iterator, Mapping
from dataclasses import dataclass, field
from typing import Protocol, cast, runtime_checkable

from .errors import NaatreClientError
from .json import JSONValue, strict_json_loads


class AsyncStreamTransport(Protocol):
    async def open(
        self, request: bytes, *, timeout: float | None = None
    ) -> AsyncIterator[bytes]: ...


@runtime_checkable
class AsyncCloseable(Protocol):
    async def aclose(self) -> None: ...


@dataclass(frozen=True, slots=True)
class StreamFrame:
    type: str
    stream: str
    sequence: int | None
    value: Mapping[str, JSONValue]


@dataclass(slots=True)
class _EventState:
    event: str = ""
    event_id: str | None = None
    data: list[str] = field(default_factory=list)

    def consume(self, line: str) -> StreamFrame | None:
        if line == "":
            return self._finish()
        if line.startswith(":"):
            return None
        field, separator, value = line.partition(":")
        if separator and value.startswith(" "):
            value = value[1:]
        self._store(field, value)
        return None

    def unfinished(self) -> bool:
        return bool(self.event or self.event_id is not None or self.data)

    def _finish(self) -> StreamFrame | None:
        frame = _decode_event(self.event, self.event_id, self.data) if self.data else None
        self.event = ""
        self.event_id = None
        self.data = []
        return frame

    def _store(self, field: str, value: str) -> None:
        if field == "event" and not self.event:
            self.event = value
        elif field == "id" and self.event_id is None and "\x00" not in value:
            self.event_id = value
        elif field == "data":
            self.data.append(value)
        elif field != "retry":
            raise NaatreClientError("CLIENT_STREAM_INVALID")


async def decode_sse(
    source: AsyncIterator[bytes],
    *,
    maximum_frame_bytes: int = 1 << 20,
) -> AsyncGenerator[StreamFrame, None]:
    line_buffer = bytearray()
    event = _EventState()
    try:
        async for chunk in source:
            _append_chunk(line_buffer, chunk, maximum_frame_bytes)
            for line in _extract_lines(line_buffer):
                frame = event.consume(line)
                if frame is not None:
                    yield frame
        if line_buffer or event.unfinished():
            raise NaatreClientError("CLIENT_STREAM_TRUNCATED")
    finally:
        if isinstance(source, AsyncCloseable):
            await source.aclose()


def _append_chunk(buffer: bytearray, chunk: bytes, maximum_frame_bytes: int) -> None:
    if not isinstance(chunk, bytes):
        raise NaatreClientError("CLIENT_STREAM_INVALID")
    buffer.extend(chunk)
    if len(buffer) > maximum_frame_bytes:
        raise NaatreClientError("CLIENT_STREAM_LIMIT")


def _extract_lines(buffer: bytearray) -> Iterator[str]:
    while (newline := buffer.find(b"\n")) >= 0:
        raw_line = bytes(buffer[:newline])
        del buffer[: newline + 1]
        if raw_line.endswith(b"\r"):
            raw_line = raw_line[:-1]
        yield _decode_line(raw_line)


def _decode_line(value: bytes) -> str:
    try:
        return value.decode("utf-8", errors="strict")
    except UnicodeDecodeError as error:
        raise NaatreClientError("CLIENT_STREAM_INVALID") from error


def _decode_event(event: str, event_id: str | None, data: list[str]) -> StreamFrame:
    decoded = strict_json_loads("\n".join(data), maximum_bytes=1 << 20)
    if not isinstance(decoded, Mapping):
        raise NaatreClientError("CLIENT_STREAM_INVALID")
    frame_type = decoded.get("type")
    stream = decoded.get("stream")
    sequence = decoded.get("sequence")
    if not isinstance(frame_type, str) or not isinstance(stream, str):
        raise NaatreClientError("CLIENT_STREAM_INVALID")
    if event != f"naatre.{frame_type}":
        raise NaatreClientError("CLIENT_STREAM_INVALID")
    if sequence is not None and type(sequence) is not int:
        raise NaatreClientError("CLIENT_STREAM_INVALID")
    cursor = decoded.get("cursor")
    if (event_id is None) != (cursor is None) or event_id is not None and event_id != cursor:
        raise NaatreClientError("CLIENT_STREAM_INVALID")
    return StreamFrame(
        type=frame_type,
        stream=stream,
        sequence=sequence,
        value=cast(Mapping[str, JSONValue], decoded),
    )
