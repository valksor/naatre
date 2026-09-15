#!/usr/bin/env node

import fs from "node:fs";
import { fileURLToPath } from "node:url";

const protocol = "naatre.remote-worker.v1";
const workerFixture = JSON.parse(fs.readFileSync(fileURLToPath(new URL("../v1/remote-workers.json", import.meta.url)), "utf8"));
const streamFixture = JSON.parse(fs.readFileSync(fileURLToPath(new URL("../v1/streaming.json", import.meta.url)), "utf8"));
const maximumFrameBytes = workerFixture.transport.frame.maximumBytes;

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function response(kind, payload) {
  return {protocol, kind, payload};
}

function handle(envelope) {
  requireValue(envelope && envelope.protocol === protocol && typeof envelope.kind === "string" && envelope.payload, "invalid envelope");
  if (envelope.kind === "register") {
    const value = envelope.payload;
    requireValue(value.workerId === workerFixture.registration.workerId && value.schemaRevision === workerFixture.schema.revision && value.schemaDigest === workerFixture.schema.digest, "registration mismatch");
    requireValue(value.serviceIdentity === workerFixture.registration.serviceIdentity && value.audience === workerFixture.registration.audience, "identity mismatch");
    return [response("registered", {protocol, workerId: value.workerId, sessionId: "gateway-fixture-session", schemaRevision: value.schemaRevision, acceptedCapabilities: value.capabilities})];
  }
  if (envelope.kind === "invoke") {
    const value = envelope.payload;
    requireValue(value.handlerId === "fixture.greet" && value.schemaRevision === workerFixture.schema.revision, "invocation mismatch");
    requireValue(value.delegatedContext === "valid-delegation", "delegated context mismatch");
    const rejected = value.input.name === "reject";
    return [response("result", {
      protocol,
      invocationId: value.invocationId,
      attemptId: value.attemptId,
      schemaRevision: value.schemaRevision,
      ...(rejected ? {data: null, errors: [{code: "NAME_REJECTED", message: "name was rejected", retryable: false}]} : {data: {greeting: `Hello, ${value.input.name}`}, errors: []}),
    })];
  }
  if (envelope.kind === "invoke-stream") {
    const {invocation, credit} = envelope.payload;
    requireValue(invocation.handlerId === "fixture.stream" && invocation.schemaRevision === workerFixture.schema.revision, "stream invocation mismatch");
    requireValue(invocation.delegatedContext === "valid-delegation", "stream delegated context mismatch");
    requireValue(Number.isSafeInteger(credit.frames) && credit.frames >= 3 && Number.isSafeInteger(credit.bytes) && credit.bytes >= 4096, "stream credit is insufficient");
    const frames = [
      {type: "open", stream: invocation.invocationId, sequence: 1, schemaRevision: invocation.schemaRevision},
      {type: "data", stream: invocation.invocationId, sequence: 2, data: {greeting: `Hello, ${invocation.input.name}`}},
      {type: "complete", stream: invocation.invocationId, sequence: 3},
    ];
    return frames.map((frame) => response("stream", {
      protocol,
      invocationId: invocation.invocationId,
      attemptId: invocation.attemptId,
      schemaRevision: invocation.schemaRevision,
      frame,
    }));
  }
  if (envelope.kind === "cancel") {
    return [response("cancelled", {protocol, invocationId: envelope.payload.invocationId, disposition: "acknowledged"})];
  }
  throw new Error("unsupported envelope kind");
}

function encodeFrame(value) {
  const payload = Buffer.from(JSON.stringify(value));
  requireValue(payload.length > 0 && payload.length <= maximumFrameBytes, "response frame limit exceeded");
  const framed = Buffer.allocUnsafe(5 + payload.length);
  framed[0] = 0;
  framed.writeUInt32BE(payload.length, 1);
  payload.copy(framed, 5);
  return framed;
}

function takeFrame(bytes) {
  if (bytes.length < 5) return null;
  const header = new DataView(bytes.buffer, bytes.byteOffset, 5);
  const flags = header.getUint8(0);
  const size = header.getUint32(1);
  requireValue(flags === 0 && size > 0 && size <= maximumFrameBytes, "invalid frame");
  const end = 5 + size;
  if (bytes.length < end) return null;
  return {
    envelope: JSON.parse(bytes.subarray(5, end).toString("utf8")),
    remaining: bytes.subarray(end),
  };
}

async function serve() {
  let pending = Buffer.alloc(0);
  for await (const chunk of process.stdin) {
    pending = Buffer.concat([pending, chunk]);
    for (let frame = takeFrame(pending); frame !== null; frame = takeFrame(pending)) {
      pending = frame.remaining;
      process.stdout.write(Buffer.concat(handle(frame.envelope).map(encodeFrame)));
    }
  }
  requireValue(pending.length === 0, "truncated frame");
}

requireValue(workerFixture.profile === "worker.remote-1" && workerFixture.protocol === protocol, "remote-worker dependency drift");
requireValue(streamFixture.profile === "core.streaming-1" && streamFixture.protocolVersion === "1", "streaming dependency drift");
requireValue(workerFixture.claims.nativeRuntime === false, "reference worker overclaims native runtime support");

if (process.argv.includes("--serve")) await serve();
else process.stdout.write(JSON.stringify({profile: "implementation.language-neutral.reference-worker-1", status: "passed", dependencies: [workerFixture.profile, streamFixture.profile]}) + "\n");
