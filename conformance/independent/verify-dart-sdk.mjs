import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const root = new URL("../../", import.meta.url);
const dartRoot = new URL("sdk/dart/", root);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/dart-sdk.json", root), "utf8"));
process.env.ANALYZER_STATE_LOCATION_OVERRIDE ??= join(tmpdir(), "naatre-dart-analyzer");

assert.equal(fixture.profile, "sdk.dart.core-1");
assert.deepEqual(fixture.package.supportedFlutter, []);
assert.equal(fixture.targets.flutter.status, "unsupported-until-84");
assert.equal(fixture.targets.websocket.status, "unsupported-until-84");
verifyEvidence(fixture.evidence);
verifyEvidence(fixture.reports);

const report = JSON.parse(readFileSync(new URL(fixture.reports[0].path, root), "utf8"));
assert.equal(report.profile, fixture.profile);
assert.equal(report.status, "passed");
assert.deepEqual(report.unsupported, fixture.unsupported);
assert.deepEqual(report.canonicalTargets, ["vm", "native-aot", "dart2js-node"]);

run("dart", ["pub", "get", "-C", "sdk/dart", "--offline", "--enforce-lockfile"]);
run("dart", ["analyze", "sdk/dart", "--fatal-infos"]);
for (const test of [
  "json_scalars_test.dart",
  "operation_client_test.dart",
  "stream_test.dart",
  "generator_test.dart",
  "worker_test.dart",
]) {
  run("dart", ["run", `test/${test}`], dartRoot);
}

const generatedRoot = mkdtempSync(join(tmpdir(), "naatre-dart-generation-"));
const buildRoot = mkdtempSync(join(tmpdir(), "naatre-dart-build-"));
try {
  const first = join(generatedRoot, "first");
  const second = join(generatedRoot, "second");
  for (const output of [first, second]) {
    run("dart", [
      "run",
      "bin/naatre_sdkgen.dart",
      "../../conformance/v1/generator-model.json",
      "../../conformance/v1/generator-output.json",
      output,
    ], dartRoot);
  }
  for (const artifact of ["operations.dart", "operations.json"]) {
    const expected = readFileSync(new URL(`sdk/dart/lib/src/generated/${artifact}`, root));
    assert.deepEqual(readFileSync(join(first, artifact)), expected, `${artifact} regeneration drift`);
    assert.deepEqual(readFileSync(join(second, artifact)), expected, `${artifact} reproducibility drift`);
  }

  const vm = run("dart", ["run", "tool/canonical_probe.dart"], dartRoot).stdout.trim();
  const executable = join(buildRoot, "canonical-probe");
  run("dart", [
    "compile",
    "exe",
    "sdk/dart/tool/canonical_probe.dart",
    "-o",
    executable,
  ]);
  const aot = run(executable, []).stdout.trim();
  const javascript = join(buildRoot, "canonical-probe.js");
  run("dart", [
    "compile",
    "js",
    "-O2",
    "sdk/dart/tool/canonical_probe.dart",
    "-o",
    javascript,
  ]);
  const web = run("node", [javascript]).stdout.trim();
  assert.equal(aot, vm, "native AOT canonical output drift");
  assert.equal(web, vm, "dart2js canonical output drift");
  const probe = JSON.parse(vm);
  assert.equal(probe.documentHash, "cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90");
  assert.equal(probe.unsafeExtendedIntegerRejected, true);
  assert.equal(probe.unsafeIntegerRejected, true);
  assert.equal(createHash("sha256").update(vm).digest("hex"), report.canonicalProbeSha256);

  const scalarExecutable = join(buildRoot, "scalar-vectors");
  run("dart", [
    "compile",
    "exe",
    "sdk/dart/test/json_scalars_test.dart",
    "-o",
    scalarExecutable,
  ]);
  run(scalarExecutable, []);
  const scalarJavascript = join(buildRoot, "scalar-vectors.js");
  run("dart", [
    "compile",
    "js",
    "-O2",
    "sdk/dart/test/json_scalars_test.dart",
    "-o",
    scalarJavascript,
  ]);
  run("node", [scalarJavascript]);
} finally {
  rmSync(generatedRoot, { recursive: true, force: true });
  rmSync(buildRoot, { recursive: true, force: true });
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", generatorVersion: fixture.generatorVersion })}\n`);

function verifyEvidence(entries) {
  for (const evidence of entries) {
    const actual = createHash("sha256").update(readFileSync(new URL(evidence.path, root))).digest("hex");
    assert.equal(actual, evidence.sha256, evidence.path);
  }
}

function run(command, arguments_, cwd = new URL(".", root)) {
  const result = spawnSync(command, arguments_, {
    cwd,
    encoding: "utf8",
    timeout: 30_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`${command} failed: ${result.stderr || result.stdout || result.error}`);
  }
  return result;
}
