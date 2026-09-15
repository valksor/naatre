import { NaatreClientError, fail } from "./error.mjs";

const encoder = new TextEncoder();

export function canonicalStringify(value) {
  return writeCanonical(value, new Set());
}

export function parseJSON(input, maximumBytes = 16 << 20) {
  const text = typeof input === "string" ? input : new TextDecoder("utf-8", { fatal: true }).decode(input);
  if (encoder.encode(text).byteLength > maximumBytes) fail("CLIENT_RESPONSE_LIMIT");
  try {
    return new Parser(text).parse();
  } catch (error) {
    if (error instanceof NaatreClientError) throw error;
    fail("CLIENT_PROTOCOL_INVALID", error);
  }
}

export function safeObject(value, code = "CLIENT_PROTOCOL_INVALID") {
  if (!isRecord(value)) fail(code);
  const result = Object.create(null);
  if (Object.getOwnPropertySymbols(value).length !== 0) fail(code);
  const descriptors = Object.getOwnPropertyDescriptors(value);
  for (const key of Object.keys(descriptors)) {
    const descriptor = descriptors[key];
    if (!descriptor?.enumerable || !("value" in descriptor)) fail(code);
    result[key] = safeValue(descriptor.value, code);
  }
  return result;
}

export function hasOwn(value, key) {
  return Object.prototype.hasOwnProperty.call(value, key);
}

export function ownDataArray(value, invalid) {
  if (!Array.isArray(value) || Object.getOwnPropertySymbols(value).length !== 0 || Object.keys(value).length !== value.length) invalid("must be a dense array");
  const result = [];
  for (let index = 0; index < value.length; index += 1) {
    const descriptor = Object.getOwnPropertyDescriptor(value, String(index));
    if (!descriptor?.enumerable || !("value" in descriptor)) invalid("contains an accessor");
    result.push(descriptor.value);
  }
  return result;
}

function safeValue(value, code) {
  if (Array.isArray(value)) return safeArray(value, code);
  if (isRecord(value)) return safeObject(value, code);
  if (value === null || typeof value === "string" || typeof value === "boolean") return value;
  if (typeof value === "number" && Number.isFinite(value) && (!Number.isInteger(value) || Number.isSafeInteger(value))) return Object.is(value, -0) ? 0 : value;
  fail(code);
}

function safeArray(value, code) {
  return ownDataArray(value, () => fail(code)).map((entry) => safeValue(entry, code));
}

function writeCanonical(value, ancestors) {
  if (value === null) return "null";
  const primitiveWriter = primitiveWriters[typeof value];
  if (primitiveWriter) return primitiveWriter(value);
  return writeCanonicalComposite(value, ancestors);
}

const primitiveWriters = Object.freeze({
  string: (value) => {
    if (hasUnpairedSurrogate(value)) fail("CLIENT_VALUE_INVALID");
    return JSON.stringify(value);
  },
  boolean: (value) => value ? "true" : "false",
  number: (value) => {
    if (!Number.isFinite(value) || Number.isInteger(value) && !Number.isSafeInteger(value)) fail("CLIENT_VALUE_PRECISION");
    return Object.is(value, -0) ? "0" : JSON.stringify(value);
  },
  bigint: () => fail("CLIENT_VALUE_PRECISION"),
});

function writeCanonicalComposite(value, ancestors) {
  if (typeof value !== "object" || value === undefined) fail("CLIENT_VALUE_INVALID");
  if (ancestors.has(value)) fail("CLIENT_VALUE_INVALID");
  ancestors.add(value);
  try {
    return Array.isArray(value) ? writeCanonicalArray(value, ancestors) : writeCanonicalObject(value, ancestors);
  } finally {
    ancestors.delete(value);
  }
}

function writeCanonicalArray(value, ancestors) {
  const encoded = [];
  for (let index = 0; index < value.length; index += 1) {
    if (!hasOwn(value, index) || value[index] === undefined) fail("CLIENT_VALUE_INVALID");
    encoded.push(writeCanonical(value[index], ancestors));
  }
  return `[${encoded.join(",")}]`;
}

