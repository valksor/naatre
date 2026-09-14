import { fail } from "./error.mjs";
import { canonicalStringify, hasOwn, parseJSON, safeObject } from "./json.mjs";
import { decodeOperationResult } from "./result.mjs";
import { createScalarCodecs, encodeScalar } from "./scalars.mjs";

const operationNames = /^(?:[A-Za-z_][A-Za-z0-9_]{0,127})$/u;
const digest = /^[0-9a-f]{64}$/u;

export function createOperation(definition, variables, decodeData, options = {}) {
  definition = validateDefinition(definition);
  if (!isRecord(variables) || Object.getOwnPropertySymbols(variables).length !== 0) fail("CLIENT_VARIABLES_INVALID");
  const codecs = options.codecs ?? createScalarCodecs();
  const encodedVariables = encodeVariables(definition.variables, variables, codecs);
  const request = Object.freeze({
    version: "1",
    operation: definition.name,
    persisted: definition.persisted,
    variables: Object.freeze(encodedVariables),
  });
  const canonical = canonicalStringify(request);
  return Object.freeze({
    kind: definition.kind,
    request,
    canonicalRequest: () => canonical,
    decodeResult: (input) => decodeOperationResult(input, decodeData),
  });
}

function encodeVariables(variableDefinitions, variables, codecs) {
  const allowed = new Map(variableDefinitions.map((entry) => [entry.name, entry]));
  const result = Object.create(null);
  const descriptors = Object.getOwnPropertyDescriptors(variables);
  for (const key of Object.keys(descriptors)) {
    const descriptor = descriptors[key];
    const entry = allowed.get(key);
    if (!descriptor?.enumerable || !("value" in descriptor) || !entry || descriptor.value === undefined) fail("CLIENT_VARIABLES_INVALID");
    result[key] = encodeVariable(entry, descriptor.value, codecs);
  }
  for (const entry of allowed.values()) {
    if (entry.required && !hasOwn(result, entry.name)) fail("CLIENT_VARIABLES_INVALID");
  }
  return result;
}

function encodeVariable(definition, value, codecs) {
  if (value !== null) return encodeScalar(codecs, definition.type, value);
  if (!definition.nullable) fail("CLIENT_VARIABLES_INVALID");
  return null;
}

export function loadManifest(input) {
  const manifest = typeof input === "string" ? parseJSON(input, 1 << 20) : safeObject(input, "CLIENT_MANIFEST_INVALID");
  if (!hasExactKeys(manifest, ["canonicalVersion", "operations", "profile", "protocolVersion", "version"])) fail("CLIENT_MANIFEST_INVALID");
  if (manifest?.profile !== "sdk.typescript.core-1" || manifest.version !== "1" || manifest.protocolVersion !== "1" || manifest.canonicalVersion !== "c14n-1" || !Array.isArray(manifest.operations) || manifest.operations.length === 0) fail("CLIENT_MANIFEST_INVALID");
  const operations = Object.create(null);
  for (const operation of manifest.operations) {
    if (!isRecord(operation) || !hasExactKeys(operation, ["kind", "name", "persisted"])) fail("CLIENT_MANIFEST_INVALID");
    const validated = validateDefinition({ ...operation, variables: [] });
    if (hasOwn(operations, operation.name)) fail("CLIENT_MANIFEST_INVALID");
    operations[operation.name] = Object.freeze({ name: validated.name, kind: validated.kind, persisted: validated.persisted });
  }
  return Object.freeze(operations);
}

function validateDefinition(definition) {
  const value = safeObject(definition, "CLIENT_OPERATION_INVALID");
  if (!hasExactKeys(value, ["kind", "name", "persisted", "variables"]) || !isRecord(value.persisted) || !hasExactKeys(value.persisted, ["algorithm", "canonicalVersion", "digest"]) || !operationNames.test(value.name ?? "") || !["query", "mutation", "subscription"].includes(value.kind) || value.persisted.algorithm !== "sha-256" || value.persisted.canonicalVersion !== "c14n-1" || !digest.test(value.persisted.digest ?? "") || !Array.isArray(value.variables)) fail("CLIENT_OPERATION_INVALID");
  const names = new Set();
  for (const variable of value.variables) {
    if (!isRecord(variable) || !hasExactKeys(variable, ["name", "nullable", "required", "type"]) || !operationNames.test(variable?.name ?? "") || !operationNames.test(variable?.type ?? "") || typeof variable.required !== "boolean" || typeof variable.nullable !== "boolean" || names.has(variable.name)) fail("CLIENT_OPERATION_INVALID");
    names.add(variable.name);
  }
  return Object.freeze({
    name: value.name,
    kind: value.kind,
    persisted: Object.freeze({ algorithm: value.persisted.algorithm, canonicalVersion: value.persisted.canonicalVersion, digest: value.persisted.digest }),
    variables: Object.freeze(value.variables.map((variable) => Object.freeze({ name: variable.name, type: variable.type, required: variable.required, nullable: variable.nullable }))),
  });
}

function isRecord(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function hasExactKeys(value, expected) {
  const keys = Object.keys(value).sort();
  return keys.length === expected.length && keys.every((key, index) => key === expected[index]);
}
