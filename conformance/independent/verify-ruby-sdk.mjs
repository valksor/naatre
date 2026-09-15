import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/ruby-sdk.json", root), "utf8"));

assert.equal(fixture.profile, "sdk.ruby.core-1");
assert.equal(fixture.runtime.minimumRuby, "2.6");
assert.deepEqual(fixture.cancellation, {
  coreStreamDecoder: "synchronous-terminal-validation",
  http: "adapter-profile-cooperative-resource-close",
  sse: "adapter-profile-cooperative-resource-close",
  websocket: "unsupported",
});
verifyEvidence(fixture.evidence);
verifyEvidence(fixture.reports);

const report = JSON.parse(readFileSync(new URL(fixture.reports[0].path, root), "utf8"));
assert.equal(report.profile, fixture.profile);
assert.equal(report.status, "passed");
assert.equal(report.runtime.name, "cruby");
assert.deepEqual(report.unsupported, fixture.unsupported);

run("ruby", [
  "-Isdk/ruby/lib",
  "-Isdk/ruby/test",
  "-e",
  '%w[core_test.rb generated_test.rb].each { |file| require File.expand_path("sdk/ruby/test/#{file}") }',
]);
run("ruby", ["sdk/ruby/sdkgen/cli.rb"]);
verifyEvidence(fixture.evidence);

const buildRoot = mkdtempSync(join(tmpdir(), "naatre-ruby-gem-"));
try {
  const first = join(buildRoot, "first.gem");
  const second = join(buildRoot, "second.gem");
  const environment = { ...process.env, SOURCE_DATE_EPOCH: fixture.package.sourceDateEpoch };
  run("gem", ["build", "sdk/ruby/naatre.gemspec", "--output", first], environment);
  run("gem", ["build", "sdk/ruby/naatre.gemspec", "--output", second], environment);
  assert.equal(createHash("sha256").update(readFileSync(first)).digest("hex"), createHash("sha256").update(readFileSync(second)).digest("hex"));
} finally {
  rmSync(buildRoot, { recursive: true, force: true });
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", generatorVersion: fixture.generatorVersion })}\n`);

function verifyEvidence(entries) {
  for (const evidence of entries) {
    const actual = createHash("sha256").update(readFileSync(new URL(evidence.path, root))).digest("hex");
    assert.equal(actual, evidence.sha256, evidence.path);
  }
}

function run(command, arguments_, environment = process.env) {
  const result = spawnSync(command, arguments_, {
    cwd: new URL(".", root),
    encoding: "utf8",
    env: environment,
    timeout: 30_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`${command} failed: ${result.stderr || result.stdout || result.error}`);
  }
}
