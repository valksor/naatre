#!/usr/bin/env node

import fs from "node:fs";
import { fileURLToPath } from "node:url";

import { createWorker } from "../../sdk/typescript/runtime/server.mjs";
import { createGreetHandler as createPlainHandler } from "../../sdk/typescript/runtime/fixtures/plain-worker-handler.mjs";

const protocol = "naatre.remote-worker.v1";
const fixturePath = fileURLToPath(new URL("../v1/remote-workers.json", import.meta.url));
const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));
const handlerLanguage = process.env.NAATRE_HANDLER_LANGUAGE ?? "plain-javascript";
const createHandler = handlerLanguage === "typescript"
  ? (await import("../../sdk/typescript/runtime/fixtures/typed-worker-handler.test.ts")).createGreetHandler
  : createPlainHandler;
const worker = createWorker({
  workerId: fixture.registration.workerId,
  serviceIdentity: fixture.registration.serviceIdentity,
  audience: fixture.registration.audience,
  endpoint: fixture.registration.endpoint,
  schemaRevision: fixture.registration.schemaRevision,
  schemaDigest: fixture.registration.schemaDigest,
  capabilities: fixture.registration.capabilities,
  limits: fixture.registration.limits,
  handlers: [createHandler()],
  authenticate: async ({ delegatedContext }) => {
    if (delegatedContext !== "valid-delegation") throw new Error("delegated context rejected");
    return { principal: "fixture-principal", tenant: "fixture-tenant" };
  },
  now: () => Date.parse("2026-09-15T12:00:00Z"),
});

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function validateFixture() {
  requireValue(fixture.profile === "worker.remote-1" && fixture.protocol === protocol, "invalid remote-worker fixture identity");
  requireValue(fixture.schema.sharedSchemaProfile === "core.schema-1" && fixture.schema.generatorProfile === "sdk.generation-1", "remote worker must use shared schema and generator profiles");
  requireValue(fixture.claims.nativeRuntime === false && fixture.claims.productionGateway === false, "fixture overclaims implementation support");
  requireValue(fixture.supportMatrix.map((row) => row.surface).join(",") === "client,codec,native-runtime,remote-worker,http-server,streaming,transaction", "support matrix surfaces are incomplete");
  requireValue(["plain-javascript", "typescript"].includes(handlerLanguage), "unsupported handler language");
}

function response(kind, payload) {
  return { protocol, kind, payload };
}

async function handle(envelope) {
  requireValue(envelope && envelope.protocol === protocol && typeof envelope.kind === "string" && envelope.payload, "invalid envelope");
  if (envelope.kind === "register") return response("registered", await worker.register(envelope.payload));
  if (envelope.kind === "invoke") return response("result", await worker.invoke(envelope.payload));
  if (envelope.kind === "cancel") return response("cancelled", await worker.cancel(envelope.payload));
  throw new Error("unsupported envelope kind");
}

function encodeFrame(value) {
  const payload = Buffer.from(JSON.stringify(value));
  const header = Buffer.alloc(5);
  header.writeUInt8(0, 0);
  header.writeUInt32BE(payload.length, 1);
  return Buffer.concat([header, payload]);
}

function serve() {
  let buffered = Buffer.alloc(0);
  let pending = Promise.resolve();
  process.stdin.on("data", (chunk) => {
    buffered = Buffer.concat([buffered, chunk]);
    while (buffered.length >= 5) {
      const flags = buffered.readUInt8(0);
      const size = buffered.readUInt32BE(1);
      requireValue(flags === 0 && size > 0 && size <= fixture.transport.frame.maximumBytes, "invalid frame");
      if (buffered.length < 5 + size) return;
      const payload = JSON.parse(buffered.subarray(5, 5 + size).toString("utf8"));
      buffered = buffered.subarray(5 + size);
      pending = pending.then(async () => process.stdout.write(encodeFrame(await handle(payload))));
    }
  });
  process.stdin.on("end", () => pending.catch((error) => {
    process.stderr.write(`${String(error?.stack ?? error)}\n`);
    process.exitCode = 1;
  }));
}

async function verify() {
  await worker.register(worker.registration());
  const results = [];
  let index = 0;
  for (const operation of fixture.operations) {
    index += 1;
    const result = await worker.invoke({
      protocol,
      requestId: `fixture-request-${index}`,
      invocationId: `fixture-invocation-${index}`,
      attemptId: `fixture-attempt-${index}`,
      handlerId: operation.handlerId,
      schemaRevision: fixture.schema.revision,
      deadlineUnixMilli: Date.parse("2026-09-15T12:01:00Z"),
      delegatedContext: "valid-delegation",
      input: operation.input,
    });
    results.push({ data: result.data, errors: result.errors });
  }
  return { profile: fixture.profile, status: "passed", handlerLanguage, operations: results };
}

validateFixture();
if (process.argv.includes("--serve")) serve();
else process.stdout.write(`${JSON.stringify(await verify())}\n`);
