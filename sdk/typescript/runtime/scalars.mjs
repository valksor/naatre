import { fail } from "./error.mjs";
import { safeObject } from "./json.mjs";

const signedMinimum = -(1n << 63n);
const signedMaximum = (1n << 63n) - 1n;
const unsignedMaximum = (1n << 64n) - 1n;
const decimal = /^-?[0-9]+(?:\.[0-9]+)?$/u;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/u;

export function encodeInt64(value) {
  return boundedInteger(value, signedMinimum, signedMaximum, "Int64");
}

export function encodeUInt64(value) {
  return boundedInteger(value, 0n, unsignedMaximum, "UInt64");
}

export function encodeBigInt(value) {
  return normalizeInteger(value, "BigInt").toString();
}

export function encodeDecimal(value) {
  if (typeof value !== "string" || !decimal.test(value)) fail("CLIENT_SCALAR_INVALID");
  const negative = value.startsWith("-");
  const unsigned = negative ? value.slice(1) : value;
  const [integer = "0", fraction = ""] = unsigned.split(".");
  const left = integer.replace(/^0+(?=[0-9])/u, "");
  const right = fraction.replace(/0+$/u, "");
  const result = right === "" ? left : `${left}.${right}`;
  return /^0(?:\.0*)?$/u.test(result) ? "0" : `${negative ? "-" : ""}${result}`;
}

export function encodeTimestamp(value) {
  const parsed = parseTimestamp(value);
  let { year, month, day, hour } = parsed;
  const { minute, second } = parsed;
  const offset = parseZoneOffset(parsed.zone);
  let utcMinutes = hour * 60 + minute - offset;
  if (utcMinutes < 0) {
    ({ year, month, day } = previousDay(year, month, day));
    utcMinutes += 1440;
  } else if (utcMinutes >= 1440) {
    ({ year, month, day } = nextDay(year, month, day));
    utcMinutes -= 1440;
  }
  if (year < 0 || year > 9999) fail("CLIENT_SCALAR_INVALID");
  hour = Math.floor(utcMinutes / 60);
  const utcMinute = utcMinutes % 60;
  const fraction = parsed.fraction.replace(/0+$/u, "");
  return `${pad(year, 4)}-${pad(month, 2)}-${pad(day, 2)}T${pad(hour, 2)}:${pad(utcMinute, 2)}:${pad(second, 2)}${fraction === "" ? "" : `.${fraction}`}Z`;
}

function parseTimestamp(value) {
  if (value instanceof Date || typeof value !== "string") fail("CLIENT_SCALAR_INVALID");
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/u.exec(value);
  if (!match) fail("CLIENT_SCALAR_INVALID");
  const parsed = { year: Number(match[1]), month: Number(match[2]), day: Number(match[3]), hour: Number(match[4]), minute: Number(match[5]), second: Number(match[6]), fraction: match[7] ?? "", zone: match[8] };
  if (parsed.month < 1 || parsed.month > 12 || parsed.day < 1 || parsed.day > daysInMonth(parsed.year, parsed.month) || parsed.hour > 23 || parsed.minute > 59 || parsed.second > 59) fail("CLIENT_SCALAR_INVALID");
  return parsed;
}

function parseZoneOffset(zone) {
  if (zone === "Z") return 0;
  const hour = Number(zone.slice(1, 3));
  const minute = Number(zone.slice(4, 6));
  if (hour > 23 || minute > 59) fail("CLIENT_SCALAR_INVALID");
  return (zone[0] === "+" ? 1 : -1) * (hour * 60 + minute);
}

export function encodeDuration(value) {
  return normalizeInteger(value).toString();
}

export function encodeUUID(value) {
  if (typeof value !== "string" || !uuid.test(value)) fail("CLIENT_SCALAR_INVALID");
  return value;
}

export function encodeBytes(value) {
  if (!(value instanceof Uint8Array)) fail("CLIENT_SCALAR_INVALID");
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  let output = "";
  for (let index = 0; index < value.length; index += 3) {
    const first = value[index] ?? 0;
    const second = value[index + 1] ?? 0;
    const third = value[index + 2] ?? 0;
    const bits = first << 16 | second << 8 | third;
    output += alphabet[(bits >> 18) & 63] + alphabet[(bits >> 12) & 63];
    if (index + 1 < value.length) output += alphabet[(bits >> 6) & 63];
    if (index + 2 < value.length) output += alphabet[bits & 63];
  }
  return output;
}

export function decodeBytes(value) {
  if (typeof value !== "string" || !/^[A-Za-z0-9_-]*$/u.test(value) || value.length % 4 === 1) fail("CLIENT_SCALAR_INVALID");
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const output = [];
  for (let index = 0; index < value.length; index += 4) {
    const chunk = value.slice(index, index + 4);
    let bits = 0;
    for (const token of chunk) bits = bits << 6 | alphabet.indexOf(token);
    bits <<= (4 - chunk.length) * 6;
    output.push((bits >> 16) & 255);
    if (chunk.length > 2) output.push((bits >> 8) & 255);
    if (chunk.length > 3) output.push(bits & 255);
  }
  return new Uint8Array(output);
}

