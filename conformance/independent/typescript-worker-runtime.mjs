import { createWorker } from "../../sdk/typescript/runtime/server.mjs";
import { createGreetHandler as createPlainHandler } from "../../sdk/typescript/runtime/fixtures/plain-worker-handler.mjs";
import { runtimeID } from "./runtime-id.mjs";

const protocol = "naatre.remote-worker.v1";
const remoteVectors = Object.freeze(["schema-defined-success", "schema-defined-error"]);

export async function runWorkerRuntime(runtime = runtimeID("NAATRE_WORKER_RUNTIME_ID", "unknown-fetch-runtime"), options = {}) {
  const implementations = [["plain-javascript", createPlainHandler]];
  if (options.typescript !== false) {
    const typed = await import("../../sdk/typescript/runtime/fixtures/typed-worker-handler.test.ts");
    implementations.push(["typescript", typed.createGreetHandler]);
  }
  const results = [];
  for (const [language, factory] of implementations) {
    const worker = fixtureWorker(factory());
    const registration = await worker.register(worker.registration());
    equal(registration.workerId, "fixture-worker");
    const success = await worker.invoke(invocation({ input: { name: "Ada" } }));
    equal(success.data.greeting, "Hello, Ada");
    equal(success.errors.length, 0);
    const rejected = await worker.invoke(invocation({ invocationId: `${language}-rejected`, attemptId: `${language}-attempt`, input: { name: "reject" } }));
    equal(rejected.data, null);
    equal(rejected.errors[0].code, "NAME_REJECTED");
    results.push(Object.freeze({ language, vectors: remoteVectors }));
  }
  return Object.freeze({
    profile: "worker.javascript-typescript-1",
    protocol,
    runtime,
    status: "passed",
    implementations: Object.freeze(results),
    vectors: remoteVectors,
  });
}

function fixtureWorker(handler) {
  return createWorker({
    workerId: "fixture-worker",
    serviceIdentity: "spiffe://example/fixture-worker",
    audience: "naatre-gateway",
    endpoint: "fixture-fetch",
    schemaRevision: "schema-1",
    schemaDigest: "a".repeat(64),
    capabilities: ["unary-1", "cancellation-ack-1"],
    limits: { maxInFlight: 2, maxRequestBytes: 4096, maxResponseBytes: 4096, maxStreamFrames: 4, maxStreamBytes: 16384 },
    handlers: [handler],
    authenticate: async ({ delegatedContext }) => {
      if (delegatedContext !== "valid-delegation") throw new Error("invalid delegation");
      return { principal: "fixture-principal", tenant: "fixture-tenant" };
    },
  });
}

function invocation(overrides = {}) {
  return {
    protocol,
    requestId: "fixture-request",
    invocationId: "fixture-invocation",
    attemptId: "fixture-attempt",
    handlerId: "fixture.greet",
    schemaRevision: "schema-1",
    deadlineUnixMilli: Date.now() + 60_000,
    delegatedContext: "valid-delegation",
    input: { name: "Ada" },
    ...overrides,
  };
}

function equal(actual, expected) {
  if (actual !== expected) throw new Error(`expected ${String(expected)}, received ${String(actual)}`);
}

if (!globalThis.NAATRE_WORKER_NO_AUTORUN) {
  const result = await runWorkerRuntime();
  if (typeof globalThis.process !== "undefined") globalThis.process.stdout.write(`${JSON.stringify(result)}\n`);
  else if (typeof globalThis.console !== "undefined") globalThis.console.log(JSON.stringify(result));
}
