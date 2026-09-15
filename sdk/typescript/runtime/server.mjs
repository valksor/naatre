import { NaatreClientError } from "./error.mjs";
import { canonicalStringify, hasOwn, ownDataArray, parseJSON, safeObject } from "./json.mjs";
import { createScalarCodecs, decodeScalar, encodeScalar } from "./scalars.mjs";

export const workerProtocol = "naatre.remote-worker.v1";
export const workerRuntimeVersion = "naatre.typescript.worker-1";

const mediaType = "application/naatre-worker+json";
const identifierPattern = /^[A-Za-z][A-Za-z0-9._:-]{0,127}$/u;
const errorCodePattern = /^[A-Z][A-Z0-9_]{2,63}$/u;
const forbiddenNames = new Set(["__proto__", "constructor", "prototype"]);
const reservedApplicationCodes = new Set(["CANCELLED", "INTERNAL", "OVERLOADED", "OUTPUT_COMPLETION", "UNAUTHORIZED"]);
const effects = new Set(["query", "mutation", "transaction", "subscription"]);
const executionProfiles = new Set(["inline", "worker", "process"]);
const capabilities = new Set([
  "unary-1",
  "client-streaming-1",
  "server-streaming-1",
  "bidirectional-streaming-1",
  "cancellation-ack-1",
  "idempotency-replay-1",
  "transaction-provider-1",
  "subscription-resume-1",
]);
const handlerRecords = new WeakMap();
const workerRecords = new WeakMap();

export class NaatreWorkerError extends Error {
  constructor(code, message, status = 400, options = undefined) {
    super(message, options);
    this.name = "NaatreWorkerError";
    this.code = code;
    this.status = status;
    this.applicationError = false;
    this.retryable = false;
  }

  static application(code, message, options = {}) {
    if (!errorCodePattern.test(code) || code.startsWith("REMOTE_") || reservedApplicationCodes.has(code) || typeof message !== "string" || message.trim() === "" || message.length > 32768) {
      throw new NaatreWorkerError("REMOTE_WORKER_MALFORMED", "application error is invalid", 500);
    }
    const error = new NaatreWorkerError(code, message, 200);
    error.applicationError = true;
    error.retryable = options.retryable === true;
    if (hasOwn(options, "details")) error.details = safeObject(cloneJSON(options.details, "REMOTE_WORKER_MALFORMED"), "REMOTE_WORKER_MALFORMED");
    return error;
  }
}

export function defineHandler(definition, handler) {
  if (typeof handler !== "function") invalidRegistration("handler must be callable");
  const source = closedDataObject(definition, ["id", "inputSchema", "outputSchema", "codec", "effect", "requiredCapabilities", "executionProfile", "input", "output", "codecs"], "handler definition");
  const id = registrationIdentifier(source.id, "handler id", true);
  const inputSchema = registrationIdentifier(source.inputSchema, "input schema", true);
  const outputSchema = registrationIdentifier(source.outputSchema, "output schema", true);
  if (source.codec !== "naatre.json-1" || !effects.has(source.effect) || !executionProfiles.has(source.executionProfile)) invalidRegistration("handler contract is invalid");
  const requiredCapabilities = uniqueCapabilities(source.requiredCapabilities);
  const record = Object.freeze({
    id,
    inputSchema,
    outputSchema,
    codec: source.codec,
    effect: source.effect,
    requiredCapabilities,
    executionProfile: source.executionProfile,
    input: normalizeSchema(source.input),
    output: normalizeSchema(source.output),
    codecs: createScalarCodecs(source.codecs ?? {}),
    handler,
  });
  const binding = Object.freeze(Object.create(null));
  handlerRecords.set(binding, record);
  return binding;
}

