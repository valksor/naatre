# Naatre Python remote-worker bindings

`naatre.worker` is the dependency-free Python handler layer for the
`worker.remote-1` boundary. It supplies explicit generated sync and async
interfaces, strict schema codecs, bounded blocking execution, request context,
transaction/resource hooks, and credit-gated async sources. It is a remote
worker implementation: planning and public completion remain in the Naatre
gateway, so this package does not claim a Python native-runtime profile.

The source and typing floor is CPython 3.11. The supported CPython range is
3.11 through 3.14; the checked offline evidence in this repository executes on
the available CPython runtime. `asyncio` is the supported event loop. Trio,
Curio, custom event-loop policies, process isolation, and hard thread
termination are not claimed.

## Registration and values

Generate the checked server interface from the shared model:

```sh
PYTHONPATH=sdk/python/src python3 -m naatre.servergen \
  conformance/v1/generator-model.json \
  sdk/python/src/naatre/generated/server.py
```

Each generated operation has distinct `Protocol` types for sync and async
handlers, typed input and output dataclasses, strict codecs, and explicit
`bind_*_sync` / `bind_*_async` functions. A `Registry` rejects duplicate or
late registration. The base implementation uses only dataclasses and typing;
it never imports Pydantic. `PydanticCodec` is an optional protocol adapter that
forces strict, extra-forbidden validation for both decoded inputs and encoded
outputs without adding Pydantic as a package dependency. FastAPI and Starlette
convenience adapters are owned by issue #99.

Strict codecs preserve missing separately from null, require exact Boolean,
Int32, and Float64 host types, carry extended integers and Decimal as strings,
validate timestamp and unpadded base64url bytes spellings, and validate tagged
unions before transmission. Both inputs and outputs pass through canonical
JSON validation. Unknown input fields, coercions, invalid output shapes, and
closed-union tags fail before handler execution or gateway transmission.

## Cancellation, request context, and streaming

Async handlers run in isolated tasks and are cancellable. Sync handlers run
only in the configured bounded executor. Cancelling their await sets the
request cancellation signal but does not claim to stop a running thread; its
executor and invocation slots remain held until the function actually returns.

Every handler receives `HandlerRequest` with verified identity, absolute
deadline, trace fields, cancellation signal, and fresh request-local loader and
value dictionaries. `current_request()` exposes the same object through a
context variable while the handler or source pull is active. The runtime starts
each task/thread in a fresh context and resets it on all exits.

`bind_stream` is usable only when `server-streaming-1` is negotiated. A
`WorkerStream` does not pull its source without frame credit, validates and
bounds each encoded item against byte credit, and owns `aclose()`. Completion,
failure, explicit close, cancellation, disconnect, drain, and shutdown close
the source and release its transaction and invocation state.

## ASGI and application resources

`WorkerASGI` is the dependency-free ASGI callable for unary worker envelopes.
Its lifespan starts resources, drains active invocations, then closes resources
in reverse ownership order. HTTP disconnect cancels async work and initiates
the same cleanup. The application supplies finite connection pools as
`LifecycleResource` objects and transactions through `TransactionProvider`;
the worker does not create hidden global pools or transactions.

`Worker.reload(new_registry)` first stops admission and drains the old registry,
then atomically admits the frozen replacement. Shutdown rejects new work,
cancels owned async work, waits for still-running executor work, drains pools,
and closes them. FastAPI and Starlette convenience constructors are deliberately
left to issue #99; they compose this ASGI callable rather than defining another
runtime.

Run the framework-neutral and ASGI example without binding a listener:

```sh
PYTHONPATH=sdk/python/src python3 examples/python-worker/app.py
PYTHONPATH=sdk/python/src python3 examples/python-worker/asgi_probe.py
GOCACHE=/tmp/naatre-go-cache go run ./examples/python-worker
PYTHONPATH=sdk/python/src python3 conformance/independent/verify-python-worker.py
```

The Go program is the complete cross-language example: the reference Go gateway
starts the Python worker over the shared length-delimited stdio test transport,
registers it, verifies and invokes its generated handler, validates completion,
and drains the subprocess without opening a listener. The verifier exercises
the shared `worker.remote-1` reference scenarios and
the negotiated `server-streaming-1` capability. It is implementation evidence
for the Python worker slice only. The combined Go-gateway-to-Python report and
official multi-runtime matrix remain owned by issue #69.
