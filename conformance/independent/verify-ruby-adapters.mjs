#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/ruby-adapters.json", root), "utf8"));

assert.equal(fixture.profile, "sdk.ruby.adapters-1");
assert.equal(fixture.runtimeVersion, "naatre.ruby.adapters-1");
assert.deepEqual(fixture.package.runtimeDependencies, []);
assert.deepEqual(fixture.package.optionalLoadPaths, ["naatre/faraday", "naatre/rails"]);
assert.deepEqual(fixture.advertisedProfiles.map((entry) => [entry.profile, entry.status]), [
  ["sdk.ruby.core-1", "passed"],
  ["sdk.ruby.adapters-1", "passed"],
]);
assert.deepEqual(fixture.adapterContracts.map((entry) => [entry.name, entry.status]), [
  ["framework-neutral", "passed"],
  ["faraday-run-request", "passed-contract"],
  ["rails-railtie-rack-lifecycle", "passed-contract"],
]);
assert.equal(fixture.dependencies.length, 1);
assert.deepEqual(
  [fixture.dependencies[0].issue, fixture.dependencies[0].profile, fixture.dependencies[0].gitCommit],
  [44, "sdk.ruby.core-1", "0009b4027f85f3130e842ab7dbb68e43ed9686a1"],
);
for (const dependency of fixture.dependencies) {
  assert.match(dependency.gitCommit, /^[0-9a-f]{40}$/u);
  assertDigest(dependency.fixture);
}
for (const evidence of fixture.evidence) assertDigest(evidence);
for (const code of fixture.failureCodes) assert.match(code, /^CLIENT_[A-Z_]+$/u);
assert.equal(new Set(fixture.failureCodes).size, fixture.failureCodes.length);
assert.equal(fixture.limits.detachedThreads, 0);
assert.equal(fixture.limits.internalQueues, 0);
for (const platform of fixture.platforms) {
  assert.equal(platform.status, "passed");
  assert.deepEqual(platform.profiles, fixture.advertisedProfiles.map((entry) => entry.profile));
}

run("ruby", [
  "-Isdk/ruby/lib",
  "-Isdk/ruby/test",
  "-e",
  'Dir["sdk/ruby/test/**/*_test.rb"].sort.each { |file| require File.expand_path(file) }',
]);
run("ruby", [
  "-Isdk/ruby/lib",
  "-e",
  'require "naatre"; abort if defined?(Naatre::FaradayAdapter) || defined?(Naatre::Rails); require "naatre/faraday"; require "naatre/rails"',
]);
run("ruby", [
  "-e",
  'spec = Gem::Specification.load("sdk/ruby/naatre.gemspec"); abort unless spec.runtime_dependencies.empty?; abort unless %w[lib/naatre/transport.rb lib/naatre/faraday.rb lib/naatre/rails.rb].all? { |path| spec.files.include?(path) }',
]);
for (const source of ["transport.rb", "faraday.rb", "rails.rb"]) {
  run("ruby", ["-c", fileURLToPath(new URL(`sdk/ruby/lib/naatre/${source}`, root))]);
}

const runtime = run("ruby", ["-e", 'print "#{RUBY_ENGINE} #{RUBY_VERSION} #{RUBY_PLATFORM}"']).stdout;
process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  runtime,
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
    cwd: fileURLToPath(root),
    encoding: "utf8",
    timeout: 30_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`${command} failed: ${result.stderr || result.stdout || result.error}`);
  }
  return result;
}
