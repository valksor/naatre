# Go HTTP adapter

`transport/http` owns the Go standard-library `http.Handler` binding for unary
`core.http-1` POST execution. `NewHandler` returns a complete handler for
`/v1/execute`; it does not require or mutate an application router. The caller
owns listener construction, TLS, authentication policy, trusted principal and
tenant context, process supervision, logging, and graceful listener shutdown.

The handler validates media types and singleton headers before decoding, bounds
compressed and decompressed request bytes independently, propagates disconnect,
deadline, and shutdown cancellation, emits only stable public failures, and
defaults every response to `Cache-Control: no-store`. Exact-origin CORS is an
explicit deployment policy. `RuntimeExecutor` connects an immutable runtime
snapshot to the adapter without making the Go implementation a second protocol
authority.

`NewSubscriptionHandler` separately implements the
`core.streaming-handles-1` browser binding at `/v1/subscriptions`. It
authenticates before decoding establishment bodies, delegates complete
operation/schema/cost authorization to `SubscriptionHandleCoordinator`, and
returns typed no-store handle metadata. Fetch delivery uses a bearer header and
singleton `Last-Event-ID`; native EventSource is limited to a same-origin
cookie with `Sec-Fetch-Site: same-origin` and a server-held initial cursor.
Cookie-authenticated state changes require exact-origin double-submit CSRF.
Every route rejects query strings and redirects.

The minimum runtime is Go 1.27. The checked conformance evidence records Go
1.27.1 on Darwin arm64; other operating-system, architecture, and Go patch
combinations are not claimed by that evidence. The same handler semantics are
executed for HTTP/1.1, HTTP/2, and HTTP/3 request metadata and do not depend on
connection fields, header casing, reason phrases, stream IDs, push, or
trailers. Actual HTTP/2 and HTTP/3 listener, TLS, QUIC, proxy, and certificate
configuration are host responsibilities and are not certified by this package.

Unsupported optional capabilities are persisted-operation GET, private or
shared response storage, conditional GET and ETag generation, response
trailers, and server push. General-purpose cookie authentication remains a
host policy; only the narrowly configured subscription-handle path owns its
same-origin cookie and CSRF boundary. Automatic unary `Retry-After` generation
is unsupported; the subscription handler emits a bounded delivery retry hint.
GET receives `405` with `Allow: POST, OPTIONS`; all POST responses remain
`no-store`. SSE framing is available separately, but stream lifecycle and
endpoint ownership remain outside the unary handler.

Run the adapter and machine-readable conformance evidence with:

```sh
go test ./transport/http ./runtime -count=1
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"http","command":"run","path":{"source":{"kind":"sdk","language":"go"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["core.http-1"]}' | go run ./cmd/naatre-conformance --require-pass
```
