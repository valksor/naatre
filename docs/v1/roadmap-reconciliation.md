# v1 roadmap reconciliation — closing out deferred-but-done issues

All 110 v1 issues are closed on GitHub, but `conformance/v1/roadmap.json` still
marked 32 of them `deferred-to-owner` / `unsupported-until-owner-closes` despite
each having real code and executable conformance evidence. This reconciles the
roadmap to the truth: every genuinely-done issue is now `implemented-contract`
(citing its owning profile, or covered-elsewhere), and only the two release-gated
aggregate issues (#69, #70) remain deferred by design.

Each row was owner-matched from the fixture's declared owner / README ownership /
`profiles.json` `certificationExecutionOwnerIssue`, and verified that the roadmap
`execution` equals the cited fixture's `profile` header, with an executing runner.

| Issue | Title | Resolution | Execution profile | Evidence fixture |
|------:|-------|------------|-------------------|------------------|
| #28 | Tooling: provide an idiomatic Go client and operation builder | implemented-contract | sdk.go.client-1 | v1/go-client.json |
| #31 | Documentation: publish the v1 guide, examples, and design rationale | covered-elsewhere | — | — |
| #32 | Roadmap: define and deliver the complete Naatre specification and ecosystem | covered-elsewhere | — | — |
| #35 | Tooling: build a language-neutral code generation framework and SDK contract | implemented-contract | sdk.generation-1 | v1/generation.json |
| #39 | Tooling: provide a Rust client and schema bindings | implemented-contract | sdk.rust.core-1 | v1/rust-sdk.json |
| #51 | Runtime: define polyglot server bindings and remote handler execution | implemented-contract | worker.remote-1 | v1/remote-workers.json |
| #52 | Mutations: define optimistic concurrency, read consistency, and typed updates | implemented-contract | mutation.update-1 | v1/mutation-updates.json |
| #53 | Collections: define typed filtering, sorting, and query capability discovery | implemented-contract | collection.page-1 | v1/collections.json |
| #54 | Schema: specify portable validation constraints and JSON Schema mappings | implemented-contract | core.validation-1 | v1/validation.json |
| #56 | Interoperability: define explicit adapters for OpenAPI, GraphQL, JSON-RPC, and gRPC | implemented-contract | core.adapters-1 | v1/adapters.json |
| #59 | Python: implement server handler bindings and ASGI integration | implemented-contract | worker.remote-1 | v1/remote-workers.json |
| #61 | Rust: implement server handler bindings and async transport adapters | implemented-contract | worker.remote-1.rust | v1/rust-worker.json |
| #62 | Events: define an AsyncAPI mapping and event-channel description profile | implemented-contract | adapter.asyncapi-1 | v1/asyncapi.json |
| #65 | Transport: define a deterministic CBOR encoding profile | implemented-contract | transport.cbor.unary-1 | v1/cbor.json |
| #69 | Release gate: execute the full cross-language and cross-profile conformance matrix | deferred (release-gated aggregate) | _re-earned per release_ | releases/1.0.0-rc.1/release-gate.json |
| #70 | Security gate: verify authorization and resource limits across optional profiles | deferred (release-gated aggregate) | _re-earned per release_ | releases/1.0.0-rc.1/release-gate.json |
| #77 | Documentation: publish executable multi-language examples and troubleshooting | implemented-contract | documentation.examples-1 | v1/documentation-examples.json |
| #84 | Dart SDK: implement Flutter, VM, and web transport adapters | implemented-contract | sdk.dart.adapters-1 | v1/dart-adapters.json |
| #85 | .NET SDK: implement C#, F#, and HTTP integration adapters | implemented-contract | sdk.dotnet.adapters-1 | v1/dotnet-adapters.json |
| #87 | Large payloads: implement upload/download adapters and deterministic cleanup | implemented-contract | implementation.go.large-value-adapters-1 | v1/large-value-adapters.json |
| #89 | Async operations: implement durable stores, leases, and worker adapters | implemented-contract | operations.async-adapters-go-1 | v1/async-operation-adapters.json |
| #90 | Webhooks: implement durable delivery, retry, and signature verification | implemented-contract | events.webhook-adapters-go-1 | v1/webhook-adapters.json |
| #91 | Developer tooling: implement the playground and schema-driven mock server | implemented-contract | naatre.playground-mock-1 | tooling/playground-mock.json |
| #92 | Adapter: implement GraphQL import/export and runtime integration | implemented-contract | core.adapters.graphql-1 | v1/graphql-adapter.json |
| #93 | Adapter: implement OpenAPI import/export and runtime integration | implemented-contract | adapter.openapi-1 | v1/openapi-adapter.json |
| #101 | JavaScript/TypeScript worker: implement Node, Bun, Deno, and edge adapters | covered-elsewhere | — | v1/typescript-worker.json |
| #102 | AsyncAPI: implement exporter, importer diagnostics, and round-trip fixtures | implemented-contract | adapter.asyncapi.projection-1 | v1/asyncapi-projection.json |
| #106 | Schema: implement authenticated discovery and compatibility-diff tooling | implemented-contract | schema.discovery.http-1 | v1/schema-discovery.json |
| #107 | Observability: implement OpenTelemetry and durable audit integrations | implemented-contract | operations.observability-integrations-go-1 | v1/observability-integrations.json |
| #108 | Filtering: implement provider translators and generated SDK mappings | implemented-contract | collection.query.codegen-1 | v1/collection-query-generation.json |
| #109 | Federation: implement the coordinator and distributed execution planner | implemented-contract | runtime.go.federation-coordinator-1 | v1/federation-coordinator.json |
| #110 | PHP native extension: accelerate Naatre on PHP 8.5 and 8.6 | implemented-contract | sdk.php.native-1 | v1/php-native.json |