export function createWorker(configuration) {
  const source = closedDataObject(configuration, ["workerId", "serviceIdentity", "audience", "endpoint", "schemaRevision", "schemaDigest", "capabilities", "limits", "handlers", "authenticate", "createRequestState", "executors", "now"], "worker configuration");
  const workerID = registrationIdentifier(source.workerId, "worker id", true);
  const serviceIdentity = boundedString(source.serviceIdentity, 2048, "service identity");
  const audience = registrationIdentifier(source.audience, "audience", true);
  const endpoint = registrationIdentifier(source.endpoint, "endpoint", true);
  const schemaRevision = registrationIdentifier(source.schemaRevision, "schema revision", true);
  if (typeof source.schemaDigest !== "string" || !/^[0-9a-f]{64}$/u.test(source.schemaDigest)) invalidRegistration("schema digest is invalid");
  const advertisedCapabilities = uniqueCapabilities(source.capabilities);
  if (!advertisedCapabilities.includes("unary-1")) invalidRegistration("unary capability is required");
  const limits = normalizeLimits(source.limits);
  const configuredHandlers = denseDataArray(source.handlers, "handlers");
  if (configuredHandlers.length === 0) invalidRegistration("handlers must be a non-empty array");
  if (typeof source.authenticate !== "function") invalidRegistration("authenticate must be callable");
  if (source.createRequestState !== undefined && typeof source.createRequestState !== "function") invalidRegistration("request-state factory must be callable");
  if (source.now !== undefined && typeof source.now !== "function") invalidRegistration("clock must be callable");
  const executors = normalizeExecutors(source.executors);
  const handlers = new Map();
  for (const binding of configuredHandlers) {
    const handler = handlerRecords.get(binding);
    if (!handler || handlers.has(handler.id) || handler.requiredCapabilities.some((capability) => !advertisedCapabilities.includes(capability))) invalidRegistration("handler registration is invalid");
    if (handler.effect === "transaction" && !advertisedCapabilities.includes("transaction-provider-1")) invalidRegistration("transaction handler lacks provider capability");
    if (handler.executionProfile !== "inline" && typeof executors[handler.executionProfile] !== "function") invalidRegistration("isolated handler lacks an explicit executor");
    handlers.set(handler.id, handler);
  }

  const registration = freezeJSON({
    protocol: workerProtocol,
    workerId: workerID,
    serviceIdentity,
    audience,
    endpoint,
    schemaRevision,
    schemaDigest: source.schemaDigest,
    capabilities: advertisedCapabilities,
    limits,
    handlers: [...handlers.values()].map(({ id, inputSchema, outputSchema, codec, effect, requiredCapabilities }) => ({ id, inputSchema, outputSchema, codec, effect, requiredCapabilities })),
  });
  const record = {
    registration,
    handlers,
    authenticate: source.authenticate,
    createRequestState: source.createRequestState ?? (() => ({ cache: new Map(), loaders: new Map(), state: Object.create(null) })),
    executors,
    now: source.now ?? Date.now,
    active: new Map(),
    scopedObjects: new WeakSet(),
  };
  const worker = Object.freeze({
    registration: () => cloneJSON(record.registration, "REMOTE_REGISTRATION_INVALID"),
    register: (candidate) => register(record, candidate),
    invoke: (invocation, options) => invoke(record, invocation, options),
    stream: (invocation, options) => stream(record, invocation, options),
    cancel: (cancellation) => cancel(record, cancellation),
    metrics: () => metrics(record),
  });
  workerRecords.set(worker, record);
  return worker;
}

export function createFetchWorkerAdapter(worker, options = {}) {
  const record = workerRecords.get(worker);
  if (!record) invalidRegistration("Fetch adapter requires a Naatre worker");
  const adapterOptions = closedDataObject(options, ["signal"], "Fetch adapter options");
  if (adapterOptions.signal !== undefined && !(adapterOptions.signal instanceof AbortSignal)) invalidRegistration("Fetch adapter signal is invalid");
  return async function fetchWorker(request) {
    let linked;
    try {
      if (!(request instanceof Request) || request.method !== "POST") return response({ code: "REMOTE_INVOCATION_INVALID", message: "worker endpoint requires POST" }, 405);
      if (request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() !== mediaType) return response({ code: "REMOTE_INVOCATION_INVALID", message: "worker media type is invalid" }, 415);
      const member = new URL(request.url).pathname.split("/").at(-1);
      const maximum = member === "Register" ? record.registration.limits.maxRequestBytes : record.registration.limits.maxRequestBytes;
      linked = linkedAbortSignal(request.signal, adapterOptions.signal);
      const body = parseJSON(await readRequestBody(request, maximum, linked.signal), maximum);
      if (member === "Register") return response(await worker.register(body));
      if (member === "Invoke") return response(await worker.invoke(body, { signal: linked.signal }));
      if (member === "Cancel") return response(await worker.cancel(body));
      return response({ code: "REMOTE_HANDLER_UNKNOWN", message: "worker endpoint is unknown" }, 404);
    } catch (error) {
      const failure = asWorkerError(error);
      return response({ code: failure.code, message: failure.message }, failure.status);
    } finally {
      linked?.dispose();
    }
  };
}

