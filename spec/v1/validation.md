# Portable validation constraints

## Constraint metadata and evaluation boundary

- **VALID-001:** `naatre.constraints-1` is the sole core v1 validation trait.
  It has `validation` semantics and may occur on a type or field. Its closed
  vocabulary is `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`,
  `precision`, `scale`, `minLength`, `maxLength`, `lengthUnit`, `pattern`,
  `patternMode`, `format`, `minItems`, `maxItems`, `uniqueItems`,
  `minProperties`, `maxProperties`, `keyPattern`, and `rules`. Implementations
  MUST reject unknown members and constraint families applied to an
  incompatible declared type; an unknown validation trait remains critical as
  required by TYPE-410.
- **VALID-002:** Input evaluation order is variable substitution, missing-only
  default application, declared-type coercion and canonicalization, portable
  constraint evaluation, authorization, then application execution. Schema
  registration MUST coerce and validate defaults in the same order. A value
  used in a cache, idempotency, or persisted identity MUST use the canonical
  post-default representation, never host-map order or a pre-coercion spelling.
- **VALID-003:** Missing and explicit null are outside constraint evaluation.
  Missing first follows required/default rules; explicit null first follows the
  declared nullability rule and MUST NOT select a default. A present non-null
  value cannot bypass a field trait by satisfying only its referenced type
  trait, or vice versa.
- **VALID-004:** Server validation is authoritative and MUST finish before the
  affected handler runs. Client validation is advisory but MUST accept and
  reject the shared corpus identically when advertised. Portable validation is
  pure, deterministic, bounded, and has no I/O, ambient clock, randomness,
  mutable process state, database lookup, or access to undeclared fields.
  Application and database uniqueness checks occur during application
  execution and are not `uniqueItems`.

## Scalar and container constraints

- **VALID-100:** `minimum` and `maximum` compare exact decimal values and each
  may be inclusive or exclusive. No comparison passes through binary64 unless
  Float64 is the declared type. `precision` and `scale` apply only to Decimal;
  precision counts coefficient digits after removing leading zeroes, with zero
  having precision one, and scale counts fractional digits in the canonical
  decimal spelling. Negative
  limits, a scale greater than precision, or an inverted bound are invalid
  metadata.
- **VALID-101:** `minLength` and `maxLength` count Unicode scalar values by
  default. `lengthUnit: "bytes"` instead counts UTF-8 bytes. Combining scalar
  sequences are not grapheme-normalized and astral scalars count as one scalar.
  Invalid UTF-8 and unpaired surrogate escapes fail strict JSON decoding before
  constraints; implementations MUST NOT repair or normalize them.
- **VALID-102:** `pattern` and `keyPattern` use the RE2-compatible regular
  expression language, with at most 1024 UTF-8 bytes of pattern source. The
  default mode is `full`, equivalent to wrapping the source in `^(?:...)$`;
  `search` uses unanchored search semantics. Look-around, backreferences, and
  every construct rejected by RE2 MUST be rejected at schema registration.
  Evaluation MUST use a linear-time engine or an implementation proven
  equivalent to the pinned subset and bounds.
- **VALID-103:** A format object contains `id` and optional `assertion`. Without
  `assertion: true`, format is descriptive metadata. Core assertions are
  limited to lowercase identifiers `uuid`, `timestamp`, `uri`, and `email`:
  UUID and timestamp use the TYPE-109 and TYPE-111 grammars and calendar/range
  checks; URI requires an absolute URI with scheme and authority; email uses
  the portable ASCII mailbox-and-DNS-label corpus contract and does not claim
  complete RFC internet-address validation. An unregistered asserting format
  is invalid metadata; a custom non-asserting format may be preserved.
- **VALID-104:** `minItems` and `maxItems` count list members;
  `minProperties` and `maxProperties` count present map or object members; and
  `keyPattern` validates each map key. `uniqueItems` compares canonical JSON
  values under CANON-001 through CANON-004, so object member order cannot make
  equal values distinct. The first duplicate in list order owns the violation
  path.

## Cross-field rules and violations

- **VALID-110:** `rules` is an ordered list of at most 32 stable rule IDs. Each
  assertion is a typed tree containing at most 128 nodes and depth 16. Leaf
  operators are `present`, `absent`, `eq`, and `ne`; boolean operators are
  `and`, `or`, and unary `not`. Equality compares canonical JSON. A rule may
  name only a field declared by its owning object or one-of type and may inspect
  only presence or value, never request context or external data.
