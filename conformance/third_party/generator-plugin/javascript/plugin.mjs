#!/usr/bin/env node

import { stdin, stdout, stderr } from "node:process";

const chunks = [];
for await (const chunk of stdin) chunks.push(chunk);

let request;
try {
  request = JSON.parse(Buffer.concat(chunks).toString("utf8"));
} catch {
  stdout.write(`${JSON.stringify({ profile: "sdk.generator-plugin-host-1", status: "failed", artifacts: [] })}\n`);
  process.exit(0);
}

const mode = request.configuration?.mode ?? "generate";
if (mode === "hang") {
  setInterval(() => {}, 60_000);
} else if (mode === "oversized") {
  stdout.write("x".repeat(16 * 1024 * 1024));
} else if (mode === "reject") {
  stderr.write(`rejected private model: ${JSON.stringify(request.model)}\n`);
  process.exit(9);
} else {
  const operations = [...request.model.operations]
    .sort((left, right) => left.name < right.name ? -1 : left.name > right.name ? 1 : 0)
    .map((operation) => ({ name: operation.name, description: operation.description ?? "" }));
  const metadata = {
    modelVersion: request.model.version,
    algorithmVersion: "fixture.javascript-typescript-1",
    protocolVersion: request.model.protocolVersion,
    canonicalVersion: request.model.canonicalVersion,
  };
  const source = [
    `export const generatorMetadata = ${JSON.stringify(metadata)};`,
    `export const operations = ${JSON.stringify(operations)};`,
    "",
  ].join("\n");
  const response = {
    profile: "sdk.generator-plugin-host-1",
    status: "ok",
    artifacts: [{ path: mode === "path-escape" ? "../injected.mjs" : "generated/fixture.mjs", content: source }],
  };
  stdout.write(`${JSON.stringify(response)}\n`);
}
