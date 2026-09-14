import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { canonicalStringify, createOperation, decodeOperationResult, encodeBigInt, encodeDecimal, encodeTimestamp, parseJSON } from "../../sdk/typescript/runtime/index.mjs";
import { generate } from "../../sdk/typescript/sdkgen/generator.mjs";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/typescript-sdk.json", root), "utf8"));

assert.equal(fixture.profile, "sdk.typescript.core-1");
for (const evidence of fixture.evidence) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256);
}

const model = readFileSync(new URL(fixture.sources.model.path, root));
const reference = readFileSync(new URL(fixture.sources.referenceOutput.path, root));
const artifacts = generate(model, reference);
assert.equal(artifacts.source, readFileSync(new URL(fixture.generated.source.path, root), "utf8"));
assert.equal(artifacts.manifest, readFileSync(new URL(fixture.generated.manifest.path, root), "utf8"));

const referenceOutput = JSON.parse(reference);
const operation = createOperation({
  name: "GetAccount",
  kind: "query",
  persisted: referenceOutput.operations[0].persisted,
  variables: [{ name: "id", type: "ID", required: true, nullable: false }],
}, { id: "acct-1" }, (value) => value);
assert.equal(operation.canonicalRequest(), canonicalStringify({ ...referenceOutput.operations[0].request, variables: { id: "acct-1" } }));
assert.equal(encodeBigInt("9007199254740993"), "9007199254740993");
assert.equal(encodeDecimal("001.2300"), "1.23");
assert.equal(encodeTimestamp("2026-09-11T23:30:01.123456789+03:00"), "2026-09-11T20:30:01.123456789Z");
assert.throws(() => canonicalStringify(new Array(1)));
assert.throws(() => canonicalStringify({ value: 9007199254740992 }));
const hostile = parseJSON('{"__proto__":{"polluted":true},"constructor":1,"prototype":2}');
assert.equal(Object.getPrototypeOf(hostile), null);
assert.equal(Object.prototype.polluted, undefined);
const partial = decodeOperationResult('{"complete":false,"data":{"name":"Ada"},"errors":[{"code":"PARTIAL"}]}', (value) => value);
assert.equal(partial.data.name, "Ada");
assert.equal(partial.errors[0].code, "PARTIAL");

const tests = spawnSync(process.execPath, [
  "--test",
  fileURLToPath(new URL("sdk/typescript/runtime/index.test.mjs", root)),
  fileURLToPath(new URL("sdk/typescript/sdkgen/generator.test.mjs", root)),
], { encoding: "utf8", timeout: 30_000 });
if (tests.error || tests.status !== 0) throw new Error("JavaScript SDK tests failed");

const typecheck = spawnSync("tsc", ["-p", fileURLToPath(new URL("sdk/typescript/tsconfig.json", root))], { encoding: "utf8", timeout: 30_000 });
if (typecheck.error || typecheck.status !== 0) throw new Error("TypeScript declarations failed strict compilation");

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", generatorVersion: fixture.generatorVersion })}\n`);
