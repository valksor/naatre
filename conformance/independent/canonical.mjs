import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const fixture = JSON.parse(
  readFileSync(new URL("../v1/canonical.json", import.meta.url), "utf8"),
);
const scalarFixture = JSON.parse(
  readFileSync(new URL("../v1/scalars.json", import.meta.url), "utf8"),
);

if (
  fixture.profile !== "core.interop.c14n-1" ||
  fixture.canonicalVersion !== "c14n-1" ||
  fixture.hashAlgorithm !== "sha-256" ||
  fixture.scalarProfile !== scalarFixture.profile
) {
  throw new Error("invalid canonical conformance fixture profile");
}

let names;

function runFixture() {
  names = new Set();
  verifyJSONVectors();
  verifyHashVectors();
  verifyEquivalenceVectors();
  verifyIsolationVectors();
  verifyMalformedVectors();
  verifyScalarReferences();
  reportSuccess();
}

function verifyJSONVectors() {
  for (const vector of fixture.jsonVectors) {
    uniqueName(vector.name);
    const canonical = canonicalize(Buffer.from(vector.input, "utf8"));
    require(canonical ===
      vector.canonical, `${vector.name}: canonical mismatch`);
    require(jsonType(strictParse(vector.input)) ===
      vector.parsedType, `${vector.name}: parsed type mismatch`);
  }
}

function verifyHashVectors() {
  const domainPurposes = new Set();
  const domainDigests = new Set();
  for (const vector of fixture.hashVectors) {
    uniqueName(vector.name);
    const canonical = vector.name.startsWith("same-payload-")
      ? canonicalize(Buffer.from(vector.input, "utf8"))
      : canonicalizePurpose(vector.purpose, vector.input);
    require(canonical ===
      vector.canonical, `${vector.name}: canonical mismatch`);
    require(jsonType(strictParse(vector.input)) ===
      vector.parsedType, `${vector.name}: parsed type mismatch`);
    const domainInput = `naatre:${vector.purpose}:${fixture.canonicalVersion}\n${canonical}`;
    require(domainInput ===
      vector.domainInput, `${vector.name}: domain input mismatch`);
    require(semanticHash(vector.purpose, canonical) ===
      vector.digest, `${vector.name}: digest mismatch`);
    if (vector.name.startsWith("same-payload-")) {
      domainPurposes.add(vector.purpose);
      domainDigests.add(vector.digest);
    }
  }
  require(domainPurposes.size === 6 &&
    domainDigests.size === 6, "six-way domain separation is incomplete");
}

function verifyEquivalenceVectors() {
  for (const vector of fixture.equivalenceVectors) {
    uniqueName(vector.name);
    const left = canonicalDigest(vector.purpose, vector.left);
    const right = canonicalDigest(vector.purpose, vector.right);
    require((left === right) ===
      vector.equal, `${vector.name}: equivalence mismatch`);
  }
}

function verifyIsolationVectors() {
  for (const vector of fixture.isolationVectors) {
    uniqueName(vector.name);
    assertPurposeIsolationPayload(vector.purpose, strictParse(vector.payloadLeft));
    assertPurposeIsolationPayload(vector.purpose, strictParse(vector.payloadRight));
    const documentEqual =
      canonicalDigest("document", vector.documentLeft) ===
      canonicalDigest("document", vector.documentRight);
    const identityEqual =
      canonicalDigest(vector.purpose, vector.payloadLeft) ===
      canonicalDigest(vector.purpose, vector.payloadRight);
    require(documentEqual ===
      vector.documentEqual, `${vector.name}: document isolation mismatch`);
    require(identityEqual ===
      vector.identityEqual, `${vector.name}: purpose isolation mismatch`);
  }
}

function verifyMalformedVectors() {
  for (const vector of fixture.malformed) {
    uniqueName(vector.name);
    const input = vector.inputBase64
      ? Buffer.from(vector.inputBase64, "base64")
      : Buffer.from(vector.input, "utf8");
    let code;
    try {
      canonicalize(input);
    } catch (error) {
      code = error.code;
    }
    require(code ===
      vector.code, `${vector.name}: code ${code}, expected ${vector.code}`);
  }
}

function verifyScalarReferences() {
  const scalarNames = new Set(
    scalarFixture.vectors.map((vector) => vector.name),
  );
  for (const name of fixture.requiredScalarVectors) {
    require(scalarNames.has(name), `missing required scalar vector ${name}`);
  }
}

