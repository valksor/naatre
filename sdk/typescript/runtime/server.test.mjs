import assert from "node:assert/strict";
import test from "node:test";

import {
  NaatreWorkerError,
  canonicalStringify,
  createFetchWorkerAdapter,
  createWorker,
  defineHandler,
} from "./index.mjs";

const protocol = "naatre.remote-worker.v1";
const schemaDigest = "a".repeat(64);
const stringSchema = Object.freeze({ kind: "scalar", type: "String" });
const numberSchema = Object.freeze({ kind: "scalar", type: "Float64" });
const greetInput = objectSchema([
  { name: "name", schema: stringSchema, required: true, nullable: false },
]);
const greetOutput = objectSchema([
  { name: "greeting", schema: stringSchema, required: true, nullable: false },
]);

test("plain JavaScript handlers execute the remote-worker success and error vectors", async () => {
  const worker = fixtureWorker(defineHandler(handlerDefinition("fixture.greet", greetInput, greetOutput), async (input) => {
    if (input.name === "reject") throw NaatreWorkerError.application("NAME_REJECTED", "name was rejected");
    return { greeting: `Hello, ${input.name}` };
  }));

  const success = await worker.invoke(invocation({ input: { name: "Ada" } }));
  assert.deepEqual(success.data, nullObject({ greeting: "Hello, Ada" }));
  assert.deepEqual(success.errors, []);

  const rejected = await worker.invoke(invocation({ invocationId: "invocation-2", attemptId: "attempt-2", input: { name: "reject" } }));
  assert.equal(rejected.data, null);
  assert.deepEqual(rejected.errors, [{ code: "NAME_REJECTED", message: "name was rejected", retryable: false }]);
});

test("registration and dispatch never traverse prototype or native object members", async () => {
  for (const id of ["__proto__", "constructor", "prototype", "fixture.__proto__", "fixture.constructor", "fixture.prototype"]) {
    assert.throws(
      () => defineHandler(handlerDefinition(id, greetInput, greetOutput), async () => ({ greeting: "bad" })),
      workerError("REMOTE_REGISTRATION_INVALID"),
    );
  }

  const worker = fixtureWorker(defineHandler(handlerDefinition("fixture.greet", greetInput, greetOutput), async () => ({ greeting: "ok" })));
  for (const handlerId of ["__proto__", "constructor", "prototype", "toString", "hasOwnProperty"]) {
    await assert.rejects(worker.invoke(invocation({ handlerId })), (error) => error instanceof NaatreWorkerError && ["REMOTE_INVOCATION_INVALID", "REMOTE_HANDLER_UNKNOWN"].includes(error.code));
  }
  assert.equal(Object.prototype.polluted, undefined);
});

test("concurrent warm-instance invocations isolate authentication, caches, loaders, and mutable state", async () => {
  const entered = [];
  let release;
  const barrier = new Promise((resolve) => { release = resolve; });
  const handler = defineHandler(handlerDefinition("fixture.greet", greetInput, greetOutput), async (input, context) => {
    context.cache.set("tenant", context.tenant);
    context.loaders.set("name", input.name);
    context.state.owner = context.principal;
    entered.push(context.requestId);
    if (entered.length === 2) release();
    await barrier;
    return { greeting: `${context.cache.get("tenant")}:${context.loaders.get("name")}:${context.state.owner}` };
  });
  const worker = fixtureWorker(handler);

  const [first, second] = await Promise.all([
    worker.invoke(invocation({ requestId: "request-a", invocationId: "invocation-a", attemptId: "attempt-a", delegatedContext: "tenant-a", input: { name: "Ada" } })),
    worker.invoke(invocation({ requestId: "request-b", invocationId: "invocation-b", attemptId: "attempt-b", delegatedContext: "tenant-b", input: { name: "Bob" } })),
  ]);
  assert.equal(first.data.greeting, "tenant-a:Ada:principal-tenant-a");
  assert.equal(second.data.greeting, "tenant-b:Bob:principal-tenant-b");
  assert.deepEqual(worker.metrics(), { activeInvocations: 0, cancellationRequested: 0, activeStreams: 0 });
});

test("cancellation releases registered I/O while ignored work remains accounted until settlement", async () => {
  let finish;
  let markStarted;
  let cleaned = 0;
  const pending = new Promise((resolve) => { finish = resolve; });
  const started = new Promise((resolve) => { markStarted = resolve; });
  const handler = defineHandler(handlerDefinition("fixture.greet", greetInput, greetOutput), async (_input, context) => {
    context.onCleanup(() => { cleaned += 1; });
    markStarted();
    return pending;
  });
  const worker = fixtureWorker(handler);
  const result = worker.invoke(invocation({ input: { name: "slow" } }));
  await started;

  assert.deepEqual(worker.metrics(), { activeInvocations: 1, cancellationRequested: 0, activeStreams: 0 });
  const acknowledgement = await worker.cancel({ protocol, requestId: "request-1", invocationId: "invocation-1" });
  assert.equal(acknowledgement.disposition, "acknowledged");
  assert.equal(cleaned, 1);
  assert.deepEqual(worker.metrics(), { activeInvocations: 1, cancellationRequested: 1, activeStreams: 0 });

  finish({ greeting: "too late" });
  await assert.rejects(result, workerError("CANCELLED"));
  assert.equal(cleaned, 1);
  assert.deepEqual(worker.metrics(), { activeInvocations: 0, cancellationRequested: 0, activeStreams: 0 });
});

