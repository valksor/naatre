import { NaatreClientError } from "./error.mjs";
import { decodeSSEStream } from "./stream.mjs";

const requestMediaType = "application/vnd.naatre.request+json;version=1";
const responseMediaType = "application/vnd.naatre.response+json";
const sensitiveHeaders = new Set(["authorization", "cookie", "proxy-authorization", "x-csrf-token", "naatre-tenant", "naatre-principal"]);
const redirectStatuses = new Set([307, 308]);
const rejectedRedirectStatuses = new Set([301, 302, 303]);
const credentialModes = new Set(["omit", "same-origin"]);

export function createFetchAdapter(configuration) {
  const endpoint = parseEndpoint(configuration?.endpoint);
  const fetchImplementation = configuration?.fetch ?? globalThis.fetch;
  if (typeof fetchImplementation !== "function") transportFail("CLIENT_UNSUPPORTED");
  const limits = Object.freeze({
    compressedBytes: positiveLimit(configuration?.compressedBytes, 8 << 20),
    decompressedBytes: positiveLimit(configuration?.decompressedBytes, 16 << 20),
    frameBytes: positiveLimit(configuration?.frameBytes, 1 << 20),
    redirects: nonNegativeLimit(configuration?.redirects, 5),
  });
  const credentials = configuration?.credentials ?? "omit";
  if (!credentialModes.has(credentials)) transportFail("CLIENT_CONFIG_INVALID");
  const policy = Object.freeze({
    redirectOrigins: originSet(configuration?.redirectOrigins),
    credentialOrigins: originSet(configuration?.credentialOrigins),
    credentials,
  });
  const authenticate = configuration?.authenticate;
  if (authenticate !== undefined && typeof authenticate !== "function") transportFail("CLIENT_CONFIG_INVALID");
  return Object.freeze({
    execute: (operation, options = {}) => executeUnary({ endpoint, fetchImplementation, limits, policy, authenticate }, operation, options),
    stream: (operation, options = {}) => executeSSE({ endpoint, fetchImplementation, limits, policy, authenticate }, operation, options),
  });
}

async function executeUnary(configuration, operation, options) {
  const response = await fetchOperation(configuration, operation, options.signal, responseMediaTypeWithVersion());
  try {
    const mediaType = parseMediaType(response.headers.get("content-type"));
    if (mediaType.name !== responseMediaType || mediaType.parameters.version !== "1") transportFail("CLIENT_UNSUPPORTED_MEDIA_TYPE", response.status);
    validateUnaryEncoding(response.headers.get("content-encoding"));
  } catch (error) {
    try { await response.body?.cancel(); } catch {}
    throw error;
  }
  const payload = await readBoundedBody(response, configuration.limits, options.signal, false);
  if (!response.ok) transportFail("CLIENT_REMOTE_ERROR", response.status);
  return operation.decodeResult(payload);
}

async function executeSSE(configuration, operation, options) {
  if (operation?.kind !== "subscription") transportFail("CLIENT_OPERATION_INVALID");
  const response = await fetchOperation(configuration, operation, options.signal, "text/event-stream");
  try {
    const mediaType = parseMediaType(response.headers.get("content-type"));
    if (mediaType.name !== "text/event-stream" || mediaType.parameters.charset && mediaType.parameters.charset !== "utf-8") transportFail("CLIENT_UNSUPPORTED_MEDIA_TYPE", response.status);
    if (normalizedEncoding(response.headers.get("content-encoding")) !== "identity") transportFail("CLIENT_UNSUPPORTED_ENCODING", response.status);
    if (!response.ok) transportFail("CLIENT_REMOTE_ERROR", response.status);
    validateDeclaredLength(response, configuration.limits.compressedBytes);
  } catch (error) {
    try { await response.body?.cancel(); } catch {}
    throw error;
  }
  return decodeSSEStream(response.body, { maximumFrameBytes: configuration.limits.frameBytes, maximumResponseBytes: configuration.limits.decompressedBytes, signal: options.signal });
}

