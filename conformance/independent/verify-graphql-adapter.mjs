#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const evidence = JSON.parse(readFileSync(new URL("../v1/graphql-adapter.json", import.meta.url), "utf8"));

function repositoryPath(path) {
  return fileURLToPath(new URL(path, root));
}
function digest(path) {
  return createHash("sha256").update(readFileSync(repositoryPath(path))).digest("hex");
}

try {
  if (evidence.profile !== "core.adapters.graphql-1" || evidence.fixtureSuite !== "1.0.0") throw new Error("invalid profile identity");
  if (evidence.upstreamSpecification !== "https://spec.graphql.org/September2025/") throw new Error("unpinned GraphQL specification");
  if (!Array.isArray(evidence.dependencies) || evidence.dependencies.length < 3) throw new Error("missing dependency revisions");
  for (const dependency of evidence.dependencies) {
    if (!/^[0-9a-f]{64}$/.test(dependency.sha256) || digest(dependency.path) !== dependency.sha256) throw new Error("dependency digest mismatch");
  }
  const directions = new Set(evidence.directions);
  for (const direction of ["schema-import", "schema-export", "runtime-consume", "runtime-expose"]) {
    if (!directions.has(direction)) throw new Error("missing direction evidence");
  }
  const classes = new Set(evidence.cases?.map((testCase) => testCase.class));
  for (const required of ["positive", "negative", "boundary", "cancellation", "resource-limit", "security"]) {
    if (!classes.has(required)) throw new Error("missing conformance case class");
  }
  if (evidence.cases.some((testCase) => testCase.result !== "passed")) throw new Error("non-passing conformance case");
  if (!evidence.unsupported?.includes("graphql-http-transport") || !evidence.unsupported?.includes("graphql-websocket-transport")) throw new Error("transport capability overclaim");
  if (!evidence.commands?.includes("node conformance/independent/verify-graphql-adapter.mjs")) throw new Error("missing reproducible verifier command");
} catch {
  process.stderr.write("GraphQL adapter profile failed\n");
  process.exitCode = 1;
}
