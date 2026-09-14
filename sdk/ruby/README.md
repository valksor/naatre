# Ruby SDK core

This directory is the dependency-free `sdk.ruby.core-1` reference client. It
supports CRuby 2.6 and newer and provides immutable operation values, generated
selected-result shapes, exact scalar codecs, strict response decoding,
persisted requests, safe retry eligibility, and transport-independent stream
terminal validation. Ruby string and symbol keys normalize to one string-keyed
wire model; a collision is rejected.

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
git diff --exit-code -- sdk/ruby/lib/naatre/generated sdk/ruby/generated sdk/ruby/sig/generated.rbs
```

## Runtime and transport matrix

| Runtime or transport | Status | Executed report |
| --- | --- | --- |
| CRuby 2.6.10, Darwin arm64, core | Passed | [`conformance/reports/ruby-core-cruby-2.6.10-darwin-arm64.json`](../../conformance/reports/ruby-core-cruby-2.6.10-darwin-arm64.json) |
| Other Ruby implementations | Unsupported | No conformance report |
| HTTP and redirect adapter | Unsupported until #83 | No conformance report |
| Rails integration | Unsupported until #83 | No conformance report |
| SSE transport | Unsupported until #83 | Core terminal/truncation decoder only |
| WebSocket transport | Unsupported until #83 | No conformance report |
| Automatic auth refresh and retry | Unsupported until #83 | `Operation#replayable?` never marks a mutation replayable |

Unsupported capabilities raise `CLIENT_CAPABILITY_UNSUPPORTED`. The core
enforces the common 8 MiB response, 16 MiB decompressed response, 1 MiB frame,
and five-redirect metadata limits, but makes no transport cancellation claim.
HTTP body closure, active cancellation, credential stripping across redirects,
timeouts, pagination transport, authenticated POST-SSE, optional WebSocket,
Rails hooks, and runtime-specific packaging reports belong to #83. The final
official SDK comparison remains owned by #69.

## Gem and Bundler policy

The gem has no runtime dependencies. `Gemfile` contains only the gemspec, files
are sorted explicitly, and builds must use the checked-in source with
`SOURCE_DATE_EPOCH` set to the commit timestamp. Release automation must build
twice in clean directories and compare SHA-256 digests before publishing. A
release must use a locked Bundler/RubyGems toolchain, require MFA, run the Ruby
profile, and publish only if generated files are byte-identical. This repository
does not publish a gem as part of #44.
