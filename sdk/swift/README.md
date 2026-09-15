# Swift SDK core and Apple adapters

`sdk.swift.core-1` is the reflection-free Swift Package Manager client core and
generated Codable binding for the shared Naatre generator model. It requires
Swift 6.0 or newer and declares iOS 17, macOS 14, tvOS 17, watchOS 10, and
visionOS 1 as its source and package compatibility floor.

The generated surface preserves missing, explicit null, pending, present,
failed, and skipped states through `Selected`; optional inputs use `Input` so
omission is distinct from explicit null. Open enums retain unknown wire values,
and open unions use `OpenVariant` to retain both the discriminator and payload.
Generated decoding is ordinary keyed Codable code and performs no runtime
reflection.

Extended integer, decimal, timestamp, duration, UUID, byte, and URL values are
string-backed canonical wrappers. Foundation `Decimal`, `Date`, `UUID`, `Data`,
and `URL` accessors are conveniences only: the original lossless wire value is
authoritative. Timestamp normalization uses an explicit proleptic Gregorian
calculation and POSIX formatting, so locale, calendar, and device time zone do
not affect encoded values.

Regenerate and verify the checked artifacts from the repository root:

```sh
GOCACHE=/tmp/naatre-swift-go-cache go generate ./sdk/swift
git diff --exit-code -- sdk/swift/Sources/NaatreGenerated
SWIFTPM_MODULECACHE_OVERRIDE=/tmp/naatre-swift-module-cache \
  swift build --disable-sandbox --package-path sdk/swift
SWIFTPM_MODULECACHE_OVERRIDE=/tmp/naatre-swift-module-cache \
  swift run --disable-sandbox --package-path sdk/swift naatre-swift-conformance
node conformance/independent/verify-swift-apple-adapters.mjs
```

## Package and ownership boundaries

| Product | Owner | Lifecycle |
| --- | --- | --- |
| `NaatreCore` | #42, `sdk.swift.core-1` | Stable transport-neutral protocol and value authority |
| `NaatreGenerated` | #42, `sdk.swift.core-1` | Deterministic Codable output generated only from the shared model |
| `NaatreApple` | #86, `sdk.swift.apple-1` | Additive Apple Foundation adapter; depends on `NaatreCore` and defines no schema |
| `naatre-swift-conformance` | #42 | Core executable profile |
| `naatre-swift-apple-conformance` | #86 | Listener-free URLSession and Apple-boundary profile |

The adapter profile pins #42 at
`f6b77807cc762893020e726e8ae5221b58bf5309`. #42 remains the sole protocol,
Codable, scalar, error-envelope, and generated-operation authority. #86 owns
only concrete Apple transport and lifecycle integration.

## Transport boundary

`NaatreClient` supplies async execution, deadline racing, task and
`AsyncSequence` cancellation, bounded response/frame handling, authentication
hooks, background cancellation policy, pagination values, redirect credential
stripping, and typed unsupported-capability errors. A conforming transport must
actively release work from `cancel(requestID:)` and `cancelAll()`.

The separate `NaatreApple` product provides an HTTPS-only `URLSessionTransport`
for unary JSON POST and authenticated POST-SSE. URLSession tasks are owned by
request ID; task cancellation, sequence termination, explicit cancellation,
and background policy all actively cancel the underlying URLSession work.
Responses and SSE frames are bounded while bytes are read, and the returned
sequence holds at most 16 frames before failing with
`CLIENT_STREAM_BUFFER_FULL`. Only 307 and 308
redirects are followed, the redirect count is bounded, and credentials and
tenant metadata are stripped across origins unless the destination is
explicitly trusted. Cookie persistence and URL caching are disabled.

`AppleLifecycleAdapter.applicationDidEnterBackground()` is the explicit bridge
from an application or scene lifecycle callback to the core cancellation
policy. The package does not observe UIKit, AppKit, WatchKit, or SwiftUI state
automatically, because the application owns which transition is a true
background boundary.

No automatic auth refresh or retry is provided, so a non-idempotent write is
never silently replayed. Normal stream completion requires an explicit terminal
frame; source completion without it is `CLIENT_STREAM_TRUNCATED`.

## Compatibility evidence

The checked [macOS arm64 report](../../conformance/reports/swift-apple-macos-arm64.json)
records the executed package build and profile command. The package manifest
declares the complete source policy:

| Swift | Platform | Minimum | Core package | Native adapter |
| --- | --- | --- | --- | --- |
| 6.0+ | macOS | 14 | passed on macOS 26.6.2 arm64 with Swift 6.4 | URLSession/SSE and exact scalar/time vectors passed |
| 6.0+ | iOS | 17 | source compatibility declared | Same scalar/time entrypoint required; runtime execution remains unclaimed without an iOS simulator report |
| 6.0+ | tvOS | 17 | source compatibility declared | Runtime adapter execution unclaimed |
| 6.0+ | watchOS | 10 | source compatibility declared | Runtime adapter execution unclaimed |
| 6.0+ | visionOS | 1 | source compatibility declared | Runtime adapter execution unclaimed |

No unexecuted platform/transport combination is marked passed. The local
Command Line Tools environment has no iOS SDK or simulator; with full Xcode,
build the same package and execute the exact vectors through an iOS host before
adding an iOS report. #69 owns the complete official-SDK comparison.

Unsupported optional capabilities are WebSocket, SSE automatic reconnect and
`Last-Event-ID` replay, automatic authentication refresh, automatic mutation or
subscription replay, non-HTTPS endpoints, 301/302/303 redirect rewriting,
caller-invisible proxy or TLS policy, persistent cookies and URL cache, a
compressed-wire byte limit distinct from URLSession's decoded bytes, automatic
platform-notification observation, background execution entitlement handling,
Combine adapters, and native server-runtime, device, simulator, framework, or
deployment certification beyond the checked report.
