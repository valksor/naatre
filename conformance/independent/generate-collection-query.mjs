#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const generatorVersion = "collection-query-ts-1";
const defaultSource = fileURLToPath(new URL("../v1/collections.json", import.meta.url));
const sourcePath = process.argv[2] ? resolve(process.argv[2]) : defaultSource;
const sourceBytes = readFileSync(sourcePath);
const fixture = JSON.parse(sourceBytes);
const descriptor = fixture?.query?.schema;

if (fixture?.query?.profile !== "collection.query-1" || descriptor?.capability !== "collection.query-1" || !Array.isArray(descriptor.fields)) {
  throw new Error("source does not contain one collection.query-1 descriptor");
}

const knownOperators = new Set([
  "eq", "ne", "lt", "lte", "gt", "gte", "in", "not-in",
  "is-null", "is-not-null", "exists", "not-exists", "any", "all",
  "regex", "full-text", "geo-within",
]);
const membershipOperators = new Set(["in", "not-in"]);
const unaryOperators = new Set(["is-null", "is-not-null", "exists", "not-exists"]);
const listOperators = new Set(["any", "all"]);
const orderedTypes = new Set(["String", "ID", "Int32", "Float64", "Int64", "UInt64", "BigInt", "Decimal", "Timestamp", "Duration", "UUID"]);

function quotedUnion(values) {
  if (values.length === 0) return "never";
  return values.map((value) => JSON.stringify(value)).join(" | ");
}

function symbolFor(fieldID) {
  const words = fieldID.split(/[^A-Za-z0-9]+/u).filter(Boolean);
  if (words.length === 0) throw new Error("field ID cannot produce a TypeScript symbol");
  const symbol = words.map((word) => word[0].toUpperCase() + word.slice(1)).join("");
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(symbol)) throw new Error(`unsafe generated symbol for ${fieldID}`);
  return symbol;
}

function typescriptValue(typeID) {
  if (typeID === "Boolean") return "boolean";
  if (typeID === "Int32" || typeID === "Float64") return "number";
  if (["String", "ID", "Int64", "UInt64", "BigInt", "Decimal", "Timestamp", "Duration", "UUID", "Bytes"].includes(typeID)) return "string";
  throw new Error(`unsupported collection query scalar ${typeID}`);
}

function predicateVariants(field) {
  const operators = [...field.operators].sort();
  for (const operator of operators) {
    if (!knownOperators.has(operator)) throw new Error(`unknown collection query operator ${operator}`);
  }
  const prefix = `{ kind: "predicate"; field: ${JSON.stringify(field.id)};`;
  const valueType = typescriptValue(field.type);
  const variants = [];
  const scalar = operators.filter((operator) => !membershipOperators.has(operator) && !unaryOperators.has(operator) && !listOperators.has(operator));
  const membership = operators.filter((operator) => membershipOperators.has(operator));
  const unary = operators.filter((operator) => unaryOperators.has(operator));
  if (scalar.length > 0) variants.push(`${prefix} operator: ${quotedUnion(scalar)}; value: CollectionFilterValue<${JSON.stringify(field.type)}, ${valueType}> }`);
  if (membership.length > 0) variants.push(`${prefix} operator: ${quotedUnion(membership)}; value: CollectionFilterValue<${JSON.stringify(field.type)}, readonly ${valueType}[]> }`);
  if (unary.length > 0) variants.push(`${prefix} operator: ${quotedUnion(unary)}; value?: never }`);
  return variants;
}

const fields = [...descriptor.fields].sort((left, right) => left.id < right.id ? -1 : left.id > right.id ? 1 : 0);
const symbols = new Set();
const aliases = [];
const leaves = [];
const sorts = [];

for (const field of fields) {
  if (typeof field.id !== "string" || !Array.isArray(field.operators)) throw new Error("invalid collection query field descriptor");
  const symbol = symbolFor(field.id);
  if (symbols.has(symbol)) throw new Error(`generated symbol collision for ${field.id}`);
  symbols.add(symbol);
  if (field.elementType) {
    const elementValue = typescriptValue(field.elementType);
    const ordered = orderedTypes.has(field.elementType) ? "true" : "false";
    aliases.push(`export type ${symbol}ElementPredicate = CollectionElementPredicate<${JSON.stringify(field.elementType)}, ${elementValue}, ${ordered}>;`);
    const operators = field.operators.filter((operator) => listOperators.has(operator)).sort();
    if (operators.length !== field.operators.length || operators.length === 0) throw new Error(`list field ${field.id} has incompatible operators`);
    leaves.push(`{ kind: ${quotedUnion(operators)}; field: ${JSON.stringify(field.id)}; predicate: ${symbol}ElementPredicate }`);
  } else {
    leaves.push(...predicateVariants(field));
  }
  if (field.sort) {
    sorts.push(`{ field: ${JSON.stringify(field.id)}; direction: ${quotedUnion([...field.sort.directions].sort())}; nulls: ${quotedUnion([...field.sort.nulls].sort())} }`);
  }
}

const sourceSha256 = createHash("sha256").update(sourceBytes).digest("hex");
const metadata = JSON.stringify({
  generatorVersion,
  sourceProfile: fixture.query.profile,
  sourceSha256,
  fields: fields.map((field) => ({ id: field.id, type: field.type, elementType: field.elementType ?? null, operators: [...field.operators].sort(), sortable: Boolean(field.sort) })),
}, null, 2);

const output = `// Code generated by conformance/independent/generate-collection-query.mjs; DO NOT EDIT.
// Source profile: collection.query-1
// Source SHA-256: ${sourceSha256}

export const collectionQueryGeneration = ${metadata} as const;

export type CollectionFilterValue<TType extends string, TValue> =
  | { type: TType; literal: TValue; variable?: never }
  | { type: TType; variable: string; literal?: never };

export type CollectionElementPredicate<TType extends string, TValue, TOrdered extends boolean> =
  | { kind: "predicate"; field: "@"; operator: "eq" | "ne"; value: CollectionFilterValue<TType, TValue> }
  | { kind: "predicate"; field: "@"; operator: "in" | "not-in"; value: CollectionFilterValue<TType, readonly TValue[]> }
  | (TOrdered extends true
      ? { kind: "predicate"; field: "@"; operator: "lt" | "lte" | "gt" | "gte"; value: CollectionFilterValue<TType, TValue> }
      : never);

${aliases.join("\n")}

export type CollectionLeafFilter =
  | ${leaves.join("\n  | ")};

export type CollectionFilter =
  | CollectionLeafFilter
  | { kind: "and" | "or"; children: readonly CollectionFilter[] }
  | { kind: "not"; child: CollectionFilter };

export type CollectionSortTerm =
  | ${sorts.join("\n  | ")};
`;

process.stdout.write(output);