function reportSuccess() {
  console.log(
    `canonical conformance: ${fixture.jsonVectors.length} JSON, ${fixture.hashVectors.length} hash, ` +
      `${fixture.equivalenceVectors.length} equivalence, ${fixture.isolationVectors.length} isolation, ` +
      `${fixture.malformed.length} malformed vectors passed`,
  );
}

function canonicalDigest(purpose, input) {
  return semanticHash(purpose, canonicalizePurpose(purpose, input));
}

function canonicalizePurpose(purpose, input) {
  const value = strictParse(input);
  if (purpose === "document") normalizeDocument(value);
  if (purpose === "schema") normalizeSchema(value);
  if (purpose === "approval" || purpose === "result-cache") {
    require(jsonType(value) === "object", "hash payload must be an object");
    normalizeStringSet(value, "capabilities");
  }
  return writeCanonical(value);
}

function normalizeDocument(value) {
  require(jsonType(value) === "object", "document must be an object");
  if (!Object.hasOwn(value, "requires")) return;
  require(Array.isArray(value.requires), "requires must be an array");
  const seen = new Set();
  for (const identifier of value.requires) {
    require(typeof identifier === "string" &&
      /^[A-Za-z_][A-Za-z0-9_.-]{0,127}$/.test(identifier) &&
      !seen.has(
        identifier,
      ), "requires entries must be unique portable identifiers");
    seen.add(identifier);
  }
  value.requires.sort();
}

function normalizeSchema(value) {
  require(jsonType(value) === "object", "schema must be an object");
  require(isPortableIdentifier(value.revision), "schema requires a portable revision");
  require(Array.isArray(value.types), "schema requires a types array");
  normalizeStringSet(value, "capabilities");
  normalizeSchemaTypes(value.types);
  normalizeSchemaCallables(value);
  normalizeIDArray(value, "retired");
  normalizeIDArray(value, "traits");
  normalizeReferences(value);
}

function normalizeSchemaTypes(types) {
  const identifiers = new Set();
  for (const descriptor of types) {
    require(jsonType(descriptor) === "object" &&
      isPortableIdentifier(descriptor.id) &&
      !identifiers.has(descriptor.id), "types require unique portable identifiers");
    identifiers.add(descriptor.id);
    normalizeSchemaType(descriptor);
  }
  types.sort(compareIdentifiers);
}

function normalizeSchemaType(descriptor) {
  for (const member of ["variants", "enumValues", "capabilities"]) {
    normalizeStringSet(descriptor, member);
  }
  for (const member of ["fields", "enumMembers", "variantMembers"]) {
    for (const declaration of normalizeIDArray(descriptor, member)) {
      normalizeIDArray(declaration, "traits");
    }
  }
  normalizeIDArray(descriptor, "retired");
  normalizeIDArray(descriptor, "traits");
  normalizeNestedStringSet(descriptor, "entity", "keys", false);
  normalizeNestedStringSet(descriptor, "scalar", "acceptedWireShapes");
}

function normalizeNestedStringSet(descriptor, objectMember, arrayMember, identifiers = true) {
  if (!Object.hasOwn(descriptor, objectMember)) return;
  require(jsonType(descriptor[objectMember]) === "object", `${objectMember} descriptor must be an object`);
  normalizeStringSet(descriptor[objectMember], arrayMember, identifiers);
}

function normalizeSchemaCallables(value) {
  for (const member of ["operations", "members"]) {
    const declarations = normalizeIDArray(value, member);
    for (const declaration of declarations) {
      normalizeStringSet(declaration, "capabilities");
      normalizeIDArray(declaration, "traits");
    }
  }
}

function normalizeIDArray(value, member) {
  return normalizeArrayMember(value, member, (declaration) =>
    jsonType(declaration) === "object" && isPortableIdentifier(declaration.id) ? declaration.id : null,
  `${member} entries require unique portable identifiers`, compareIdentifiers);
}

function normalizeReferences(value) {
  if (!Object.hasOwn(value, "references")) return;
  require(Array.isArray(value.references), "references must be an array");
  const seen = new Set();
  for (const reference of value.references) {
    require(jsonType(reference) === "object" && typeof reference.uri === "string" &&
      typeof reference.revision === "string", "references require uri and revision strings");
    const identifier = `${reference.uri}\u0000${reference.revision}`;
    require(!seen.has(identifier), "references must be unique");
    seen.add(identifier);
  }
  value.references.sort((left, right) => left.uri === right.uri
    ? left.revision < right.revision ? -1 : left.revision > right.revision ? 1 : 0
    : left.uri < right.uri ? -1 : 1);
}

