import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/swift-apple-adapters.json", root), "utf8"));
assert.equal(fixture.profile, "sdk.swift.apple-1");
assert.equal(fixture.runtimeVersion, "naatre.swift.urlsession-1");
assert.deepEqual(
  [fixture.dependencies[0].issue, fixture.dependencies[0].profile, fixture.dependencies[0].gitCommit],
  [42, "sdk.swift.core-1", "f6b77807cc762893020e726e8ae5221b58bf5309"],
);
assert.deepEqual(
  fixture.platforms.map(({ platform, minimum, status }) => [platform, minimum, status]),
  [
    ["macOS", "14", "passed"],
    ["iOS", "17", "source-compatible-not-executed"],
    ["tvOS", "17", "source-compatible-not-executed"],
    ["watchOS", "10", "source-compatible-not-executed"],
    ["visionOS", "1", "source-compatible-not-executed"],
  ],
);

for (const dependency of fixture.dependencies) assertDigest(dependency.fixture);
for (const evidence of fixture.evidence) assertDigest(evidence);

const packagePath = fileURLToPath(new URL("sdk/swift", root));
const cacheArguments = [
  "--cache-path", "/tmp/naatre-swift-package-cache",
  "--config-path", "/tmp/naatre-swift-config",
  "--security-path", "/tmp/naatre-swift-security",
  "--scratch-path", "/tmp/naatre-swift-build",
];
const manifest = JSON.parse(runSwift([
  "package",
  "--disable-sandbox",
  ...cacheArguments,
  "--package-path", packagePath,
  "dump-package",
], "package manifest").stdout);
assert.deepEqual(
  manifest.platforms.map(({ platformName, version }) => [platformName, version]),
  [["ios", "17.0"], ["macos", "14.0"], ["tvos", "17.0"], ["watchos", "10.0"], ["visionos", "1.0"]],
);
assert.deepEqual(manifest.products.find(({ name }) => name === "NaatreApple")?.targets, ["NaatreApple"]);
assert.deepEqual(
  manifest.targets.find(({ name }) => name === "NaatreApple")?.dependencies,
  [{ byName: ["NaatreCore", null] }],
);

const execution = runSwift([
  "run",
  "--disable-sandbox",
  ...cacheArguments,
  "--package-path", packagePath,
  "naatre-swift-apple-conformance",
], "Apple adapter conformance");
const report = JSON.parse(execution.stdout.trim().split("\n").at(-1));
assert.deepEqual(report, {
  profile: fixture.profile,
  status: "passed",
  vectors: "urlsession-sse-cancellation-limits-scalars-time-presence-open-variants",
});
process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  runtime: "swift-6.4-macos-arm64",
  capabilities: fixture.supported,
  dependencies: fixture.dependencies,
  platforms: fixture.platforms,
  vectors: fixture.vectors,
})}\n`);

function assertDigest(entry) {
  const content = readFileSync(new URL(entry.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), entry.sha256, entry.path);
}

function runSwift(arguments_, label) {
  const result = spawnSync("swift", arguments_, { encoding: "utf8", timeout: 120_000 });
  if (result.error || result.status !== 0) {
    throw new Error(`Swift ${label} failed: ${result.error?.message ?? result.stderr}`);
  }
  return result;
}