function linkedAbortSignal(requestSignal, lifecycleSignal) {
  if (lifecycleSignal === undefined) return Object.freeze({ signal: requestSignal, dispose() {} });
  const controller = new AbortController();
  const sources = [requestSignal, lifecycleSignal];
  const listeners = [];
  for (const source of sources) {
    if (source.aborted) {
      controller.abort(source.reason);
      break;
    }
    const abort = () => controller.abort(source.reason);
    source.addEventListener("abort", abort, { once: true });
    listeners.push([source, abort]);
  }
  return Object.freeze({
    signal: controller.signal,
    dispose() {
      for (const [source, abort] of listeners) source.removeEventListener("abort", abort);
    },
  });
}

async function register(record, candidate) {
  let normalized;
  try {
    normalized = cloneJSON(candidate, "REMOTE_REGISTRATION_INVALID");
  } catch (error) {
    throw wrap(error, "REMOTE_REGISTRATION_INVALID", "worker registration is invalid");
  }
  if (canonicalStringify(normalized) !== canonicalStringify(record.registration)) invalidRegistration("worker registration does not match this process");
  return freezeJSON({
    protocol: workerProtocol,
    workerId: record.registration.workerId,
    sessionId: `${record.registration.workerId}-session`,
    schemaRevision: record.registration.schemaRevision,
    acceptedCapabilities: record.registration.capabilities,
  });
}

async function invoke(record, candidate, options = {}) {
  const entry = await begin(record, candidate, options, false);
  try {
    let value;
    try {
      value = await executeHandler(record, entry);
    } catch (error) {
      if (entry.controller.signal.aborted) throw canceled();
      if (error instanceof NaatreWorkerError && error.applicationError) {
        const result = applicationResult(entry, error);
        if (byteLength(canonicalStringify(result)) > record.registration.limits.maxResponseBytes) throw new NaatreWorkerError("REMOTE_WORKER_MALFORMED", "worker result exceeds response limit", 500);
        return result;
      }
      throw new NaatreWorkerError("INTERNAL", "handler execution failed", 500, { cause: error });
    }
    if (entry.controller.signal.aborted) throw canceled();
    let output;
    try {
      output = validateValue(entry.handler.output, value, entry.handler.codecs, "output");
      canonicalStringify(output);
    } catch (error) {
      throw wrap(error, "OUTPUT_COMPLETION", "handler output does not satisfy its schema");
    }
    const result = freezeJSON(resultEnvelope(entry, output, []));
    if (byteLength(canonicalStringify(result)) > record.registration.limits.maxResponseBytes) throw new NaatreWorkerError("REMOTE_WORKER_MALFORMED", "worker result exceeds response limit", 500);
    return result;
  } finally {
    await finish(record, entry);
  }
}

async function stream(record, candidate, options = {}) {
  const entry = await begin(record, candidate, options, true);
  if (!record.registration.capabilities.includes("server-streaming-1")) {
    await finish(record, entry);
    throw new NaatreWorkerError("REMOTE_CAPABILITY_MISMATCH", "server streaming is not registered", 400);
  }
  let iterator;
  try {
    const source = await executeHandler(record, entry);
    const createIterator = source?.[Symbol.asyncIterator];
    if (typeof createIterator !== "function") throw new TypeError("stream handler did not return an AsyncIterable");
    iterator = createIterator.call(source);
    if (!iterator || typeof iterator.next !== "function") throw new TypeError("stream handler returned an invalid iterator");
  } catch (error) {
    await finish(record, entry);
    throw wrap(error, "REMOTE_WORKER_MALFORMED", "stream handler did not return a valid source");
  }
  let frames = 0;
  let bytes = 0;
  let busy = false;
  let closed = false;
  const close = async (value) => {
    if (closed) return { done: true, value };
    closed = true;
    try {
      if (typeof iterator.return === "function") await iterator.return(value);
    } finally {
      await finish(record, entry);
    }
    return { done: true, value };
  };
  const output = Object.freeze({
    [Symbol.asyncIterator]() { return this; },
    async next() {
      if (closed) return { done: true, value: undefined };
      if (busy) throw new NaatreWorkerError("OVERLOADED", "concurrent stream reads are not permitted", 429);
      if (entry.controller.signal.aborted) {
        await close();
        throw canceled();
      }
      busy = true;
      try {
        const result = await iterator.next();
        if (entry.controller.signal.aborted) {
          await close();
          throw canceled();
        }
        if (result.done) {
          await close(result.value);
          return { done: true, value: result.value };
        }
        let value;
        try {
          value = validateValue(entry.handler.output, result.value, entry.handler.codecs, "output");
        } catch (error) {
          await close();
          throw wrap(error, "OUTPUT_COMPLETION", "stream output does not satisfy its schema");
        }
        frames += 1;
        bytes += byteLength(canonicalStringify(value));
        if (frames > record.registration.limits.maxStreamFrames || bytes > record.registration.limits.maxStreamBytes) {
          await close();
          throw new NaatreWorkerError("OVERLOADED", "stream output exceeds its admitted credit", 429);
        }
        return { done: false, value };
      } finally {
        busy = false;
      }
    },
    return: close,
    async throw(error) {
      try {
        if (typeof iterator.throw === "function") return await iterator.throw(error);
        throw error;
      } finally {
        await close();
      }
    },
  });
  return output;
}

