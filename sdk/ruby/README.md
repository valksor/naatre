# Ruby SDK

This directory is the dependency-free `sdk.ruby.core-1` reference client. It
supports CRuby 2.6 and newer and provides immutable operation values, generated
selected-result shapes, exact scalar codecs, strict response decoding,
persisted requests, safe retry eligibility, and transport-independent stream
terminal validation. Ruby string and symbol keys normalize to one string-keyed
wire model; a collision is rejected.

Issue #83 adds the dependency-free `sdk.ruby.adapters-1` client contract. It
owns immutable HTTP requests, bounded unary and authenticated POST-SSE
responses, 307/308 redirect policy, cross-origin credential stripping,
cooperative cancellation, deterministic body/enumerator cleanup, bounded query
pagination, and optional Faraday and Rails load paths. The main `naatre`
require loads no framework gem.

The runtime maps arbitrary and 64-bit integers without a float conversion,
maps `BigDecimal` through canonical decimal strings, normalizes `Time` through
UTC without consulting the ambient time zone or locale, and provides lossless
`Timestamp`, `Duration`, `UUID`, and binary `Bytes` wrappers. Open enums retain
unknown values as `Naatre::UnknownVariant`. Selected fields distinguish
missing, null, pending, present, failed, and skipped states, while operation
results preserve partial data and structured errors together.

Regenerate and verify the checked-in bindings from the repository root:

```sh
ruby sdk/ruby/sdkgen/cli.rb
ruby -Isdk/ruby/lib -Isdk/ruby/test -e 'Dir["sdk/ruby/test/**/*_test.rb"].sort.each { |file| require File.expand_path(file) }'
node conformance/independent/verify-ruby-adapters.mjs
git diff --exit-code -- sdk/ruby/lib/naatre/generated sdk/ruby/generated sdk/ruby/sig/generated.rbs
```

## Runtime and transport matrix

| Runtime or transport | Status | Executed report |
| --- | --- | --- |
| CRuby 2.6.10, Darwin arm64, core | Passed | [`conformance/reports/ruby-core-cruby-2.6.10-darwin-arm64.json`](../../conformance/reports/ruby-core-cruby-2.6.10-darwin-arm64.json) |
| CRuby 2.6.10, Darwin arm64, framework-neutral adapter contract | Passed | [`sdk.ruby.adapters-1`](../../conformance/v1/ruby-adapters.json) |
| CRuby 2.6 and newer | Supported package/runtime floor; not separately certified | The executed 2.6.10 profile is the evidence floor |
| JRuby, TruffleRuby, Windows, Linux, other macOS architectures | Unclaimed | No executed report |
| Injected unary and enumerable POST-SSE adapters | Supported | Offline fake-adapter fixtures, no listener |
| Faraday `run_request` integration | Implemented optional contract; gem/runtime versions unclaimed | Offline Faraday-compatible fixture |
| Rails Railtie and Rack response-lifecycle middleware | Implemented optional contract; gem/runtime versions unclaimed | Offline Rails/Rack-compatible fixture |
| WebSocket transport | Unsupported | No implementation or report |
| Automatic auth refresh and retry | Unsupported | Authentication is one explicit hook call; no operation is replayed |

The client enforces the common 8 MiB response, 1 MiB SSE frame, and five
redirect limits. Cancellation closes a returned response body and halts future
pulls, but cannot claim that a blocking adapter call or a remote side effect has
stopped. Breaking or raising from stream enumeration always cancels and closes
the owned body. Clients are immutable, cancellation is request-owned, Rails
stores it in the Rack environment, and no thread/fiber local or mutable global
registry is used; failed and cancelled calls do not poison a persistent client.

Transport failures expose only stable `CLIENT_*` codes. Backend exceptions,
status bodies, credentials, protected headers, tenant/principal metadata, and
implementation details are never copied into public messages. Cross-origin
redirects require an exact allowlisted origin and always strip authorization,
cookies, CSRF, event cursor, tenant, and principal headers.

## Optional adapters

Pass any object implementing `call(request, cancellation)` to
`Naatre::Transport::Client`. It must return `Naatre::Transport::Response` (or a
response-shaped object); the client owns response closure.

```ruby
require "naatre/faraday"

adapter = Naatre::FaradayAdapter.new(Faraday.new)
client = Naatre::Transport::Client.new(
  endpoint: "https://naatre.internal/v1",
  adapter: adapter,
  authenticate: ->(headers) { headers.merge("Authorization" => "Bearer ...") }
)
result = client.execute(Naatre::Generated.get_account(id: "acct-1"))
```

`require "naatre/rails"` defines `Naatre::Rails::Middleware` and, when
Railties are present, registers it automatically. The middleware gives each
Rack request an independent `Naatre::Transport::Cancellation` at
`env["naatre.cancellation"]` and cancels it when the response body closes.

## Ownership, lifecycle, and unsupported capabilities

Issue #44 owns `sdk.ruby.core-1`, the generated schema/wire values, scalar
mapping, and protocol authority at commit
`0009b4027f85f3130e842ab7dbb68e43ed9686a1`. Issue #83 owns only
`sdk.ruby.adapters-1` and depends on the exact core fixture digest recorded in
[`ruby-adapters.json`](../../conformance/v1/ruby-adapters.json). Fixture suite
`1.0.0` and the checked hashes define the profile lifecycle; changing the core
wire contract requires a core-profile revision. Issue #69 retains complete SDK
and platform certification ownership.

Unsupported optional capabilities are concrete Faraday/Rails/Rack version
certification, active interruption of a blocking Faraday call, proof that a
remote mutation stopped, response decompression, WebSocket, HTTP/2 or HTTP/3
semantics, QUIC, TLS/certificate/proxy policy beyond the injected adapter,
credential forwarding across origins, 301/302/303 rewriting, automatic retry,
auth refresh, reconnect/replay, durable cursor storage, client-side batching,
multipart upload, Rails server handlers, background jobs, ActiveSupport
instrumentation, non-CRuby runtimes, native/mobile bindings, deployment
certification, and the complete official cross-language matrix. Unsupported
configured adapters raise `CLIENT_CAPABILITY_UNSUPPORTED`; other unavailable
behavior is not advertised.

## Gem and Bundler policy

The gem has no runtime dependencies. `Gemfile` contains only the gemspec, files
are sorted explicitly, and builds must use the checked-in source with
`SOURCE_DATE_EPOCH` set to the commit timestamp. Release automation must build
twice in clean directories and compare SHA-256 digests before publishing. A
release must use a locked Bundler/RubyGems toolchain, require MFA, run the Ruby
profile, and publish only if generated files are byte-identical. This repository
does not publish a gem as part of #44.
