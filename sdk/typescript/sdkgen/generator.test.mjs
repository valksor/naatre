import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { canonicalStringify } from "../runtime/index.mjs";
import { TypeScriptGeneratorError, generate, generatorVersion } from "./generator.mjs";

const modelURL = new URL("../../../conformance/v1/generator-model.json", import.meta.url);
const referenceURL = new URL("../../../conformance/v1/generator-output.json", import.meta.url);

test("generator reproduces checked artifacts byte for byte", async () => {
  const [model, reference, source, manifest] = await Promise.all([
    readFile(modelURL),
    readFile(referenceURL),
    readFile(new URL("../generated/operations.ts", import.meta.url), "utf8"),
    readFile(new URL("../generated/operations.json", import.meta.url), "utf8"),
  ]);
  const first = generate(model, reference);
  const second = generate(model, reference);
  assert.equal(first.source, source);
  assert.equal(first.manifest, manifest);
  assert.deepEqual(first, second);
  assert.match(first.source, new RegExp(generatorVersion));
  assert.match(first.source, /unknown: string/u);
});

test("generator rejects drift, unmapped scalars, and normalized symbol collisions", async () => {
  const [modelBytes, referenceBytes] = await Promise.all([readFile(modelURL), readFile(referenceURL)]);
  const reference = JSON.parse(referenceBytes);
  reference.operations[0].persisted.digest = "0".repeat(64);
  assert.throws(() => generate(modelBytes, JSON.stringify(reference)), generatorCode("TYPESCRIPT_SDK_GENERATOR_REFERENCE_DRIFT"));

  const model = JSON.parse(modelBytes);
  delete model.configuration.scalarMappings.Money;
  assert.throws(() => generate(JSON.stringify(model), referenceBytes), generatorCode("TYPESCRIPT_SDK_GENERATOR_UNMAPPED_SCALAR"));

  const collision = JSON.parse(modelBytes);
  collision.schema.types.push({ id: "status", name: "status", kind: "scalar", input: true, output: true });
  collision.configuration.scalarMappings.status = "string";
  assert.throws(() => generate(JSON.stringify(collision), referenceBytes), generatorCode("TYPESCRIPT_SDK_GENERATOR_SYMBOL_COLLISION"));

  const badMapping = JSON.parse(modelBytes);
  badMapping.configuration.scalarMappings.Money = "lossy-number";
  const badReference = JSON.parse(referenceBytes);
  badReference.scalarMappings.Money = "lossy-number";
  badReference.schema.digest = schemaDigest(badMapping.schema);
  assert.throws(() => generate(JSON.stringify(badMapping), JSON.stringify(badReference)), generatorCode("TYPESCRIPT_SDK_GENERATOR_UNMAPPED_SCALAR"));
});

test("generator represents open enums, tagged unions, input objects, one-of inputs, and deprecations", async () => {
  const [modelBytes, referenceBytes] = await Promise.all([readFile(modelURL), readFile(referenceURL)]);
  const model = JSON.parse(modelBytes);
  const reference = JSON.parse(referenceBytes);
  model.schema.types.push(
    { id: "Admin", name: "Admin", kind: "object", output: true, fields: [{ id: "Admin.id", name: "id", type: "ID", required: true }] },
    { id: "Choice", name: "Choice", kind: "oneof", input: true, fields: [{ id: "Choice.email", name: "email", type: "String", nullable: true }, { id: "Choice.phone", name: "phone", type: "String" }] },
    { id: "Filter", name: "Filter", kind: "input-object", input: true, description: "Filter input", fields: [{ id: "Filter.query", name: "query", type: "String", required: true, deprecation: { reason: "use term", replacement: "term" } }] },
    { id: "SearchResult", name: "SearchResult", kind: "union", output: true, open: true, variantMembers: [{ id: "SearchResult.Admin", type: "Admin" }, { id: "SearchResult.Account", type: "Account" }] },
    { id: "Tags", name: "Tags", kind: "list", input: true, output: true, element: "String", elementNullable: true },
    { id: "Translations", name: "Translations", kind: "map", input: true, output: true, element: "String" },
  );
  reference.schema.digest = schemaDigest(model.schema);
  const { source } = generate(JSON.stringify(model), JSON.stringify(reference));
  assert.match(source, /export type Status = "ACTIVE" \| "PENDING" \| \{ readonly unknown: string \};/u);
  assert.match(source, /readonly "\$type": "Account"; readonly "\$value": Account/u);
  assert.match(source, /readonly unknown: true/u);
  assert.match(source, /export interface Filter/u);
  assert.match(source, /@deprecated use term; use term/u);
  assert.match(source, /export type Choice = \{ readonly email: string \| null; readonly phone\?: never \} \| \{ readonly email\?: never; readonly phone: string \};/u);
  assert.match(source, /export type Tags = readonly \(string \| null\)\[\];/u);
  assert.match(source, /export type Translations = Readonly<Record<string, string>>;/u);
});

test("generated result decoding uses configured custom scalar codecs", async () => {
  const [modelBytes, referenceBytes] = await Promise.all([readFile(modelURL), readFile(referenceURL)]);
  const model = JSON.parse(modelBytes);
  const reference = JSON.parse(referenceBytes);
  model.operations[0].document.operations[0].select[0].$call.select[0].$field.name = "balance";
  model.operations[0].result.fields[0].result.fields[0].result.type = "Money";
  reference.operations[0].result.data = structuredClone(model.operations[0].result);
  const digest = semanticDigest("document", model.operations[0].document);
  reference.operations[0].persisted.digest = digest;
  reference.operations[0].request.persisted.digest = digest;
  const { source } = generate(JSON.stringify(model), JSON.stringify(reference));
  assert.match(source, /decodeScalar/u);
  assert.match(source, /decodeScalar\(codecs, "Money", value\)/u);
});

function generatorCode(code) {
  return (error) => error instanceof TypeScriptGeneratorError && error.code === code;
}

function schemaDigest(schema) {
  const value = structuredClone(schema);
  sortStrings(value, "capabilities");
  sortByID(value, "types");
  for (const descriptor of value.types) {
    for (const member of ["variants", "enumValues", "capabilities"]) sortStrings(descriptor, member);
    for (const member of ["fields", "enumMembers", "variantMembers", "retired", "traits"]) sortByID(descriptor, member);
    if (descriptor.entity) sortStrings(descriptor.entity, "keys");
    if (descriptor.scalar) sortStrings(descriptor.scalar, "acceptedWireShapes");
  }
  return semanticDigest("schema", value);
}

function semanticDigest(purpose, value) {
  return createHash("sha256").update(`naatre:${purpose}:c14n-1\n`).update(canonicalStringify(value)).digest("hex");
}

function sortStrings(value, member) {
  if (Array.isArray(value?.[member])) value[member].sort();
}

function sortByID(value, member) {
  if (Array.isArray(value?.[member])) value[member].sort((left, right) => left.id < right.id ? -1 : left.id > right.id ? 1 : 0);
}
