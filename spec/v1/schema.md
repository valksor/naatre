# Type system and value model

## Type forms

- **TYPE-001:** Input and output positions are distinct. Output-only object,
  interface, collection, and stream types MUST NOT be accepted as client input
  literals or variables. A completed server value may enter a typed call through
  `$current`, `$parent`, or `$result` only when its retained schema type exactly
  matches that argument; this runtime-only coercion does not make the type
  client-input-capable.
- **TYPE-002:** Core scalars are `Boolean`, `String`, `ID`, `Int32`, and
  `Float64`. Extended scalars are `Int64`, `UInt64`, `BigInt`, `Decimal`,
  `Timestamp`, `Duration`, `UUID`, and `Bytes`.
- **TYPE-003:** Composite forms are list, string-keyed map, object, enum, open
  tagged union, interface, and one-of input object. Every named type has a
  stable portable ASCII identifier.
- **TYPE-004:** Required/optional describes presence. Nullable/non-null
  describes the value when present. They MUST NOT be conflated.
- **TYPE-005:** Recursive type graphs are permitted only with configured schema
  and value depth limits. A positive per-type limit counts recursive descents
  beyond the first occurrence; exceeding it is a validation or completion
  error, never truncation.
- **TYPE-006:** Objects and interfaces use declared field names. Dynamic union
  and interface values use exactly `{"$type":"TypeID","$value":value}`;
  `$type` identifies a declared concrete variant and `$value` is validated as
  that type. Reference runtimes MUST expose an explicit tagged host value and
  MUST NOT infer tags from arbitrary maps. Unions and interfaces are output-only
  in v1.

Valid: an optional non-null field is missing, or a required nullable field is
present as null.

Invalid: a required field is missing, or a non-null field is explicitly null.

## Scalar wire and canonical forms

| Clause | Type | JSON form | Canonical rule |
| --- | --- | --- | --- |
| TYPE-100 | Boolean | boolean | `true` or `false` |
| TYPE-101 | String | string | unchanged Unicode scalars; no normalization |
| TYPE-102 | ID | string | application-opaque, schema validated |
| TYPE-103 | Int32 | number | decimal integer, -2147483648..2147483647, no exponent |
| TYPE-104 | Float64 | number | finite IEEE-754 binary64; shortest round-tripping decimal; -0 becomes 0 |
| TYPE-105 | Int64 | string | optional `-`, then minimal decimal digits |
| TYPE-106 | UInt64 | string | minimal decimal digits |
| TYPE-107 | BigInt | string | optional `-`, minimal digits, zero is `"0"` |
| TYPE-108 | Decimal | string | sign plus coefficient; remove leading/trailing zeros; zero is `"0"` |
| TYPE-109 | Timestamp | string | RFC 3339 UTC `Z`, nanoseconds trimmed right; leap seconds rejected |
| TYPE-110 | Duration | string | signed integer nanoseconds in v1 core codec |
| TYPE-111 | UUID | string | lowercase RFC 4122 hyphenated form |
| TYPE-112 | Bytes | string | RFC 4648 base64url without padding |

- **TYPE-113:** Int64, UInt64, BigInt, and Decimal MUST NOT use JSON numbers.
- **TYPE-114:** NaN and infinities are invalid. JSON negative zero is accepted
  only for Float64 and canonicalizes to zero.
- **TYPE-115:** A string containing `"123"` remains String unless the declared
  schema type selects an extended numeric codec.
- **TYPE-116:** Scalar input coercion accepts only the declared JSON form. It
  performs no string-to-Boolean, string-to-core-number, number-to-string, or
  locale-dependent conversion. Scalar output completion applies the same
  canonical codec after checking the registered native representation.

Valid: `"9007199254740993"` as an Int64.

Invalid: `9007199254740993` as an Int64 or a Decimal canonicalized as `"1.00"`.

## Composite values

- **TYPE-200:** Lists preserve order. Failed output elements use null
  placeholders with indexed errors; missing list elements do not exist.
- **TYPE-201:** Maps accept every valid Unicode string key, including empty and
  numeric-looking keys. Empty map `{}` and empty list `[]` are distinct.
- **TYPE-202:** Reserved expression keys have syntax meaning only at language
  control positions. Application maps remain inert literals.
- **TYPE-203:** Closed enums reject unknown values. Open enums retain an
  explicit unknown-value representation in clients.
- **TYPE-204:** Open unions contain a stable tag and value. Unknown tags remain
  representable; closed unions reject them.
- **TYPE-205:** A one-of input has exactly one present member after variable
  substitution and defaults. A present null member counts as active only when
  that member is nullable.