async function fetchOperation(configuration, operation, signal, accept) {
  if (!operation || typeof operation.canonicalRequest !== "function") transportFail("CLIENT_OPERATION_INVALID");
  if (signal?.aborted) transportFail("CLIENT_CANCELED");
  const body = operation.canonicalRequest();
  const authenticated = await authenticationHeaders(configuration.authenticate, configuration.endpoint, operation, signal);
  let headers;
  try {
    headers = new Headers(authenticated);
    headers.set("accept", accept);
    headers.set("accept-encoding", accept === "text/event-stream" ? "identity" : "gzip");
    headers.set("content-type", requestMediaType);
  } catch {
    transportFail("CLIENT_AUTHENTICATION_FAILED");
  }
  let target = configuration.endpoint;
  let credentials = configuration.policy.credentials;
  for (let redirects = 0; ; redirects += 1) {
    if (signal?.aborted) transportFail("CLIENT_CANCELED");
    let response;
    try {
      response = await configuration.fetchImplementation(target.href, { method: "POST", body, headers, signal, redirect: "manual", credentials });
    } catch (error) {
      if (signal?.aborted || error?.name === "AbortError") transportFail("CLIENT_CANCELED");
      transportFail("CLIENT_TRANSPORT_ERROR");
    }
    if (response.type === "opaqueredirect") {
      try { await response.body?.cancel(); } catch {}
      transportFail("CLIENT_REDIRECT_UNSUPPORTED", response.status);
    }
    if (!isRedirect(response.status)) return response;
    try { await response.body?.cancel(); } catch {}
    if (redirects >= configuration.limits.redirects) transportFail("CLIENT_REDIRECT_LIMIT", response.status);
    if (rejectedRedirectStatuses.has(response.status)) transportFail("CLIENT_REDIRECT_REJECTED", response.status);
    const next = redirectTarget(target, response.headers.get("location"));
    const crossOrigin = next.origin !== target.origin;
    if (crossOrigin && !configuration.policy.redirectOrigins.has(next.origin)) transportFail("CLIENT_REDIRECT_REJECTED", response.status);
    if (crossOrigin && !configuration.policy.credentialOrigins.has(next.origin)) {
      stripSensitiveHeaders(headers);
      credentials = "omit";
    }
    target = next;
  }
}

async function authenticationHeaders(authenticate, endpoint, operation, signal) {
  if (!authenticate) return undefined;
  try {
    const headers = await authenticate(Object.freeze({ url: endpoint.href, operation, signal }));
    return headers ?? undefined;
  } catch {
    transportFail("CLIENT_AUTHENTICATION_FAILED");
  }
}

async function readBoundedBody(response, limits, signal, streaming) {
  const encoding = normalizedEncoding(response.headers.get("content-encoding"));
  const declaredLimit = encoding === "identity" ? Math.min(limits.compressedBytes, limits.decompressedBytes) : limits.compressedBytes;
  let reader;
  try {
    if (signal?.aborted) transportFail("CLIENT_CANCELED");
    validateDeclaredLength(response, declaredLimit);
    reader = response.body?.getReader?.();
    if (!reader) transportFail("CLIENT_MALFORMED_RESPONSE", response.status);
  } catch (error) {
    try { await response.body?.cancel(); } catch {}
    if (error instanceof NaatreClientError) throw error;
    transportFail("CLIENT_MALFORMED_RESPONSE", response.status);
  }
  const chunks = [];
  let received = 0;
  const limit = streaming || encoding === "gzip" ? limits.decompressedBytes : declaredLimit;
  const cancel = () => { void reader.cancel(); };
  signal?.addEventListener("abort", cancel, { once: true });
  try {
    for (;;) {
      if (signal?.aborted) transportFail("CLIENT_CANCELED");
      const { done, value } = await reader.read();
      if (done) break;
      if (!(value instanceof Uint8Array)) transportFail("CLIENT_MALFORMED_RESPONSE", response.status);
      received += value.byteLength;
      if (received > limit) transportFail("CLIENT_RESPONSE_LIMIT", response.status);
      chunks.push(value);
    }
  } catch (error) {
    if (error instanceof NaatreClientError) throw error;
    if (signal?.aborted || error?.name === "AbortError") transportFail("CLIENT_CANCELED");
    transportFail("CLIENT_MALFORMED_RESPONSE", response.status);
  } finally {
    signal?.removeEventListener("abort", cancel);
    try { await reader.cancel(); } catch {}
  }
  const result = new Uint8Array(received);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return result;
}

