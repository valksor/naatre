import { readFileSync } from "node:fs";

const fixture = JSON.parse(
  readFileSync(new URL("../v1/scalars.json", import.meta.url), "utf8"),
);

if (fixture.profile !== "core.scalar.c14n-1" || !Array.isArray(fixture.vectors)) {
  throw new Error("invalid scalar conformance fixture");
}

for (const vector of fixture.vectors) {
  if (
    typeof vector.name !== "string" ||
    typeof vector.kind !== "string" ||
    typeof vector.input !== "string" ||
    typeof vector.valid !== "boolean"
  ) {
    throw new Error("incomplete scalar vector");
  }
  let canonical;
  let failure;
  try {
    canonical = canonicalScalar(vector.kind, vector.input);
  } catch (error) {
    failure = error;
  }
  if (!vector.valid) {
    if (!failure) {
      throw new Error(`${vector.name}: accepted invalid scalar`);
    }
    continue;
  }
  if (failure || canonical !== vector.canonical) {
    throw new Error(
      `${vector.name}: canonical ${canonical}, expected ${vector.canonical}: ${failure ?? ""}`,
    );
  }
}

function canonicalScalar(kind, raw) {
  switch (kind) {
    case "Boolean": {
      const value = JSON.parse(raw);
      require(typeof value === "boolean");
      return value ? "true" : "false";
    }
    case "String":
    case "ID":
      return JSON.stringify(parseUnicodeString(raw));
    case "Int32": {
      require(/^-?(?:0|[1-9][0-9]*)$/.test(raw) && raw !== "-0");
      const value = Number(raw);
      require(Number.isInteger(value) && value >= -2147483648 && value <= 2147483647);
      return String(value);
    }
    case "Float64": {
      require(/^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$/.test(raw));
      const value = Number(raw);
      require(Number.isFinite(value));
      return value === 0 ? "0" : JSON.stringify(value);
    }
    case "Int64":
      return canonicalIntegerString(raw, -(2n ** 63n), 2n ** 63n - 1n, true);
    case "UInt64":
      return canonicalIntegerString(raw, 0n, 2n ** 64n - 1n, false);
    case "BigInt":
      return canonicalIntegerString(raw, undefined, undefined, true);
    case "Decimal":
      return canonicalDecimal(raw);
    case "Timestamp":
      return canonicalTimestamp(raw);
    case "Duration":
      return canonicalIntegerString(raw, -(2n ** 63n), 2n ** 63n - 1n, true);
    case "UUID": {
      const value = parseUnicodeString(raw);
      require(/^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$/.test(value));
      return JSON.stringify(value.toLowerCase());
    }
    case "Bytes": {
      const value = parseUnicodeString(raw);
      require(/^[A-Za-z0-9_-]*$/.test(value) && value.length % 4 !== 1);
      const canonical = Buffer.from(value, "base64url").toString("base64url");
      require(canonical === value);
      return JSON.stringify(canonical);
    }
    default:
      throw new Error(`unknown scalar ${kind}`);
  }
}

function canonicalIntegerString(raw, minimum, maximum, signed) {
  const value = parseUnicodeString(raw);
  require((signed ? /^-?[0-9]+$/ : /^[0-9]+$/).test(value));
  const integer = BigInt(value);
  require(minimum === undefined || integer >= minimum);
  require(maximum === undefined || integer <= maximum);
  return JSON.stringify(integer.toString());
}

function canonicalDecimal(raw) {
  let value = parseUnicodeString(raw);
  require(/^-?[0-9]+(?:\.[0-9]+)?$/.test(value));
  const negative = value.startsWith("-");
  value = value.replace(/^-/, "");
  let [integer, fraction = ""] = value.split(".");
  integer = integer.replace(/^0+/, "") || "0";
  fraction = fraction.replace(/0+$/, "");
  let canonical = fraction ? `${integer}.${fraction}` : integer;
  if (negative && canonical !== "0") canonical = `-${canonical}`;
  return JSON.stringify(canonical);
}

function canonicalTimestamp(raw) {
  const value = parseUnicodeString(raw);
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  require(match !== null);
  const [, year, month, day, hour, minute, second, rawFraction = "", zone] = match;
  require(Number(second) <= 59);
  // Date.UTC treats years 0..99 as 1900..1999. Set the full year explicitly so
  // the verifier covers RFC 3339's complete four-digit year range.
  const localDate = new Date(0);
  localDate.setUTCHours(Number(hour), Number(minute), Number(second), 0);
  localDate.setUTCFullYear(Number(year), Number(month) - 1, Number(day));
  require(
    localDate.getUTCFullYear() === Number(year) &&
      localDate.getUTCMonth() === Number(month) - 1 &&
      localDate.getUTCDate() === Number(day) &&
      localDate.getUTCHours() === Number(hour) &&
      localDate.getUTCMinutes() === Number(minute) &&
      localDate.getUTCSeconds() === Number(second),
  );
  const local = localDate.getTime();
  let offsetMinutes = 0;
  if (zone !== "Z") {
    const sign = zone[0] === "+" ? 1 : -1;
    const zoneHour = Number(zone.slice(1, 3));
    const zoneMinute = Number(zone.slice(4, 6));
    require(zoneHour <= 23 && zoneMinute <= 59);
    offsetMinutes = sign * (zoneHour * 60 + zoneMinute);
  }
  const utc = new Date(local - offsetMinutes * 60000);
  require(utc.getUTCFullYear() >= 0 && utc.getUTCFullYear() <= 9999);
  const prefix = utc.toISOString().slice(0, 19);
  const fraction = rawFraction.replace(/0+$/, "");
  return JSON.stringify(`${prefix}${fraction ? `.${fraction}` : ""}Z`);
}

function parseUnicodeString(raw) {
  const value = JSON.parse(raw);
  require(typeof value === "string" && hasOnlyUnicodeScalars(value));
  return value;
}

function hasOnlyUnicodeScalars(value) {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index);
    if (unit >= 0xdc00 && unit <= 0xdfff) return false;
    if (unit < 0xd800 || unit > 0xdbff) continue;
    index += 1;
    if (index >= value.length) return false;
    const next = value.charCodeAt(index);
    if (next < 0xdc00 || next > 0xdfff) return false;
  }
  return true;
}

function require(condition) {
  if (!condition) throw new Error("invalid scalar");
}
