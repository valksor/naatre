#!/usr/bin/env node

import { readFileSync } from "node:fs";

const fixture = JSON.parse(readFileSync(new URL("../v1/adapters.json", import.meta.url), "utf8"));
const exact = (actual, expected, label) => {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) throw new Error(`${label} mismatch`);
};

if (fixture.profile !== "core.adapters-1" || fixture.fixtureSuite !== "1.0.0") throw new Error("invalid adapter profile metadata");
exact(fixture.directions, ["schema-import", "schema-export", "runtime-consume", "runtime-expose"], "directions");
exact(fixture.classifications, ["lossless", "explicitly-adapted", "unsupported", "application-supplied"], "classifications");
const protocols = Object.keys(fixture.specifications).sort();
exact(protocols, ["graphql", "openapi", "openrpc-jsonrpc", "protobuf-grpc-connect"], "protocols");
for (const protocol of protocols) {
  const rows = fixture.matrix.filter((entry) => entry.protocol === protocol);
  for (const direction of fixture.directions) if (!rows.some((entry) => entry.direction === direction)) throw new Error(`${protocol} omits ${direction}`);
  for (const category of ["operation", "type", "transport"]) if (!rows.some((entry) => entry.category === category)) throw new Error(`${protocol} omits ${category}`);
  for (const feature of fixture.requiredFeatures) if (!rows.some((entry) => entry.feature === feature)) throw new Error(`${protocol} omits ${feature}`);
}
for (const entry of fixture.matrix) {
  if (!fixture.classifications.includes(entry.classification)) throw new Error(`invalid classification for ${entry.feature}`);
  if (entry.classification !== "lossless" && !entry.resolution) throw new Error(`unresolved mapping for ${entry.feature}`);
}
const golden = new Set(fixture.goldenCases.map((entry) => entry.name));
for (const required of ["scalar-boundaries", "absent-and-null-inputs", "partial-failure", "streaming-mismatch", "cancellation", "authentication-forwarding", "graphql-non-null-propagation", "jsonrpc-envelope"]) {
  if (!golden.has(required)) throw new Error(`missing golden case ${required}`);
}
if (!fixture.security.operations.includes("allowlist") || fixture.security.legacyParsers.length !== 0) throw new Error("unsafe registration or legacy parser claim");
if (!fixture.example.languageNeutral || fixture.example.approvedOperations.length !== 1 || fixture.example.approvedOperations[0] !== "lookupProfile") throw new Error("invalid language-neutral example");
if (fixture.roundTrip.claimed.some((claim) => fixture.roundTrip.excluded.includes(claim))) throw new Error("round-trip subset overlaps exclusions");
for (const claim of fixture.roundTrip.claimed) {
  const vector = fixture.roundTrip.vectors.find((entry) => entry.claim === claim);
  if (!vector || JSON.stringify(vector.external) !== JSON.stringify(vector.externalRoundTrip)) throw new Error(`round-trip fixture failed for ${claim}`);
}
console.log(`adapter conformance: ${fixture.matrix.length} mappings and ${fixture.goldenCases.length} golden cases passed`);
