import { createHash } from "node:crypto";

import { canonicalStringify, parseJSON } from "../runtime/index.mjs";

export const generatorVersion = "naatre.generator.typescript-sdk-1";
const maximumInputBytes = 4 << 20;
const identifier = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/u;
const reserved = new Set(["break", "case", "catch", "class", "const", "continue", "debugger", "default", "delete", "do", "else", "enum", "export", "extends", "false", "finally", "for", "function", "if", "import", "in", "instanceof", "new", "null", "return", "super", "switch", "this", "throw", "true", "try", "typeof", "var", "void", "while", "with", "yield"]);
const builtInTypes = Object.freeze({ Boolean: "boolean", Int32: "number", Float64: "number", Int64: "bigint", UInt64: "bigint", BigInt: "bigint", Decimal: "string", Timestamp: "string", Duration: "string", UUID: "string", Bytes: "Uint8Array", String: "string", ID: "string", StringList: "readonly string[]", StringMap: "Readonly<Record<string, string>>" });

export class TypeScriptGeneratorError extends Error {
  constructor(code) {
    super(`TypeScript SDK generation failed: ${code}`);
    this.name = "TypeScriptGeneratorError";
    this.code = code;
  }
}

export function generate(modelBytes, referenceBytes) {
  if (byteLength(modelBytes) === 0 || byteLength(modelBytes) > maximumInputBytes || byteLength(referenceBytes) === 0 || byteLength(referenceBytes) > maximumInputBytes) reject("TYPESCRIPT_SDK_GENERATOR_INPUT_LIMIT");
  let model;
  let reference;
  try {
    model = parseJSON(modelBytes, maximumInputBytes);
    reference = parseJSON(referenceBytes, maximumInputBytes);
  } catch {
    reject("TYPESCRIPT_SDK_GENERATOR_INVALID_INPUT");
  }
  if (model.version !== "naatre.generator-model-1" || model.protocolVersion !== "1" || model.canonicalVersion !== "c14n-1" || reference.modelVersion !== model.version || reference.protocolVersion !== model.protocolVersion || reference.canonicalVersion !== model.canonicalVersion || !Array.isArray(model.operations) || !Array.isArray(reference.operations)) reject("TYPESCRIPT_SDK_GENERATOR_VERSION_SKEW");
  if (reference.generatorVersion !== "naatre.generator.reference-json-1") reject("TYPESCRIPT_SDK_GENERATOR_VERSION_SKEW");
  const referenceByName = new Map(reference.operations.map((operation) => [operation.name, operation]));
  if (referenceByName.size !== model.operations.length) reject("TYPESCRIPT_SDK_GENERATOR_REFERENCE_DRIFT");
  const customMappings = model.configuration?.scalarMappings;
  if (!isRecord(customMappings)) reject("TYPESCRIPT_SDK_GENERATOR_UNMAPPED_SCALAR");
  validateSchema(model.schema, customMappings);
  if (!isRecord(reference.schema) || reference.schema.algorithm !== "sha-256" || reference.schema.canonicalVersion !== "c14n-1" || reference.schema.digest !== semanticDigest("schema", normalizeSchema(model.schema)) || canonicalStringify(reference.scalarMappings) !== canonicalStringify(customMappings)) reject("TYPESCRIPT_SDK_GENERATOR_REFERENCE_DRIFT");
  const operations = [...model.operations].sort(compareName).map((operation) => operationBinding(operation, referenceByName.get(operation.name)));
  const source = generateSource(model, operations, customMappings);
  const manifest = canonicalStringify({
    profile: "sdk.typescript.core-1",
    version: "1",
    protocolVersion: "1",
    canonicalVersion: "c14n-1",
    operations: operations.map(({ name, kind, persisted }) => ({ name, kind, persisted })),
  });
  return Object.freeze({ source: `${source}\n`, manifest: `${manifest}\n` });
}

function operationBinding(operation, reference) {
  if (!reference || reference.name !== operation.name || !identifier.test(reference.symbol ?? "") || !isRecord(reference.persisted) || reference.persisted.algorithm !== "sha-256" || reference.persisted.canonicalVersion !== "c14n-1" || !/^[0-9a-f]{64}$/u.test(reference.persisted.digest ?? "")) reject("TYPESCRIPT_SDK_GENERATOR_REFERENCE_DRIFT");
  const documentOperation = operation.document?.operations?.find((candidate) => candidate.name === operation.name);
  if (!documentOperation || !["query", "mutation", "subscription"].includes(documentOperation.kind) || !Array.isArray(operation.variables) || !isRecord(operation.result)) reject("TYPESCRIPT_SDK_GENERATOR_INVALID_OPERATION");
  if (semanticDigest("document", normalizeDocument(operation.document)) !== reference.persisted.digest) reject("TYPESCRIPT_SDK_GENERATOR_REFERENCE_DRIFT");
  if (canonicalStringify([...operation.variables].sort(compareName)) !== canonicalStringify([...(reference.variables ?? [])].sort(compareName)) || canonicalStringify(operation.result) !== canonicalStringify(reference.result?.data)) reject("TYPESCRIPT_SDK_GENERATOR_REFERENCE_DRIFT");
  return { name: operation.name, symbol: safeSymbol(reference.symbol), kind: documentOperation.kind, persisted: reference.persisted, variables: operation.variables, result: operation.result, description: operation.description };
}

