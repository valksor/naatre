import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  NaatreClientError,
  canonicalStringify,
  createOperation,
  createScalarCodecs,
  decodeBytes,
  decodeOperationResult,
  decodeScalar,
  decodeSelected,
  decodeString,
  encodeBigInt,
  encodeBytes,
  encodeDecimal,
  encodeDuration,
  encodeInt64,
  encodeTimestamp,
  encodeUInt64,
  encodeUUID,
  loadManifest,
  parseJSON,
} from "./index.mjs";

test("plain JavaScript operations match the canonical shared request", async () => {
  const output = JSON.parse(await readFile(new URL("../../../conformance/v1/generator-output.json", import.meta.url), "utf8"));
  const expected = output.operations[0];
  const operation = createOperation({
    name: "GetAccount",
    kind: "query",
    persisted: expected.persisted,
    variables: [
      { name: "id", type: "ID", required: true, nullable: false },
      { name: "nickname", type: "String", required: false, nullable: true },
      { name: "tags", type: "StringList", required: false, nullable: false },
      { name: "filter", type: "StringMap", required: false, nullable: false },
    ],
  }, { id: "acct-1", nickname: null, tags: [], filter: Object.create(null) }, (value) => value);
  assert.equal(operation.canonicalRequest(), canonicalStringify(expected.request));
  const requiredID = { name: "GetAccount", kind: "query", persisted: expected.persisted, variables: [{ name: "id", type: "ID", required: true, nullable: false }] };
  assert.throws(() => createOperation(requiredID, {}, (value) => value), clientCode("CLIENT_VARIABLES_INVALID"));
  assert.throws(() => createOperation(requiredID, { id: 1 }, (value) => value), clientCode("CLIENT_SCALAR_INVALID"));
});

test("selected states and partial errors remain distinct", () => {
  const data = parseJSON('{"present":"Ada","null":null,"__proto__":{"polluted":true}}');
  assert.deepEqual(decodeSelected(data, "missing", false, decodeString), { state: "missing" });
  assert.deepEqual(decodeSelected(data, "later", true, decodeString), { state: "pending" });
  assert.deepEqual(decodeSelected(data, "null", false, decodeString), { state: "null" });
  assert.deepEqual(decodeSelected(data, "present", false, decodeString), { state: "present", value: "Ada" });
  assert.equal(Object.getPrototypeOf(data), null);
  assert.equal(Object.prototype.polluted, undefined);

  const result = decodeOperationResult('{"complete":false,"data":{"name":"Ada"},"errors":[{"code":"PARTIAL"}]}', (value) => value);
  assert.equal(result.complete, false);
  assert.equal(result.data.name, "Ada");
  assert.equal(result.errors[0].code, "PARTIAL");
});

test("runtime validation rejects lossy and ambiguous JavaScript values", () => {
  assert.throws(() => canonicalStringify(9007199254740992), clientCode("CLIENT_VALUE_PRECISION"));
  assert.throws(() => canonicalStringify(1n), clientCode("CLIENT_VALUE_PRECISION"));
  assert.throws(() => canonicalStringify({ value: undefined }), clientCode("CLIENT_VALUE_INVALID"));
  assert.throws(() => canonicalStringify(new Array(1)), clientCode("CLIENT_VALUE_INVALID"));
  assert.throws(() => parseJSON('{"same":1,"same":2}'), clientCode("CLIENT_PROTOCOL_INVALID"));
  assert.throws(() => parseJSON("9007199254740993"), clientCode("CLIENT_VALUE_PRECISION"));
  assert.throws(() => decodeOperationResult({ complete: true, data: null, errors: new Array(1) }, (value) => value), clientCode("CLIENT_PROTOCOL_INVALID"));
});

