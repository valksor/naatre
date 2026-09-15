#!/usr/bin/env node

import { readFileSync } from "node:fs";

const fixture = JSON.parse(readFileSync(new URL("../v1/mcp-adapter.json", import.meta.url), "utf8"));
const requireValue = (condition, message) => { if (!condition) throw new Error(message); };
const exact = (actual, expected, label) => requireValue(JSON.stringify(actual) === JSON.stringify(expected), `${label} mismatch`);

requireValue(fixture.profile === "adapter.mcp-1" && fixture.fixtureSuite === "1.0.0", "invalid MCP profile identity");
requireValue(fixture.revisions.mcpProtocolVersion === "2025-11-25" && fixture.revisions.mcp.endsWith("/2025-11-25"), "MCP revision is not pinned");
requireValue(fixture.revisions.naatreSchema === "core.schema-1" && fixture.revisions.naatreProtocol === "core.protocol-1" && fixture.revisions.conformance === "1.0.0", "Naatre revisions are incomplete");
exact(fixture.directions, ["naatre-to-mcp", "mcp-to-naatre"], "directions");

const requiredFeatures = ["initialization", "capabilities", "tools", "resources", "resource-templates", "prompts", "progress", "cancellation", "logging", "pagination", "structured-content", "errors", "transport-lifecycle", "missing-null", "numeric", "timestamp", "closed-union", "extended-scalars", "open-variants", "partial-results", "streams", "files", "custom-constraints"];
const classifications = new Set(["lossless", "explicitly-adapted", "unsupported", "application-supplied"]);
const mappings = new Map(fixture.mappingMatrix.map((entry) => [entry.feature, entry]));
for (const feature of requiredFeatures) {
  const mapping = mappings.get(feature);
  requireValue(mapping && classifications.has(mapping.classification), `missing mapping ${feature}`);
  requireValue(mapping.classification === "lossless" || typeof mapping.resolution === "string", `unresolved mapping ${feature}`);
  requireValue(!(mapping.required && mapping.classification === "unsupported"), `required mapping ${feature} is unsupported`);
}

requireValue(fixture.roundTrip.declaredSubsetOnly, "round-trip claim is not subset-scoped");
requireValue(!Object.hasOwn(fixture.roundTrip.input, "optional"), "missing input member was materialized");
for (const field of ["nullable", "count", "ratio", "int64", "uint64", "decimal", "timestamp", "variant"]) exact(fixture.roundTrip.structuredResult.data[field], fixture.roundTrip.input[field], `round-trip ${field}`);
requireValue(fixture.roundTrip.structuredResult.complete === false && fixture.roundTrip.structuredResult.errors[0].path.join(".") === "profile.secret", "partial error semantics were lost");

for (const registration of fixture.registrationFixtures) {
  if (registration.expectedCode) requireValue(registration.invocations === 0, `${registration.name} invoked before rejection`);
}
const approved = fixture.registrationFixtures.find((entry) => entry.name === "approved-tool");
exact(approved.registered, ["lookupProfile"], "trusted tool registration");
requireValue(!approved.registered.includes("hiddenAdmin"), "untrusted discovery registered a hidden tool");
for (const hostile of fixture.securityFixtures) requireValue(hostile.authority === "untrusted-data" && hostile.changes.length === 0, `${hostile.input} became authority`);

exact(fixture.policyAuthority.fields, ["effect", "idempotency", "retrySafe", "cacheable", "cost", "batching", "transaction", "authorizationPolicy"], "trusted policy fields");
for (const state of ["principal", "tenant", "progress-token-binding", "cache", "loader", "request-local-state", "cancellation"]) requireValue(fixture.sessionIsolation.notShared.includes(state), `shared session state ${state}`);
requireValue(fixture.sessionIsolation.sameTokenTextAcrossSessions === "independent", "progress tokens are globally shared");

const outcomes = new Map(fixture.lifecycleFixtures.map((entry) => [entry.event, entry]));
for (const [event, code, complete] of [["completed", "", true], ["cancellation", "CANCELLED", false], ["disconnect", "MCP_DISCONNECTED", false], ["timeout", "DEADLINE_EXCEEDED", false], ["oversized-response", "RESPONSE_TOO_LARGE", false], ["process-death", "MCP_PROCESS_DIED", false]]) {
  const outcome = outcomes.get(event);
  requireValue(outcome?.code === code && outcome.complete === complete && outcome.releasesRequestState === true, `terminal lifecycle outcome ${event}`);
}
requireValue(outcomes.get("progress-overflow")?.code === "MCP_PROGRESS_LIMIT", "lifecycle outcome progress-overflow");

requireValue(fixture.transports.stdio.originValidation === "not-applicable" && fixture.transports.stdio.reconnect === false && fixture.transports.stdio.stdout === "protocol-only", "invalid stdio profile");
requireValue(fixture.transports["streamable-http"].originValidation === "exact-allowlist" && fixture.transports["streamable-http"].endpoint === "trusted-https-only" && fixture.transports["streamable-http"].sessionIsolation === "Mcp-Session-Id", "invalid Streamable HTTP profile");
requireValue(fixture.nonClaims.includes("mcp-wire-compatibility") && fixture.nonClaims.includes("naatre-wire-compatibility"), "wire non-claims are missing");
requireValue(fixture.ownership.runtimeAdapters === 103 && fixture.ownership.combinedMatrix === 69, "extracted issue ownership changed");

console.log(`MCP adapter conformance: ${fixture.mappingMatrix.length} mappings, ${fixture.registrationFixtures.length} registration fixtures, ${fixture.lifecycleFixtures.length} lifecycle outcomes passed`);
