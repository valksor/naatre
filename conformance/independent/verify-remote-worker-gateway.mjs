#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const manifest = JSON.parse(readFileSync(resolve(root, "conformance/v1/remote-worker-gateway.json"), "utf8"));

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function verifyFiles(entries) {
  const valid = entries.every((entry) => Object.keys(entry).length === 2 && typeof entry.path === "string" && /^[0-9a-f]{64}$/u.test(entry.sha256) &&
    createHash("sha256").update(readFileSync(resolve(root, entry.path))).digest("hex") === entry.sha256);
  requireValue(valid, "implementation evidence file mismatch");
}

requireValue(manifest.profile === "implementation.go.remote-worker-1", "unexpected implementation profile");
requireValue(manifest.ownerIssue === 88 && manifest.fixtureSuite === "1.0.0", "implementation ownership drift");
requireValue(manifest.dependencies.length === 2, "dependency inventory is incomplete");
requireValue(manifest.dependencies[0].profile === "worker.remote-1" && manifest.dependencies[0].ownerIssue === 51, "remote-worker authority drift");
requireValue(manifest.dependencies[1].profile === "core.streaming-1" && manifest.dependencies[1].ownerIssue === 23, "streaming authority drift");
for (const dependency of manifest.dependencies) {
  requireValue(/^[0-9a-f]{40}$/u.test(dependency.gitCommit), `dependency commit is not exact: ${dependency.profile}`);
  verifyFiles(dependency.files);
}
verifyFiles(manifest.implementation.files);

const requiredCases = ["unary", "streaming", "schema-mismatch", "forged-identity", "overload", "cancellation", "process-death", "duplicate-invocation", "stale-reference", "bounded-backpressure"];
const caseNames = new Set(manifest.cases.map((test) => test.name));
for (const name of requiredCases) requireValue(caseNames.has(name), `missing acceptance case: ${name}`);
const classes = new Set(manifest.cases.map((test) => test.class));
for (const fixtureClass of ["positive", "negative", "malformed", "boundary", "cancellation", "limit", "security"]) requireValue(classes.has(fixtureClass), `missing fixture class: ${fixtureClass}`);
for (const test of manifest.cases) {
  requireValue(test.result === "passed" && typeof test.test === "string" && test.test.startsWith("Test"), `invalid executed case: ${test.name}`);
  requireValue(test.code === "" || manifest.stablePublicCodes.includes(test.code), `unstable public code: ${test.name}`);
}

for (const required of ["HTTP/1.1 fallback", "client streaming", "bidirectional streaming", "dynamic stream-credit replenishment", "native runtime certification", "exactly-once effects"]) {
  requireValue(manifest.runtimeBoundary.unsupported.includes(required), `unsupported boundary omitted: ${required}`);
}
requireValue(manifest.verificationCommands.every((command) => Array.isArray(command) && command.length > 1 && command.every((argument) => typeof argument === "string" && !/token|secret|password/iu.test(argument))), "unsafe verification command");

process.stdout.write(JSON.stringify({profile: manifest.profile, status: "passed", cases: manifest.cases.length}) + "\n");
