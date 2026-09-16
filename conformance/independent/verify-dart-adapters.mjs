#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const root = new URL("../../", import.meta.url);
const dartRoot = new URL("sdk/dart/", root);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/dart-adapters.json", root), "utf8"));
process.env.ANALYZER_STATE_LOCATION_OVERRIDE ??= join(tmpdir(), "naatre-dart-analyzer");

assert.equal(fixture.profile, "sdk.dart.adapters-1");
assert.equal(fixture.runtimeVersion, "naatre.dart.adapters-1");
assert.deepEqual(fixture.package.runtimeDependencies, []);
assert.deepEqual(fixture.advertisedProfiles.map((entry) => [entry.profile, entry.status]), [
  ["sdk.dart.core-1", "passed"],
  ["sdk.dart.adapters-1", "passed"],
]);
assert.deepEqual(fixture.adapterContracts.map((entry) => [entry.name, entry.status]), [
  ["framework-neutral", "passed"],
  ["http-transport", "passed"],
  ["post-sse-transport", "passed"],
  ["websocket-transport", "passed-with-browser-boundary"],
]);
assert.deepEqual(fixture.executedTargets, ["vm", "native-aot", "dart2js-node"]);
assert.deepEqual(fixture.conditionalBindings, {
  vm: "dart-io",
  nativeAot: "dart-io",
  dart2js: "browser",
  flutterNative: "source-supported-not-certified",
  flutterWeb: "source-supported-not-certified",
});

assert.equal(fixture.dependencies.length, 1);
assert.deepEqual(
  [fixture.dependencies[0].issue, fixture.dependencies[0].profile],
  [43, "sdk.dart.core-1"],
);
for (const dependency of fixture.dependencies) {
  assert.match(dependency.gitCommit, /^[0-9a-f]{40}$/u);
  assertDigest(dependency.fixture);
}

// The adapters profile depends on the pinned core fixture, which must itself be
// the sdk.dart.core-1 fixture and advertise this exact adapter profile.
const core = JSON.parse(readFileSync(new URL(fixture.dependencies[0].fixture.path, root), "utf8"));
assert.equal(core.profile, "sdk.dart.core-1");
assert.equal(core.adapterProfile, fixture.profile);
assert.equal(core.dependencies[0].revision, fixture.dependencies[0].gitCommit);

for (const evidence of fixture.evidence) assertDigest(evidence);
for (const evidence of fixture.reports) assertDigest(evidence);
for (const code of fixture.failureCodes) assert.match(code, /^CLIENT_[A-Z_]+$/u);
assert.equal(new Set(fixture.failureCodes).size, fixture.failureCodes.length);
assert.equal(fixture.limits.redirects, 5);
for (const platform of fixture.platforms) {
  assert.equal(platform.status, "passed");
  assert.deepEqual(platform.profiles, fixture.advertisedProfiles.map((entry) => entry.profile));
}

// The bundled adapter report is the internal evidence artifact this standalone
// profile promotes; its claims must agree with the fixture.
const report = JSON.parse(readFileSync(new URL(fixture.reports[0].path, root), "utf8"));
assert.equal(report.profile, fixture.profile);
assert.equal(report.status, "passed");
assert.deepEqual(report.executedTargets, fixture.executedTargets);
assert.deepEqual(report.conditionalBindings, fixture.conditionalBindings);
assert.deepEqual(report.dependency, fixture.dependencies[0].report);
assert.deepEqual(report.vectors, fixture.vectors);
assert.deepEqual(report.unsupported, fixture.unsupported);

run("dart", ["pub", "get", "-C", "sdk/dart", "--offline", "--enforce-lockfile"]);
run("dart", ["analyze", "sdk/dart", "--fatal-infos"]);
run("dart", ["run", "test/adapter_test.dart"], dartRoot);

const buildRoot = mkdtempSync(join(tmpdir(), "naatre-dart-adapters-"));
try {
  const executable = join(buildRoot, "adapter-vectors");
  run("dart", ["compile", "exe", "sdk/dart/test/adapter_test.dart", "-o", executable]);
  run(executable, []);
  const javascript = join(buildRoot, "adapter-vectors.js");
  run("dart", ["compile", "js", "-O2", "sdk/dart/test/adapter_test.dart", "-o", javascript]);
  run("node", [javascript]);
} finally {
  rmSync(buildRoot, { recursive: true, force: true });
}

process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  dependency: fixture.dependencies[0],
  executedTargets: fixture.executedTargets,
  vectors: fixture.vectors,
})}\n`);

function assertDigest(entry) {
  const content = readFileSync(new URL(entry.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), entry.sha256, entry.path);
}

function run(command, arguments_, cwd = new URL(".", root)) {
  const result = spawnSync(command, arguments_, {
    cwd,
    encoding: "utf8",
    timeout: 120_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`${command} failed: ${result.stderr || result.stdout || result.error}`);
  }
  return result;
}
