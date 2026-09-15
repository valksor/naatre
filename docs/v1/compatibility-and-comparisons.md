# Compatibility and comparisons

Support words have fixed meanings. Planned means an owning issue or registry row exists but no compatibility claim is made. Implemented means source plus an executable component profile exists. Tested means the exact revision/runtime/platform has a passing machine-readable report or the named CI conformance command. A language name alone never implies all three, and client, adapter, remote worker, Go-gateway, native runtime, transport, framework, and platform rows are independent.

| Language | Client | Worker/server | Native runtime | Tested boundary |
| --- | --- | --- | --- | --- |
| Go | Implemented profiles and CI tests | Go HTTP adapter and reference gateway implemented; production worker pool is separately profiled | Reference native runtime implemented | Exact Go/profile evidence only; see [profiles](../../conformance/v1/profiles.json). |
| JavaScript/TypeScript | Implemented core and adapters | Executable conformance worker example; production worker binding remains a separate claim | Planned, not established by a Go gateway | Node evidence is tested; browser/edge rows require their exact reports. |
| PHP | Implemented core and PSR adapters | Executable conformance worker example; framework/native worker certification is separate | Planned | Only listed PHP/core/adapter commands and reports are tested. |
| Python | Implemented core and adapters | Executable conformance worker plus ASGI, FastAPI, and Starlette adapter profile; native worker certification is separate | Planned | Only listed CPython/profile commands and reports are tested. |
| Rust | Implemented runtime-neutral core and task adapters | Executable conformance worker example; concrete production transport/server is separate | Planned | Rust feature/toolchain commands are tested where recorded. |
| JVM | Implemented Java/Kotlin client core and selected adapters | Planned | Planned | JDK 17/21/25 and Android rows differ; consult [JVM evidence](../../conformance/v1/jvm-sdk.json). |
| .NET | Implemented C#/F# client core and selected adapters | Planned | Planned | net8.0 and net10.0 reports are independent; consult [.NET evidence](../../conformance/v1/dotnet-sdk.json). |
| Swift | Implemented core and Apple adapters | Planned | Planned | Swift core and Apple platform evidence are separate. |
| Dart | Implemented client core and adapters | Implemented worker models only where their profile says so | Planned | VM/web/platform claims require exact rows. |
| Ruby | Implemented client core and adapters | Planned | Planned | CRuby and integration evidence are separate. |

The final cross-language publication matrix remains [planned pending #69 evidence](../../conformance/v1/compatibility.json). Individual implemented/tested rows above come from their narrower component fixtures and do not upgrade that combined matrix.

## Comparisons

Each row separates sourced fact, trade-off, and Naatre project opinion. No row claims wire compatibility.

| System | Sourced fact | Trade-off | Project opinion |
| --- | --- | --- | --- |
| GraphQL | The September 2025 specification defines a typed selection language and response/error behavior. | Rich field selection and ecosystem; null propagation and resolver/runtime policy differ from Naatre. | Ordered JSON selection nodes and explicit selected states fit Naatre's deterministic fixtures better. |
| REST/OpenAPI | OpenAPI 3.2.0 describes HTTP APIs, operations, parameters, schemas, and responses. | Excellent HTTP/tooling fit; cross-resource composition is application-specific. | Use a bounded directional adapter where fidelity is explicit. |
| JSON-RPC/OpenRPC | JSON-RPC 2.0 defines request/response RPC envelopes; OpenRPC 1.4.1 describes methods. | Simple method calls; typed deep selection, partial selected states, and transport policy require extra contracts. | Treat import/export as adapter projections, not a native Naatre wire. |
| gRPC/Connect | The pinned profile uses protobuf Edition 2024 and a specific Connect protocol revision. | Strong generated RPC and streaming; protobuf presence and status details do not map losslessly in every direction. | Keep protobuf/gRPC/Connect optional and fail closed on unsupported fidelity. |
| Smithy | Smithy 2.0 is a model-first service description language with traits and protocol generation. | Strong modeling/code generation; execution composition and Naatre selected-state semantics are distinct. | Borrow explicit shapes/traits, retain a separate Naatre model and hash domain. |
| TypeSpec | TypeSpec is a language for describing APIs and emitting protocol-specific artifacts. | Flexible authoring/projection; emitter behavior can be protocol- and version-specific. | A future adapter should publish a directional fidelity report before claiming support. |
| Falcor | Falcor models JSON data as a graph and retrieves paths. | Efficient graph-shaped retrieval; schema, mutation, authorization, and error contracts differ. | Similar compositional goals do not justify protocol equivalence. |
| Deepr | Deepr supplied design inspiration for compositional API ideas. | Historical ideas can inform ergonomics; its contract and maintenance assumptions are not Naatre's. | Inspiration only—no wire, schema, SDK, conformance, lineage, or version compatibility claim. |