function writeCanonicalObject(value, ancestors) {
  if (!isRecord(value) || Object.getOwnPropertySymbols(value).length !== 0) fail("CLIENT_VALUE_INVALID");
  const descriptors = Object.getOwnPropertyDescriptors(value);
  const members = [];
  for (const key of Object.keys(descriptors).sort()) {
    const descriptor = descriptors[key];
    if (!descriptor?.enumerable || !("value" in descriptor) || descriptor.value === undefined) fail("CLIENT_VALUE_INVALID");
    members.push(`${JSON.stringify(key)}:${writeCanonical(descriptor.value, ancestors)}`);
  }
  return `{${members.join(",")}}`;
}

function isRecord(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function hasUnpairedSurrogate(value) {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(index + 1);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return true;
      index += 1;
    } else if (code >= 0xdc00 && code <= 0xdfff) return true;
  }
  return false;
}

class Parser {
  constructor(input) {
    this.input = input;
    this.index = 0;
  }

  parse() {
    this.space();
    const value = this.value();
    this.space();
    if (this.index !== this.input.length) fail("CLIENT_PROTOCOL_INVALID");
    return value;
  }

  value() {
    this.space();
    const token = this.input[this.index];
    if (token === "{") return this.object();
    if (token === "[") return this.array();
    if (token === '"') return this.string();
    if (this.input.startsWith("true", this.index)) return this.keyword("true", true);
    if (this.input.startsWith("false", this.index)) return this.keyword("false", false);
    if (this.input.startsWith("null", this.index)) return this.keyword("null", null);
    return this.number();
  }

  object() {
    this.index += 1;
    const result = Object.create(null);
    const keys = new Set();
    this.space();
    if (this.take("}")) return result;
    for (;;) {
      if (this.input[this.index] !== '"') fail("CLIENT_PROTOCOL_INVALID");
      const key = this.string();
      if (keys.has(key)) fail("CLIENT_PROTOCOL_INVALID");
      keys.add(key);
      this.space();
      if (!this.take(":")) fail("CLIENT_PROTOCOL_INVALID");
      result[key] = this.value();
      this.space();
      if (this.take("}")) return result;
      if (!this.take(",")) fail("CLIENT_PROTOCOL_INVALID");
      this.space();
    }
  }

  array() {
    this.index += 1;
    const result = [];
    this.space();
    if (this.take("]")) return result;
    for (;;) {
      result.push(this.value());
      this.space();
      if (this.take("]")) return result;
      if (!this.take(",")) fail("CLIENT_PROTOCOL_INVALID");
      this.space();
    }
  }

  string() {
    const start = this.index;
    this.index += 1;
    for (;;) {
      if (this.index >= this.input.length) fail("CLIENT_PROTOCOL_INVALID");
      const token = this.input[this.index];
      if (token === '"') {
        this.index += 1;
        const value = JSON.parse(this.input.slice(start, this.index));
        if (hasUnpairedSurrogate(value)) fail("CLIENT_PROTOCOL_INVALID");
        return value;
      }
      if (token === "\\") {
        this.index += 2;
        continue;
      }
      if (this.input.charCodeAt(this.index) < 0x20) fail("CLIENT_PROTOCOL_INVALID");
      this.index += 1;
    }
  }

  number() {
    const match = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/u.exec(this.input.slice(this.index));
    if (!match) fail("CLIENT_PROTOCOL_INVALID");
    this.index += match[0].length;
    const value = Number(match[0]);
    if (!Number.isFinite(value) || Number.isInteger(value) && !Number.isSafeInteger(value)) fail("CLIENT_VALUE_PRECISION");
    return Object.is(value, -0) ? 0 : value;
  }

  keyword(keyword, value) {
    this.index += keyword.length;
    return value;
  }

  space() {
    while (/[ \t\r\n]/u.test(this.input[this.index] ?? "")) this.index += 1;
  }

  take(expected) {
    if (this.input[this.index] !== expected) return false;
    this.index += 1;
    return true;
  }
}