test("cleanup failures never leak active invocation accounting", async () => {
  const handler = defineHandler(handlerDefinition("fixture.greet", greetInput, greetOutput), async (_input, context) => {
    context.onCleanup(() => { throw new Error("cleanup failed"); });
    return { greeting: "done" };
  });
  const worker = fixtureWorker(handler);

  await assert.rejects(worker.invoke(invocation()), workerError("INTERNAL"));
  assert.deepEqual(worker.metrics(), { activeInvocations: 0, cancellationRequested: 0, activeStreams: 0 });
});

test("abandoned streams are pull-driven and release their source and request resources", async () => {
  let pulls = 0;
  let sourceClosed = 0;
  let requestCleaned = 0;
  const handler = defineHandler({
    ...handlerDefinition("fixture.events", greetInput, stringSchema),
    effect: "subscription",
  }, async function* (_input, context) {
    context.onCleanup(() => { requestCleaned += 1; });
    try {
      for (;;) {
        pulls += 1;
        yield `event-${pulls}`;
      }
    } finally {
      sourceClosed += 1;
    }
  });
  const worker = fixtureWorker(handler, ["server-streaming-1"]);
  const stream = await worker.stream(invocation({ handlerId: "fixture.events" }));
  const iterator = stream[Symbol.asyncIterator]();

  assert.equal(pulls, 0);
  assert.deepEqual(await iterator.next(), { done: false, value: "event-1" });
  assert.equal(pulls, 1);
  assert.deepEqual(worker.metrics(), { activeInvocations: 1, cancellationRequested: 0, activeStreams: 1 });
  await iterator.return();
  assert.equal(sourceClosed, 1);
  assert.equal(requestCleaned, 1);
  assert.deepEqual(worker.metrics(), { activeInvocations: 0, cancellationRequested: 0, activeStreams: 0 });
});

test("invalid AsyncIterable initialization releases request resources and accounting", async () => {
  const sources = [
    { [Symbol.asyncIterator]() { throw new Error("iterator creation failed"); } },
    { [Symbol.asyncIterator]() { return {}; } },
  ];
  for (const [index, source] of sources.entries()) {
    let cleaned = 0;
    const handler = defineHandler({
      ...handlerDefinition(`fixture.invalid-stream-${index}`, greetInput, stringSchema),
      effect: "subscription",
    }, async (_input, context) => {
      context.onCleanup(() => { cleaned += 1; });
      return source;
    });
    const worker = fixtureWorker(handler, ["server-streaming-1"]);

    await assert.rejects(worker.stream(invocation({ handlerId: `fixture.invalid-stream-${index}` })), workerError("REMOTE_WORKER_MALFORMED"));
    assert.equal(cleaned, 1);
    assert.deepEqual(worker.metrics(), { activeInvocations: 0, cancellationRequested: 0, activeStreams: 0 });
  }
});

test("worker and process isolation profiles require an explicit host executor", async () => {
  let observedProfile;
  const handler = defineHandler({
    ...handlerDefinition("fixture.isolated", greetInput, greetOutput),
    executionProfile: "worker",
  }, async (_input, context) => {
    observedProfile = context.executionProfile;
    return { greeting: "isolated" };
  });
  assert.throws(() => fixtureWorker(handler), workerError("REMOTE_REGISTRATION_INVALID"));

  const worker = createWorker({
    workerId: "fixture-worker",
    serviceIdentity: "spiffe://example/fixture-worker",
    audience: "naatre-gateway",
    endpoint: "fixture-fetch",
    schemaRevision: "schema-1",
    schemaDigest,
    capabilities: ["unary-1", "cancellation-ack-1"],
    limits: { maxInFlight: 8, maxRequestBytes: 4096, maxResponseBytes: 4096, maxStreamFrames: 4, maxStreamBytes: 16384 },
    handlers: [handler],
    authenticate: async () => ({ principal: "principal", tenant: "tenant" }),
    executors: { worker: (execute, input, context) => execute(input, context) },
  });
  const result = await worker.invoke(invocation({ handlerId: "fixture.isolated" }));
  assert.equal(result.data.greeting, "isolated");
  assert.equal(observedProfile, "worker");
});

