import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/typescript-adapters.json", root), "utf8"));

assert.equal(fixture.profile, "sdk.typescript.adapters-1");
assert.equal(fixture.runtimeVersion, "naatre.typescript.runtime-1");
assert.deepEqual(fixture.transports.map((entry) => [entry.profile, entry.required, entry.status]), [
  ["sse-post-fetch", true, "passed"],
  ["stream.websocket-1", false, "passed"],
]);
for (const dependency of fixture.dependencies) {
  assert.match(dependency.gitCommit, /^[0-9a-f]{40}$/u);
  const content = readFileSync(new URL(dependency.fixture.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), dependency.fixture.sha256);
}
assert.deepEqual(fixture.platforms.map((entry) => entry.runtime), ["node", "bun", "deno"]);
assert.equal(new Set(fixture.platforms.map((entry) => entry.runtimeID)).size, fixture.platforms.length);
for (const platform of fixture.platforms) {
  assert.equal(platform.status, "passed");
  assert.deepEqual(platform.vectors, fixture.vectors);
}
assert.deepEqual(fixture.unverifiedRuntimes.map((entry) => [entry.runtime, entry.status]), [
  ["chrome-headless-shell", "not-claimed"],
  ["workerd", "not-claimed"],
]);
for (const evidence of fixture.evidence) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256);
}

const tests = spawnSync(process.execPath, [
  "--test",
  fileURLToPath(new URL("sdk/typescript/runtime/exports.test.mjs", root)),
  fileURLToPath(new URL("sdk/typescript/runtime/transport.test.mjs", root)),
], { encoding: "utf8", timeout: 30_000 });
if (tests.error || tests.status !== 0) throw new Error("TypeScript adapter tests failed");

const matrix = spawnSync(process.execPath, [fileURLToPath(new URL("conformance/independent/typescript-adapter-runtime.mjs", root))], { encoding: "utf8", timeout: 30_000 });
if (matrix.error || matrix.status !== 0) throw new Error("TypeScript adapter runtime matrix failed");
const result = JSON.parse(matrix.stdout.trim());
assert.equal(result.profile, fixture.profile);
assert.equal(result.status, "passed");
assert.deepEqual(result.capabilities, fixture.transports.map((entry) => entry.profile));
assert.deepEqual(result.dependencies, fixture.dependencies);
assert.deepEqual(result.vectors, fixture.vectors);

const typecheck = spawnSync("tsc", ["-p", fileURLToPath(new URL("sdk/typescript/tsconfig.json", root))], { encoding: "utf8", timeout: 30_000 });
if (typecheck.error || typecheck.status !== 0) throw new Error("TypeScript adapter declarations failed strict compilation");

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", runtime: result.runtime })}\n`);