function normalizeStringSet(value, member, identifiers = true) {
  normalizeArrayMember(value, member, (identifier) =>
    typeof identifier === "string" && (!identifiers || isPortableIdentifier(identifier)) ? identifier : null,
  `${member} entries must be unique portable identifiers`, (left, right) => left.localeCompare(right));
}

function normalizeArrayMember(value, member, identity, message, compare) {
  if (!Object.hasOwn(value, member)) return [];
  require(Array.isArray(value[member]), `${member} must be an array`);
  const seen = new Set();
  for (const item of value[member]) {
    const identifier = identity(item);
    require(identifier !== null && !seen.has(identifier), message);
    seen.add(identifier);
  }
  value[member].sort(compare);
  return value[member];
}

function compareIdentifiers(left, right) {
  return left.id < right.id ? -1 : left.id > right.id ? 1 : 0;
}

function isPortableIdentifier(value) {
  return typeof value === "string" && /^[A-Za-z_][A-Za-z0-9_.-]{0,127}$/.test(value);
}

function assertPurposeIsolationPayload(purpose, payload) {
  const required = {
    "result-cache": ["document", "schema", "variables", "capabilities"],
    approval: ["document", "schema", "policyRevision", "capabilities", "approvalMetadata"],
  }[purpose];
  require(required !== undefined &&
    Object.keys(payload).length === required.length &&
    required.every((name) => Object.hasOwn(payload, name)),
  `${purpose} isolation payload has invalid members`);
  for (const name of ["document", "schema"]) {
    const digest = payload[name];
    require(jsonType(digest) === "object" &&
      Object.keys(digest).length === 3 &&
      digest.algorithm === "sha-256" &&
      digest.canonicalVersion === "c14n-1" &&
      /^[0-9a-f]{64}$/.test(digest.digest),
    `${purpose} isolation payload has invalid ${name} digest record`);
  }
}

function semanticHash(purpose, canonical) {
  const allowed = new Set([
    "document",
    "schema",
    "approval",
    "result-cache",
    "idempotency",
    "signed-message",
  ]);
  require(allowed.has(purpose), `unknown hash purpose ${purpose}`);
  return createHash("sha256")
    .update(`naatre:${purpose}:c14n-1\n`, "utf8")
    .update(canonical, "utf8")
    .digest("hex");
}

function canonicalize(bytes) {
  if (
    bytes.length >= 3 &&
    bytes[0] === 0xef &&
    bytes[1] === 0xbb &&
    bytes[2] === 0xbf
  ) {
    fail("INVALID_UTF8", "UTF-8 BOM is not allowed");
  }
  let input;
  try {
    input = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(
      bytes,
    );
  } catch {
    fail("INVALID_UTF8", "invalid UTF-8");
  }
  return writeCanonical(strictParse(input));
}

function strictParse(input) {
  return new StrictParser(input).parse();
}

class StrictParser {
  constructor(input) {
    this.input = input;
    this.index = 0;
  }

  parse() {
    const value = this.value();
    this.space();
    if (this.index !== this.input.length)
      fail("TRAILING_DATA", "trailing data");
    return value;
  }

  value() {
    this.space();
    if (this.index >= this.input.length)
      fail("MALFORMED_JSON", "unexpected end");
    const current = this.input[this.index];
    if (current === "{") return this.object();
    if (current === "[") return this.array();
    if (current === '"') return this.string();
    if (this.input.startsWith("true", this.index))
      return this.keyword("true", true);
    if (this.input.startsWith("false", this.index))
      return this.keyword("false", false);
    if (this.input.startsWith("null", this.index))
      return this.keyword("null", null);
    return this.number();
  }

  object() {
    this.index += 1;
    this.space();
    const value = Object.create(null);
    const names = new Set();
    if (this.take("}")) return value;
    while (true) {
      if (this.input[this.index] !== '"')
        fail("MALFORMED_JSON", "expected member name");
      const name = this.string();
      if (names.has(name)) fail("DUPLICATE_KEY", "duplicate member");
      names.add(name);
      this.space();
      if (!this.take(":")) fail("MALFORMED_JSON", "expected colon");
      value[name] = this.value();
      this.space();
      if (this.take("}")) return value;
      if (!this.take(",")) fail("MALFORMED_JSON", "expected comma");
      this.space();
    }
  }