async function begin(record, candidate, options, streaming) {
  let invocation;
  try {
    invocation = cloneJSON(candidate, "REMOTE_INVOCATION_INVALID");
  } catch (error) {
    throw wrap(error, "REMOTE_INVOCATION_INVALID", "remote invocation is invalid");
  }
  const value = closedJSON(invocation, ["protocol", "requestId", "invocationId", "attemptId", "handlerId", "schemaRevision", "deadlineUnixMilli", "delegatedContext", "input", "parent", "idempotencyKey", "resumeCursor"], "REMOTE_INVOCATION_INVALID");
  if (value.protocol !== workerProtocol || !validIdentifier(value.requestId) || !validIdentifier(value.invocationId) || !validIdentifier(value.attemptId) || !validHandlerID(value.handlerId) || value.schemaRevision !== record.registration.schemaRevision || !Number.isSafeInteger(value.deadlineUnixMilli) || value.deadlineUnixMilli <= record.now() || typeof value.delegatedContext !== "string" || value.delegatedContext.length === 0 || value.delegatedContext.length > 8192) {
    throw new NaatreWorkerError("REMOTE_INVOCATION_INVALID", "remote invocation is invalid", 400);
  }
  const handler = record.handlers.get(value.handlerId);
  if (!handler) throw new NaatreWorkerError("REMOTE_HANDLER_UNKNOWN", "remote handler is not registered", 404);
  if (record.active.has(value.invocationId)) throw new NaatreWorkerError("REMOTE_INVOCATION_DUPLICATE", "remote invocation is already active", 409);
  if (record.active.size >= record.registration.limits.maxInFlight) throw new NaatreWorkerError("OVERLOADED", "worker capacity is full", 429);
  if (streaming !== (handler.effect === "subscription")) throw new NaatreWorkerError("REMOTE_CAPABILITY_MISMATCH", "handler invocation mode is invalid", 400);
  if (byteLength(canonicalStringify(value.input)) > record.registration.limits.maxRequestBytes) throw new NaatreWorkerError("REMOTE_INVOCATION_INVALID", "remote input exceeds its limit", 413);
  let input;
  try {
    input = validateValue(handler.input, value.input, handler.codecs, "input");
  } catch (error) {
    throw wrap(error, "REMOTE_INVOCATION_INVALID", "handler input does not satisfy its schema");
  }
  const controller = new AbortController();
  const entry = {
    invocation: value,
    input,
    handler,
    controller,
    streaming,
    cancellationRequested: false,
    cleanupStarted: false,
    cleanupCallbacks: [],
    cleanupPromise: undefined,
    detachSignal: undefined,
    context: undefined,
  };
  const signal = options?.signal;
  if (signal !== undefined && !(signal instanceof AbortSignal)) throw new NaatreWorkerError("REMOTE_INVOCATION_INVALID", "invocation signal is invalid", 400);
  if (signal?.aborted) controller.abort(signal.reason);
  else if (signal) {
    const abort = () => abortEntry(entry, signal.reason);
    signal.addEventListener("abort", abort, { once: true });
    entry.detachSignal = () => signal.removeEventListener("abort", abort);
  }
  record.active.set(value.invocationId, entry);
  try {
    const authentication = closedDataObject(await record.authenticate(Object.freeze({
      delegatedContext: value.delegatedContext,
      requestId: value.requestId,
      invocationId: value.invocationId,
      handlerId: value.handlerId,
      schemaRevision: value.schemaRevision,
      signal: controller.signal,
    })), ["principal", "tenant"], "authentication result");
    const principal = boundedString(authentication.principal, 2048, "principal");
    const tenant = boundedString(authentication.tenant, 2048, "tenant");
    const scoped = normalizeRequestState(await record.createRequestState(Object.freeze({ principal, tenant, requestId: value.requestId, signal: controller.signal })), record.scopedObjects);
    entry.cleanupCallbacks.push(...scoped.cleanup);
    entry.context = Object.freeze({
      requestId: value.requestId,
      invocationId: value.invocationId,
      attemptId: value.attemptId,
      principal,
      tenant,
      cache: scoped.cache,
      loaders: scoped.loaders,
      state: scoped.state,
      signal: controller.signal,
      executionProfile: handler.executionProfile,
      onCleanup(callback) {
        if (typeof callback !== "function") throw new TypeError("cleanup callback must be callable");
        if (entry.cleanupStarted) void Promise.resolve().then(callback);
        else entry.cleanupCallbacks.push(callback);
      },
    });
    if (controller.signal.aborted) throw canceled();
    return entry;
  } catch (error) {
    await finish(record, entry);
    if (error instanceof NaatreWorkerError) throw error;
    throw new NaatreWorkerError("UNAUTHORIZED", "delegated context was rejected", 403, { cause: error });
  }
}

