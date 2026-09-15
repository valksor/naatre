#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

const repositoryURL = new URL("../../", import.meta.url);
const evidence = JSON.parse(readFileSync(new URL("../v1/documentation-examples.json", import.meta.url), "utf8"));
const guide = readFileSync(new URL("../../docs/v1/language-guides.md", import.meta.url), "utf8");
const errorIndex = readFileSync(new URL("../../docs/v1/error-codes.md", import.meta.url), "utf8");

function repositoryPath(path) {
  return fileURLToPath(new URL(path, repositoryURL));
}

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function digest(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function pinnedFiles() {
  const result = new Map();
  for (const item of [...evidence.components, ...evidence.contractEvidence]) {
    requireValue(!result.has(item.path), "duplicate pinned evidence path");
    const content = readFileSync(repositoryPath(item.path));
    requireValue(digest(content) === item.sha256, `pinned evidence digest mismatch: ${item.path}`);
    result.set(item.path, content);
  }
  return result;
}

function workflowText() {
  const directory = repositoryPath(".github/workflows/");
  return readdirSync(directory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && /\.ya?ml$/u.test(entry.name))
    .map((entry) => readFileSync(`${directory}/${entry.name}`, "utf8"))
    .join("\n");
}

function validateIdentity() {
  requireValue(evidence.profile === "documentation.examples-1" && evidence.fixtureSuite === "1.0.0" && evidence.ownerIssue === 77, "invalid documentation evidence identity");
  requireValue(/^[0-9a-f]{40}$/u.test(evidence.dependencyRevision), "dependency revision is not exact");
  requireValue(evidence.dependencyIssues.join(",") === "28,29,31,37,38,39,40,41,42,43,44,51,58,59,60,61", "dependency issue set is incomplete");
  requireValue(evidence.lifecycle.owner === "Naatre maintainers" && evidence.lifecycle.stability === "development-snapshot" && evidence.lifecycle.publication === "source-checkout-only", "documentation lifecycle is incomplete");
}

function validateComponents(files) {
  const languages = evidence.components.map((item) => item.language).sort();
  requireValue(languages.join(",") === "dart,dotnet,go,javascript-typescript,jvm,php,python,ruby,rust,swift", "component language set is incomplete");
  for (const component of evidence.components) {
    const fixture = JSON.parse(files.get(component.path));
    requireValue(typeof fixture.profile === "string" && fixture.fixtureSuite === "1.0.0", "component fixture identity is invalid");
    requireValue(Array.isArray(fixture.unsupported) && fixture.unsupported.length > 0, "component fixture lacks unsupported inventory");
    for (const capability of fixture.unsupported) {
      requireValue(guide.includes(`\`${capability}\``), "guide omits unsupported capability");
    }
  }
}

function validateLanguages(files) {
  const workflows = workflowText();
  const languages = evidence.languages.map((item) => item.language).sort();
  requireValue(languages.join(",") === "dart,dotnet,go,javascript-typescript,jvm,php,python,ruby,rust,swift", "published language set is incomplete");
  for (const language of evidence.languages) {
    requireValue(language.package && language.runtimes.length > 0 && language.platforms.length > 0, "language package/runtime/platform boundary is incomplete");
    for (const surfaceName of ["client", "remoteWorker", "nativeRuntime"]) {
      const surface = language[surfaceName];
      requireValue(surface && typeof surface.status === "string", "language surface status is missing");
      if (surface.status !== "executed") continue;
      requireValue(surface.profile && surface.source && surface.verify && surface.ciAnchor, "executed surface evidence is incomplete");
      readFileSync(repositoryPath(surface.source));
      requireValue(workflows.includes(surface.ciAnchor), "executed surface has no CI anchor");
      requireValue(guide.includes(`\`${surface.profile}\``) && guide.includes(`\`${surface.verify}\``), "guide omits an executed profile or command");
      requireValue([...files.values()].some((content) => content.includes(`\"${surface.profile}\"`)), "executed profile is not pinned");
    }
  }
  requireValue(evidence.languages.find((item) => item.language === "go").nativeRuntime.status === "executed", "Go native runtime example is missing");
  requireValue(evidence.languages.filter((item) => item.language !== "go").every((item) => item.nativeRuntime.status === "not-established"), "non-Go native runtime is overclaimed");
}

function validateScenarios(files) {
  const classes = evidence.scenarios.map((item) => item.class).sort();
  requireValue(classes.join(",") === "boundary,cancellation,negative,positive,resource-limit", "required scenario classes are incomplete");
  const codes = new Set();
  for (const scenario of evidence.scenarios) {
    requireValue(files.has(scenario.fixture), "scenario fixture is not revision-pinned");
    const fixture = JSON.parse(files.get(scenario.fixture));
    requireValue(Object.hasOwn(fixture, scenario.key), "scenario fixture key is missing");
    for (const code of scenario.stableCodes) {
      requireValue(errorIndex.includes(`\`${code}\``), "scenario uses an unpublished stable code");
      codes.add(code);
    }
  }
  const publicFields = new Set(evidence.redaction.publicFields);
  const forbiddenFields = new Set(evidence.redaction.forbiddenFields.map((field) => field.toLowerCase()));
  for (const failure of evidence.safeFailures) {
    requireValue(codes.has(failure.code), "safe failure code has no scenario");
    for (const key of Object.keys(failure)) {
      requireValue(publicFields.has(key), "safe failure exposes a non-public field");
      requireValue(!forbiddenFields.has(key.toLowerCase()), "safe failure exposes a protected field");
    }
  }
}

try {
  validateIdentity();
  const files = pinnedFiles();
  validateComponents(files);
  validateLanguages(files);
  validateScenarios(files);
  process.stdout.write(`${JSON.stringify({ profile: evidence.profile, status: "passed", dependencyRevision: evidence.dependencyRevision })}\n`);
} catch (error) {
  const reason = error instanceof Error ? error.message : "unknown validation error";
  process.stderr.write(`documentation examples profile failed: ${reason}\n`);
  process.exitCode = 1;
}
