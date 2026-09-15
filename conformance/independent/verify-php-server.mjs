import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { join } from "node:path";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/php-server.json", root), "utf8"));
assert.equal(fixture.profile, "sdk.php.server-1");
assert.equal(fixture.protocol, "naatre.remote-worker.v1");
assert.deepEqual(fixture.profiles.fpm.capabilities, ["unary-1"]);
assert.deepEqual(fixture.profiles.worker.capabilities, ["unary-1", "cancellation-ack-1"]);
assert.deepEqual(fixture.frameworkFixtureConsumers, ["neutral", "symfony", "laravel"]);
assert.equal(fixture.claims.nativeRuntime, false);
assert.equal(fixture.claims.productionFrameworkAdapters, false);
assert.equal(fixture.claims.fpmDisconnectForcesRollback, false);

const generatedHandlers = readFileSync(new URL(fixture.generated.handlers.path, root));
assert.equal(createHash("sha256").update(generatedHandlers).digest("hex"), fixture.generated.handlers.sha256);

for (const evidence of fixture.evidence) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256, evidence.path);
}

const temporary = mkdtempSync(join(tmpdir(), "naatre-php-server-"));
try {
  const generated = spawnSync("php", [
    fileURLToPath(new URL("sdk/php/sdkgen/generate.php", root)),
    fileURLToPath(new URL("conformance/v1/generator-model.json", root)),
    fileURLToPath(new URL("conformance/v1/generator-output.json", root)),
    join(temporary, "Operations.php"),
    join(temporary, "operations.json"),
    join(temporary, "Handlers.php"),
  ], { encoding: "utf8", timeout: 30_000 });
  if (generated.error || generated.status !== 0) throw new Error("PHP server binding generation failed");
  assert.equal(readFileSync(join(temporary, "Handlers.php"), "utf8"), readFileSync(new URL("sdk/php/generated/Handlers.php", root), "utf8"));
} finally {
  rmSync(temporary, { recursive: true, force: true });
}

for (const command of [
  ["php", "sdk/php/tests/server.php"],
  ["php", "sdk/php/examples/symfony-server.php"],
  ["php", "sdk/php/examples/laravel-server.php"],
]) {
  const result = spawnSync(command[0], command.slice(1), {
    cwd: fileURLToPath(root),
    encoding: "utf8",
    timeout: 60_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`PHP server conformance failed: ${command.join(" ")}\n${result.stderr}`);
  }
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", runtimeVersion: fixture.runtimeVersion })}\n`);