async function executeHandler(record, entry) {
  if (entry.handler.executionProfile === "inline") return entry.handler.handler(entry.input, entry.context);
  return record.executors[entry.handler.executionProfile](entry.handler.handler, entry.input, entry.context);
}

async function cancel(record, candidate) {
  let cancellation;
  try {
    cancellation = closedJSON(cloneJSON(candidate, "REMOTE_CANCELLATION_INVALID"), ["protocol", "requestId", "invocationId"], "REMOTE_CANCELLATION_INVALID");
  } catch (error) {
    throw wrap(error, "REMOTE_CANCELLATION_INVALID", "cancellation request is invalid");
  }
  if (cancellation.protocol !== workerProtocol || !validIdentifier(cancellation.requestId) || !validIdentifier(cancellation.invocationId)) throw new NaatreWorkerError("REMOTE_CANCELLATION_INVALID", "cancellation request is invalid", 400);
  const entry = record.active.get(cancellation.invocationId);
  if (!entry || entry.invocation.requestId !== cancellation.requestId || !record.registration.capabilities.includes("cancellation-ack-1")) throw new NaatreWorkerError("REMOTE_CANCELLATION_INVALID", "invocation is not active", 404);
  abortEntry(entry, new DOMException("canceled", "AbortError"));
  await cleanupEntry(entry);
  return freezeJSON({ protocol: workerProtocol, invocationId: cancellation.invocationId, disposition: "acknowledged" });
}

function abortEntry(entry, reason) {
  if (!entry.cancellationRequested) {
    entry.cancellationRequested = true;
    entry.controller.abort(reason);
    void cleanupEntry(entry);
  }
}

async function finish(record, entry) {
  entry.detachSignal?.();
  try {
    await cleanupEntry(entry);
  } finally {
    record.active.delete(entry.invocation.invocationId);
  }
}

function cleanupEntry(entry) {
  if (entry.cleanupPromise) return entry.cleanupPromise;
  entry.cleanupStarted = true;
  entry.cleanupPromise = (async () => {
    let failure;
    while (entry.cleanupCallbacks.length > 0) {
      const callback = entry.cleanupCallbacks.pop();
      try {
        await callback();
      } catch (error) {
        failure ??= error;
      }
    }
    if (failure) throw new NaatreWorkerError("INTERNAL", "request cleanup failed", 500, { cause: failure });
  })();
  return entry.cleanupPromise;
}