test("lossless scalar codecs preserve exact wire spellings", () => {
  assert.equal(encodeInt64("-9223372036854775808"), "-9223372036854775808");
  assert.equal(encodeUInt64(18446744073709551615n), "18446744073709551615");
  assert.equal(encodeBigInt("-0009007199254740993"), "-9007199254740993");
  assert.equal(encodeDecimal("001.2300"), "1.23");
  assert.equal(encodeDecimal("-0.000"), "0");
  assert.equal(encodeTimestamp("2026-09-11T23:30:01.123456789+03:00"), "2026-09-11T20:30:01.123456789Z");
  assert.equal(encodeTimestamp("0000-02-29T12:34:56Z"), "0000-02-29T12:34:56Z");
  assert.throws(() => encodeTimestamp(new Date()), clientCode("CLIENT_SCALAR_INVALID"));
  assert.equal(encodeDuration("-00042"), "-42");
  assert.equal(encodeUUID("550e8400-e29b-41d4-a716-446655440000"), "550e8400-e29b-41d4-a716-446655440000");
  assert.throws(() => encodeUUID("550E8400-E29B-41D4-A716-446655440000"), clientCode("CLIENT_SCALAR_INVALID"));
  assert.equal(encodeBytes(new TextEncoder().encode("Hello")), "SGVsbG8");
  assert.equal(new TextDecoder().decode(decodeBytes("SGVsbG8")), "Hello");
  assert.throws(() => decodeBytes("SGVsbG8="), clientCode("CLIENT_SCALAR_INVALID"));
  const codecs = createScalarCodecs({ Money: { encode: encodeDecimal, decode: encodeDecimal } });
  assert.equal(codecs.Money.encode("12.30"), "12.3");
  assert.equal(decodeScalar(codecs, "Money", "01.20"), "1.2");
});

test("operation codecs validate and encode values before canonical JSON", async () => {
  const output = JSON.parse(await readFile(new URL("../../../conformance/v1/generator-output.json", import.meta.url), "utf8"));
  const definition = {
    name: "Pay",
    kind: "mutation",
    persisted: output.operations[0].persisted,
    variables: [
      { name: "amount", type: "Money", required: true, nullable: false },
      { name: "sequence", type: "BigInt", required: true, nullable: false },
      { name: "at", type: "Timestamp", required: false, nullable: false },
      { name: "labels", type: "StringList", required: false, nullable: false },
    ],
  };
  const codecs = createScalarCodecs({ Money: { encode: encodeDecimal, decode: encodeDecimal } });
  const operation = createOperation(definition, { amount: "001.2300", sequence: 9007199254740993n }, (value) => value, { codecs });
  assert.equal(operation.request.variables.amount, "1.23");
  assert.equal(operation.request.variables.sequence, "9007199254740993");
  assert.throws(() => createOperation(definition, { amount: "1", sequence: 9007199254740992 }, (value) => value, { codecs }), clientCode("CLIENT_VALUE_PRECISION"));
  assert.throws(() => createOperation(definition, { amount: "1", sequence: 1n, at: new Date() }, (value) => value, { codecs }), clientCode("CLIENT_SCALAR_INVALID"));
  assert.throws(() => createOperation(definition, { amount: "1", sequence: 1n, labels: new Array(1) }, (value) => value, { codecs }), clientCode("CLIENT_SCALAR_INVALID"));
  let accessed = false;
  const hostile = { amount: "1", sequence: 1n };
  Object.defineProperty(hostile, "at", { enumerable: true, get() { accessed = true; return "2026-01-01T00:00:00Z"; } });
  assert.throws(() => createOperation(definition, hostile, (value) => value, { codecs }), clientCode("CLIENT_VARIABLES_INVALID"));
  assert.equal(accessed, false);
});

test("persisted manifests are strict and prototype safe", async () => {
  const manifestText = await readFile(new URL("../generated/operations.json", import.meta.url), "utf8");
  const manifest = loadManifest(manifestText);
  assert.equal(manifest.GetAccount.kind, "query");
  assert.equal(Object.getPrototypeOf(manifest), null);
  assert.throws(() => loadManifest(manifestText.replace('"version":"1"', '"version":"2"')), clientCode("CLIENT_MANIFEST_INVALID"));
  assert.throws(() => loadManifest(manifestText.replace('"profile":', '"unexpected":true,"profile":')), clientCode("CLIENT_MANIFEST_INVALID"));
});

function clientCode(code) {
  return (error) => error instanceof NaatreClientError && error.code === code;
}
