# Naatre v1 specification

This directory is the normative source for Naatre v1. The key words MUST,
MUST NOT, REQUIRED, SHALL, SHALL NOT, SHOULD, SHOULD NOT, RECOMMENDED, NOT
RECOMMENDED, MAY, and OPTIONAL are interpreted as described by RFC 2119 and
RFC 8174 when, and only when, they appear in all capitals.

Clauses have stable identifiers. Changes to the v1 documents require a
decision record and migration note. Examples marked valid or invalid are
normative fixtures once represented under `conformance/`.

Documents:

1. [Core execution model](core.md)
2. [Request and response protocol](protocol.md)
3. [Type system and value model](schema.md)
4. [Composition language](language.md)
5. [Canonicalization and semantic hashing](canonicalization.md)
6. [Authentication, authorization, and interceptor security](security.md)
7. [HTTP binding](http.md)
8. [Persisted and allowlisted operations](persisted.md)
9. [Cursor pagination and collection metadata](collections.md)
10. [Mutations and transaction boundaries](mutations.md)
11. [Reliability, idempotency, and retries](reliability.md)
12. [Request-scoped batching and caching](batching.md)
13. [Observability and mutation audit](observability.md)
14. [Process admission and lifecycle](operations.md)
15. [Streaming and incremental delivery](streaming.md)
16. [Federation composition and execution](federation.md)
17. [Generator model and common SDK behavior](generation.md)
18. [Portable validation constraints](validation.md)
19. [Namespaced protocol and runtime extensions](extensions.md)
20. [JVM Java and Kotlin client core](jvm.md)
21. [Asynchronous long-running operations](async-operations.md)
22. [Large values, files, and byte streams](large-values.md)
23. [Transport batching and request multiplexing](request-batching.md)
24. [HTTP digest fields](http-digest.md)
25. [Developer tooling](tooling.md)
26. [Interoperability adapters](adapters.md)
27. [Events and outbound webhooks](events.md)
28. [Remote worker protocol](remote-workers.md)
29. [Deterministic CBOR transport profile](cbor.md)
30. [JSON Type Definition projection and fidelity](jtd.md)
31. [Normalized cache contract](normalized-cache.md)
32. [AsyncAPI event-description profile](asyncapi.md)

The specification is language-neutral. Go is a reference implementation and
does not override these documents or their portable fixtures.
