# Conformance profiles and evidence

[`v1/profiles.json`](v1/profiles.json) is the authoritative, versioned profile
registry. It defines separate claims for a wire codec, client, HTTP server,
native execution runtime, streaming transport, persisted-operation component,
schema tooling, federation component, extension host, remote worker, and stable
release. A profile entry names every required specification document, stable
normative clause, fixture and fixture digest. Profile requirements are
cumulative: an experimental label or unsupported optional capability never
removes a required clause or fixture.

The profile registry and fixture suite are independently versioned. Changing a
profile boundary changes `registryVersion`; changing portable fixture content
changes `fixtureVersion`. Reports bind both versions, the v1 specification,
the runner protocol, canonicalization version, and the exact schema revision.
Any mismatch is an infrastructure failure, not a skipped or passing test.

## Evidence roles

Profiles certify only their named role. In particular:

- `sdk.client-1` proves client behavior even when the destination is Go.
- `worker.remote-1` proves a gateway-to-worker path, not a native runtime.
- `runtime.execution-1` requires an eligible path whose destination is
  `native-runtime`.
- HTTP, streaming, persisted-operation, federation, extension-host, and schema
  tooling evidence remain distinct from codec, client, worker, and runtime
  evidence.

Delegation never promotes a client or worker report into execution-runtime
evidence. A report may claim a profile only when its result is `passed`, its
evidence role and source/destination kinds are eligible, and its executed
clauses and fixture digests exactly equal that profile's registry entry.
Partial runs remain useful named evidence but produce no profile claim.

## Result states and skips

The profile-report protocol has five disjoint result states:

- `passed`: every required test in the result's scope executed and passed.
- `failed`: the implementation produced a conforming test failure.
- `unsupported`: only an optional capability declared by the profile is not
  implemented. A required profile or fixture cannot use this state.
- `invalid-skip`: a required fixture was skipped, a skip had no bounded public
  reason, or the skip did not match an optional capability.
- `infrastructure-failure`: the run could not establish trustworthy evidence,
  including unavailable tools, corrupt artifacts, or version skew.

Unsupported required tests, invalid skips, failures, infrastructure failures,
and incomplete runs are non-certifying. Discovery may still report an entire
runner profile as unsupported; that discovery fact is not an optional-
capability result and is never a pass.

Every profile requires positive, negative, malformed, limit, cancellation, and
security fixture classes. The registry pins the exact fixture files while
`v1/suite.json` maps their stable clauses to exact JSON pointers and polarity.

## Publishable reports

[`profile-report.schema.json`](profile-report.schema.json) defines
`naatre.conformance.report-1`. Reports contain only bounded identification,
version, environment, claim, diagnostic, command, artifact-digest, clause, and
fixture-digest fields. They do not contain request payloads, response data,
credentials, arbitrary environment variables, or open metadata objects.

A report records:

- implementation name and version, language and runtime version;
- exact spec, fixture, profile, runner, canonicalization, and schema versions;
- OS, architecture, feature flags, wire and stream transports, scalar
  precision, and cancellation capabilities;
- who ran it, a credential-free argument-vector command, and SHA-256 artifact
  identities;
- the tested path, exact executed clauses and fixtures, result states, and the
  narrower set of passing claims.

Use the dependency-free semantic validator before publication:

```sh
node conformance/independent/profiles.mjs
node conformance/independent/profiles.mjs --report report.json
```

The first command validates the registry, report schema digest, compatibility
matrix, result-state model, and negative claim/version/security cases. The
second additionally validates one report. Generic JSON Schema validation
against `profile-report.schema.json` checks its closed portable shape; the
independent validator enforces cross-file equality and exact-coverage rules
that JSON Schema cannot express.

## Compatibility matrix and stable release

[`v1/compatibility.json`](v1/compatibility.json) is the public compatibility
matrix. It has explicit official rows for Go, JavaScript/TypeScript, PHP,
Python, Rust, JVM, .NET, Swift, Dart, and Ruby, plus a third-party submission
surface. Rows without complete reports remain `planned`, with empty claims and
reports, so repository location or a self-declared partial run cannot appear as
certification.

`release.stable-1` requires at minimum `runtime.execution-1` evidence from the
Go runtime and `sdk.client-1` evidence from the JavaScript/TypeScript client.
Issue #69 owns executing the full cross-language/cross-profile matrix,
publishing the resulting reports, and changing the release matrix from
`awaiting-complete-evidence`. This issue defines and verifies that contract; it
does not fabricate the deferred evidence.
