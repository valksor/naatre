import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/php-sdk.json", root), "utf8"));
assert.equal(fixture.profile, "sdk.php.core-1");

for (const evidence of fixture.evidence) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256);
}

const temporary = mkdtempSync(join(tmpdir(), "naatre-php-sdk-"));
try {
  const generated = spawnSync("php", [
    fileURLToPath(new URL("sdk/php/sdkgen/generate.php", root)),
    fileURLToPath(new URL(fixture.sources.model.path, root)),
    fileURLToPath(new URL(fixture.sources.referenceOutput.path, root)),
    join(temporary, "Operations.php"),
    join(temporary, "operations.json"),
  ], { encoding: "utf8", timeout: 30_000 });
  if (generated.error || generated.status !== 0) throw new Error("PHP SDK generation failed");
  assert.equal(readFileSync(join(temporary, "Operations.php"), "utf8"), readFileSync(new URL(fixture.generated.source.path, root), "utf8"));
  assert.equal(readFileSync(join(temporary, "operations.json"), "utf8"), readFileSync(new URL(fixture.generated.manifest.path, root), "utf8"));
} finally {
  rmSync(temporary, { recursive: true, force: true });
}

for (const command of [
  ["php", "sdk/php/tests/run.php"],
]) {
  const checked = spawnSync(command[0], command.slice(1), {
    cwd: fileURLToPath(root),
    encoding: "utf8",
    timeout: 60_000,
  });
  if (checked.error || checked.status !== 0) throw new Error("PHP SDK runtime conformance failed");
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", generatorVersion: fixture.generatorVersion })}\n`);