function generateSource(model, operations, mappings) {
  const runtimeImports = new Set(["createOperation", "createScalarCodecs", "decodeSelected", "safeObject"]);
  for (const operation of operations) collectDecoders(operation.result, runtimeImports);
  const lines = [
    "// Code generated by naatre TypeScript SDK generator; DO NOT EDIT.",
    "",
    `import { ${[...runtimeImports].sort().join(", ")} } from "../runtime/index.mjs";`,
    'import type { Operation, OperationDefinition, ScalarCodecs, Selected } from "../runtime/index.mjs";',
    "",
    `export const generatorVersion = ${JSON.stringify(generatorVersion)} as const;`,
    "",
  ];
  for (const descriptor of [...(model.schema?.types ?? [])].sort(compareID)) lines.push(...schemaDeclaration(descriptor, mappings), "");
  for (const operation of operations) lines.push(...operationDeclaration(operation, mappings), "");
  lines.push(`export const manifest = ${formatValue({ profile: "sdk.typescript.core-1", version: "1", protocolVersion: "1", canonicalVersion: "c14n-1", operations: operations.map(({ name, kind, persisted }) => ({ name, kind, persisted })) })} as const;`);
  return lines.join("\n").trimEnd();
}

function schemaDeclaration(descriptor, mappings) {
  const name = safeSymbol(descriptor.name ?? descriptor.id);
  const documentation = documentationLines(descriptor);
  const renderer = schemaRenderers[descriptor.kind];
  if (!renderer) reject("TYPESCRIPT_SDK_GENERATOR_UNSUPPORTED_TYPE");
  return [...documentation, ...renderer(descriptor, name, mappings)];
}

const schemaRenderers = Object.freeze({
  scalar: (descriptor, name, mappings) => [`export type ${name} = ${scalarType(descriptor.id, mappings)};`],
  enum: renderEnum,
  union: renderUnion,
  list: (descriptor, name, mappings) => [`export type ${name} = readonly (${scalarType(descriptor.element, mappings)}${descriptor.elementNullable ? " | null" : ""})[];`],
  map: (descriptor, name, mappings) => [`export type ${name} = Readonly<Record<string, ${scalarType(descriptor.element, mappings)}${descriptor.elementNullable ? " | null" : ""}>>;`],
  object: renderObject,
  input: renderObject,
  "input-object": renderObject,
  oneof: renderOneOf,
});

function renderEnum(descriptor, name) {
  const members = [...(descriptor.enumMembers ?? [])].sort(compareID).map((member) => JSON.stringify(member.name ?? member.id));
  if (descriptor.open) members.push("{ readonly unknown: string }");
  return [`export type ${name} = ${members.join(" | ") || "never"};`];
}

function renderUnion(descriptor, name) {
  const members = [...(descriptor.variantMembers ?? [])].sort(compareID).map((member) => `{ readonly "$type": ${JSON.stringify(member.type)}; readonly "$value": ${safeSymbol(member.type)} }`);
  if (descriptor.open) members.push('{ readonly "$type": string; readonly "$value": unknown; readonly unknown: true }');
  return [`export type ${name} = ${members.join(" | ") || "never"};`];
}

function renderObject(descriptor, name, mappings) {
  const fields = [...(descriptor.fields ?? [])].sort(compareID).flatMap((field) => [...documentationLines(field, "  "), `  readonly ${propertyName(field.name)}${field.required ? "" : "?"}: ${scalarType(field.type, mappings)}${field.nullable ? " | null" : ""};`]);
  return [`export interface ${name} {`, ...fields, "}"];
}

function renderOneOf(descriptor, name, mappings) {
  const sourceFields = [...(descriptor.fields ?? [])].sort(compareID);
  const members = sourceFields.map((field) => `{ ${sourceFields.map((candidate) => candidate === field
    ? `readonly ${propertyName(candidate.name)}: ${scalarType(candidate.type, mappings)}${candidate.nullable ? " | null" : ""}`
    : `readonly ${propertyName(candidate.name)}?: never`).join("; ")} }`);
  return [`export type ${name} = ${members.join(" | ") || "never"};`];
}

