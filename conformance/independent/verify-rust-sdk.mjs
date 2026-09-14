#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/rust-sdk.json", root), "utf8"));

assert.equal(fixture.profile, "sdk.rust.core-1");
assert.equal(fixture.package.minimumRust, "1.85.0");
assert.deepEqual(fixture.package.defaultFeatures, []);
for (const evidence of fixture.evidence) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256, evidence.path);
}

const reference = JSON.parse(readFileSync(new URL(fixture.sources.referenceOutput.path, root), "utf8"));
const manifest = JSON.parse(readFileSync(new URL(fixture.generated.manifest.path, root), "utf8"));
assert.equal(manifest.operations[0].persisted.digest, reference.operations[0].persisted.digest);

const cargoManifest = fileURLToPath(new URL("sdk/rust/Cargo.toml", root));
const metadata = spawnSync("cargo", [
  "metadata",
  "--manifest-path",
  cargoManifest,
  "--format-version",
  "1",
  "--no-deps",
], { encoding: "utf8", timeout: 60_000 });
if (metadata.error || metadata.status !== 0) throw new Error("Rust SDK metadata failed");
const rustPackage = JSON.parse(metadata.stdout).packages.find((candidate) => candidate.name === fixture.package.name);
assert.equal(rustPackage.rust_version, "1.85");
assert.deepEqual(rustPackage.features.default, fixture.package.defaultFeatures);

const tests = spawnSync("cargo", [
  "test",
  "--manifest-path",
  cargoManifest,
  "--test",
  "conformance",
  "--locked",
], { encoding: "utf8", timeout: 60_000 });
if (tests.error || tests.status !== 0) {
  process.stderr.write(tests.stderr || "Rust SDK conformance profile failed\n");
  process.exit(1);
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", generatorVersion: fixture.generatorVersion })}\n`);