- **VALID-111:** CEL is not a core v1 evaluator. A `cel` member or CEL-only
  operator MUST fail with `CONSTRAINT_FEATURE_UNSUPPORTED` and a pointer to the
  unsupported member. A future optional adapter may translate only a pinned,
  side-effect-free subset that is exactly expressible by VALID-110 and MUST
  publish a fidelity report; it cannot expand core semantics.
- **VALID-200:** A constraint violation contains stable `id`, stable `code`, an
  RFC 6901 path relative to the constrained value, and only safe string
  parameters. Core codes are `CONSTRAINT_TYPE`, `CONSTRAINT_MINIMUM`,
  `CONSTRAINT_MAXIMUM`, `CONSTRAINT_PRECISION`, `CONSTRAINT_SCALE`,
  `CONSTRAINT_MIN_LENGTH`, `CONSTRAINT_MAX_LENGTH`, `CONSTRAINT_PATTERN`,
  `CONSTRAINT_FORMAT`, `CONSTRAINT_MIN_ITEMS`, `CONSTRAINT_MAX_ITEMS`,
  `CONSTRAINT_UNIQUE_ITEMS`, `CONSTRAINT_MIN_PROPERTIES`,
  `CONSTRAINT_MAX_PROPERTIES`, `CONSTRAINT_KEY_PATTERN`, and
  `CONSTRAINT_RULE`. Public errors MUST NOT echo rejected values or rule
  literals.
- **VALID-201:** Violations sort by ascending RFC 6901 path and then stable ID
  and are capped at 32 per constrained value. Nested input paths prefix list
  indices as integer pointer tokens and object/map keys as escaped string
  tokens. Planning maps each violation code and complete source pointer without
  invoking a handler; implementations MUST NOT depend on host map iteration
  order.
- **VALID-202:** Output completion applies the same type and field constraints
  after scalar canonicalization and structural completion but before a value is
  emitted or becomes an executable `$current`, `$parent`, or `$result` source.
  A violating member produces `OUTPUT_COMPLETION` at its response path and is
  unavailable while unrelated completed siblings remain. Container constraints
  are not evaluated against a partial container after a child structural or
  field-constraint failure, preventing cascading synthetic violations.

## JSON Schema Draft 2020-12 fidelity

- **VALID-300:** JSON Schema interoperability is pinned to
  `https://json-schema.org/draft/2020-12/schema`. Import treats an omitted
  `$schema` as this caller-selected dialect and MUST reject every other dialect
  or a non-string dialect marker. The result includes `dialect`, `exact`, and
  ordered diagnostics containing stable code, RFC 6901 pointer, keyword, and a
  safe message.
- **VALID-301:** The exact import/export subset is numeric bounds, Unicode-scalar
  string lengths, RE2-compatible search patterns, descriptive `format`, list
  and property cardinality, `uniqueItems`, and `propertyNames` containing only
  `pattern`. Export anchors a Naatre full-match pattern to preserve semantics.
  `$id`, composition keywords, type/nullability keywords, assertion
  vocabularies, precision, scale, byte length, native rules, and any other
  unsupported semantics MUST fail explicitly rather than be dropped or
  weakened.
- **VALID-302:** `$ref` resolution never performs network or filesystem I/O.
  Import may resolve only an exact reference key present in the caller's pinned
  in-memory bundle. The default budgets are 1 MiB per root or referenced
  document and reference depth 16; callers may lower them. Blocked references,
  non-string references, siblings, cycles, depth excess, and byte excess MUST
  fail with located diagnostics.
- **VALID-303:** A mapping is exact only when round-trip behavior preserves all
  selected validation semantics. Unsupported vocabularies, recursive shape,
  `oneOf`, custom scalar behavior, and nullability have no implicit portable
  approximation. An implementation MAY preserve such a source outside the core
  trait for tooling, but it MUST report `exact: false` and MUST NOT advertise
  the resulting trait as authoritative validation.

The language-neutral `core.validation-1` fixture is authoritative for scalar,
container, rule, diagnostic, authority, and JSON Schema boundary examples.