function operationDeclaration(operation, mappings) {
  const lines = [];
  if (operation.description) lines.push("/**", ` * ${safeComment(operation.description)}`, " */");
  lines.push(`export interface ${operation.symbol}Variables {`);
  for (const variable of [...operation.variables].sort(compareName)) {
    if (!identifier.test(variable.name ?? "")) reject("TYPESCRIPT_SDK_GENERATOR_INVALID_OPERATION");
    lines.push(`  readonly ${propertyName(variable.name)}${variable.required ? "" : "?"}: ${scalarType(variable.type, mappings)}${variable.nullable ? " | null" : ""};`);
  }
  lines.push("}", "");
  const declarations = [];
  const decoder = renderResult(operation.result, `${operation.symbol}Result`, mappings, declarations);
  lines.push(...declarations);
  if (operation.result.kind === "object") lines.push(`export interface ${operation.symbol}Result {`, ...decoder.interfaceFields, "}", "");
  else lines.push(`export type ${operation.symbol}Result = ${decoder.type};`, "");
  lines.push(...decoder.functions);
  const definition = {
    name: operation.name,
    kind: operation.kind,
    persisted: operation.persisted,
    variables: [...operation.variables].sort(compareName).map(({ name, type, required, nullable }) => ({ name, type, required: Boolean(required), nullable: Boolean(nullable) })),
  };
  lines.push(`const ${lowerFirst(operation.symbol)}Definition = ${formatValue(definition)} as const satisfies OperationDefinition;`, "", `export function create${operation.symbol}(variables: ${operation.symbol}Variables, customCodecs: ScalarCodecs = {}): Operation<${operation.symbol}Variables, ${operation.symbol}Result> {`, "  const codecs = createScalarCodecs(customCodecs);", `  return createOperation(${lowerFirst(operation.symbol)}Definition, variables, ${decoder.decoder}, { codecs });`, "}");
  return lines;
}

function renderResult(node, name, mappings, declarations) {
  if (node.kind === "scalar") {
    const decoder = scalarDecoder(node.type);
    return { type: scalarType(node.type, mappings), decoder, functions: [], usesCodecs: decoder.includes("codecs") };
  }
  if (node.kind === "list" && node.element) {
    const element = renderResult(node.element, `${name}Element`, mappings, declarations);
    return { type: `readonly ${element.type}[]`, decoder: `(value) => decodeList(value, ${element.decoder})`, functions: element.functions, usesCodecs: element.usesCodecs };
  }
  if (node.kind !== "object" || !Array.isArray(node.fields)) reject("TYPESCRIPT_SDK_GENERATOR_UNSUPPORTED_RESULT");
  const nestedFunctions = [];
  const fields = [];
  const assignments = [];
  let usesCodecs = false;
  for (const field of node.fields) {
    const nestedName = field.result.kind === "object" ? `${name}${safeSymbol(field.name)}` : `${name}Value`;
    const child = renderResult(field.result, nestedName, mappings, declarations);
    if (field.result.kind === "object") declarations.push(`export interface ${nestedName} {`, ...child.interfaceFields, "}", "");
    fields.push(`  readonly ${propertyName(field.name)}: Selected<${child.type}>;`);
    assignments.push(`    ${propertyName(field.name)}: decodeSelected(fields, ${JSON.stringify(field.name)}, ${field.presence === "pending"}, ${child.decoder}),`);
    nestedFunctions.push(...child.functions);
    usesCodecs ||= child.usesCodecs;
  }
  const functionLines = [...nestedFunctions, `function decode${name}(value: unknown${usesCodecs ? ", codecs: ScalarCodecs" : ""}): ${name} {`, '  const fields = safeObject(value, "CLIENT_RESULT_INVALID");', "  return {", ...assignments, "  };", "}", ""];
  return { type: name, decoder: `(value) => decode${name}(value${usesCodecs ? ", codecs" : ""})`, interfaceFields: fields, functions: functionLines, usesCodecs };
}

function scalarType(type, mappings) {
  if (Object.hasOwn(builtInTypes, type)) return builtInTypes[type];
  if (Object.hasOwn(mappings, type)) {
    if (["lossless-decimal-string", "string"].includes(mappings[type])) return "string";
    if (mappings[type] === "bigint") return "bigint";
    if (mappings[type] === "bytes") return "Uint8Array";
    if (mappings[type] === "json-raw-message") return "unknown";
    reject("TYPESCRIPT_SDK_GENERATOR_UNMAPPED_SCALAR");
  }
  return safeSymbol(type);
}

function scalarDecoder(type) {
  if (type === "Boolean") return "decodeBoolean";
  if (type === "Int32" || type === "Float64") return "decodeNumber";
  if (["String", "ID"].includes(type)) return "decodeString";
  return `(value) => decodeScalar(codecs, ${JSON.stringify(type)}, value)`;
}

