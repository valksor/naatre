import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/php-native.json", root), "utf8"));
assert.equal(fixture.profile, "sdk.php.native-1");
assert.deepEqual(fixture.php.minors, ["8.5", "8.6"]);
assert.deepEqual(fixture.defaultEnabled, []);
assert.equal(fixture.lifecycle.persistentState, false);
assert.equal(fixture.lifecycle.preemptiveCancellation, false);

for (const evidence of fixture.evidence) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256, evidence.path);
}

const extension = process.env.NAATRE_EXTENSION;
const args = [];
if (extension) args.push("-d", `extension=${extension}`);
args.push("sdk/php/tests/native.php");
const checked = spawnSync("php", args, {
  cwd: fileURLToPath(root),
  encoding: "utf8",
  timeout: 60_000,
});
if (checked.error || checked.status !== 0 || checked.stderr !== "") {
  throw new Error(`PHP native conformance failed: ${checked.stderr || checked.stdout}`);
}
const receipt = JSON.parse(checked.stdout);
assert.equal(receipt.profile, fixture.profile);
assert.equal(receipt.status, extension ? "native-passed" : "fallback-passed");

process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  path: extension ? "extension-loaded" : "fallback",
  phpVersion: spawnSync("php", ["-r", "echo PHP_VERSION;"], { encoding: "utf8" }).stdout,
})}\n`);
