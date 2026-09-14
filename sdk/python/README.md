# Naatre Python SDK core

`naatre-sdk` is the dependency-free Python client and generated-model core for
`sdk.python.core-1`. It supports CPython 3.11, 3.12, 3.13, and 3.14. Every
documented version is checked with strict mypy settings; Python 3.11 defines
the syntax and typing floor.

The core exports frozen dataclasses, structural transport protocols, identical
sync/async request and result semantics, strict JSON, exact scalar codecs,
persisted operations, pagination primitives, bounded retries, SSE decoding,
and a framework-neutral lifecycle helper usable from ASGI lifespan hooks. HTTP
backends implement `SyncTransport` or `AsyncTransport`; no HTTP, event-loop,
ASGI, validation, or WebSocket framework is mandatory. Concrete async HTTP and
optional Pydantic integration belong to issue #81. ASGI server handlers belong
to issue #59.

`Decimal`, arbitrary integers, `Timestamp`, and bytes use explicit lossless
wire spellings. `bool` is rejected by integer codecs. `Timestamp` retains all
fractional-second digits and deliberately does not accept `datetime`, whose
microsecond ceiling can lose protocol precision. JSON rejects duplicate keys,
NaN, Infinity, unpaired surrogates, cycles, and integer values that should use
an extended-integer string codec. `MISSING` is a singleton distinct from
`None`; generated optional inputs and `Selected` outputs preserve that
difference. Unknown open enum and union values use `OpenEnum` and
`OpenVariant` wrappers.

Sync timeout values are passed to the owning backend. A timeout does not imply
that Python can actively cancel a blocking transport thread; the backend owns
that work until `execute` returns. `asyncio.CancelledError` is never translated
to an application failure. Async stream iterators own their source until normal
completion or `aclose()`, and always close the source in `finally`.

Generate the checked artifacts from the shared language-neutral model:

```sh
uv run naatre-sdkgen ../../conformance/v1/generator-model.json \
  ../../conformance/v1/generator-output.json src/naatre/generated
```

Verification and reproducible build commands:

```sh
uv sync --frozen
uvx --from pytest==9.1.1 pytest
uvx --from ruff==0.16.6 ruff check src tests
uvx --from mypy==2.3.1 mypy --python-version 3.11 src tests
uvx --from mypy==2.3.1 mypy --python-version 3.12 src tests
uvx --from mypy==2.3.1 mypy --python-version 3.13 src tests
uvx --from mypy==2.3.1 mypy --python-version 3.14 src tests
SOURCE_DATE_EPOCH=0 uv build
```

Setting the same `SOURCE_DATE_EPOCH` produces byte-identical source and wheel
archives from a clean tree. The package includes `py.typed` for downstream type
checkers.