function validateUnaryEncoding(value) {
  const encoding = normalizedEncoding(value);
  if (encoding !== "identity" && encoding !== "gzip") transportFail("CLIENT_UNSUPPORTED_ENCODING");
}

function validateDeclaredLength(response, maximum) {
  const value = response.headers.get("content-length");
  if (value === null) return;
  if (!/^(?:0|[1-9][0-9]*)$/u.test(value)) transportFail("CLIENT_MALFORMED_RESPONSE", response.status);
  const length = Number(value);
  if (!Number.isSafeInteger(length) || length > maximum) transportFail("CLIENT_RESPONSE_LIMIT", response.status);
}

function parseMediaType(value) {
  if (typeof value !== "string") transportFail("CLIENT_UNSUPPORTED_MEDIA_TYPE");
  const parts = value.toLowerCase().split(";").map((part) => part.trim());
  const parameters = Object.create(null);
  for (const part of parts.slice(1)) {
    const match = /^([a-z0-9_-]+)=(?:"([^"]*)"|([^\s"]+))$/u.exec(part);
    if (!match || Object.hasOwn(parameters, match[1])) transportFail("CLIENT_UNSUPPORTED_MEDIA_TYPE");
    parameters[match[1]] = match[2] ?? match[3];
  }
  return { name: parts[0], parameters };
}

function responseMediaTypeWithVersion() {
  return `${responseMediaType};version=1`;
}

function redirectTarget(current, location) {
  let target;
  try { target = new URL(location, current); } catch { transportFail("CLIENT_REDIRECT_REJECTED"); }
  if (!["http:", "https:"].includes(target.protocol) || target.username || target.password || target.hash) transportFail("CLIENT_REDIRECT_REJECTED");
  return target;
}

function stripSensitiveHeaders(headers) {
  for (const name of [...headers.keys()]) if (sensitiveHeaders.has(name.toLowerCase())) headers.delete(name);
}

function isRedirect(status) {
  return redirectStatuses.has(status) || rejectedRedirectStatuses.has(status);
}

function normalizedEncoding(value) {
  return value === null || value.trim() === "" ? "identity" : value.trim().toLowerCase();
}

function parseEndpoint(value) {
  let endpoint;
  try { endpoint = new URL(value); } catch { transportFail("CLIENT_CONFIG_INVALID"); }
  if (!["http:", "https:"].includes(endpoint.protocol) || endpoint.username || endpoint.password || endpoint.hash) transportFail("CLIENT_CONFIG_INVALID");
  return endpoint;
}

function originSet(value = []) {
  if (!Array.isArray(value)) transportFail("CLIENT_CONFIG_INVALID");
  const result = new Set();
  for (const entry of value) {
    let origin;
    try { origin = new URL(entry).origin; } catch { transportFail("CLIENT_CONFIG_INVALID"); }
    if (origin === "null") transportFail("CLIENT_CONFIG_INVALID");
    result.add(origin);
  }
  return result;
}

function positiveLimit(value, fallback) {
  if (value === undefined) return fallback;
  if (!Number.isSafeInteger(value) || value < 1) transportFail("CLIENT_CONFIG_INVALID");
  return value;
}

function nonNegativeLimit(value, fallback) {
  if (value === undefined) return fallback;
  if (!Number.isSafeInteger(value) || value < 0) transportFail("CLIENT_CONFIG_INVALID");
  return value;
}

function transportFail(code, status = 0) {
  throw new NaatreClientError(code, { status });
}