function normalizeRequestState(candidate, used) {
  const source = closedDataObject(candidate, ["cache", "loaders", "state", "cleanup"], "request state");
  const cache = source.cache ?? new Map();
  const loaders = source.loaders ?? new Map();
  const state = source.state ?? Object.create(null);
  if (!(cache instanceof Map) || !(loaders instanceof Map) || state === null || typeof state !== "object") throw new TypeError("request state resources are invalid");
  for (const resource of [cache, loaders, state]) {
    if (used.has(resource)) throw new TypeError("request state resources must not be reused");
    used.add(resource);
  }
  const cleanup = source.cleanup === undefined ? [] : Array.isArray(source.cleanup) ? denseDataArray(source.cleanup, "request cleanup") : [source.cleanup];
  if (cleanup.some((callback) => typeof callback !== "function")) throw new TypeError("request cleanup is invalid");
  return { cache, loaders, state, cleanup };
}

function applicationResult(entry, error) {
  const item = { code: error.code, message: error.message, retryable: error.retryable };
  if (hasOwn(error, "details")) item.details = error.details;
  const result = freezeJSON(resultEnvelope(entry, null, [item]));
  canonicalStringify(result);
  return result;
}

function resultEnvelope(entry, data, errors) {
  return { protocol: workerProtocol, invocationId: entry.invocation.invocationId, attemptId: entry.invocation.attemptId, schemaRevision: entry.invocation.schemaRevision, data, errors };
}

function metrics(record) {
  let cancellationRequested = 0;
  let activeStreams = 0;
  for (const entry of record.active.values()) {
    if (entry.cancellationRequested) cancellationRequested += 1;
    if (entry.streaming) activeStreams += 1;
  }
  return Object.freeze({ activeInvocations: record.active.size, cancellationRequested, activeStreams });
}

function normalizeSchema(candidate, depth = 0) {
  if (depth > 64) invalidRegistration("value schema nesting is too deep");
  const source = closedDataObject(candidate, ["kind", "type", "fields", "additionalProperties", "element", "elementNullable", "values", "open", "tag", "value", "variants"], "value schema");
  if (source.kind === "scalar") return Object.freeze({ kind: "scalar", type: registrationIdentifier(source.type, "scalar type", true) });
  if (source.kind === "list") return Object.freeze({ kind: "list", element: normalizeSchema(source.element, depth + 1), elementNullable: source.elementNullable === true });
  if (source.kind === "enum") {
    const values = denseDataArray(source.values, "enum values");
    if (values.length === 0 || values.some((value) => typeof value !== "string") || new Set(values).size !== values.length) invalidRegistration("enum schema is invalid");
    return Object.freeze({ kind: "enum", values: Object.freeze(values), open: source.open === true });
  }
  if (source.kind === "object") {
    if (source.additionalProperties !== false) invalidRegistration("object schema is invalid");
    const sourceFields = denseDataArray(source.fields, "schema fields");
    const names = new Set();
    const fields = sourceFields.map((candidateField) => {
      const field = closedDataObject(candidateField, ["name", "schema", "required", "nullable"], "schema field");
      const name = schemaKey(field.name, "schema field");
      if (names.has(name) || typeof field.required !== "boolean" || typeof field.nullable !== "boolean") invalidRegistration("schema field is invalid");
      names.add(name);
      return Object.freeze({ name, schema: normalizeSchema(field.schema, depth + 1), required: field.required, nullable: field.nullable });
    });
    return Object.freeze({ kind: "object", fields: Object.freeze(fields), additionalProperties: false });
  }
  if (source.kind === "union") {
    const tag = schemaKey(source.tag, "union tag");
    const value = schemaKey(source.value, "union value");
    const sourceVariants = denseDataArray(source.variants, "union variants");
    if (tag === value || sourceVariants.length === 0) invalidRegistration("union schema is invalid");
    const tags = new Set();
    const variants = sourceVariants.map((candidateVariant) => {
      const variant = closedDataObject(candidateVariant, ["tag", "schema"], "union variant");
      const variantTag = registrationIdentifier(variant.tag, "union variant tag", true);
      if (tags.has(variantTag)) invalidRegistration("union variant is duplicated");
      tags.add(variantTag);
      return Object.freeze({ tag: variantTag, schema: normalizeSchema(variant.schema, depth + 1) });
    });
    return Object.freeze({ kind: "union", tag, value, variants: Object.freeze(variants), open: source.open === true });
  }
  invalidRegistration("value schema kind is unsupported");
}

