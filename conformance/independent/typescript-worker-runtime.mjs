import { createWorker, defineHandler, NaatreWorkerError } from "../../sdk/typescript/runtime/server.mjs";
import { createBunWorkerAdapter } from "../../sdk/typescript/runtime/worker-bun.mjs";
import { createDenoWorkerAdapter } from "../../sdk/typescript/runtime/worker-deno.mjs";
import { createEdgeWorkerAdapter } from "../../sdk/typescript/runtime/worker-edge.mjs";
import { createNodeWorkerAdapter } from "../../sdk/typescript/runtime/worker-node.mjs";
import { createGreetHandler as createPlainHandler } from "../../sdk/typescript/runtime/fixtures/plain-worker-handler.mjs";
import { greetInput, greetOutput, handlerDefinition, stringSchema } from "../../sdk/typescript/runtime/fixtures/worker-test-contract.mjs";
import { runtimeID } from "./runtime-id.mjs";

const protocol = "naatre.remote-worker.v1";
const vectors = Object.freeze([
  "registration",
  "prototype-safety",
  "abort-signal",
  "streaming",
  "backpressure",
  "warm-instance-isolation",
  "output-validation",
  "shutdown",
  "stable-public-failures",
  "resource-limits",
]);

export async function runWorkerRuntime(runtime = runtimeID("NAATRE_WORKER_RUNTIME_ID", "unknown-fetch-runtime"), options = {}) {
  const implementations = [["plain-javascript", createPlainHandler]];
  if (options.typescript !== false) {
    const typed = await import("../../sdk/typescript/runtime/fixtures/typed-worker-handler.test.ts");
    implementations.push(["typescript", typed.createGreetHandler]);
  }
  const createAdapter = adapterFactory(runtime);
  const results = [];
  for (const [language, factory] of implementations) {
    await runFixtures(createAdapter, factory);
    results.push(Object.freeze({ language, vectors }));
  }
  return Object.freeze({
    profile: "worker.javascript-typescript-1",
    protocol,
    runtime,
    runtimeProfile: createAdapter.profile,
    status: "passed",
    implementations: Object.freeze(results),
    vectors,
  });
}

async function runFixtures(createAdapter, greetFactory) {
  await verifyRegistration(createAdapter, greetFactory);
  await verifyPrototypeSafety(createAdapter, greetFactory);
  await verifyAbortSignal(createAdapter);
  await verifyStreamingAndBackpressure();
  await verifyWarmIsolation();
  await verifyOutputValidation();
  await verifyShutdown(createAdapter);
  await verifySafeFailures(createAdapter, greetFactory);
  await verifyResourceLimits(createAdapter);
}

async function verifyRegistration(createAdapter, greetFactory) {
  const worker = fixtureWorker([greetFactory()]);
  const adapter = createAdapter(worker);
  equal(adapter.status().phase, "ready");
  const registered = await adapter.fetch(workerRequest("Register", worker.registration()));
  equal(registered.status, 200);
  equal(registered.headers.get("content-type"), "application/naatre-worker+json");
  equal((await registered.json()).workerId, "fixture-worker");
  const success = await adapter.fetch(workerRequest("Invoke", invocation({ input: { name: "Ada" } })));
  equal(success.status, 200);
  equal((await success.json()).data.greeting, "Hello, Ada");
  const rejected = await adapter.fetch(workerRequest("Invoke", invocation({ invocationId: "rejected", attemptId: "rejected-attempt", input: { name: "reject" } })));
  equal((await rejected.json()).errors[0].code, "NAME_REJECTED");
  await adapter.shutdown();
}

async function verifyPrototypeSafety(createAdapter, greetFactory) {
  for (const id of ["__proto__", "constructor", "prototype", "fixture.__proto__", "fixture.constructor", "fixture.prototype"]) {
    await expectWorkerError(() => defineHandler(handlerDefinition(id, greetInput, greetOutput), async () => ({ greeting: "bad" })), "REMOTE_REGISTRATION_INVALID");
  }
  const adapter = createAdapter(fixtureWorker([greetFactory()]));
  for (const handlerId of ["__proto__", "constructor", "prototype", "toString", "hasOwnProperty"]) {
    const failure = await (await adapter.fetch(workerRequest("Invoke", invocation({ handlerId })))).json();
    includes(["REMOTE_INVOCATION_INVALID", "REMOTE_HANDLER_UNKNOWN"], failure.code);
  }
  equal(Object.prototype.polluted, undefined);
  await adapter.shutdown();
}

