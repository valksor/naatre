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
20. [Asynchronous long-running operations](async-operations.md)
21. [Large values, files, and byte streams](large-values.md)
8. [Transport batching and request multiplexing](request-batching.md)
8. [HTTP digest fields](http-digest.md)
9. [Persisted and allowlisted operations](persisted.md)
10. [Cursor pagination and collection metadata](collections.md)
11. [Mutations and transaction boundaries](mutations.md)
12. [Reliability, idempotency, and retries](reliability.md)
13. [Request-scoped batching and caching](batching.md)
14. [Observability and mutation audit](observability.md)
15. [Process admission and lifecycle](operations.md)
16. [Streaming and incremental delivery](streaming.md)
17. [Federation composition and execution](federation.md)
18. [Generator model and common SDK behavior](generation.md)
19. [Portable validation constraints](validation.md)
20. [Namespaced protocol and runtime extensions](extensions.md)
20. [Developer tooling](tooling.md)
20. [Interoperability adapters](adapters.md)
21. [Asynchronous long-running operations](async-operations.md)
22. [Transport batching and request multiplexing](request-batching.md)
23. [HTTP digest fields](http-digest.md)
24. [Events and outbound webhooks](events.md)
25. [Developer tooling](tooling.md)
26. [Interoperability adapters](adapters.md)
21. [Asynchronous long-running operations](async-operations.md)
22. [Transport batching and request multiplexing](request-batching.md)
23. [HTTP digest fields](http-digest.md)
24. [Developer tooling](tooling.md)
25. [Interoperability adapters](adapters.md)
26. [Remote worker protocol](remote-workers.md)

The specification is language-neutral. Go is a reference implementation and
does not override these documents or their portable fixtures.
