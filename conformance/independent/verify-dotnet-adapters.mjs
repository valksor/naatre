import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../", import.meta.url));
const fixture = JSON.parse(readFileSync(new URL("../v1/dotnet-adapters.json", import.meta.url), "utf8"));
const dotnet = process.env.DOTNET ?? "dotnet";
const runtimeState = join(tmpdir(), `naatre-dotnet-adapters-state-${process.pid}`);
const environment = {
  ...process.env,
  DOTNET_CLI_HOME: process.env.DOTNET_CLI_HOME ?? join(runtimeState, "home"),
  DOTNET_CLI_TELEMETRY_OPTOUT: "1",
  DOTNET_SKIP_FIRST_TIME_EXPERIENCE: "1",
  NUGET_PACKAGES: process.env.NUGET_PACKAGES ?? join(runtimeState, "packages"),
};
const dotnetOptions = {
  cwd: root,
  encoding: "utf8",
  env: environment,
  timeout: 240_000,
  maxBuffer: 16 * 1024 * 1024,
};

assert.equal(fixture.profile, "sdk.dotnet.adapters-1");
assert.equal(fixture.runtimeVersion, "naatre.dotnet.adapters-1");
assert.deepEqual(fixture.packages.map((entry) => entry.surface), [
  "csharp-http-client",
  "fsharp-presence-and-async",
  "optional-dependency-injection",
]);

for (const dependency of fixture.dependencies) {
  assert.match(dependency.gitCommit, /^[0-9a-f]{40}$/u);
  const committed = execFileSync("git", ["show", `${dependency.gitCommit}:${dependency.fixture.path}`], {
    cwd: root,
    encoding: null,
    timeout: 30_000,
  });
  assert.equal(createHash("sha256").update(committed).digest("hex"), dependency.fixture.sha256, `dependency revision: ${dependency.profile}`);
  assert.equal(createHash("sha256").update(readFileSync(join(root, dependency.fixture.path))).digest("hex"), dependency.fixture.sha256, `dependency fixture: ${dependency.profile}`);
}

for (const evidence of fixture.evidence) {
  assert.equal(createHash("sha256").update(readFileSync(join(root, evidence.path))).digest("hex"), evidence.sha256, evidence.path);
}

assert.deepEqual(
  fixture.runtimeMatrix.filter((entry) => entry.status === "passed").map((entry) => `${entry.language}:${entry.target}`),
  ["csharp:net8.0", "csharp:net10.0", "fsharp:net8.0", "fsharp:net10.0"],
);
assert.deepEqual(
  fixture.runtimeMatrix.filter((entry) => entry.status === "not-claimed").map((entry) => `${entry.language}:${entry.target}`),
  ["csharp:trimmed", "fsharp:trimmed", "csharp:nativeaot", "fsharp:nativeaot"],
);
assert.deepEqual(fixture.transportMatrix.map((entry) => [entry.transport, entry.status]), [
  ["unary-http", "passed"],
  ["post-sse", "passed"],
  ["websocket", "unsupported"],
]);

if (process.argv.includes("--metadata-only")) {
  process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "metadata-passed", dependencies: fixture.dependencies })}\n`);
  process.exit(0);
}

try {
  for (const project of [
    "sdk/dotnet/Naatre.Client/Naatre.Client.csproj",
    "sdk/dotnet/Naatre.FSharp/Naatre.FSharp.fsproj",
    "sdk/dotnet/Naatre.AspNetCore/Naatre.AspNetCore.csproj",
    "sdk/dotnet/Naatre.AdapterConformance/Naatre.AdapterConformance.csproj",
    "sdk/dotnet/Naatre.FSharpConformance/Naatre.FSharpConformance.fsproj",
  ]) {
    execFileSync(dotnet, ["build", join(root, project), "--configuration", "Release", "--no-restore", "--disable-build-servers", "-m:1"], dotnetOptions);
  }

  const packageOutput = join(runtimeState, "packages-output");
  mkdirSync(packageOutput, { recursive: true });
  for (const package_ of fixture.packages) {
    const projectName = package_.assembly.replace("Valksor.", "");
    const csproj = join(root, `sdk/dotnet/${projectName}/${projectName}.csproj`);
    const project = existsSync(csproj) ? csproj : join(root, `sdk/dotnet/${projectName}/${projectName}.fsproj`);
    execFileSync(dotnet, [
      "pack", project, "--configuration", "Release", "--no-build", "--no-restore", "--output", packageOutput,
    ], dotnetOptions);
    assert.ok(existsSync(join(packageOutput, `${package_.packageId}.${package_.version}.nupkg`)), `package: ${package_.packageId}`);
  }

  for (const framework of ["net8.0", "net10.0"]) {
    const csharp = execFileSync(dotnet, [
      "run", "--project", join(root, "sdk/dotnet/Naatre.AdapterConformance/Naatre.AdapterConformance.csproj"),
      "--configuration", "Release", "--framework", framework, "--no-build", "--no-restore", "--disable-build-servers",
    ], dotnetOptions);
    const csharpReport = report(csharp);
    assert.equal(csharpReport.profile, fixture.profile);
    assert.equal(csharpReport.status, "passed");
    assert.deepEqual(csharpReport.vectors, fixture.vectors.filter((vector) => vector !== "fsharp-missing-null-value-roundtrip"));

    const fsharp = execFileSync(dotnet, [
      "run", "--project", join(root, "sdk/dotnet/Naatre.FSharpConformance/Naatre.FSharpConformance.fsproj"),
      "--configuration", "Release", "--framework", framework, "--no-build", "--no-restore", "--disable-build-servers",
    ], dotnetOptions);
    const fsharpReport = report(fsharp);
    assert.equal(fsharpReport.profile, fixture.profile);
    assert.equal(fsharpReport.status, "passed");
    assert.equal(fsharpReport.surface, "fsharp");
  }
} finally {
  rmSync(runtimeState, { recursive: true, force: true });
}

process.stdout.write(`${JSON.stringify({ profile: fixture.profile, status: "passed", dependencies: fixture.dependencies })}\n`);

function report(output) {
  const line = output.trim().split(/\r?\n/u).findLast((entry) => entry.startsWith("{"));
  assert.ok(line, "conformance report was not emitted");
  return JSON.parse(line);
}