- **TYPE-206:** Output completion performs no broad implicit coercion. A failed
  required output is represented as unavailable partial data, never by deleting
  unrelated parent data. A nullable root output returned as null remains a
  present response key with value null; a null non-null root output is
  unavailable and produces an output-completion error.
- **TYPE-207:** Input objects reject unknown fields. Variable substitution runs
  before coercion; defaults apply only to missing values, are schema-validated
  when registered, and never replace explicit null. Required is checked after
  defaults. Optional missing members remain missing.
- **TYPE-208:** Canonical composite JSON preserves list order, omits missing
  object members, emits explicit null, and orders object/map keys by ascending
  UTF-16 code-unit order as required by CANON-003 and RFC 8785. Empty list
  and map remain distinct. A canonical one-of object contains its one active
  member. Schema declaration order is not semantic. Duplicate object members,
  invalid UTF-8, and unpaired surrogate escapes are rejected before coercion;
  a host JSON decoder MUST NOT collapse or repair them.
- **TYPE-209:** Enum values are JSON strings. Closed enums reject an undeclared
  spelling; open enums preserve the exact spelling plus a known/unknown state.
  Closed unions reject an undeclared `$type`; open unions preserve the exact
  tag and canonical `$value` as an unknown-variant representation.
- **TYPE-210:** List elements cannot be missing. Null elements require nullable
  element metadata. Map keys are arbitrary valid Unicode strings rather than
  protocol identifiers, and map values follow the declared element type and
  nullability.

Valid: `{ "0": "zero", "": "empty", "😀": "face" }` as a string-keyed map.

Invalid: one-of input `{}` or `{ "email": null, "phone": "1" }` when `email`
is nullable, because zero and two active members respectively are forbidden.

## Custom scalars

- **TYPE-300:** A custom scalar declares a stable type identifier, accepted
  wire shapes, language-neutral validator, serializer, and canonicalizer
  identifiers, a canonicalization profile, resource limits, and at least two
  portable input/canonical-output conformance vectors. These fields are part of
  the portable type descriptor rather than implementation-only runtime state.
- **TYPE-301:** An implementation MUST reject registration unless it implements
  the descriptor's validator, serializer, canonicalizer, and canonical profile
  as a closed, deterministic profile. Arbitrary host callbacks are not a
  portable custom-scalar implementation. Profile execution MUST be pure and
  independent of locale, Unicode-library version, host clock, I/O, and mutable
  process state. The v1 Go reference runtime supports strict JSON shape
  validation, identity JSON serialization, RFC 8785 `c14n-1`, and ASCII
  lowercase-string followed by `c14n-1`.
- **TYPE-302:** Native date/decimal adapters MAY be used only when they preserve
  the declared range and precision; otherwise they retain a lossless wrapper or
  reject conversion.
- **TYPE-303:** A custom scalar profile MUST reject invalid wire shapes and
  produce exactly one JSON value. Implementations MUST strict-parse and apply
  the selected canonicalizer, then canonicalize the result using the complete
  CANON-001 through CANON-004 `c14n-1` profile before output or semantic
  hashing. At schema registration they MUST reproduce every declared
  conformance vector exactly. Runtime canonicalization uses only the immutable
  descriptor and the selected closed profile; no application callback or
  mutable runtime state participates.

Valid: a custom slug scalar selects the advertised ASCII-lowercase profile and
normalizes `"Mixed-Case"` to `"mixed-case"` without application code.

Invalid: canonicalization uses the machine locale or current timezone.

## Portable schema documents

- **TYPE-400:** The normative schema authority is a UTF-8 JSON document with
  `version`, `canonicalVersion`, `revision`, `types`, `operations`, and
  `members`. Version 1 uses document version `1` and canonical version
  `c14n-1`. The revision is an immutable application-assigned portable
  identifier. A process MUST publish a new immutable document rather than
  mutate declarations associated with an existing revision.
- **TYPE-401:** Types, fields, enum members, union/interface variants, root
  operations, and object members each carry a stable `id` independent of their
  display `name`. These declarations are arrays sorted by `id` for canonical
  identity; host maps, reflection names, source order, and Go package layout are
  not part of the portable contract. Renaming a declaration retains its ID and
  is therefore detectable rather than appearing as unrelated removal/addition.
- **TYPE-402:** A type declaration carries its input/output positions, kind,
  list or map element contract, object/input fields, enum or variant members,
  openness, recursion bound, custom-scalar contract, description, deprecation,
  entity keys, required capabilities, traits, source provenance, and retired
  identities as applicable. Built-in scalar references need not be repeated in
  `types`.
