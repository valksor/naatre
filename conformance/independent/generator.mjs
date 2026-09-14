#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const modelVersion = "naatre.generator-model-1";
const generatorVersion = "naatre.generator.reference-json-1";
const defaultModel = fileURLToPath(new URL("../v1/generator-model.json", import.meta.url));
const model = JSON.parse(readFileSync(process.argv[2] ? resolve(process.argv[2]) : defaultModel, "utf8"));

const builtInScalars = new Set([
  "BigInt", "Boolean", "Bytes", "Decimal", "Duration", "Float64", "ID",
  "Int32", "Int64", "String", "Timestamp", "UInt64", "UUID",
]);
const reservedWords = new Set([
  "class", "const", "default", "delete", "enum", "export", "extends",
  "function", "import", "interface", "new", "package", "private",
  "protected", "public", "return", "static", "struct", "switch", "type",
  "var", "yield",
]);

function require(condition, message) {
  if (!condition) throw new Error(message);
}

function clone(value) {
  return structuredClone(value);
}

function normalizeSchema(schema) {
  const value = clone(schema);
  for (const member of ["capabilities", "retired"]) {
    if (Array.isArray(value[member])) value[member].sort();
  }
  if (Array.isArray(value.types)) {
    value.types.sort((left, right) => left.id < right.id ? -1 : left.id > right.id ? 1 : 0);
    for (const type of value.types) {
      for (const member of ["fields", "enumMembers", "unionMembers", "traits"]) {
        if (Array.isArray(type[member])) type[member].sort((left, right) => left.id < right.id ? -1 : left.id > right.id ? 1 : 0);
      }
    }
  }
  return value;
}

function normalizeDocument(document) {
  const value = clone(document);
  if (Array.isArray(value.requires)) value.requires.sort();
  return value;
}

function encodeCanonical(value) {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) {
    const elements = [];
    for (const element of value) elements.push(encodeCanonical(element));
    return `[${elements.join(",")}]`;
  }
  const members = [];
  for (const key of Object.keys(value).sort()) {
    members.push(`${JSON.stringify(key)}:${encodeCanonical(value[key])}`);
  }
  return `{${members.join(",")}}`;
}

function semanticDigest(purpose, value) {
  return createHash("sha256")
    .update(`naatre:${purpose}:c14n-1\n`, "utf8")
    .update(encodeCanonical(value), "utf8")
    .digest("hex");
}

function digestRecord(purpose, value) {
  return { algorithm: "sha-256", canonicalVersion: "c14n-1", digest: semanticDigest(purpose, value) };
}

function symbolFor(name) {
  const words = name.match(/[A-Za-z0-9]+/gu) ?? [];
  require(words.length > 0, "operation name cannot produce a portable symbol");
  let symbol = words.map((word) => word[0].toUpperCase() + word.slice(1)).join("");
  require(!/^[0-9]/u.test(symbol), "operation name cannot produce a portable symbol");
  if (reservedWords.has(symbol.toLowerCase())) symbol += "_";
  return symbol;
}

function validateModel() {
  require(model.profile === "sdk.generation-1", "unsupported generator profile");
  require(model.version === modelVersion, "unsupported model version");
  require(model.protocolVersion === "1" && model.canonicalVersion === "c14n-1", "unsupported protocol or canonical version");
  require(model.schema?.revision && Array.isArray(model.schema.types), "invalid schema header");
  require(model.configuration?.scalarMappings && typeof model.configuration.scalarMappings === "object", "missing scalar mappings");
  require(Array.isArray(model.operations) && model.operations.length > 0, "missing operations");
  for (const descriptor of model.schema.types) {
    if (descriptor.kind === "scalar" && !builtInScalars.has(descriptor.id)) {
      require(typeof model.configuration.scalarMappings[descriptor.id] === "string" && model.configuration.scalarMappings[descriptor.id].trim() !== "", "unmapped custom scalar");
    }
  }
}

function generateOperation(operation, symbols) {
  require(operation.document?.operations && Array.isArray(operation.variables), "invalid operation model");
  require(operation.requestVariables !== null && typeof operation.requestVariables === "object" && !Array.isArray(operation.requestVariables), "invalid request variables");
  require(operation.result !== null && typeof operation.result === "object", "invalid selected result");
  const symbol = symbolFor(operation.name);
  require(!symbols.has(symbol), "generated symbol collision");
  symbols.add(symbol);
  const persisted = digestRecord("document", normalizeDocument(operation.document));
  return {
    name: operation.name,
    symbol,
    artifact: `operations/${symbol}.json`,
    ...(operation.description ? { description: operation.description } : {}),
    variables: clone(operation.variables),
    persisted,
    request: {
      version: model.protocolVersion,
      operation: operation.name,
      persisted,
      variables: clone(operation.requestVariables),
    },
    result: {
      kind: "operation-result",
      data: clone(operation.result),
      errors: "structured-errors",
      complete: "explicit-completion-state",
    },
  };
}

try {
  validateModel();
  const symbols = new Set();
  const output = {
    profile: model.profile,
    modelVersion: model.version,
    generatorVersion,
    protocolVersion: model.protocolVersion,
    canonicalVersion: model.canonicalVersion,
    schema: digestRecord("schema", normalizeSchema(model.schema)),
    scalarMappings: clone(model.configuration.scalarMappings),
    forwardCompatibility: {
      openEnum: "preserve-unknown-raw-value",
      openUnion: "preserve-unknown-raw-discriminator",
      closed: "reject-as-protocol-error",
    },
    behavior: {
      partialData: "preserve-data-and-structured-errors",
      missing: "omit-property",
      explicitNull: "preserve-null",
      unknownDomainVariant: "preserve-open-variant",
      unknownControlField: "reject-required-control-field",
      retry: "idempotent-only",
      authRefresh: "single-flight-no-write-replay",
      limits: { responseBytes: 8 << 20, frameBytes: 1 << 20, decompressedBytes: 16 << 20, redirects: 5 },
      cancellation: { http: "active-abort", sse: "active-abort", websocket: "active-close" },
    },
    operations: [...model.operations].sort((left, right) => left.name < right.name ? -1 : left.name > right.name ? 1 : 0).map((operation) => generateOperation(operation, symbols)),
  };
  process.stdout.write(`${encodeCanonical(output)}\n`);
} catch {
  process.stderr.write("independent generator failed\n");
  process.exitCode = 1;
}
