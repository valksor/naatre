#!/usr/bin/env node

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const fixture = JSON.parse(readFileSync(new URL("../v1/cbor.json", import.meta.url), "utf8"));
const tags = Object.freeze({ timestamp: 0n, bigPos: 2n, bigNeg: 3n, decimal: 4n, uuid: 37n, int64: 60000n, uint64: 60001n, duration: 60002n });

function concat(...parts) {
  const size = parts.reduce((total, part) => total + part.length, 0);
  const output = new Uint8Array(size);
  let offset = 0;
  for (const part of parts) { output.set(part, offset); offset += part.length; }
  return output;
}

function head(major, value) {
  value = BigInt(value);
  if (value < 24n) return Uint8Array.of((major << 5) | Number(value));
  if (value <= 0xffn) return Uint8Array.of((major << 5) | 24, Number(value));
  if (value <= 0xffffn) return Uint8Array.of((major << 5) | 25, Number((value >> 8n) & 0xffn), Number(value & 0xffn));
  if (value <= 0xffffffffn) return Uint8Array.of((major << 5) | 26, ...[24n, 16n, 8n, 0n].map((shift) => Number((value >> shift) & 0xffn)));
  return Uint8Array.of((major << 5) | 27, ...[56n, 48n, 40n, 32n, 24n, 16n, 8n, 0n].map((shift) => Number((value >> shift) & 0xffn)));
}

function text(value) {
  const encoded = new TextEncoder().encode(value);
  return concat(head(3, encoded.length), encoded);
}

function signed(value) {
  value = BigInt(value);
  return value >= 0n ? head(0, value) : head(1, -value - 1n);
}

function magnitude(value) {
  if (value === 0n) return new Uint8Array();
  let hex = value.toString(16);
  if (hex.length % 2) hex = `0${hex}`;
  return Uint8Array.from(hex.match(/../g).map((pair) => Number.parseInt(pair, 16)));
}

function bigInteger(value) {
  value = BigInt(value);
  const negative = value < 0n;
  const bytes = magnitude(negative ? -value - 1n : value);
  return concat(head(6, negative ? tags.bigNeg : tags.bigPos), head(2, bytes.length), bytes);
}

function arbitraryInteger(value) {
  value = BigInt(value);
  if (value >= -(1n << 64n) && value <= (1n << 64n) - 1n) return signed(value);
  return bigInteger(value);
}

function map(value) {
  const entries = Object.entries(value).filter(([, current]) => current?.kind !== "missing").map(([key, current]) => ({ key: text(key), value: current }));
  entries.sort((left, right) => left.key.length - right.key.length || Buffer.compare(left.key, right.key));
  return concat(head(5, entries.length), ...entries.flatMap((entry) => [entry.key, encode(entry.value)]));
}

function encode(value) {
  if (value === null) return Uint8Array.of(0xf6);
  if (typeof value === "boolean") return Uint8Array.of(value ? 0xf5 : 0xf4);
  if (typeof value === "string") return text(value);
  if (typeof value === "number") {
    if (Number.isInteger(value) && !Object.is(value, -0)) {
      assert(Number.isSafeInteger(value), "bounded integer safe range");
      return signed(BigInt(value));
    }
    assert(Number.isFinite(value), "finite Float64");
    const output = new Uint8Array(9);
    output[0] = 0xfb;
    new DataView(output.buffer).setFloat64(1, value === 0 ? 0 : value);
    return output;
  }
  if (Array.isArray(value)) return concat(head(4, value.length), ...value.map(encode));
  if (!value || typeof value !== "object") throw new Error("unsupported value");
  switch (value.kind) {
    case "missing": throw new Error("missing is map-only");
    case "id": case "enum": return text(value.value);
    case "int64": return concat(head(6, tags.int64), signed(BigInt(value.value)));
    case "uint64": return concat(head(6, tags.uint64), head(0, BigInt(value.value)));
    case "bigint": return bigInteger(BigInt(value.value));
    case "decimal": return concat(head(6, tags.decimal), head(4, 2), signed(BigInt(value.exponent)), arbitraryInteger(BigInt(value.mantissa)));
    case "timestamp": return concat(head(6, tags.timestamp), text(value.value));
    case "duration": return concat(head(6, tags.duration), arbitraryInteger(BigInt(value.value)));
    case "uuid": {
      const hex = value.value.replaceAll("-", "");
      assert.match(hex, /^[0-9a-f]{32}$/);
      return concat(head(6, tags.uuid), head(2, 16), Uint8Array.from(hex.match(/../g).map((pair) => Number.parseInt(pair, 16))));
    }
    case "bytes": {
      const bytes = Buffer.from(value.value.replaceAll("-", "+").replaceAll("_", "/"), "base64");
      return concat(head(2, bytes.length), bytes);
    }
    case "union": return map({ $type: value.type, $value: value.value });
    default: return map(value);
  }
}

function fixtureValue(vector) {
  switch (vector.type) {
    case "object": return { missing: { kind: "missing" }, null: null };
    case "Null": case "Boolean": case "String": case "Int32": case "Float64": case "List": case "Map": return vector.value === "-0" ? -0 : vector.value;
    case "ID": return { kind: "id", value: vector.value };
    case "BoundedInteger": return Number(vector.value);
    case "Enum": return { kind: "enum", value: vector.value };
    case "Int64": return { kind: "int64", value: vector.value };
    case "UInt64": return { kind: "uint64", value: vector.value };
    case "BigInt": return { kind: "bigint", value: vector.value };
    case "Decimal": return { kind: "decimal", exponent: vector.exponent, mantissa: vector.mantissa };
    case "Timestamp": return { kind: "timestamp", value: vector.value };
    case "Duration": return { kind: "duration", value: vector.value };
    case "UUID": return { kind: "uuid", value: vector.value };
    case "Bytes": return { kind: "bytes", value: vector.value };
    case "Union": return { kind: "union", type: vector.value.$type, value: vector.value.$value };
    default: throw new Error(`unknown fixture type ${vector.type}`);
  }
}

assert.equal(fixture.profile, "transport.cbor.unary-1");
assert.equal(fixture.codecRevision, "cbor-det-1");
for (const vector of fixture.vectors) {
  assert.equal(Buffer.from(encode(fixtureValue(vector))).toString("hex"), vector.hex, vector.name);
}

const canonical = Buffer.from(fixture.semanticIdentity.canonicalJSON);
const encodedDocument = encode(JSON.parse(canonical));
assert.equal(Buffer.from(encodedDocument).toString("hex"), fixture.semanticIdentity.cborHex);
assert.equal(createHash("sha256").update(canonical).digest("hex"), fixture.semanticIdentity.jsonRepresentationSHA256);
assert.equal(createHash("sha256").update(encodedDocument).digest("hex"), fixture.semanticIdentity.cborRepresentationSHA256);
assert.notEqual(fixture.semanticIdentity.jsonRepresentationSHA256, fixture.semanticIdentity.cborRepresentationSHA256);

console.log(`verified ${fixture.vectors.length} deterministic CBOR vectors with independent JavaScript codec ${fixture.codecRevision}`);