- **TYPE-403:** Root operation and object-member manifests carry stable IDs,
  public names, operation/member kinds, owner, input/output identifiers,
  independent input/output nullability, effect, determinism, cacheability,
  retry safety, thread safety, batching, transaction participation,
  authorization policy, idempotency policy, cost, parallel-mutation
  eligibility, capabilities, traits, description, deprecation, and source
  provenance as applicable. An implementation MUST NOT infer these contracts
  from a transport method or host function signature.
- **TYPE-404:** `ExportDocument`-equivalent APIs MUST export only portable data
  and MUST retain supported metadata through document -> host registry ->
  document round trips. A host registry MAY derive an omitted legacy handler ID
  deterministically (`kind.name` for roots and `Owner.name.resolver` for object
  members), but new registrations SHOULD supply an explicit stable ID. A
  derived collision is an error, never insertion-order resolution.
- **TYPE-405:** A schema reference contains a URI, a revision, and a lowercase
  `sha256:` digest with exactly 64 hexadecimal digits. Import does not fetch the
  URI. Fetching, trust roots, signature verification, and local cache policy
  require separate explicit configuration; an implementation MUST NOT retrieve
  arbitrary schema URLs while parsing a bundle.

### Traits and extensions

- **TYPE-410:** A trait contains a stable ID, one of `documentation`,
  `validation`, `execution`, `authorization`, or `identity` semantics, and one
  strict JSON value. Documentation traits may be preserved by an implementation
  that does not understand them. Unknown validation, execution, authorization,
  and identity traits are critical and MUST make import fail unless their exact
  ID is explicitly supported.
- **TYPE-411:** Trait and capability arrays are sets for canonical identity:
  they reject duplicate IDs and sort by ID. Trait values remain ordinary
  canonical JSON. Implementing one trait ID does not imply support for another
  version or vendor prefix.

### Lifecycle and compatibility

- **TYPE-420:** Deprecation contains a required human-readable `reason` and may
  contain `replacement`, RFC 3339 `sunset`, and `removalRevision`. Deprecation is
  advisory until an authorization or validation trait explicitly gives it
  execution semantics. Generators and documentation tools MUST preserve all
  supplied lifecycle fields.
- **TYPE-421:** Removed identities and, when supplied, names remain reserved in
  `retired` with a reason. An active declaration MUST NOT reuse a retired ID or
  a retired name in its owning namespace. Omitting the retirement record in a
  later revision does not make reuse compatible; comparison with the prior
  accepted revision still classifies reuse as breaking.
- **TYPE-422:** Compatibility comparison is by stable ID and emits a
  deterministic path and one of `breaking`, `dangerous`, `additive`, or
  `behavior-only`. Removal, rename, identity reuse, type changes, nullability or
  required-input tightening, and closing an enum or union are breaking.
  Changing effects, authorization, constraints, cost, scalar behavior,
  concurrency, transaction, caching, or retry contracts is dangerous. New
  declarations and variants of an open union are additive. Description,
  deprecation, and defaults are behavior-only unless another change independently
  receives a stronger classification.
- **TYPE-423:** Adding a required input field without a default is breaking.
  Adding an optional field or one with a canonical default is additive. Adding
  a member to an open enum or union is additive; adding one to a closed output
  set is dangerous because generated consumers may be exhaustive. Removing or
  renaming any member is breaking.
- **TYPE-424:** Deployment tooling MUST compare the registered operation/member
  manifest with the proposed document and bind an accepted revision into the
  deployment artifact. Matching only type names or accepting a diff report
  without validating the frozen registry is insufficient.

### Filtered and authenticated discovery

- **TYPE-430:** In-process discovery is disabled unless explicitly configured
  by the application. A network binding is not created by importing or freezing
  a schema. When enabled, every discovery request requires an authenticated
  principal and an explicit authorization decision before any unfiltered
  metadata is returned. #106 owns the HTTP adapter, stable failure codes,
  migration reports, and rolling-revision integration for this contract.
- **TYPE-431:** Visibility is a deny-by-default allow-list of stable type,
  field, enum-member, variant, operation, object-member, and retired IDs. A
  filtered document MUST contain no hidden declaration or retired name and MUST
  pass the same import and reference validation as a complete document.
- **TYPE-432:** Filtering may prune output fields and open enum/union members.
  It MUST remove a selected declaration whose referenced type is hidden and
  repeat pruning until no dangling reference remains. An input object is
  all-or-nothing: hiding any input member removes the input object and every
  operation/member that depends on it, because silently narrowing accepted
  input would change execution semantics.
- **TYPE-433:** One discovery response is derived from exactly one immutable
  revision and one authorization decision. Types, operations, members, hash,
  ETag, or diff metadata from different revisions MUST NOT be combined. Cache
  identity includes the schema revision and every principal/tenant/policy
  dimension used by visibility.
