#!/usr/bin/env node

import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const repositoryURL = new URL("../../", import.meta.url);
const evidence = JSON.parse(readFileSync(new URL("../v1/generation.json", import.meta.url), "utf8"));

function repositoryPath(path) {
  return fileURLToPath(new URL(path, repositoryURL));
}

function require(condition, message) {
  if (!condition) throw new Error(message);
}

function requireDigest(path, expected) {
  const content = readFileSync(repositoryPath(path));
  const actual = createHash("sha256").update(content).digest("hex");
  require(actual === expected, "generator evidence digest mismatch");
  return content;
}

try {
  require(evidence.profile === "sdk.generation-1" && evidence.fixtureSuite === "1.0.0", "invalid generator evidence");
  const model = requireDigest(`conformance/${evidence.model.path}`, evidence.model.sha256);
  const expected = requireDigest(`conformance/${evidence.referenceOutput.path}`, evidence.referenceOutput.sha256);
  const javascript = evidence.implementations.find((implementation) => implementation.language === "javascript-typescript");
  require(javascript?.path && javascript.sha256, "independent generator evidence is missing");
  requireDigest(javascript.path, javascript.sha256);
  const generated = spawnSync(process.execPath, [repositoryPath(javascript.path), repositoryPath(`conformance/${evidence.model.path}`)], {
    encoding: null,
    timeout: 30_000,
    maxBuffer: 4 * 1024 * 1024,
  });
  require(!generated.error && generated.status === 0 && generated.stdout.equals(expected), "independent generator output mismatch");
  const output = JSON.parse(expected);
  require(output.modelVersion === "naatre.generator-model-1" && output.generatorVersion === "naatre.generator.reference-json-1", "generated metadata mismatch");
  require(output.operations?.[0]?.persisted?.digest === output.operations?.[0]?.request?.persisted?.digest, "persisted request mismatch");
  require(output.operations?.[0]?.request?.variables?.nickname === null, "explicit null was not preserved");
  require(Object.keys(JSON.parse(model).operations[0].requestVariables).length === Object.keys(output.operations[0].request.variables).length, "request variables changed");
  require(evidence.sdkBehavior.malformedResponse === "typed-protocol-error-no-success", "hostile response behavior mismatch");
} catch {
  process.stderr.write("language-neutral generator profile failed\n");
  process.exitCode = 1;
}
