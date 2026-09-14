# Swift SDK core

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
```

## Transport boundary

`NaatreClient` supplies async execution, deadline racing, task and
`AsyncSequence` cancellation, bounded response/frame handling, authentication
hooks, background cancellation policy, pagination values, redirect credential
stripping, and typed unsupported-capability errors. A conforming transport must
actively release work from `cancel(requestID:)` and `cancelAll()`.

Concrete URLSession, authenticated POST-SSE, WebSocket, and Apple lifecycle
adapters are intentionally unsupported by this core package and owned by #86.
No automatic auth refresh or retry is provided, so a non-idempotent write is
never silently replayed. Normal stream completion requires an explicit terminal
frame; source completion without it is `CLIENT_STREAM_TRUNCATED`.

## Compatibility evidence

The checked [macOS arm64 report](../../conformance/reports/swift-sdk-macos-arm64.json)
records the executed package build and profile command. The package manifest
declares the complete source policy:

| Swift | Platform | Minimum | Core package | Native adapter |
| --- | --- | --- | --- | --- |
| 6.0+ | macOS | 14 | passed on macOS 26.6.2 arm64 with Swift 6.4 | unsupported until #86 |
| 6.0+ | iOS | 17 | declared; cross-SDK report owned by #69 | unsupported until #86 |
| 6.0+ | tvOS | 17 | declared; cross-SDK report owned by #69 | unsupported until #86 |
| 6.0+ | watchOS | 10 | declared; cross-SDK report owned by #69 | unsupported until #86 |
| 6.0+ | visionOS | 1 | declared; cross-SDK report owned by #69 | unsupported until #86 |

No unexecuted platform/transport combination is marked passed. #69 owns the
complete official-SDK comparison and #86 owns device/simulator adapter evidence.
