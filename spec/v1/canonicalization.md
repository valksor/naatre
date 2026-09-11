# Canonicalization and semantic hashing

## Parsed value requirements

- **CANON-001:** Inputs are UTF-8 without BOM. Invalid byte sequences, unpaired
  surrogate escapes, duplicate object members after unescaping, and trailing
  data are rejected before canonicalization.
- **CANON-002:** Application strings are preserved as Unicode scalar sequences;
  canonicalization MUST NOT normalize NFC/NFD or apply locale rules.
- **CANON-003:** Arrays retain order. Object keys sort by UTF-16 code units as
  in RFC 8785, including surrogate-pair code units for non-BMP scalars.
- **CANON-004:** Strings use JSON escapes for control characters, quote, and
  backslash; other Unicode scalars are encoded directly as UTF-8.
- **CANON-005:** Core JSON numbers meet RFC 8785's finite binary64 constraints.
  Schema-directed extended scalars use the string codecs in TYPE-105..112 and
  are never converted through binary64.
- **CANON-006:** Empty object and list remain `{}` and `[]`. Missing members are
  absent. Explicit null is `null`. Object member order from input is discarded.

Valid: objects with the same members in different input orders canonicalize to
the same bytes while two selection arrays in different orders do not.

Invalid: sorting keys by UTF-8 bytes, NFC-normalizing strings, or accepting two
escaped spellings of the same member name.

## Canonical AST

- **CANON-100:** Canonical document bytes contain only defined language fields,
  omit absent optional fields, preserve explicit defaults, preserve every
  ordered selection, and sort every unordered object.
- **CANON-101:** Only finite normalization is required. Implementations MUST NOT
  hash optimized plans or attempt arbitrary program equivalence.
- **CANON-102:** Document content identity is schema-free. Schema-dependent
  coerced variables bind cache/idempotency identities, not document identity.
- **CANON-103:** Operation kind, variable declarations/defaults, referenced
  capabilities/extensions, aliases, directives, and fragments contribute to
  document identity.

Valid: reordering document-object members preserves a document hash.

Invalid: reordering selection arrays, changing a default, or changing query to
mutation preserves a document hash.

## Domains and algorithms

- **CANON-200:** A hash input is UTF-8 bytes
  `"naatre:" + purpose + ":" + canonicalVersion + "\\n" + payload`.
- **CANON-201:** v1 requires algorithm identifier `sha-256` and canonical
  version `c14n-1`. Digests are lowercase hexadecimal.
- **CANON-202:** Purposes are `document`, `schema`, `approval`, `result-cache`,
  `idempotency`, and `signed-message`. A digest from one purpose MUST NOT be
  accepted for another.
- **CANON-203:** Document payload is canonical AST bytes. Schema payload is
  canonical schema bytes. Approval additionally binds document digest, policy
  revision, schema revision, capabilities, and approval metadata. Result-cache
  and idempotency bind coerced variables and their relevant runtime metadata.
  Signed-message binds the exact signed envelope defined by its profile.
- **CANON-204:** Transport formatting, request correlation IDs, and unrelated
  extension metadata do not alter document identity. Variables and approval
  metadata alter their own purpose-specific identities.

Valid: one canonical document produces six distinct purpose-separated hashes.

Invalid: reuse a document digest as an approval identity or include a request
correlation ID in document content identity.
