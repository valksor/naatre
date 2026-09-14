import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../", import.meta.url));
const fixture = JSON.parse(readFileSync(new URL("../v1/dotnet-sdk.json", import.meta.url), "utf8"));
const dotnet = process.env.DOTNET ?? "dotnet";
const runtimeState = join(tmpdir(), `naatre-dotnet-sdk-state-${process.pid}`);
const environment = {
  ...process.env,
  DOTNET_CLI_HOME: process.env.DOTNET_CLI_HOME ?? join(runtimeState, "home"),
  DOTNET_CLI_TELEMETRY_OPTOUT: "1",
  DOTNET_SKIP_FIRST_TIME_EXPERIENCE: "1",
  NUGET_PACKAGES: process.env.NUGET_PACKAGES ?? join(runtimeState, "packages"),
};

assert.equal(fixture.profile, "sdk.dotnet.core-1");
for (const evidence of fixture.evidence) {
  const content = readFileSync(new URL(`../../${evidence.path}`, import.meta.url));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256, evidence.path);
}

for (const generated of Object.values(fixture.generated)) {
  const content = readFileSync(new URL(`../../${generated.path}`, import.meta.url));
  assert.equal(createHash("sha256").update(content).digest("hex"), generated.sha256, generated.path);
}

const temporary = mkdtempSync(join(tmpdir(), "naatre-dotnet-sdk-"));
try {
  const generated = join(temporary, "generated");
  run([
    "run", "--project", join(root, "sdk/dotnet/Naatre.Generator/Naatre.Generator.csproj"),
    "--configuration", "Release", "--framework", "net10.0", "--no-restore", "--disable-build-servers", "-p:TargetFrameworks=net10.0", "--",
    join(root, fixture.sources.model.path),
    join(root, fixture.sources.referenceOutput.path),
    generated,
  ]);
  assert.deepEqual(readFileSync(join(generated, "Operations.g.cs")), readFileSync(join(root, fixture.generated.source.path)));
  assert.deepEqual(readFileSync(join(generated, "operations.json")), readFileSync(join(root, fixture.generated.manifest.path)));

  const hostile = JSON.parse(readFileSync(join(root, fixture.sources.model.path), "utf8"));
  hostile.configuration.scalarMappings = {};
  const hostilePath = join(temporary, "hostile.json");
  writeFileSync(hostilePath, JSON.stringify(hostile));
  const rejected = execute([
    "run", "--project", join(root, "sdk/dotnet/Naatre.Generator/Naatre.Generator.csproj"),
    "--configuration", "Release", "--framework", "net10.0", "--no-restore", "--disable-build-servers", "-p:TargetFrameworks=net10.0", "--",
    hostilePath,
    join(root, fixture.sources.referenceOutput.path),
    join(temporary, "rejected"),
  ]);
  assert.notEqual(rejected.status, 0, "unmapped custom scalar was accepted");
  assert.match(rejected.stderr, /DOTNET_SDK_GENERATOR_UNMAPPED_SCALAR/u);

  run(["build", join(root, "sdk/dotnet/Naatre.Core/Naatre.Core.csproj"), "--configuration", "Release", "--no-restore", "--disable-build-servers", "-m:1"]);
  run(["build", join(root, "sdk/dotnet/Naatre.FSharpSmoke/Naatre.FSharpSmoke.fsproj"), "--configuration", "Release", "--no-restore", "--disable-build-servers", "-m:1"]);
  for (const framework of ["net8.0", "net10.0"]) {
    run(["run", "--project", join(root, "sdk/dotnet/Naatre.FSharpSmoke/Naatre.FSharpSmoke.fsproj"), "--configuration", "Release", "--framework", framework, "--no-build", "--no-restore", "--disable-build-servers"]);
  }

  run(["build", join(root, "sdk/dotnet/Naatre.Conformance/Naatre.Conformance.csproj"), "--configuration", "Release", "--no-restore", "--disable-build-servers", "-m:1"]);
  const conformance = run([
    "run", "--project", join(root, "sdk/dotnet/Naatre.Conformance/Naatre.Conformance.csproj"),
    "--configuration", "Release", "--no-build", "--no-restore", "--disable-build-servers",
  ]);
  const reportLine = conformance.stdout.trim().split(/\r?\n/u).findLast((line) => line.startsWith("{"));
  assert.ok(reportLine, "conformance report was not emitted");
  const report = JSON.parse(reportLine);
  assert.equal(report.profile, fixture.profile);
  assert.equal(report.status, "passed");
  assert.deepEqual(report.vectors, [
    "shared-lossless-scalars",
    "missing-null-partial-errors-unknown-variants",
    "unary-active-cancellation",
    "sse-terminal-truncation-and-cancellation",
    "redirect-credentials-and-idempotent-retry",
    "malformed-and-bounded-responses",
  ]);
} finally {
  rmSync(temporary, { recursive: true, force: true });
  rmSync(runtimeState, { recursive: true, force: true });
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", generatorVersion: fixture.generatorVersion })}\n`);

function execute(arguments_) {
  return spawnSync(dotnet, arguments_, {
    cwd: root,
    encoding: "utf8",
    env: environment,
    timeout: 120_000,
  });
}

function run(arguments_) {
  const result = execute(arguments_);
  if (result.error || result.status !== 0) {
    throw new Error(`dotnet command failed: ${result.stderr || result.stdout || result.error?.message || result.status}`);
  }
  return result;
}
