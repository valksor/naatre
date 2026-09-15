# GraphQL adapter

`graphqladapter` is the optional Go 1.27 reference integration for
`core.adapters.graphql-1`. It maps a bounded GraphQL September 2025 subset to
the existing `schema`, `protocol`, `runtime`, and `interopadapter` packages. It
does not define another schema or protocol authority.

## Ownership and lifecycle

Issue #56 owns the normative `core.adapters-1` fidelity and policy contract.
Issue #92 owns this GraphQL package, its fixture, and its runtime integration.
The profile is versioned and experimental in v1: changes to supported syntax or
mapping semantics require a fixture-suite revision, and a different upstream
GraphQL specification requires a distinct profile report. Applications own
their schema revision, resolver code, policy, authentication, and transport.

Schema import produces an immutable `schema.Document`. Import alone marks
operations unapproved and never registers handlers. `CompileConsumer` accepts
only an explicit `Resolver` list, requires `Approved`, and requires an `Adapt`
callback that sees both partial data and safe errors. It then delegates ordinary
runtime registration to `interopadapter`. Query, mutation, subscription,
directive names, and SDL metadata never establish effect, retry, cache,
transaction, cost, or authorization policy.

`Runtime.Execute` and `Runtime.Subscribe` are in-process boundaries over an
existing Naatre executor. The caller context is propagated unchanged so the
registry remains the authorization authority. GraphQL errors retain bounded
response paths and stable codes; unknown executor codes and all
executor/upstream messages, extensions, credentials, endpoints, stack traces,
and causes are replaced with public adapter failures. GraphQL non-null bubbling
is applied only while producing a GraphQL response and does not change Naatre's
partial-result rules.

## Supported profile

The dependency-free package is supported with Go 1.27 on Linux, macOS, and
Windows on amd64 and arm64. The executed profile covers:

- SDL object, input object, enum, output union, standard scalar, list, and
  non-null mappings;
- explicitly mapped custom scalar names (the application still owns codecs);
- query, mutation, and subscription declarations with application policy;
- named operations, variables, scalar/list/object literals, aliases, nested
  selections, named fragments, inline fragments, and `@include`/`@skip`;
- deterministic schema and operation export for the same subset;
- explicit imported resolver registration and transport-neutral upstream
  requests;
- bounded partial responses, safe error paths, non-null propagation,
  cancellation, and ordered in-process subscription outcomes.

## Unsupported optional capabilities

The profile fails closed for schema extension and `extend`, interface and
`implements`, directive definitions or schema directives, argument/input
defaults in SDL, block-string descriptions, input unions and `@oneOf`, custom
scalar codec execution, introspection execution, arbitrary Naatre composition,
parameterized Naatre fragments, custom executable directives, `@defer`,
`@stream`, inline enum literals (use typed variables), variables nested inside
composite literals, live queries, uploads,
federation, schema stitching, remote schema
discovery, automatic persisted queries, and resolver discovery/reflection.

No HTTP server/client policy, WebSocket protocol, SSE, multipart response,
batching transport, cookie/header forwarding, credential storage, redirect,
DNS, TLS, proxy, endpoint allowlist, framework binding, browser runtime,
mobile/native runtime, or certification claim is included. A concrete client or
server must publish separate evidence for those capabilities. Subscription
support here is only the in-process `Subscriber` callback; it does not claim a
GraphQL-over-HTTP or GraphQL-over-WebSocket transport.

## Reproducible conformance

From the repository root, with no network or listener:

```sh
GOCACHE=/tmp/naatre-graphql-gocache go test ./graphqladapter -count=1
GOCACHE=/tmp/naatre-graphql-gocache go test ./internal/conformance -run TestGraphQLAdapterEvidence -count=1
node conformance/independent/verify-graphql-adapter.mjs
```

The machine-readable evidence is
`conformance/v1/graphql-adapter.json`. It pins the exact SHA-256 revisions of
the #56 specification and fixture, the module dependency set, and the GraphQL
September 2025 URL. The verifier recalculates every local dependency digest.