function validateValue(schema, candidate, codecs, direction) {
  if (schema.kind === "scalar") return direction === "input" ? decodeScalar(codecs, schema.type, candidate) : encodeScalar(codecs, schema.type, candidate);
  if (schema.kind === "list") {
    const values = denseDataArray(candidate, "list");
    return values.map((value) => {
      if (value === null) {
        if (!schema.elementNullable) throw new TypeError("list element is not nullable");
        return null;
      }
      return validateValue(schema.element, value, codecs, direction);
    });
  }
  if (schema.kind === "enum") {
    if (typeof candidate !== "string" || (!schema.open && !schema.values.includes(candidate))) throw new TypeError("enum value is unknown");
    return candidate;
  }
  if (schema.kind === "object") {
    const source = safeObject(candidate, direction === "input" ? "REMOTE_INVOCATION_INVALID" : "OUTPUT_COMPLETION");
    const fields = new Map(schema.fields.map((field) => [field.name, field]));
    if (Object.keys(source).some((key) => !fields.has(key))) throw new TypeError("object contains an unknown field");
    const output = Object.create(null);
    for (const field of schema.fields) {
      if (!hasOwn(source, field.name)) {
        if (field.required) throw new TypeError("object omits a required field");
        continue;
      }
      const value = source[field.name];
      if (value === null) {
        if (!field.nullable) throw new TypeError("field is not nullable");
        output[field.name] = null;
      } else output[field.name] = validateValue(field.schema, value, codecs, direction);
    }
    return output;
  }
  if (schema.kind === "union") {
    const source = safeObject(candidate, direction === "input" ? "REMOTE_INVOCATION_INVALID" : "OUTPUT_COMPLETION");
    if (Object.keys(source).length !== 2 || typeof source[schema.tag] !== "string" || !hasOwn(source, schema.value)) throw new TypeError("tagged union shape is invalid");
    const variant = schema.variants.find(({ tag }) => tag === source[schema.tag]);
    if (!variant) {
      if (!schema.open) throw new TypeError("tagged union variant is unknown");
      return source;
    }
    const output = Object.create(null);
    output[schema.tag] = variant.tag;
    output[schema.value] = validateValue(variant.schema, source[schema.value], codecs, direction);
    return output;
  }
  throw new TypeError("value schema is invalid");
}

function normalizeLimits(candidate) {
  const source = closedDataObject(candidate, ["maxInFlight", "maxRequestBytes", "maxResponseBytes", "maxStreamFrames", "maxStreamBytes"], "worker limits");
  const output = Object.create(null);
  for (const name of ["maxInFlight", "maxRequestBytes", "maxResponseBytes", "maxStreamFrames", "maxStreamBytes"]) {
    if (!Number.isSafeInteger(source[name]) || source[name] < 1) invalidRegistration("worker limits are invalid");
    output[name] = source[name];
  }
  return output;
}

function normalizeExecutors(candidate) {
  if (candidate === undefined) return Object.freeze(Object.create(null));
  const source = closedDataObject(candidate, ["worker", "process"], "isolated executors");
  for (const name of Object.keys(source)) if (typeof source[name] !== "function") invalidRegistration("isolated executor is invalid");
  return Object.freeze(source);
}

function uniqueCapabilities(candidate) {
  const values = denseDataArray(candidate, "capabilities");
  const seen = new Set();
  for (const value of values) {
    if (!capabilities.has(value) || seen.has(value)) invalidRegistration("capability is invalid or duplicated");
    seen.add(value);
  }
  return Object.freeze(values);
}

function denseDataArray(candidate, label) {
  return ownDataArray(candidate, (reason) => { throw new TypeError(`${label} ${reason}`); });
}

function closedDataObject(candidate, allowed, label) {
  if (candidate === null || typeof candidate !== "object" || Array.isArray(candidate) || Object.getOwnPropertySymbols(candidate).length !== 0) throw new TypeError(`${label} must be an object`);
  const prototype = Object.getPrototypeOf(candidate);
  if (prototype !== Object.prototype && prototype !== null) throw new TypeError(`${label} has an ambiguous prototype`);
  const descriptors = Object.getOwnPropertyDescriptors(candidate);
  const output = Object.create(null);
  for (const key of Object.keys(descriptors)) {
    if (!allowed.includes(key)) throw new TypeError(`${label} contains an unknown member`);
    const descriptor = descriptors[key];
    if (!descriptor?.enumerable || !("value" in descriptor)) throw new TypeError(`${label} contains an accessor`);
    output[key] = descriptor.value;
  }
  return output;
}

