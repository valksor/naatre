# Go HTTP digest integration profile

`implementation.go.http-digest-1` is the Go implementation surface for the
normative [`core.http.digest-1`](../spec/v1/http-digest.md) contract. Issue #63
owns that contract, its RFC 9530/RFC 9651 interpretation, algorithms, byte
scopes, and stable clauses. The inward `protocol/httpdigest` package is the
single Go implementation of those shared primitives. Issue #104 owns only the
`transport/http` compatibility surface and middleware, the opt-in Go `client`
completion gate, and their executable evidence. This implementation does not
define a second digest header, schema, or protocol profile.

## Packages and lifecycle

`protocol/httpdigest` has no listener, transport lifecycle, credentials, or
application runtime ownership. Both outward packages consume it without
reversing the documented dependency direction. `transport/http` retains the
public digest field aliases and functions used by the #63 profile.

`transport/http.NewDigestMiddleware` wraps an application handler. The
recommended `CoreDigestMiddlewareConfig` rejects malformed negotiation and
digest headers before body reads, stages bounded request content, verifies the
content and representation before calling the protected handler, buffers a
bounded response, and commits it only after generating or reconciling both
digest fields. The application still owns listener/TLS setup, authentication,
authorization, handler recovery, response content creation, and shutdown.

For `206`, the middleware hashes only returned content for `Content-Digest`.
It never relabels those bytes as the complete representation. An origin that
requires `Repr-Digest` supplies a trusted `RangeRepresentationDigest` derived
from the complete selected representation and retains ownership of validators,
variant identity, and source consistency. Multipart byte-range support covers
the exact outer content only.

`client.Config.HTTPDigest` opts the Go SDK into the same profile. The client
adds request `Content-Digest` and `Repr-Digest`, negotiates both response
fields, rejects a missing selected algorithm as `DIGEST_DOWNGRADE`, verifies
all present allowed values, and withholds parsing and successful completion
until verified EOF. Successful `Result.Integrity` contains only profile,
algorithm, byte-count, and range evidence. A `206` requires an explicit
`RangeRepresentation` source for the complete representation. Digest-enabled
clients stop at redirects so an unverified intermediary response is never
silently crossed; an application may verify and issue a separate request.

Request and response objects are per exchange. Staging buffers and hash state
are released when the handler or `Execute` returns. Context cancellation is
checked while reading and maps to `DIGEST_CANCELED`. Public failures contain
only a stable code and phase; they do not retain or serialize bodies, raw
digests, credentials, arbitrary headers, callback errors, or transport details.

## Runtime and capability boundary

The source-supported runtime is Go 1.27 `net/http` on platforms supported by
that Go release. The checked-in evidence records an executed Go 1.27.1
Darwin/arm64 run. It proves in-process HTTP message behavior, not a listener,
TLS stack, proxy, kernel, HTTP/2, HTTP/3, native mobile, browser, framework, or
cloud certification.

The implemented representation codings are identity and gzip. Single byte
ranges and the exact outer content of multipart byte ranges are supported.
Configurable fixed content and representation limits are mandatory for active
gates. Optional mode verifies or emits a field only when the peer presents its
field or Want field; required mode enforces the complete gate.

Every optional capability not implemented by this profile is explicit:

- digest values arriving only in trailers or any dependency on trailer
  preservation;
- whole-content digests for indefinite SSE, WebSocket frame integrity,
  bidirectional streams, response flushing, HTTP full duplex, hijacking, push,
  and long-lived streaming writers;
- brotli, zstd, or other representation decoding beyond identity and gzip;
- multipart per-part digest parsing, resumable-upload chunk assembly, durable
  object promotion, object-store checksum substitution, and automatic fetch of
  a complete representation for range verification;
- automatic following of digest-protected redirects, cache insertion, proxy
  transformation/recomputation, webhook signatures, and HTTP Message
  Signatures;
- non-Go SDK transports, framework adapters, native/mobile/browser runtimes,
  cross-language certification, and HTTP/3 transport certification.

These are unsupported, not silently downgraded or inferred from the Go package.
The relevant host component may implement a separate higher-level lifecycle
while continuing to obey the #63 byte and phase contract.

## Reproducible evidence

[`http-digest-integration.json`](../conformance/v1/http-digest-integration.json)
is machine-readable evidence. It pins the exact #63 dependency commit and the
SHA-256 of every normative dependency and implementation/test file, enumerates
the positive, malformed, boundary, cancellation, limit, and security cases,
and records the supported and unsupported runtime boundary.

Run from the repository root:

```sh
go test ./transport/http -run TestDigestMiddleware -count=1
go test ./client -run 'TestExecute(VerifiesHTTPDigest|DigestFailures|VerifiesRangeRepresentation)' -count=1
node conformance/independent/http-digest-integration.mjs
```

The Node command is dependency-free and verifies every pinned revision, closed
case phase/code shape, required fixture class, command safety, and capability
boundary. The Go commands execute the middleware and SDK gates without binding
a listener.
