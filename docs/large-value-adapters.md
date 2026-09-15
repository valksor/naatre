# Go large-value transfer adapters

Package `github.com/valksor/naatre/largevalueadapter` implements profile
`implementation.go.large-value-adapters-1`. It consumes the capability,
metadata, digest, authorization, range, and cleanup contract from
`core.large-value-1`; package `largevalue`, `spec/v1/large-values.md`, and
`conformance/v1/large-values.json` remain the only protocol and schema
authority. This adapter package defines no wire envelope or alternative
capability.

## Adapter boundary

The supported Go 1.27 library surface is:

- `DirectHandler`, an in-process `net/http.Handler` for direct PUT, POST,
  PATCH, GET, and HEAD transfers. It enforces the methods signed into the
  capability, streams in 32 KiB copy buffers, applies conditional requests
  before a single byte range, and emits `private, no-store` responses.
- `PresignedAdapter`, which finalizes a presigned upload by streaming an
  approved GET response through the core size/digest/scanner gate, or exports a
  finalized download through one approved PUT. `PinnedTransport` receives the
  addresses resolved and approved immediately before every request and must
  dial one of those addresses without a second DNS lookup.
  `HTTPPinnedTransport` is the concrete HTTP/1.1 implementation: it disables
  proxies, redirects, connection reuse, and protocol upgrades, and passes only
  approved IP literals to its dialer.
- `Assembler`, a filesystem-backed multipart and resumable upload adapter. It
  verifies each exact offset, length, validator, and RFC 9530 digest before an
  atomic part rename, then streams a gap-free assembly through the core final
  digest gate. Session directory names are hashes; durable manifests never
  contain bearer references.
- `ApplicationAdapter`, which streams application-provided sources into the
  core gate and finalized content into application-provided transactional
  sinks.

The application owns `largevalue.Coordinator` construction, final object
storage, authentication and principal context, authorization and scanning,
capability issuance, route selection, assembly-root placement and durability,
high-entropy ID generation, TLS policy and pinned-dialer configuration,
credentials, server admission/timeouts/shutdown, quota charging, and the
cleanup scheduler.

## Lifecycle and cleanup ownership

Authorization is checked before direct staging, application source/sink open,
each presigned hop, each chunk write, assembly completion, and protected
download open. Incoming bytes remain untrusted until the core coordinator has
verified exact decoded length and representation digest, completed scanning,
reauthorized finalization, and committed the store stage. Handlers and
application sinks receive only finalized content.

The direct and presigned adapters close every body they open. The application
adapter closes sources and aborts a failed, cancelled, or uncommitted sink with
a cleanup context derived from `context.WithoutCancel` and bounded by
`CleanupTimeout`; a successful `Commit` transfers ownership to the
application. Core staging remains coordinator-owned until finalization and is
aborted by the core's independent cleanup context on every failed transfer.

For multipart and resumable sessions, the currently written temporary file is
coordinator-owned and removed on write failure or cancellation. Atomically
accepted part files are assembly-store-owned, remain quota-chargeable, and are
removed after finalization or explicit abort. `SweepExpired` processes a
caller-bounded deterministic directory page and deletes expired manifests plus
malformed crash-orphan directories only after `OrphanTTL`. The host must run
that sweep. One `Assembler` serializes metadata transitions; sharing one root
between processes is unsupported.

## Redirect and failure policy

`PresignedAdapter` never delegates redirects to `http.Client`. Each GET
starts only when its URL exactly matches the capability-bound `TransferURL`;
the same binding applies to the initial PUT target. Each GET redirect is
resolved relative to the prior URL, reauthorized, passed through
the `largevalue.EgressPolicy`, re-resolved, and delivered with the approved
address set to `PinnedTransport`. Authorization, cookie, proxy authorization,
and digest headers are removed when the origin changes. PUT redirects are
rejected because replaying an already-started protected stream would require
buffering or an ambiguous remote side effect.

Public failures expose only the stable codes in
`conformance/v1/large-value-adapters.json` and a fixed generic message. They do
not retain or render capabilities, URLs, query signatures, headers,
credentials, filenames, record metadata, response bodies, filesystem paths,
or backend errors. A response-stream failure after HTTP headers have started
terminates that response; it does not attempt to append a JSON diagnostic to
payload bytes.

## Runtime and capability matrix

This profile is a framework-neutral Go 1.27 library and handler contract.
Tests execute entirely in process with fake pinned transports and temporary
directories; no listener or network endpoint is required. It does not certify
an operating system, architecture, filesystem, object store, HTTP server,
proxy, TLS stack, deployment, or native runtime.

Unsupported optional capabilities are multipart HTTP range responses,
automatic presigned PUT redirects or retries, resumable downloads, parallel
part writes for one session, multi-process shared assembly roots, cross-process
file locking, distributed cleanup leadership, a bundled final object store,
S3/GCS/Azure vendor SDKs, provider-specific multipart APIs, automatic
credential acquisition or refresh, default DNS or proxy dialing, HTTP/2 or
HTTP/3 transport certification, WebSocket or SSE transfer, trailers-only
digests, brotli or zstd decoding, encryption at rest or key management,
malware-scanner implementation, antivirus signatures, quota billing,
framework middleware, browser/mobile/native bindings, non-Go runtimes,
cross-language certification, operating-system or architecture certification,
and deployment certification.

## Reproducible offline verification

From the repository root, with the Go 1.27 toolchain and the exact dependency
revision recorded in `conformance/v1/large-value-adapters.json`:

```sh
GOWORK=off go test ./largevalue ./largevalueadapter -count=1
GOWORK=off go test -race ./largevalue ./largevalueadapter -count=1
GOWORK=off go test ./internal/conformance -run 'TestLargeValueAdapterProfile|TestLargeValueTransportFixture|TestLargeValueEgressAndSecurityFixture' -count=1
GOWORK=off go vet ./largevalue ./largevalueadapter ./internal/conformance
GOWORK=off go generate ./...
```

The checked-in profile manifest binds these commands, every implementation
file, the normative specification and fixture, the core package revision, all
stable public codes, finite default limits, fixture classes, and the complete
supported/unsupported boundary.
