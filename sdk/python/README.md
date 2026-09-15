# Naatre Python SDK core and adapters

`naatre-sdk` is the typed Python client package for the shared Naatre v1
contract. Issue #38 owns the dependency-free `sdk.python.core-1` request,
result, scalar, generated-model, and transport-protocol authority. Issue #81
owns the additive `sdk.python.adapters-1` implementation in `naatre.transport`
and the optional strict bridge in `naatre.pydantic`; neither defines another
wire model. Both profiles are pre-1.0 and follow the package lifecycle.

The core supports CPython 3.11, 3.12, 3.13, and 3.14. Its frozen dataclasses,
canonical sync/async clients, strict JSON, exact scalar codecs, persisted
operations, pagination, bounded retries, SSE decoder, and ASGI lifecycle helper
have no runtime dependencies. Python 3.11 remains the syntax and typing floor.

## Adapter boundary and lifecycle

`HTTPTransport` implements bounded unary HTTP(S) POST using the standard
library. It sends the canonical bytes supplied by `SyncClient`, accepts only the
versioned Naatre response media type and identity content encoding, rejects
redirects, bounds response bytes before acceptance, and closes every response.
`ThreadedAsyncTransport` gives any `SyncTransport` an `asyncio` view through a
dedicated `ThreadPoolExecutor`. `AsyncHTTPTransport` composes those two types.

`ExecutorLimits` bounds workers to 1-32 and queued submissions to 0-256. Work
over capacity fails with `CLIENT_EXECUTOR_SATURATED`; it is never retained in an
unbounded executor queue. Each submission runs in a fresh copy of the caller's
`contextvars.Context`, and worker mutations are discarded after that request.
Cancelling an await cancels the async wait. If the blocking call already began,
it continues under backend ownership until it returns; cancellation never
claims otherwise. `aclose()` rejects new work and cancels queued work without
waiting for already-running threads.

Adapter support is intentionally narrow:

| Surface | Published status |
| --- | --- |
| CPython 3.14.7 on Darwin arm64 | passed by `sdk.python.adapters-1` |
| CPython 3.11-3.13 and other operating systems | core supported; adapter certification not claimed until #69 executes it |
| `asyncio` with bounded blocking transports | supported |
| Unary HTTP/1.1 over `http:` or `https:` with identity responses | supported; production deployments should use `https:` |
| Pydantic 2.13.4 / pydantic-core 2.46.4 | optional strict adapter passed |

The base import never imports Pydantic. Applications that select
`naatre.pydantic` install Pydantic themselves at the revision recorded by the
adapter profile. `model_decoder(Model)` uses `strict=True` and
`extra="forbid"`, returns a decoder compatible with the core `Operation`, and
maps every validation failure to redacted `CLIENT_RESULT_INVALID`.
`model_variables(instance)` preserves unset-field omission, requires a JSON
object, and revalidates the value through the core canonical JSON authority.
Application models should also set `ConfigDict(strict=True, extra="forbid",
frozen=True)` so construction and mutation follow the same strict policy.

All adapter failures expose only stable public codes. Request bytes, endpoint
details, credentials, tenant headers, Pydantic validation input, exception
causes, and backend implementation messages are not retained in public errors.

## Unsupported optional capabilities

The adapter profile does not claim native non-threaded async sockets, Trio or
AnyIO execution, HTTP/2, HTTP/3, proxy configuration, automatic redirects,
cross-origin credential forwarding, gzip response decoding, cookies, automatic
authentication refresh, transport retry beyond the core query-only policy,
concrete POST-SSE transport, WebSocket transport, Pydantic v1, Pydantic settings
or dataclass plugins, ASGI server handlers, PyPy or native/mobile runtimes, or
official cross-platform certification. Issue #59 owns ASGI server handlers and
#69 owns the complete official SDK/runtime matrix.

## Generation and reproducible verification

Generate checked artifacts only from the shared language-neutral model:

```sh
uv run naatre-sdkgen ../../conformance/v1/generator-model.json \
  ../../conformance/v1/generator-output.json src/naatre/generated
```

From `sdk/python`, verify the dependency-free package:

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

From the repository root, after installing the exact optional dependency, run
the adapter evidence profile:

```sh
python3 -m pip install pydantic==2.13.4
python3 conformance/independent/verify-python-sdk-adapters.py
```

`conformance/v1/python-sdk-adapters.json` pins the #38 source commit, the exact
core fixture digest, Pydantic and pydantic-core revisions, runtime/platform,
commands, vectors, source evidence, and unsupported capabilities. Setting the
same `SOURCE_DATE_EPOCH` produces byte-identical source and wheel archives from
a clean tree. The package includes `py.typed` for downstream type checkers.