  array() {
    this.index += 1;
    this.space();
    const value = [];
    if (this.take("]")) return value;
    while (true) {
      value.push(this.value());
      this.space();
      if (this.take("]")) return value;
      if (!this.take(",")) fail("MALFORMED_JSON", "expected comma");
    }
  }

  string() {
    this.index += 1;
    let value = "";
    while (this.index < this.input.length) {
      const unit = this.input.charCodeAt(this.index);
      if (unit === 0x22) {
        this.index += 1;
        return value;
      }
      if (unit === 0x5c) {
        this.index += 1;
        value += this.escape();
        continue;
      }
      value += this.rawScalar(unit);
    }
    fail("MALFORMED_JSON", "unterminated string");
  }

  rawScalar(unit) {
    if (unit < 0x20) fail("MALFORMED_JSON", "unescaped control");
    if (unit >= 0xdc00 && unit <= 0xdfff)
      fail("INVALID_UNICODE", "unpaired surrogate");
    if (unit < 0xd800 || unit > 0xdbff) return this.input[this.index++];
    const next = this.input.charCodeAt(this.index + 1);
    if (next < 0xdc00 || next > 0xdfff)
      fail("INVALID_UNICODE", "unpaired surrogate");
    const value = this.input.slice(this.index, this.index + 2);
    this.index += 2;
    return value;
  }

  escape() {
    if (this.index >= this.input.length)
      fail("MALFORMED_JSON", "unterminated escape");
    const escape = this.input[this.index++];
    const simple = {
      '"': '"',
      "\\": "\\",
      "/": "/",
      b: "\b",
      f: "\f",
      n: "\n",
      r: "\r",
      t: "\t",
    };
    if (Object.hasOwn(simple, escape)) return simple[escape];
    if (escape !== "u") fail("MALFORMED_JSON", "invalid escape");
    const first = this.hexUnit();
    if (first >= 0xdc00 && first <= 0xdfff)
      fail("INVALID_UNICODE", "unpaired surrogate");
    if (first < 0xd800 || first > 0xdbff) return String.fromCharCode(first);
    if (this.input.slice(this.index, this.index + 2) !== "\\u") {
      fail("INVALID_UNICODE", "unpaired surrogate");
    }
    this.index += 2;
    const second = this.hexUnit();
    if (second < 0xdc00 || second > 0xdfff)
      fail("INVALID_UNICODE", "unpaired surrogate");
    return String.fromCodePoint(
      0x10000 + ((first - 0xd800) << 10) + second - 0xdc00,
    );
  }

  hexUnit() {
    const raw = this.input.slice(this.index, this.index + 4);
    if (!/^[0-9A-Fa-f]{4}$/.test(raw))
      fail("MALFORMED_JSON", "invalid Unicode escape");
    this.index += 4;
    return Number.parseInt(raw, 16);
  }

  number() {
    const match = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(
      this.input.slice(this.index),
    );
    if (!match) fail("MALFORMED_JSON", "invalid token");
    this.index += match[0].length;
    const value = Number(match[0]);
    if (!Number.isFinite(value)) fail("MALFORMED_JSON", "non-finite number");
    return value;
  }

  keyword(keyword, value) {
    this.index += keyword.length;
    return value;
  }

  space() {
    while (
      this.index < this.input.length &&
      /[ \t\r\n]/.test(this.input[this.index])
    ) {
      this.index += 1;
    }
  }

  take(expected) {
    if (this.input[this.index] !== expected) return false;
    this.index += 1;
    return true;
  }
}

function writeCanonical(value) {
  if (value === null) return "null";
  if (typeof value === "boolean") return value ? "true" : "false";
  if (typeof value === "string") return JSON.stringify(value);
  if (typeof value === "number")
    return Object.is(value, -0) ? "0" : JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(writeCanonical).join(",")}]`;
  return `{${Object.keys(value)
    .sort()
    .map((name) => `${JSON.stringify(name)}:${writeCanonical(value[name])}`)
    .join(",")}}`;
}

function jsonType(value) {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  return typeof value;
}

function uniqueName(name) {
  require(typeof name === "string" &&
    name !== "" &&
    !names.has(name), `invalid vector name ${name}`);
  names.add(name);
}

function require(condition, message) {
  if (!condition) throw new Error(message);
}

function fail(code, message) {
  const error = new Error(message);
  error.code = code;
  throw error;
}

runFixture();
