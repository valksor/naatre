#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { hasUnpairedSurrogateValue } from "./unicode.mjs";

const fixture = JSON.parse(readFileSync(fileURLToPath(new URL("../v1/validation.json", import.meta.url)), "utf8"));
const maxViolations = 32;

function require(condition, message) {
  if (!condition) throw new Error(message);
}

function validateVector(vector) {
  const value = JSON.parse(vector.inputJSON);
  let violations;
  if (hasUnpairedSurrogateValue(value)) {
    violations = [{ id: "unicode", code: "INVALID_UNICODE", path: "" }];
  } else {
    violations = validate(value, vector.constraints);
  }
  require(JSON.stringify(violations) === JSON.stringify(vector.violations), `validation vector failed: ${vector.name}`);
}

function validate(value, constraints) {
  if (value === null) return [];
  const violations = [];
  const add = (id, code, path = "") => {
    if (violations.length < maxViolations) violations.push({ id, code, path });
  };
  validateNumber(value, constraints, add);
  validateString(value, constraints, add);
  validateArray(value, constraints, add);
  validateObject(value, constraints, add);
  return violations.sort((left, right) => left.path.localeCompare(right.path) || left.id.localeCompare(right.id));
}

function validateNumber(value, constraints, add) {
  if (constraints.minimum === undefined && constraints.maximum === undefined && constraints.precision === undefined && constraints.scale === undefined) return;
  const text = typeof value === "string" ? value : String(value);
  const numeric = exactDecimal(text);
  if (!numeric) return add("numeric", "CONSTRAINT_TYPE");
  validateNumberBounds(numeric, constraints, add);
  validateDecimalDimensions(text, constraints, add);
}

function validateNumberBounds(numeric, constraints, add) {
  if (constraints.minimum !== undefined) {
    const comparison = compareDecimal(numeric, exactDecimal(String(constraints.minimum)));
    if (comparison < 0 || (comparison === 0 && constraints.exclusiveMinimum)) add("minimum", "CONSTRAINT_MINIMUM");
  }
  if (constraints.maximum !== undefined) {
    const comparison = compareDecimal(numeric, exactDecimal(String(constraints.maximum)));
    if (comparison > 0 || (comparison === 0 && constraints.exclusiveMaximum)) add("maximum", "CONSTRAINT_MAXIMUM");
  }
}

function validateDecimalDimensions(text, constraints, add) {
  const shape = decimalShape(text);
  if (constraints.precision !== undefined && (!shape || shape.precision > constraints.precision)) add("precision", "CONSTRAINT_PRECISION");
  if (constraints.scale !== undefined && (!shape || shape.scale > constraints.scale)) add("scale", "CONSTRAINT_SCALE");
}

function validateString(value, constraints, add) {
  if (constraints.minLength === undefined && constraints.maxLength === undefined && !constraints.pattern && !constraints.format) return;
  if (typeof value !== "string") return add("string", "CONSTRAINT_TYPE");
  const length = constraints.lengthUnit === "bytes" ? Buffer.byteLength(value, "utf8") : Array.from(value).length;
  if (constraints.minLength !== undefined && length < constraints.minLength) add("minLength", "CONSTRAINT_MIN_LENGTH");
  if (constraints.maxLength !== undefined && length > constraints.maxLength) add("maxLength", "CONSTRAINT_MAX_LENGTH");
  if (constraints.pattern && !portablePattern(constraints.pattern, constraints.patternMode).test(value)) add("pattern", "CONSTRAINT_PATTERN");
  if (constraints.format?.assertion && !validFormat(constraints.format.id, value)) add("format", "CONSTRAINT_FORMAT");
}

function validateArray(value, constraints, add) {
  if (constraints.minItems === undefined && constraints.maxItems === undefined && !constraints.uniqueItems) return;
  if (!Array.isArray(value)) return add("items", "CONSTRAINT_TYPE");
  if (constraints.minItems !== undefined && value.length < constraints.minItems) add("minItems", "CONSTRAINT_MIN_ITEMS");
  if (constraints.maxItems !== undefined && value.length > constraints.maxItems) add("maxItems", "CONSTRAINT_MAX_ITEMS");
  if (constraints.uniqueItems) {
    const seen = new Set();
    for (let index = 0; index < value.length; index += 1) {
      const key = canonical(value[index]);
      if (seen.has(key)) {
        add("uniqueItems", "CONSTRAINT_UNIQUE_ITEMS", `/${index}`);
        break;
      }
      seen.add(key);
    }
  }
}

function validateObject(value, constraints, add) {
  if (constraints.minProperties === undefined && constraints.maxProperties === undefined && !constraints.keyPattern && !constraints.rules?.length) return;
  if (value === null || Array.isArray(value) || typeof value !== "object") return add("properties", "CONSTRAINT_TYPE");
  const keys = Object.keys(value).sort();
  validateObjectCardinality(keys.length, constraints, add);
  validateObjectKeys(keys, constraints, add);
  validateObjectRules(value, constraints, add);
}

function validateObjectCardinality(count, constraints, add) {
  if (constraints.minProperties !== undefined && count < constraints.minProperties) add("minProperties", "CONSTRAINT_MIN_PROPERTIES");
  if (constraints.maxProperties !== undefined && count > constraints.maxProperties) add("maxProperties", "CONSTRAINT_MAX_PROPERTIES");
}

