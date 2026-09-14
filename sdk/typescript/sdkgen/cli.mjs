#!/usr/bin/env node

import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import { generate } from "./generator.mjs";

const root = new URL("../../../", import.meta.url);
const model = new URL("conformance/v1/generator-model.json", root);
const reference = new URL("conformance/v1/generator-output.json", root);
const output = new URL("sdk/typescript/generated/", root);

try {
  const artifacts = generate(await readFile(model), await readFile(reference));
  await Promise.all([
    writeFile(new URL("operations.ts", output), artifacts.source),
    writeFile(new URL("operations.json", output), artifacts.manifest),
  ]);
} catch {
  process.stderr.write("TypeScript SDK generation failed\n");
  process.exitCode = 1;
}
