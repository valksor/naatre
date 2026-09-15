# Troubleshooting

Start with the stable public code and the conformance clause identifier carried by the diagnostic. Search the [normative specification index](../../spec/v1/README.md) for the clause, then find the code in the generated [public error-code index](error-codes.md) and in `conformance/v1`. Never branch on public message text, and never return a private cause to make diagnosis easier.

## Diagnostic workflow

Record the exact spec, schema, fixture, runtime, SDK/profile, and source revisions from [versions](versions.md). Preserve the safe code, phase, clause, JSON Pointer/source position, response path, request correlation identifier, and effect state. Reproduce with the smallest named fixture/profile. If the result differs, classify whether decoding, validation, planning, authorization, execution, completion, serialization, transport, or lifecycle owns the failure. Compare canonical bytes and digests before comparing host-language objects. Check private logs only through authorized correlation; redact credentials, variables, protected values, causes, and topology before sharing evidence.

For `LIMIT_*`, `RESOURCE_EXHAUSTED`, `OVERLOADED`, `RATE_LIMITED`, or response/frame limit codes, identify which finite budget was consumed and whether accounting remains held by uncooperative work. Do not raise every limit together. For `UNAUTHENTICATED`, `UNAUTHORIZED`, or `POLICY_DENIED`, verify trust/audience/current revisions and deny-by-default policy without probing protected existence. For persisted, cursor, replay, federation, worker, adapter, digest, or idempotency prefixes, run the matching fixture named in [domain examples](domain-examples.md).

Missing versus null failures usually mean a host model collapsed presence. JavaScript numeric failures usually mean an extended number passed through `number`; retain its decimal string. Timestamp mismatches usually mean local-zone coercion or lost fractional precision. Unicode/hash mismatches usually mean normalization, locale sorting, Unicode-scalar sorting instead of UTF-16 key sorting, array reordering, or hashing without the Naatre purpose prefix. Cancellation mismatches usually mean code claimed work stopped when only a signal or local handle was released.

If a stable code is absent from [the generated index](error-codes.md), the fixture/code addition is incomplete. Run `go generate ./internal/doccheck` and `go test ./internal/doccheck`; a new code must enter a code-bearing fixture field and resolve to this workflow before publication.

## Executable troubleshooting scenarios

The [documentation example evidence](../../conformance/v1/documentation-examples.json) pins one minimal fixture for each required scenario class. Run `node conformance/independent/documentation.mjs` first to verify the dependency digests, stable-code inventory, safe failure shapes, sample sources, and CI command anchors. Then run the fixture's owning test or independent verifier from [domain examples](domain-examples.md).

| Class | Fixture slice | Expected public result |
| --- | --- | --- |
| Positive | [`generator-output.json` operations](../../conformance/v1/generator-output.json) | The language client constructs the canonical request and decodes the checked response without a public failure. |
| Negative | [`security.json` safe denial](../../conformance/v1/security.json) | `UNAUTHORIZED`; no protected existence, principal, tenant, policy input, or cause is exposed. |
| Boundary | [`streaming.json` safe handle failures](../../conformance/v1/streaming.json) | `REESTABLISH_REQUIRED`, `REFETCH_REQUIRED`, or `DELIVERY_CAPABILITY_UNSUPPORTED`; unknown and unauthorized handles remain indistinguishable. |
| Cancellation | [`remote-worker-gateway.json` cases](../../conformance/v1/remote-worker-gateway.json) | `CANCELLED`; the result states effect progress and never upgrades an acknowledgement into proof of rollback or hard termination. |
| Resource limit | [`resources.json` limit vectors](../../conformance/v1/resources.json) | `LIMIT_BYTES` before execution or `RESOURCE_EXHAUSTED` at the bounded runtime phase; the response identifies the safe phase/path without echoing request values. |

Published failure examples contain only `code`, `phase`, `clause`, `path`, `requestId`, and `effectState`. They must never contain authorization or cookie values, credentials or tokens, variables, principal or tenant identifiers, private causes or stacks, topology or endpoints, service identities, or schema digests. Messages remain non-contractual and must not carry implementation-only details.