function closedJSON(candidate, allowed, code) {
  const value = safeObject(candidate, code);
  if (Object.keys(value).some((key) => !allowed.includes(key))) throw new NaatreWorkerError(code, "object contains an unknown member", 400);
  return value;
}

function registrationIdentifier(value, label, rejectReserved) {
  if (!validIdentifier(value) || rejectReserved && value.split(/[.:-]/u).some((part) => forbiddenNames.has(part))) invalidRegistration(`${label} is invalid`);
  return value;
}

function schemaKey(value, label) {
  if (typeof value !== "string" || value.length === 0 || value.length > 128 || forbiddenNames.has(value)) invalidRegistration(`${label} is invalid`);
  try {
    canonicalStringify(value);
  } catch {
    invalidRegistration(`${label} is invalid`);
  }
  return value;
}

function validIdentifier(value) {
  return typeof value === "string" && identifierPattern.test(value);
}

function validHandlerID(value) {
  return validIdentifier(value) && !value.split(/[.:-]/u).some((part) => forbiddenNames.has(part));
}

function boundedString(value, maximum, label) {
  if (typeof value !== "string" || value.length === 0 || value.length > maximum) invalidRegistration(`${label} is invalid`);
  return value;
}

function cloneJSON(value, code) {
  try {
    return parseJSON(canonicalStringify(value));
  } catch (error) {
    throw wrap(error, code, "value is not canonical JSON");
  }
}

function freezeJSON(value) {
  if (value && typeof value === "object") {
    for (const entry of Object.values(value)) freezeJSON(entry);
    Object.freeze(value);
  }
  return value;
}

async function readRequestBody(request, maximum, signal) {
  const declared = request.headers.get("content-length");
  if (declared !== null && (!/^[0-9]+$/u.test(declared) || Number(declared) > maximum)) throw new NaatreWorkerError("REMOTE_INVOCATION_INVALID", "worker request exceeds its limit", 413);
  if (!request.body) throw new NaatreWorkerError("REMOTE_INVOCATION_INVALID", "worker request body is required", 400);
  const reader = request.body.getReader();
  const chunks = [];
  let size = 0;
  const cancellation = cancelReaderOnAbort(reader, signal);
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > maximum) throw new NaatreWorkerError("REMOTE_INVOCATION_INVALID", "worker request exceeds its limit", 413);
      chunks.push(value);
    }
    await cancellation.throwIfAborted();
  } catch (error) {
    await Promise.allSettled([reader.cancel(error)]);
    throw error;
  } finally {
    cancellation.dispose();
    reader.releaseLock();
  }
  const output = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    output.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return output;
}

function cancelReaderOnAbort(reader, signal) {
  let result;
  const abort = () => {
    result = reader.cancel(signal.reason).then(
      () => undefined,
      (error) => error,
    );
  };
  if (signal.aborted) abort();
  else signal.addEventListener("abort", abort, { once: true });
  return Object.freeze({
    async throwIfAborted() {
      if (!signal.aborted) return;
      const failure = await result;
      if (failure !== undefined) throw new NaatreWorkerError("CANCELLED", "worker request was canceled", 499, { cause: failure });
      throw canceled();
    },
    dispose() {
      signal.removeEventListener("abort", abort);
    },
  });
}

function response(value, status = 200) {
  return new Response(canonicalStringify(value), { status, headers: { "content-type": mediaType } });
}

function byteLength(value) {
  return new TextEncoder().encode(value).byteLength;
}

function invalidRegistration(message) {
  throw new NaatreWorkerError("REMOTE_REGISTRATION_INVALID", message, 400);
}

function canceled() {
  return new NaatreWorkerError("CANCELLED", "handler invocation was canceled", 499);
}

function wrap(error, code, message) {
  if (error instanceof NaatreWorkerError && error.code === code) return error;
  return new NaatreWorkerError(code, message, code === "OUTPUT_COMPLETION" ? 500 : 400, { cause: error });
}

function asWorkerError(error) {
  if (error instanceof NaatreWorkerError) return error;
  if (error instanceof NaatreClientError) return new NaatreWorkerError("REMOTE_INVOCATION_INVALID", "worker request is invalid", 400, { cause: error });
  return new NaatreWorkerError("REMOTE_WORKER_MALFORMED", "worker request failed", 500, { cause: error });
}