async function verifyAbortSignal(createAdapter) {
  let started;
  let cleaned = 0;
  const entered = new Promise((resolve) => { started = resolve; });
  const slow = defineHandler(handlerDefinition("fixture.slow", greetInput, greetOutput), async (_input, context) => {
    context.onCleanup(() => { cleaned += 1; });
    started();
    await new Promise((resolve) => context.signal.addEventListener("abort", resolve, { once: true }));
    return { greeting: "late" };
  });
  const adapter = createAdapter(fixtureWorker([slow]));
  const controller = new AbortController();
  const pending = adapter.fetch(workerRequest("Invoke", invocation({ handlerId: "fixture.slow" }), controller.signal));
  await entered;
  controller.abort(new DOMException("caller canceled", "AbortError"));
  const response = await pending;
  equal(response.status, 499);
  equal((await response.json()).code, "CANCELLED");
  equal(cleaned, 1);
  await adapter.shutdown();
}

async function verifyStreamingAndBackpressure() {
  let pulls = 0;
  let closed = 0;
  const streamHandler = defineHandler({ ...handlerDefinition("fixture.events", greetInput, stringSchema), effect: "subscription" }, async function* () {
    try {
      for (;;) {
        pulls += 1;
        yield `event-${pulls}`;
      }
    } finally {
      closed += 1;
    }
  });
  const worker = fixtureWorker([streamHandler], { capabilities: ["server-streaming-1"], maxStreamFrames: 2 });
  const iterator = (await worker.stream(invocation({ handlerId: "fixture.events" })))[Symbol.asyncIterator]();
  equal(pulls, 0);
  equal((await iterator.next()).value, "event-1");
  equal(pulls, 1);
  equal((await iterator.next()).value, "event-2");
  await expectWorkerError(() => iterator.next(), "OVERLOADED");
  equal(closed, 1);
  equal(worker.metrics().activeStreams, 0);
}

async function verifyWarmIsolation() {
  let release;
  const entered = [];
  const barrier = new Promise((resolve) => { release = resolve; });
  const isolated = defineHandler(handlerDefinition("fixture.isolated", greetInput, greetOutput), async (input, context) => {
    context.cache.set("tenant", context.tenant);
    context.state.owner = context.principal;
    entered.push(context.requestId);
    if (entered.length === 2) release();
    await barrier;
    return { greeting: `${context.cache.get("tenant")}:${input.name}:${context.state.owner}` };
  });
  const worker = fixtureWorker([isolated]);
  const [first, second] = await Promise.all([
    worker.invoke(invocation({ requestId: "request-a", invocationId: "invocation-a", attemptId: "attempt-a", delegatedContext: "tenant-a", handlerId: "fixture.isolated", input: { name: "Ada" } })),
    worker.invoke(invocation({ requestId: "request-b", invocationId: "invocation-b", attemptId: "attempt-b", delegatedContext: "tenant-b", handlerId: "fixture.isolated", input: { name: "Bob" } })),
  ]);
  equal(first.data.greeting, "tenant-a:Ada:principal-tenant-a");
  equal(second.data.greeting, "tenant-b:Bob:principal-tenant-b");
  equal(worker.metrics().activeInvocations, 0);
}

async function verifyOutputValidation() {
  const invalid = defineHandler(handlerDefinition("fixture.invalid-output", greetInput, greetOutput), async () => ({ greeting: undefined }));
  const worker = fixtureWorker([invalid]);
  await expectWorkerError(() => worker.invoke(invocation({ handlerId: "fixture.invalid-output" })), "OUTPUT_COMPLETION");
}

async function verifyShutdown(createAdapter) {
  let started;
  let cleaned = 0;
  const entered = new Promise((resolve) => { started = resolve; });
  const slow = defineHandler(handlerDefinition("fixture.shutdown", greetInput, greetOutput), async (_input, context) => {
    context.onCleanup(() => { cleaned += 1; });
    started();
    await new Promise((resolve) => context.signal.addEventListener("abort", resolve, { once: true }));
    return { greeting: "late" };
  });
  const adapter = createAdapter(fixtureWorker([slow]));
  const pending = adapter.fetch(workerRequest("Invoke", invocation({ handlerId: "fixture.shutdown" })));
  await entered;
  const firstShutdown = adapter.shutdown();
  const secondShutdown = adapter.shutdown();
  equal(firstShutdown, secondShutdown);
  equal(adapter.status().phase, "draining");
  const canceled = await pending;
  equal((await canceled.json()).code, "CANCELLED");
  await firstShutdown;
  equal(cleaned, 1);
  equal(adapter.status().phase, "stopped");
  const unavailable = await adapter.fetch(workerRequest("Invoke", invocation({ delegatedContext: "credential-never-echo" })));
  equal(unavailable.status, 503);
  const failureText = await unavailable.text();
  equal(JSON.parse(failureText).code, "OVERLOADED");
  excludes(failureText, "credential-never-echo");
}

