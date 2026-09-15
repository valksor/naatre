#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const root = new URL("../../", import.meta.url);
const evidence = JSON.parse(readFileSync(new URL("conformance/v1/openapi-adapter.json", root), "utf8"));
const requireValue = (condition, message) => {
  if (!condition) throw new Error(message);
};

requireValue(evidence.profile === "adapter.openapi-1" && evidence.issue === 93, "invalid OpenAPI adapter evidence identity");
requireValue(evidence.specification === "https://spec.openapis.org/oas/v3.2.0", "OpenAPI revision is not pinned");
requireValue(evidence.jsonSchemaDialect === "https://json-schema.org/draft/2020-12/schema", "JSON Schema dialect is not pinned");
requireValue(evidence.dependencies.repositoryBase.length === 40 && evidence.dependencies.coreAdapterRevision.length === 40, "dependency revision is not exact");
for (const source of evidence.dependencies.sources) {
  const digest = createHash("sha256").update(readFileSync(new URL(source.path, root))).digest("hex");
  requireValue(digest === source.sha256, `dependency digest mismatch for ${source.path}`);
}
for (const direction of ["schema-import", "schema-export", "runtime-consume", "runtime-expose"]) {
  requireValue(evidence.directions.some((entry) => entry.direction === direction), `missing direction ${direction}`);
}
for (const category of ["positive", "negative", "boundary", "cancellation", "resource-limit", "security"]) {
  requireValue(evidence.cases.some((entry) => entry.category === category), `missing ${category} evidence`);
}
for (const code of evidence.cases.flatMap((entry) => entry.expectedCode ? [entry.expectedCode] : [])) {
  requireValue(evidence.stablePublicCodes.includes(code), `case uses undeclared public code ${code}`);
}
requireValue(evidence.directions.find((entry) => entry.direction === "runtime-expose")?.status === "unsupported", "runtime expose must not be overclaimed");
console.log(`OpenAPI adapter conformance: ${evidence.cases.length} cases and ${evidence.dependencies.sources.length} dependency digests passed`);
