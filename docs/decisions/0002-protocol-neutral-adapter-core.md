# Decision 0002: protocol-neutral adapter core

Status: accepted for v1.

The core adapter contract records fidelity and policy without parsing or
serving a specific external protocol. A small Go compiler turns explicitly
approved runtime-consume operations into ordinary runtime definitions, while
protocol integrations remain independently shippable packages.

This boundary prevents an imported HTTP method, GraphQL label, RPC method name,
or vendor extension from becoming authorization, retry, cache, effect, or
transaction policy. The application supplies those values in the Naatre
descriptor, and startup validation rejects incomplete or unsupported mappings.

The reference HTTP-JSON invoker is deliberately not an OpenAPI implementation.
It demonstrates bounded egress, explicit origin and credential forwarding,
deadline and cancellation propagation, deterministic projections, safe errors,
and runtime registration against an existing service. Full protocol parsers,
exports, streaming modes, and multi-SDK certification retain their extracted
issue ownership.
