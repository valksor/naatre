#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const fixtureURL = new URL("../v1/mcp-runtime.json", import.meta.url);
const fixture = JSON.parse(readFileSync(fixtureURL, "utf8"));
const core = JSON.parse(readFileSync(new URL("../v1/mcp-adapter.json", import.meta.url), "utf8"));
const requireValue = (condition, message) => { if (!condition) throw new Error(message); };
const exact = (actual, expected, label) => requireValue(JSON.stringify(actual) === JSON.stringify(expected), `${label} mismatch`);
const sha256 = (path) => createHash("sha256").update(readFileSync(new URL(`../../${path}`, import.meta.url))).digest("hex");

requireValue(fixture.profile === "adapter.mcp-go-runtime-1" && fixture.version === "1.0.0", "invalid MCP runtime profile");
requireValue(fixture.ownerIssue === 103 && fixture.normativeOwnerIssue === 64 && fixture.combinedMatrixOwnerIssue === 69, "invalid profile ownership");
requireValue(fixture.dependencies.issue64Commit === "6e894e925dd7c24f3b1d7bd8c07c1040a2a11fcd", "issue 64 revision is not exact");
requireValue(fixture.dependencies.coreProfile === core.profile && fixture.dependencies.mcpRevision === core.revisions.mcpProtocolVersion, "core profile dependency mismatch");
for (const dependency of [fixture.dependencies.coreFixture, fixture.dependencies.specification, fixture.dependencies.goMod]) {
  requireValue(sha256(dependency.path) === dependency.sha256, `dependency digest mismatch: ${dependency.path}`);
}
exact(fixture.mappingMatrix, core.mappingMatrix, "core mapping matrix");

const classes = new Set(["positive", "negative", "boundary", "cancellation", "resource-limit"]);
for (const testCase of fixture.cases) {
  requireValue(testCase.id && classes.has(testCase.class) && testCase.status === "passed", `invalid case ${testCase.id}`);
  classes.delete(testCase.class);
}
requireValue(classes.size === 0, `missing case classes: ${[...classes].join(",")}`);

for (const transport of ["stdio", "streamable-http"]) {
  const profile = fixture.transports[transport];
  requireValue(profile.client === true && profile.server === true && profile.listenerBound === false, `incomplete ${transport} implementation`);
  requireValue(profile.cancellation && profile.pendingLimit > 0 && profile.responseLimit > 0, `unbounded ${transport} profile`);
}
requireValue(fixture.transports.stdio.reconnectAttempts === 0 && fixture.transports.stdio.stdout === "protocol-only", "invalid stdio lifecycle");
requireValue(fixture.transports["streamable-http"].originValidation === "exact-allowlist" && fixture.transports["streamable-http"].authentication === "application-supplied-required", "invalid HTTP trust boundary");

for (const required of ["trusted-tool-projection", "trusted-resource-projection", "partial-envelope-fidelity", "safe-public-errors", "bounded-schema-instance-conversion"]) {
  requireValue(fixture.supported.includes(required), `missing supported capability ${required}`);
}
for (const unsupported of ["prompt-execution", "resource-templates-runtime", "sampling", "roots", "elicitation", "automatic-operation-retry", "websocket", "streaming-tool-results"]) {
  requireValue(fixture.unsupported.includes(unsupported), `missing unsupported capability ${unsupported}`);
}
requireValue(new Set(fixture.unsupported).size === fixture.unsupported.length && new Set(fixture.failureCodes).size === fixture.failureCodes.length, "duplicate capability or failure code");
for (const code of fixture.failureCodes) requireValue(/^MCP_[A-Z0-9_]{2,60}$/.test(code), `unsafe failure code ${code}`);
for (const source of fixture.implementation) requireValue(sha256(source.path) === source.sha256, `implementation digest mismatch: ${source.path}`);
requireValue(fixture.commands.goTest === "go test ./mcpadapter -count=1" && fixture.commands.goVet === "go vet ./mcpadapter" && fixture.commands.independent === "node conformance/independent/mcp-runtime.mjs", "commands are not reproducible");

console.log(`MCP Go runtime conformance: ${fixture.cases.length} cases across ${Object.keys(fixture.transports).length} transports passed`);