function collectDecoders(node, imports) {
  if (node.kind === "scalar") {
    if (node.type === "Boolean") imports.add("decodeBoolean");
    else if (node.type === "Int32" || node.type === "Float64") imports.add("decodeNumber");
    else if (node.type === "String" || node.type === "ID") imports.add("decodeString");
    else imports.add("decodeScalar");
  }
  else if (node.kind === "list" && node.element) {
    imports.add("decodeList");
    collectDecoders(node.element, imports);
  } else if (node.kind === "object" && Array.isArray(node.fields)) {
    for (const field of node.fields) collectDecoders(field.result, imports);
  }
}

function validateSchema(schema, mappings) {
  if (!Array.isArray(schema?.types)) reject("TYPESCRIPT_SDK_GENERATOR_INVALID_SCHEMA");
  const names = new Set();
  for (const descriptor of schema.types) {
    const name = safeSymbol(descriptor.name ?? descriptor.id);
    if (names.has(name)) reject("TYPESCRIPT_SDK_GENERATOR_SYMBOL_COLLISION");
    names.add(name);
    if (descriptor.kind === "scalar" && !["Boolean", "Int32", "Float64", "Int64", "UInt64", "BigInt", "Decimal", "Timestamp", "Duration", "UUID", "Bytes", "String", "ID"].includes(descriptor.id) && !Object.hasOwn(mappings, descriptor.id)) reject("TYPESCRIPT_SDK_GENERATOR_UNMAPPED_SCALAR");
  }
}

function normalizeDocument(document) {
  const value = structuredClone(document);
  if (Array.isArray(value.requires)) value.requires.sort();
  return value;
}

function normalizeSchema(schema) {
  const value = structuredClone(schema);
  sortStrings(value, "capabilities");
  sortByID(value, "types");
  for (const descriptor of value.types ?? []) {
    for (const member of ["variants", "enumValues", "capabilities"]) sortStrings(descriptor, member);
    for (const member of ["fields", "enumMembers", "variantMembers", "retired", "traits"]) sortByID(descriptor, member);
    if (descriptor.entity) sortStrings(descriptor.entity, "keys");
    if (descriptor.scalar) sortStrings(descriptor.scalar, "acceptedWireShapes");
  }
  for (const member of ["operations", "members", "retired", "traits", "directives", "extensions", "references"]) sortByID(value, member);
  return value;
}

function sortStrings(value, member) {
  if (Array.isArray(value?.[member])) value[member].sort(compareText);
}

function sortByID(value, member) {
  if (Array.isArray(value?.[member])) value[member].sort(compareID);
}

function semanticDigest(purpose, value) {
  return createHash("sha256").update(`naatre:${purpose}:c14n-1\n`, "utf8").update(canonicalStringify(value), "utf8").digest("hex");
}

function safeSymbol(value) {
  if (typeof value !== "string") reject("TYPESCRIPT_SDK_GENERATOR_INVALID_SYMBOL");
  const words = value.match(/[A-Za-z0-9]+/gu) ?? [];
  let result = words.map((word) => word[0].toUpperCase() + word.slice(1)).join("");
  if (!identifier.test(result)) reject("TYPESCRIPT_SDK_GENERATOR_INVALID_SYMBOL");
  if (reserved.has(result.toLowerCase())) result += "_";
  return result;
}

function propertyName(value) {
  return identifier.test(value) && !reserved.has(value) ? value : JSON.stringify(value);
}

function formatValue(value) {
  return JSON.stringify(value, null, 2).replace(/^/gmu, "");
}

function safeComment(value) {
  return String(value).replace(/\*\//gu, "* /").replace(/[\r\n]+/gu, " ");
}

function documentationLines(value, indent = "") {
  const lines = [];
  if (value.description) lines.push(safeComment(value.description));
  if (value.deprecation) lines.push(`@deprecated ${safeComment(value.deprecation.reason ?? "Deprecated")}${value.deprecation.replacement ? `; use ${safeComment(value.deprecation.replacement)}` : ""}`);
  if (lines.length === 0) return [];
  return [`${indent}/**`, ...lines.map((line) => `${indent} * ${line}`), `${indent} */`];
}

function compareName(left, right) { return compareText(left.name, right.name); }
function compareID(left, right) { return compareText(left.id, right.id); }
function compareText(left, right) { return String(left) < String(right) ? -1 : String(left) > String(right) ? 1 : 0; }
function lowerFirst(value) { return value[0].toLowerCase() + value.slice(1); }
function isRecord(value) { return value !== null && typeof value === "object" && !Array.isArray(value); }
function byteLength(value) { return new TextEncoder().encode(typeof value === "string" ? value : new TextDecoder().decode(value)).byteLength; }
function reject(code) { throw new TypeScriptGeneratorError(code); }
