#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/rust-async-adapters.json", root), "utf8"));
const cargoManifest = fileURLToPath(new URL("sdk/rust/Cargo.toml", root));

assert.equal(fixture.profile, "sdk.rust.adapters-1");
assert.equal(fixture.runtimeVersion, "naatre.rust.task-adapter-1");
assert.deepEqual(fixture.package.defaultFeatures, []);
assert.equal(fixture.package.features.tokio.dependency, null);
assert.equal(fixture.dependencies.length, 1);
assert.deepEqual(
  [fixture.dependencies[0].issue, fixture.dependencies[0].profile, fixture.dependencies[0].gitCommit],
  [39, "sdk.rust.core-1", "b66c36c1e86a0a938c7d1ab0c80fac09e79a4d87"],
);

for (const dependency of fixture.dependencies) {
  assert.match(dependency.gitCommit, /^[0-9a-f]{40}$/u);
  assertDigest(dependency.fixture);
}
for (const evidence of fixture.evidence) assertDigest(evidence);

const metadataResult = run("cargo", [
  "metadata",
  "--manifest-path",
  cargoManifest,
  "--format-version",
  "1",
  "--no-deps",
  "--offline",
]);
const rustPackage = JSON.parse(metadataResult.stdout).packages.find(
  (candidate) => candidate.name === fixture.package.name,
);
assert.equal(rustPackage.rust_version, "1.85");
assert.deepEqual(rustPackage.features.default, fixture.package.defaultFeatures);
assert.deepEqual(rustPackage.features.tokio, []);
// The base SDK stays executor-neutral: tokio is never a mandatory dependency. It
// may exist only as an optional dependency gated behind opt-in runtime features
// (added by the Tokio/Axum worker), which cargo metadata surfaces as optional.
const tokioDependency = rustPackage.dependencies.find((dependency) => dependency.name === "tokio");
assert.ok(tokioDependency === undefined || tokioDependency.optional === true);

for (const profile of fixture.featureMatrix) {
  if (profile.toolchain !== "current") continue;
  run("cargo", [
    profile.action,
    "--manifest-path",
    cargoManifest,
    ...profile.arguments,
    "--locked",
    "--offline",
  ]);
}

const rustc = run("rustc", ["--version"]);
process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  runtime: rustc.stdout.trim(),
  capabilities: [fixture.profile],
  dependencies: fixture.dependencies,
  vectors: fixture.vectors,
})}\n`);

function assertDigest(entry) {
  const content = readFileSync(new URL(entry.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), entry.sha256, entry.path);
}

function run(command, arguments_) {
  const result = spawnSync(command, arguments_, {
    encoding: "utf8",
    timeout: 120_000,
  });
  if (result.error || result.status !== 0) {
    process.stderr.write(result.stderr || `${command} failed\n`);
    process.exit(1);
  }
  return result;
}
