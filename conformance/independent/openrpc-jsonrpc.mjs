#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "../..");
const fixture = JSON.parse(readFileSync(resolve(here, "../v1/openrpc-jsonrpc.json"), "utf8"));
const fail = (message) => { throw new Error(message); };
const exact = (actual, expected, label) => {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) fail(`${label} mismatch`);
};

if (fixture.profile !== "adapter.openrpc-jsonrpc-1" || fixture.fixtureSuite !== "1.0.0") fail("invalid profile metadata");
if (fixture.specifications.openrpc !== "1.4.1" || fixture.specifications.jsonrpc !== "2.0" || fixture.specifications.naatreAdapterProfile !== "core.adapters-1") fail("specification pins changed");
exact(fixture.directions, ["schema-import", "schema-export", "runtime-consume", "runtime-expose"], "directions");
if (!/^[0-9a-f]{40}$/.test(fixture.implementation.baseRevision) || !/^[0-9a-f]{40}$/.test(fixture.implementation.coreAdapterDependencyRevision)) fail("dependency commits are not exact");

for (const entry of [...fixture.implementation.dependencies, ...fixture.implementation.files]) {
  const content = readFileSync(resolve(root, entry.path));
  const digest = createHash("sha256").update(content).digest("hex");
  if (digest !== entry.sha256) fail(`${entry.path} digest mismatch: ${digest}`);
}

const classifications = new Set(["lossless", "explicitly-adapted", "unsupported", "application-supplied"]);
const fidelity = new Map(fixture.fidelity.map((entry) => [entry.feature, entry]));
for (const feature of ["batch", "notification", "id", "result-error", "scalar", "deadline", "authentication", "effects-policy", "unsupported-schema"]) {
  const entry = fidelity.get(feature);
  if (!entry || !classifications.has(entry.classification) || !entry.evidence) fail(`missing fidelity evidence for ${feature}`);
}
if (fidelity.get("effects-policy").classification !== "application-supplied" || fidelity.get("id").classification !== "lossless") fail("policy inference or ID fidelity claim changed");

for (const fixtureClass of ["positive", "negative", "boundary", "cancellation", "resourceLimit", "security"]) {
  if (!Array.isArray(fixture.fixtureClasses[fixtureClass]) || fixture.fixtureClasses[fixtureClass].length === 0) fail(`missing ${fixtureClass} fixtures`);
}
for (const command of ["package", "vet", "independent"]) if (!fixture.commands[command]) fail(`missing reproducible ${command} command`);
for (const capability of ["listener-tls-quic-framework-native-runtime-non-go-and-certification-support", "fractional-jsonrpc-ids", "atomic-or-transactional-jsonrpc-batches"]) {
  if (!fixture.unsupported.includes(capability)) fail(`missing unsupported capability ${capability}`);
}
if (JSON.stringify(fixture).match(/Bearer|password|token|cookie-value|stack trace/i)) fail("fixture contains protected material");

console.log(`OpenRPC/JSON-RPC conformance: ${fixture.fidelity.length} fidelity rows, ${Object.values(fixture.fixtureClasses).flat().length} cases, and ${fixture.implementation.dependencies.length + fixture.implementation.files.length} exact revisions passed`);
