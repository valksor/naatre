import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/typescript-worker.json", root), "utf8"));

assert.equal(fixture.profile, "worker.javascript-typescript-1");
assert.equal(fixture.protocol, "naatre.remote-worker.v1");
assert.equal(fixture.runtimeVersion, "naatre.typescript.worker-1");
assert.equal(fixture.runtimeAdapterVersion, "naatre.typescript.worker-adapters-1");
assert.equal(fixture.dependencies.issue60Commit, "2486b1ce95aeed77391081d00293eb28482a6414");
assert.equal(fixture.schema.sharedSchemaProfile, "core.schema-1");
assert.equal(fixture.schema.generatorProfile, "sdk.generation-1");
assert.deepEqual(fixture.handlerImplementations.map((entry) => entry.language), ["plain-javascript", "typescript"]);
assert.deepEqual(fixture.handlerImplementations[0].vectors, fixture.handlerImplementations[1].vectors);
const requiredVectors = ["registration", "prototype-safety", "abort-signal", "streaming", "backpressure", "warm-instance-isolation", "output-validation", "shutdown", "stable-public-failures", "resource-limits"];
assert.deepEqual(fixture.requiredRuntimeVectors, requiredVectors);
assert.deepEqual(Object.keys(fixture.package.runtimeExports), ["node", "bun", "deno", "edge"]);
assert.deepEqual(fixture.runtimes.map((entry) => entry.runtime), ["node", "bun", "deno", "workerd"]);
for (const runtime of fixture.runtimes) {
  assert.equal(runtime.status, "supported");
  assert.ok(["passed", "integration-required"].includes(runtime.execution));
  assert.equal(runtime.requiredVectors, "requiredRuntimeVectors");
  assert.ok(runtime.command.includes(runtime.runtime === "workerd" ? "workerd" : runtime.runtime));
}
assert.deepEqual(fixture.runtimes.filter((entry) => entry.execution === "passed").map((entry) => entry.runtime), ["node", "bun"]);
assert.deepEqual(fixture.commands.integration, fixture.runtimes.slice(2).map((entry) => entry.command));
for (const command of fixture.commands.offline) assert.equal(typeof command, "string");
assert.equal(fixture.claims.runtimeAdapterOwnerIssue, 101);
assert.equal(fixture.claims.combinedMatrixOwnerIssue, 69);
assert.equal(fixture.claims.runtimeLifecycleAdapters, true);
for (const claim of ["nativeRuntime", "hardTermination", "processIsolation", "productionHTTP2Server", "frameworkAdapters"]) assert.equal(fixture.claims[claim], false);

for (const evidence of [...Object.values(fixture.dependencies).filter((entry) => typeof entry === "object"), ...fixture.schema.sources, ...fixture.evidence]) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256, evidence.path);
}

run(process.execPath, ["--test", file("sdk/typescript/runtime/server.test.mjs"), file("sdk/typescript/runtime/exports.test.mjs"), file("sdk/typescript/sdkgen/generator.test.mjs")], "JavaScript worker tests");
run("tsc", ["-p", file("sdk/typescript/tsconfig.json")], "TypeScript worker declarations");
const node = run(process.execPath, [file("conformance/independent/typescript-worker-runtime.mjs")], "Node worker runtime");
validateRuntime(node.stdout, "node-26.8.2", "worker.javascript-typescript.node-1", ["plain-javascript", "typescript"]);
const bun = run("bun", [file("conformance/independent/typescript-worker-runtime.mjs")], "Bun worker runtime");
validateRuntime(bun.stdout, "bun-1.4.2", "worker.javascript-typescript.bun-1", ["plain-javascript", "typescript"]);

function validateRuntime(output, runtime, runtimeProfile, implementations) {
  const result = JSON.parse(output.trim());
  assert.equal(result.profile, fixture.profile);
  assert.equal(result.status, "passed");
  assert.equal(result.runtime, runtime);
  assert.equal(result.runtimeProfile, runtimeProfile);
  assert.deepEqual(result.vectors, requiredVectors);
  assert.deepEqual(result.implementations.map((entry) => entry.language), implementations);
  for (const implementation of result.implementations) assert.deepEqual(implementation.vectors, requiredVectors);
}

function run(command, args, label) {
  const result = spawnSync(command, args, { encoding: "utf8", timeout: 30_000 });
  if (result.error || result.status !== 0) throw new Error(`${label} failed: ${result.stderr || result.stdout}`, { cause: result.error });
  return result;
}

function file(path) {
  return fileURLToPath(new URL(path, root));
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", runtimes: ["node", "bun"], integrationRequired: ["deno", "workerd"], vectors: requiredVectors })}\n`);