function validateObjectKeys(keys, constraints, add) {
  if (constraints.keyPattern) {
    const pattern = portablePattern(constraints.keyPattern, constraints.patternMode);
    for (const key of keys) if (!pattern.test(key)) add("keyPattern", "CONSTRAINT_KEY_PATTERN", `/${escapePointer(key)}`);
  }
}

function validateObjectRules(value, constraints, add) {
  for (const rule of constraints.rules ?? []) if (!evaluateRule(rule.assert, value)) add(rule.id, "CONSTRAINT_RULE");
}

function evaluateRule(expression, value) {
  const present = Object.prototype.hasOwnProperty.call(value, expression.field);
  switch (expression.operator) {
    case "present": return present;
    case "absent": return !present;
    case "eq": return present && canonical(value[expression.field]) === canonical(expression.value);
    case "ne": return !present || canonical(value[expression.field]) !== canonical(expression.value);
    case "and": return expression.children.every((child) => evaluateRule(child, value));
    case "or": return expression.children.some((child) => evaluateRule(child, value));
    case "not": return !evaluateRule(expression.children[0], value);
    default: return false;
  }
}

function portablePattern(source, mode = "full") {
  if (/\(\?(?:[=!]|<[=!])|\\[1-9]|\\k</u.test(source)) throw new Error("pattern is outside the RE2-compatible subset");
  return new RegExp(mode === "search" ? source : `^(?:${source})$`, "u");
}

function exactDecimal(text) {
  const match = /^([+-]?)([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$/u.exec(text);
  if (!match) return null;
  const exponent = Number(match[4] ?? 0);
  if (!Number.isSafeInteger(exponent) || Math.abs(exponent) > 10000) return null;
  let numerator = BigInt(match[2] + (match[3] ?? ""));
  if (match[1] === "-") numerator = -numerator;
  const scale = (match[3]?.length ?? 0) - exponent;
  if (scale <= 0) return { numerator: numerator * 10n ** BigInt(-scale), denominator: 1n };
  return { numerator, denominator: 10n ** BigInt(scale) };
}

function compareDecimal(left, right) {
  const first = left.numerator * right.denominator;
  const second = right.numerator * left.denominator;
  return first < second ? -1 : first > second ? 1 : 0;
}

function decimalShape(text) {
  const match = /^[+-]?([0-9]+)(?:\.([0-9]+))?$/u.exec(text);
  if (!match) return null;
  const digits = (match[1] + (match[2] ?? "")).replace(/^0+/u, "") || "0";
  return { precision: digits.length, scale: match[2]?.length ?? 0 };
}

function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value !== null && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function validFormat(format, value) {
  if (format === "uuid") return /^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$/u.test(value);
  if (format === "timestamp") return validTimestamp(value);
  if (format === "uri") {
    try { return Boolean(new URL(value).host); } catch { return false; }
  }
  if (format === "email") return /^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$/u.test(value);
  return false;
}

function validTimestamp(value) {
  const match = /^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-5][0-9])(?:\.[0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$/u.exec(value);
  if (!match) return false;
  const [year, month, day, hour, minute] = match.slice(1, 6).map(Number);
  if (month < 1 || month > 12 || hour > 23 || minute > 59) return false;
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return day >= 1 && day <= days[month - 1];
}

function validateJSONSchema(document) {
  const allowed = new Set(["$schema", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "pattern", "format", "minItems", "maxItems", "uniqueItems", "minProperties", "maxProperties", "propertyNames"]);
  for (const keyword of Object.keys(document).sort()) {
    if (keyword === "$ref") return { code: "JSON_SCHEMA_REF_BLOCKED", pointer: "/$ref" };
    if (!allowed.has(keyword)) return { code: "JSON_SCHEMA_UNSUPPORTED_KEYWORD", pointer: `/${escapePointer(keyword)}` };
  }
  if (document.$schema && document.$schema !== fixture.jsonSchema.dialect) return { code: "JSON_SCHEMA_DIALECT", pointer: "/$schema" };
  if (document.pattern) {
    try { portablePattern(document.pattern, "search"); } catch { return { code: "JSON_SCHEMA_UNSUPPORTED_PATTERN", pointer: "/pattern" }; }
  }
  return null;
}

function escapePointer(value) {
  return value.replaceAll("~", "~0").replaceAll("/", "~1");
}

try {
  require(fixture.profile === "core.validation-1" && fixture.trait === "naatre.constraints-1", "validation profile metadata mismatch");
  require(fixture.limits.maxViolations === maxViolations, "validation limits mismatch");
  require(fixture.agreement.server === "authoritative-before-handler" && fixture.output.failureCode === "OUTPUT_COMPLETION", "validation authority metadata mismatch");
  for (const vector of fixture.cases) validateVector(vector);
  for (const pattern of fixture.rejectedPatterns) {
    let rejected = false;
    try { portablePattern(pattern); } catch { rejected = true; }
    require(rejected, "unsupported pattern was accepted");
  }
  require(validateJSONSchema(fixture.jsonSchema.exact) === null, "exact JSON Schema subset rejected");
  for (const vector of fixture.jsonSchema.unsupported) {
    const diagnostic = validateJSONSchema(vector.schema);
    require(diagnostic?.code === vector.code && diagnostic.pointer === vector.pointer, "JSON Schema rejection mismatch");
  }
  require(fixture.unsupportedFeatures.some((feature) => feature.feature === "cel" && feature.code === "CONSTRAINT_FEATURE_UNSUPPORTED" && feature.pointer === "/cel"), "CEL rejection metadata mismatch");
} catch (error) {
  process.stderr.write(`portable validation profile failed: ${error.message}\n`);
  process.exitCode = 1;
}
