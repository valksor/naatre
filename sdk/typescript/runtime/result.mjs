import { fail } from "./error.mjs";
import { hasOwn, parseJSON, safeObject } from "./json.mjs";

export const missing = Object.freeze({ state: "missing" });
export const nullValue = Object.freeze({ state: "null" });
export const pending = Object.freeze({ state: "pending" });

export function present(value) {
  return Object.freeze({ state: "present", value });
}

export function failed(errors) {
  return Object.freeze({ state: "failed", errors: Object.freeze([...errors]) });
}

export function skipped(reason) {
  return Object.freeze({ state: "skipped", reason });
}

export function decodeSelected(owner, key, pendingWhenMissing, decode) {
  if (!hasOwn(owner, key)) return pendingWhenMissing ? pending : missing;
  if (owner[key] === null) return nullValue;
  if (typeof decode !== "function") fail("CLIENT_RESULT_INVALID");
  return present(decode(owner[key]));
}

export function decodeString(value) {
  if (typeof value !== "string") fail("CLIENT_RESULT_INVALID");
  return value;
}

export function decodeNumber(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) fail("CLIENT_RESULT_INVALID");
  return value;
}

export function decodeBoolean(value) {
  if (typeof value !== "boolean") fail("CLIENT_RESULT_INVALID");
  return value;
}

export function decodeList(value, decode) {
  if (!Array.isArray(value)) fail("CLIENT_RESULT_INVALID");
  return Object.freeze(value.map(decode));
}

export function decodeOperationResult(input, decodeData) {
  const envelope = typeof input === "string" || input instanceof Uint8Array ? parseJSON(input) : safeObject(input);
  for (const key of Object.keys(envelope)) {
    if (!["data", "errors", "complete"].includes(key)) fail("CLIENT_PROTOCOL_INVALID");
  }
  if (!hasOwn(envelope, "complete") || typeof envelope.complete !== "boolean") fail("CLIENT_PROTOCOL_INVALID");
  const errors = hasOwn(envelope, "errors") ? envelope.errors : [];
  if (!Array.isArray(errors)) fail("CLIENT_PROTOCOL_INVALID");
  return Object.freeze({
    data: hasOwn(envelope, "data") && envelope.data !== null ? decodeData(envelope.data) : envelope.data ?? null,
    errors: Object.freeze(errors.map((entry) => safeObject(entry))),
    complete: envelope.complete,
  });
}