async function verifySafeFailures(createAdapter, greetFactory) {
  const adapter = createAdapter(fixtureWorker([greetFactory()]));
  const secret = "credential-private-metadata-stack";
  const response = await adapter.fetch(workerRequest("Invoke", invocation({ delegatedContext: secret })));
  const text = await response.text();
  equal(response.status, 403);
  equal(JSON.parse(text).code, "UNAUTHORIZED");
  excludes(text, secret);
  excludes(text, "Error");
  await adapter.shutdown();
}

async function verifyResourceLimits(createAdapter) {
  let release;
  let started;
  const barrier = new Promise((resolve) => { release = resolve; });
  const entered = new Promise((resolve) => { started = resolve; });
  const limited = defineHandler(handlerDefinition("fixture.limited", greetInput, greetOutput), async () => {
    started();
    await barrier;
    return { greeting: "done" };
  });
  const worker = fixtureWorker([limited], { maxInFlight: 1 });
  const adapter = createAdapter(worker);
  const first = worker.invoke(invocation({ handlerId: "fixture.limited" }));
  await entered;
  await expectWorkerError(() => worker.invoke(invocation({ invocationId: "over-limit", attemptId: "over-limit-attempt", handlerId: "fixture.limited" })), "OVERLOADED");
  release();
  await first;
  const oversized = await adapter.fetch(workerRequest("Invoke", invocation({ handlerId: "fixture.limited", input: { name: "x".repeat(8192) } })));
  equal(oversized.status, 413);
  equal((await oversized.json()).code, "REMOTE_INVOCATION_INVALID");
  await adapter.shutdown();
}

function adapterFactory(runtime) {
  if (runtime.startsWith("node-")) return taggedFactory(createNodeWorkerAdapter, "worker.javascript-typescript.node-1");
  if (runtime.startsWith("bun-")) return taggedFactory(createBunWorkerAdapter, "worker.javascript-typescript.bun-1");
  if (runtime.startsWith("deno-")) return taggedFactory(createDenoWorkerAdapter, "worker.javascript-typescript.deno-1");
  if (runtime.startsWith("workerd-")) return taggedFactory(createEdgeWorkerAdapter, "worker.javascript-typescript.edge-1");
  throw new Error("unsupported worker conformance runtime");
}

function taggedFactory(factory, profile) {
  const tagged = (worker) => factory(worker);
  tagged.profile = profile;
  return tagged;
}

function fixtureWorker(handlers, options = {}) {
  return createWorker({
    workerId: "fixture-worker",
    serviceIdentity: "spiffe://example/fixture-worker",
    audience: "naatre-gateway",
    endpoint: "fixture-fetch",
    schemaRevision: "schema-1",
    schemaDigest: "a".repeat(64),
    capabilities: ["unary-1", "cancellation-ack-1", ...(options.capabilities ?? [])],
    limits: { maxInFlight: options.maxInFlight ?? 2, maxRequestBytes: 4096, maxResponseBytes: 4096, maxStreamFrames: options.maxStreamFrames ?? 4, maxStreamBytes: 16384 },
    handlers,
    authenticate: async ({ delegatedContext }) => {
      if (!delegatedContext.startsWith("tenant-")) throw new Error(`rejected protected value ${delegatedContext}`);
      return { principal: `principal-${delegatedContext}`, tenant: delegatedContext };
    },
  });
}

function invocation(overrides = {}) {
  return { protocol, requestId: "request-1", invocationId: "invocation-1", attemptId: "attempt-1", handlerId: "fixture.greet", schemaRevision: "schema-1", deadlineUnixMilli: Date.now() + 60_000, delegatedContext: "tenant-a", input: { name: "Ada" }, ...overrides };
}

function workerRequest(method, body, signal = undefined) {
  return new Request(`https://worker.example/naatre.remote-worker.v1.Worker/${method}`, {
    method: "POST",
    headers: { "content-type": "application/naatre-worker+json" },
    body: JSON.stringify(body),
    ...(signal === undefined ? {} : { signal }),
  });
}

async function expectWorkerError(operation, code) {
  try {
    await operation();
  } catch (error) {
    if (error instanceof NaatreWorkerError && error.code === code) return;
    throw error;
  }
  throw new Error(`expected worker error ${code}`);
}

function equal(actual, expected) {
  if (actual !== expected) throw new Error(`expected ${String(expected)}, received ${String(actual)}`);
}

function includes(values, expected) {
  if (!values.includes(expected)) throw new Error(`expected one of ${values.join(", ")}, received ${String(expected)}`);
}

function excludes(value, prohibited) {
  if (value.includes(prohibited)) throw new Error("public failure exposed protected data");
}

if (!globalThis.NAATRE_WORKER_NO_AUTORUN) {
  const result = await runWorkerRuntime();
  if (typeof globalThis.process !== "undefined") globalThis.process.stdout.write(`${JSON.stringify(result)}\n`);
  else if (typeof globalThis.console !== "undefined") globalThis.console.log(JSON.stringify(result));
}
