# Authenticated schema discovery

`schema.discovery.http-1` is the opt-in Go 1.27 `net/http` adapter for the
filtered discovery contract in `core.schema-1`. The portable document,
visibility rules, stable-ID diff classifications, and canonical hashes remain
owned by issue #15 and `spec/v1/schema.md`; this adapter does not define a
second schema model.

Issue #106 owns only these runtime surfaces:

- `GET /v1/schema` returns the canonical schema document after authentication
  and one deny-by-default authorization decision.
- `GET /v1/schema/diff?from=<revision>` applies that same decision to a retained
  revision and the current immutable revision, then returns the existing
  `schema.SchemaDiff` in a versioned migration envelope.
- `transport/http.RevisionStore` atomically selects one current document and
  retains a finite number of earlier immutable documents for migration reports
  and rollback.

Construction is disabled unless `DiscoveryConfig.Enabled` is true. Importing
the package, freezing a registry, or constructing the disabled handler never
creates a listener or network binding. An enabled application must provide all
three deployment-owned boundaries: an immutable `DiscoverySource`, an
authenticator, and an authorizer. The authenticator derives tenant and
principal from trusted state. The authorizer returns an explicit `Visibility`
allow-list and a non-empty policy revision; omission denies discovery.
`AuthenticationChallenge` is also required and supplies the deployment's safe
`WWW-Authenticate` value for rejected credentials.

## Lifecycle and rolling revisions

Build and validate the next complete `schema.Document` before installation.
Install the same revision into the application runtime and the finite
`RevisionStore`; never mutate an installed document. `Install` is idempotent
for byte-identical documents and rejects another document that reuses a
retained revision. `Rollback` selects only a retained revision. Evicted
revisions are unavailable instead of being fetched from a request-controlled
location.

The core schema contract caps portable revision identifiers at 128 bytes, so
`DiscoveryLimits.MaxRevisionBytes` may keep that default or impose a smaller
deployment limit; handler construction rejects larger values. `RevisionStore`
accepts every portable revision, while the handler applies any smaller limit.

Each request captures the current document once. A concurrent install or
rollback cannot mix the schema, diff target, authorization decision, or ETag
from different revisions. Migration diffs are computed only after both
documents use the same visibility decision, so removed declarations and
change payloads cannot bypass filtering.

Successful ETags are opaque HMAC identities keyed independently by each
handler process. They cover the profile, schema revision, tenant, principal,
policy revision, and filtered response bytes without exposing those inputs.
They are stable only for the lifetime of that handler and are not portable
cross-process validators.

The default limits are 32 concurrent requests, 128 revision bytes, 1 MiB per
input schema document, 1 MiB of successful response data, and 4,096 migration
changes. Applications must also choose a finite `RevisionStore` capacity.
Documents over the input limit are rejected before filtering or diff
construction. Cancellation reaches authentication, authorization, and source
callbacks through the request context. Limit,
cancellation, authentication, authorization, lookup, and internal failures use
stable public codes and never serialize callback causes, credentials,
principal or tenant references, policy metadata, hidden declarations, or Go
implementation details.

Public failure mappings are:

| Code | HTTP status |
| --- | ---: |
| `DISCOVERY_DISABLED` | 404 |
| `DISCOVERY_NOT_FOUND` | 404 |
| `DISCOVERY_METHOD_NOT_ALLOWED` | 405 |
| `DISCOVERY_BAD_REQUEST` | 400 |
| `DISCOVERY_UNAUTHENTICATED` | 401 |
| `DISCOVERY_FORBIDDEN` | 403 |
| `DISCOVERY_REVISION_NOT_FOUND` | 404 |
| `DISCOVERY_CANCELLED` | 408 |
| `DISCOVERY_BUSY` | 503 |
| `DISCOVERY_LIMIT_EXCEEDED` | 500 |
| `DISCOVERY_INTERNAL` | 500 |

`DISCOVERY_REVISION_CONFLICT` is returned only by `RevisionStore.Install` and
has no HTTP status in this profile.

## Runtime and capability boundary

The supported implementation is the dependency-free standard-library adapter
in the public Go 1.27 `transport/http` package. It is a portable Go library;
this profile records no native operating-system or architecture certification.
Deployment TLS, listener ownership, routing, authentication mechanisms, and
policy storage remain application responsibilities.

The following optional capabilities are explicitly unsupported by this
profile: anonymous or public discovery; unfiltered discovery; schema mutation
or registration over HTTP; remote revision fetching; persistent or
cross-process revision history; automatic synchronization with a process
controller; watch, subscription, push, SSE, or WebSocket discovery; general
operation execution; shared/public response caching; conditional
`If-None-Match`/`304` responses; `Accept` content negotiation; compression
negotiation; CORS policy; cookie authentication and CSRF policy;
framework-specific adapters; native or non-Go runtime adapters; cross-language
migration code generation; and deployment or certification claims for any OS,
architecture, proxy, or orchestrator.

## Reproducible checks

From the repository root, using the exact `core.schema-1`, `core.http-1`, Go
module, and implementation digests recorded in
`conformance/v1/schema-discovery.json`:

```sh
go test ./transport/http -run 'TestDiscovery|TestRevisionStore' -count=1
go test -race ./transport/http -run 'TestDiscovery|TestRevisionStore' -count=1
go test ./internal/conformance -run 'TestSchemaDiscoveryProfile|TestPortableSchemaDiffFixtures' -count=1
```

The portable evidence includes positive, negative, boundary, active-
cancellation, concurrency, response-size, diff-size, retention, revision-
conflict, filtering, and failure-redaction cases. `conformance/v1/schema.json`
remains the semantic-diff authority; this profile's executed migration vector
records removal, input/nullability tightening, openness, defaults, effects, and
constraint changes against that authority.
