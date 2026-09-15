#!/usr/bin/env node

import fs from "node:fs";
import { fileURLToPath } from "node:url";

const protocol = "naatre.remote-worker.v1";
const fixturePath = fileURLToPath(new URL("../v1/remote-workers.json", import.meta.url));
const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function validateFixture() {
  requireValue(fixture.profile === "worker.remote-1" && fixture.protocol === protocol, "invalid remote-worker fixture identity");
  requireValue(fixture.schema.sharedSchemaProfile === "core.schema-1" && fixture.schema.generatorProfile === "sdk.generation-1", "remote worker must use shared schema and generator profiles");
  requireValue(fixture.claims.nativeRuntime === false && fixture.claims.productionGateway === false, "fixture overclaims implementation support");
  requireValue(fixture.supportMatrix.map((row) => row.surface).join(",") === "client,codec,native-runtime,remote-worker,http-server,streaming,transaction", "support matrix surfaces are incomplete");
}

function response(kind, payload) {
  return { protocol, kind, payload };
}

function handle(envelope) {
  requireValue(envelope && envelope.protocol === protocol && typeof envelope.kind === "string" && envelope.payload, "invalid envelope");
  if (envelope.kind === "register") {
    const value = envelope.payload;
    requireValue(value.workerId === fixture.registration.workerId && value.schemaRevision === fixture.schema.revision && value.schemaDigest === fixture.schema.digest, "registration mismatch");
    requireValue(value.serviceIdentity === fixture.registration.serviceIdentity && value.audience === fixture.registration.audience, "identity mismatch");
    return response("registered", {protocol, workerId: value.workerId, sessionId: "fixture-session", schemaRevision: value.schemaRevision, acceptedCapabilities: value.capabilities});
  }
  if (envelope.kind === "invoke") {
    const value = envelope.payload;
    requireValue(value.handlerId === "fixture.greet" && value.schemaRevision === fixture.schema.revision, "invocation mismatch");
    requireValue(value.delegatedContext === "valid-delegation", "delegated context mismatch");
    const rejected = value.input.name === "reject";
    return response("result", {
      protocol,
      invocationId: value.invocationId,
      attemptId: value.attemptId,
      schemaRevision: value.schemaRevision,
      ...(rejected ? {data: null, errors: [{code: "NAME_REJECTED", message: "name was rejected", retryable: false}]} : {data: {greeting: `Hello, ${value.input.name}`}, errors: []}),
    });
  }
  if (envelope.kind === "cancel") {
    return response("cancelled", {protocol, invocationId: envelope.payload.invocationId, disposition: "acknowledged"});
  }
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
  process.stdin.on("data", (chunk) => {
    buffered = Buffer.concat([buffered, chunk]);
    while (buffered.length >= 5) {
      const flags = buffered.readUInt8(0);
      const size = buffered.readUInt32BE(1);
      requireValue(flags === 0 && size > 0 && size <= fixture.transport.frame.maximumBytes, "invalid frame");
      if (buffered.length < 5 + size) return;
      const payload = JSON.parse(buffered.subarray(5, 5 + size).toString("utf8"));
      buffered = buffered.subarray(5 + size);
      process.stdout.write(encodeFrame(handle(payload)));
    }
  });
}

validateFixture();
if (process.argv.includes("--serve")) serve();
else process.stdout.write(JSON.stringify({profile: fixture.profile, status: "passed", operations: fixture.operations.map((operation) => operation.expected)}) + "\n");