test("output completion rejects every prohibited JavaScript shape before producing a result", async () => {
  const closedUnion = Object.freeze({
    kind: "union",
    tag: "$type",
    value: "$value",
    variants: Object.freeze([{ tag: "Known", schema: greetOutput }]),
  });
  const cases = [
    ["undefined", greetOutput, { greeting: undefined }],
    ["sparse-array", Object.freeze({ kind: "list", element: stringSchema, elementNullable: false }), new Array(1)],
    ["accessor-array", Object.freeze({ kind: "list", element: stringSchema, elementNullable: false }), accessorArray()],
    ["accessor-object", greetOutput, accessorObject()],
    ["non-finite", numberSchema, Number.POSITIVE_INFINITY],
    ["large-number-loss", numberSchema, 9007199254740992],
    ["unknown-tag", closedUnion, { $type: "Unknown", $value: { greeting: "no" } }],
    ["nullability", greetOutput, { greeting: null }],
  ];
  for (const [name, output, value] of cases) {
    const handler = defineHandler(handlerDefinition(`fixture.${name}`, greetInput, output), async () => value);
    const worker = fixtureWorker(handler);
    await assert.rejects(worker.invoke(invocation({ handlerId: `fixture.${name}` })), workerError("OUTPUT_COMPLETION"), name);
  }
});

test("Fetch adapter handles registration and invocation without owning a listener", async () => {
  const worker = fixtureWorker(defineHandler(handlerDefinition("fixture.greet", greetInput, greetOutput), async (input) => ({ greeting: `Hello, ${input.name}` })));
  const fetchWorker = createFetchWorkerAdapter(worker);
  const registration = worker.registration();

  const registered = await fetchWorker(workerRequest("Register", registration));
  assert.equal(registered.status, 200);
  assert.equal(registered.headers.get("content-type"), "application/naatre-worker+json");
  const acknowledgement = await registered.json();
  assert.equal(acknowledgement.workerId, "fixture-worker");

  const response = await fetchWorker(workerRequest("Invoke", invocation({ input: { name: "Ada" } })));
  assert.equal(response.status, 200);
  const result = await response.json();
  assert.equal(result.data.greeting, "Hello, Ada");

  const invalid = await fetchWorker(new Request("https://worker.example/naatre.remote-worker.v1.Worker/Invoke", {
    method: "POST",
    headers: { "content-type": "application/naatre-worker+json" },
    body: canonicalStringify({ ...invocation(), input: { name: 3 } }),
  }));
  assert.equal(invalid.status, 400);
  assert.equal((await invalid.json()).code, "REMOTE_INVOCATION_INVALID");
});

function fixtureWorker(handler, extraCapabilities = []) {
  return createWorker({
    workerId: "fixture-worker",
    serviceIdentity: "spiffe://example/fixture-worker",
    audience: "naatre-gateway",
    endpoint: "fixture-fetch",
    schemaRevision: "schema-1",
    schemaDigest,
    capabilities: ["unary-1", "cancellation-ack-1", ...extraCapabilities],
    limits: { maxInFlight: 8, maxRequestBytes: 4096, maxResponseBytes: 4096, maxStreamFrames: 4, maxStreamBytes: 16384 },
    handlers: [handler],
    authenticate: async ({ delegatedContext }) => {
      if (!delegatedContext.startsWith("tenant-")) throw new NaatreWorkerError("UNAUTHORIZED", "delegated context rejected", 403);
      return { principal: `principal-${delegatedContext}`, tenant: delegatedContext };
    },
    createRequestState: () => ({ cache: new Map(), loaders: new Map(), state: Object.create(null) }),
  });
}

function handlerDefinition(id, input, output) {
  return {
    id,
    inputSchema: `${id}.input`,
    outputSchema: `${id}.output`,
    codec: "naatre.json-1",
    effect: "query",
    requiredCapabilities: [],
    executionProfile: "inline",
    input,
    output,
  };
}

function objectSchema(fields) {
  return Object.freeze({ kind: "object", fields: Object.freeze(fields.map(Object.freeze)), additionalProperties: false });
}

function invocation(overrides = {}) {
  return {
    protocol,
    requestId: "request-1",
    invocationId: "invocation-1",
    attemptId: "attempt-1",
    handlerId: "fixture.greet",
    schemaRevision: "schema-1",
    deadlineUnixMilli: Date.now() + 60_000,
    delegatedContext: "tenant-a",
    input: { name: "Ada" },
    ...overrides,
  };
}

function workerRequest(method, body) {
  return new Request(`https://worker.example/naatre.remote-worker.v1.Worker/${method}`, {
    method: "POST",
    headers: { "content-type": "application/naatre-worker+json" },
    body: canonicalStringify(body),
  });
}

function workerError(code) {
  return { name: "NaatreWorkerError", code };
}

function nullObject(value) {
  return Object.assign(Object.create(null), value);
}

function accessorArray() {
  const value = [];
  Object.defineProperty(value, "0", { enumerable: true, get: () => "hidden" });
  value.length = 1;
  return value;
}

function accessorObject() {
  const value = {};
  Object.defineProperty(value, "greeting", { enumerable: true, get: () => "hidden" });
  return value;
}