export function createScalarCodecs(custom = {}) {
  const codecs = Object.create(null);
  Object.assign(codecs, {
    String: scalarCodec(stringValue),
    ID: scalarCodec(stringValue),
    Boolean: scalarCodec(booleanValue),
    Int32: scalarCodec(int32Value),
    Float64: scalarCodec(float64Value),
    Int64: { encode: encodeInt64, decode: (value) => BigInt(encodeInt64(value)) },
    UInt64: { encode: encodeUInt64, decode: (value) => BigInt(encodeUInt64(value)) },
    BigInt: { encode: encodeBigInt, decode: (value) => BigInt(encodeBigInt(value)) },
    Decimal: { encode: encodeDecimal, decode: encodeDecimal },
    Timestamp: { encode: encodeTimestamp, decode: encodeTimestamp },
    Duration: { encode: encodeDuration, decode: encodeDuration },
    UUID: { encode: encodeUUID, decode: encodeUUID },
    Bytes: { encode: encodeBytes, decode: decodeBytes },
    StringList: scalarCodec(stringList),
    StringMap: scalarCodec(stringMap),
  });
  for (const [name, codec] of Object.entries(custom)) {
    if (!name || typeof codec?.encode !== "function" || typeof codec?.decode !== "function") fail("CLIENT_SCALAR_CODEC_INVALID");
    codecs[name] = Object.freeze({ encode: codec.encode, decode: codec.decode });
  }
  return Object.freeze(codecs);
}

export function encodeScalar(codecs, type, value) {
  const codec = codecs?.[type];
  if (typeof codec?.encode !== "function") fail("CLIENT_SCALAR_CODEC_MISSING");
  return codec.encode(value);
}

export function decodeScalar(codecs, type, value) {
  const codec = codecs?.[type];
  if (typeof codec?.decode !== "function") fail("CLIENT_SCALAR_CODEC_MISSING");
  return codec.decode(value);
}

function boundedInteger(value, minimum, maximum) {
  const parsed = normalizeInteger(value);
  if (parsed < minimum || parsed > maximum) fail("CLIENT_SCALAR_INVALID");
  return parsed.toString();
}

function normalizeInteger(value) {
  if (typeof value !== "string" && typeof value !== "bigint") fail("CLIENT_VALUE_PRECISION");
  const text = typeof value === "bigint" ? value.toString() : value;
  if (!/^-?[0-9]+$/u.test(text)) fail("CLIENT_SCALAR_INVALID");
  return BigInt(text);
}

function scalarCodec(validate) {
  return Object.freeze({ encode: validate, decode: validate });
}

function stringValue(value) {
  if (typeof value !== "string") fail("CLIENT_SCALAR_INVALID");
  return value;
}

function booleanValue(value) {
  if (typeof value !== "boolean") fail("CLIENT_SCALAR_INVALID");
  return value;
}

function int32Value(value) {
  if (!Number.isInteger(value) || value < -2147483648 || value > 2147483647) fail("CLIENT_SCALAR_INVALID");
  return value;
}

function float64Value(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) fail("CLIENT_SCALAR_INVALID");
  return value;
}

function stringList(value) {
  if (!Array.isArray(value)) fail("CLIENT_SCALAR_INVALID");
  const result = [];
  for (let index = 0; index < value.length; index += 1) {
    if (!Object.prototype.hasOwnProperty.call(value, index)) fail("CLIENT_SCALAR_INVALID");
    result.push(stringValue(value[index]));
  }
  if (Object.keys(value).length !== value.length || Object.getOwnPropertySymbols(value).length !== 0) fail("CLIENT_SCALAR_INVALID");
  return result;
}

function stringMap(value) {
  const input = safeObject(value, "CLIENT_SCALAR_INVALID");
  const result = Object.create(null);
  for (const [key, entry] of Object.entries(input)) result[key] = stringValue(entry);
  return result;
}

function daysInMonth(year, month) {
  if (month === 2) return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28;
  return [4, 6, 9, 11].includes(month) ? 30 : 31;
}

function previousDay(year, month, day) {
  if (day > 1) return { year, month, day: day - 1 };
  if (month > 1) return { year, month: month - 1, day: daysInMonth(year, month - 1) };
  return { year: year - 1, month: 12, day: 31 };
}

function nextDay(year, month, day) {
  if (day < daysInMonth(year, month)) return { year, month, day: day + 1 };
  if (month < 12) return { year, month: month + 1, day: 1 };
  return { year: year + 1, month: 1, day: 1 };
}

function pad(value, length) {
  return String(value).padStart(length, "0");
}
