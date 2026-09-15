from __future__ import annotations

import asyncio
import sys
from typing import BinaryIO

from .json import canonical_json, strict_json_loads
from .worker import Worker, WorkerProtocolError


def serve_stdio(
    worker: Worker,
    *,
    reader: BinaryIO | None = None,
    writer: BinaryIO | None = None,
    maximum_frame_bytes: int = 65_536,
) -> None:
    """Serve length-delimited worker envelopes without opening a listener."""

    if not 1 <= maximum_frame_bytes <= 64 << 20:
        raise WorkerProtocolError("WORKER_CONFIG_INVALID")
    source = reader or sys.stdin.buffer
    destination = writer or sys.stdout.buffer
    loop = asyncio.new_event_loop()
    try:
        loop.run_until_complete(worker.start())
        while header := _read_exact(source, 5):
            if len(header) != 5 or header[0] != 0:
                raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
            size = int.from_bytes(header[1:], "big")
            if not 1 <= size <= maximum_frame_bytes:
                raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
            payload = _read_exact(source, size)
            if len(payload) != size:
                raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
            envelope = strict_json_loads(payload, maximum_bytes=maximum_frame_bytes)
            response = loop.run_until_complete(worker.handle_envelope(envelope))
            encoded = canonical_json(response)
            if len(encoded) > maximum_frame_bytes:
                raise WorkerProtocolError("REMOTE_INVOCATION_INVALID")
            destination.write(bytes((0,)) + len(encoded).to_bytes(4, "big") + encoded)
            destination.flush()
    finally:
        loop.run_until_complete(worker.aclose())
        loop.close()


def _read_exact(reader: BinaryIO, size: int) -> bytes:
    result = bytearray()
    while len(result) < size:
        chunk = reader.read(size - len(result))
        if not chunk:
            break
        result.extend(chunk)
    return bytes(result)
