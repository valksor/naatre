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

The specification is language-neutral. Go is a reference implementation and
does not override these documents or their portable fixtures.
