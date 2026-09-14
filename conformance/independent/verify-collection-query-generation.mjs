#!/usr/bin/env node

import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const repositoryURL = new URL("../../", import.meta.url);
const evidence = JSON.parse(readFileSync(new URL("../v1/collection-query-generation.json", import.meta.url), "utf8"));

function repositoryPath(path) {
  return fileURLToPath(new URL(path, repositoryURL));
}

function digest(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function requireDigest(path, expected) {
  const content = readFileSync(repositoryPath(path));
  if (digest(content) !== expected) throw new Error("evidence digest mismatch");
  return content;
}

try {
  if (evidence.profile !== "collection.query.codegen-1" || evidence.outputs?.length !== 1) throw new Error("invalid generation evidence");
  requireDigest(`conformance/${evidence.source.path}`, evidence.source.sha256);
  requireDigest(evidence.generator.path, evidence.generator.sha256);
  const expected = requireDigest(evidence.outputs[0].path, evidence.outputs[0].sha256);
  const generated = spawnSync(process.execPath, [repositoryPath(evidence.generator.path), repositoryPath(`conformance/${evidence.source.path}`)], {
    encoding: null,
    timeout: 30_000,
    maxBuffer: 1024 * 1024,
  });
  if (generated.error || generated.status !== 0 || !generated.stdout.equals(expected)) throw new Error("generated output mismatch");
  const parsed = spawnSync(process.execPath, ["--experimental-strip-types", repositoryPath(evidence.outputs[0].path)], {
    encoding: "utf8",
    timeout: 30_000,
    maxBuffer: 1024 * 1024,
  });
  if (parsed.error || parsed.status !== 0) throw new Error("generated TypeScript is invalid");
} catch {
  process.stderr.write("collection query generation profile failed\n");
  process.exitCode = 1;
}
