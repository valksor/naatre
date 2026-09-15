# AsyncAPI event descriptions

Naatre's `adapter.asyncapi-1` profile emits deterministic AsyncAPI 3.0.0 JSON
for authorized schema identities and implemented event transports. It is an
inspection format, not a promise that AsyncAPI tooling can speak Naatre's wire
protocol. See the [normative profile](../spec/v1/asyncapi.md) and
[conformance fixture](../conformance/v1/asyncapi.json).

The Go API builds an `asyncapi.Model` from a `schema.Document`, stable event
identities, explicit bindings, and named test evidence. `asyncapi.Export`
returns canonical bytes plus a fidelity report. `asyncapi.Import` accepts only
bounded local references and requires exact server URL allowlists. It performs
no I/O.

The CLI consumes an existing exported document:

```text
naatre asyncapi validate --document events.json --allow-server https://events.example/v1
naatre asyncapi export --document events.json --allow-server https://events.example/v1
naatre asyncapi inspect --document events.json --allow-server https://events.example/v1
naatre asyncapi mock --document events.json --allow-server https://events.example/v1 --seed 13
naatre asyncapi diff --before old.json --after new.json --allow-server https://events.example/v1
```

`--allow-server` is a comma-separated list of exact URLs. Omitting it is
default-deny. The commands never dereference references, connect to servers, or
invoke handlers. The playground exposes the same inspection model at
`GET /v1/asyncapi` only when its caller supplies an already exported document
and the exact server allowlist.

Issue #102's full payload-schema projection is a separate, strictly versioned
profile so this metadata-only contract does not widen silently. See
[`asyncapi-projection.md`](asyncapi-projection.md) for its authority, lifecycle,
runtime, unsupported-capability, and offline conformance boundaries.
