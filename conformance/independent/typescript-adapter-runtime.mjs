import { NaatreClientError, canonicalStringify, createFetchAdapter, createOperation, createWebSocketAdapter, parseJSON } from "../../sdk/typescript/runtime/index.mjs";
import { runtimeID } from "./runtime-id.mjs";

const vectors = [];
const persisted = Object.freeze({ algorithm: "sha-256", canonicalVersion: "c14n-1", digest: "b".repeat(64) });
const capabilities = Object.freeze(["sse-post-fetch", "stream.websocket-1"]);
const dependencies = Object.freeze([
  Object.freeze({ issue: 23, profile: "core.streaming-1", gitCommit: "8d91381f3f88ecc7fe6a9f1512975bf5514f285e", fixture: Object.freeze({ path: "conformance/v1/streaming.json", sha256: "8d93fe53a378a7ba458e6e7fb35758a2eb725f46ba3ff50be5643bc80d0a2749" }) }),
]);

class FakeWebSocket extends EventTarget {
  static frames = [];
  static last;
  static autoClose = true;
  readyState = 0;
  closeCalls = [];

  constructor() {
    super();
    FakeWebSocket.last = this;
    queueMicrotask(() => {
      this.readyState = 1;
      this.dispatchEvent(new Event("open"));
    });
  }

  send() {
    if (FakeWebSocket.frames === null) return;
    for (const data of FakeWebSocket.frames) queueMicrotask(() => this.dispatchEvent(messageEvent(data)));
    if (FakeWebSocket.autoClose) queueMicrotask(() => {
      if (this.readyState === 1) {
        this.readyState = 3;
        this.dispatchEvent(new Event("close"));
      }
    });
  }

  close(code, reason) {
    this.closeCalls.push([code, reason]);
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.dispatchEvent(new Event("close"));
  }
}

export async function runAdapterRuntime(runtimeOverride) {
const runtime = runtimeOverride ?? runtimeID("NAATRE_RUNTIME_ID", "browser-web");
vectors.length = 0;

await check("canonical-bigint-sparse-prototype", async () => {
  const bigintOperation = createOperation({ name: "Exact", kind: "query", persisted, variables: [{ name: "value", type: "BigInt", required: true, nullable: false }] }, { value: 9007199254740993n }, (value) => value);
  equal(bigintOperation.request.variables.value, "9007199254740993");
  throwsCode(() => canonicalStringify(new Array(1)), "CLIENT_VALUE_INVALID");
  const hostile = parseJSON('{"__proto__":{"polluted":true},"constructor":1,"prototype":2}');
  equal(Object.getPrototypeOf(hostile), null);
  equal(Object.prototype.polluted, undefined);
});

await check("fetch-unary-partial-data", async () => {
  let request;
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async (url, init) => {
    request = { url, init };
    return response('{"complete":false,"data":{"value":"ok"},"errors":[{"code":"PARTIAL"}]}');
  } });
  const result = await adapter.execute(operation());
  equal(result.data.value, "ok");
  equal(result.errors[0].code, "PARTIAL");
  equal(request.init.method, "POST");
});

await check("fetch-decompression-limit", async () => {
  let canceled = false;
  const body = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode("123456789")); }, cancel() { canceled = true; } });
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", decompressedBytes: 8, fetch: async () => new Response(body, { headers: { "Content-Type": "application/vnd.naatre.response+json;version=1", "Content-Encoding": "gzip", "Content-Length": "1" } }) });
  await rejectsCode(adapter.execute(operation()), "CLIENT_RESPONSE_LIMIT");
  equal(canceled, true);
});

await check("fetch-rejected-body-lifecycle", async () => {
  let canceled = false;
  const body = new ReadableStream({ cancel() { canceled = true; } });
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => ({ type: "opaqueredirect", status: 0, body }) });
  await rejectsCode(adapter.execute(operation()), "CLIENT_REDIRECT_UNSUPPORTED");
  equal(canceled, true);
});

await check("fetch-cross-origin-credential-strip", async () => {
  const calls = [];
  const adapter = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    redirectOrigins: ["https://other.example"],
    authenticate: () => ({ Authorization: "Bearer matrix-secret", "Naatre-Tenant": "tenant", "X-Public": "safe" }),
    fetch: async (url, init) => {
      calls.push({ url, authorization: init.headers.get("authorization"), tenant: init.headers.get("naatre-tenant"), public: init.headers.get("x-public") });
      return calls.length === 1 ? new Response(null, { status: 307, headers: { Location: "https://other.example/v1/execute" } }) : response('{"complete":true,"data":null,"errors":[]}');
    },
  });
  await adapter.execute(operation());
  equal(calls[1].authorization, null);
  equal(calls[1].tenant, null);
  equal(calls[1].public, "safe");
});

await check("fetch-active-abort", async () => {
  const controller = new AbortController();
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async (_url, init) => {
    if (init.signal.aborted) throw new DOMException("aborted", "AbortError");
    return new Promise((_resolve, reject) => init.signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true }));
  } });
  const pending = adapter.execute(operation(), { signal: controller.signal });
  controller.abort();
  await rejectsCode(pending, "CLIENT_CANCELED");
});

await runSSEFixtures();

await check("websocket-terminal-and-active-close", async () => {
  FakeWebSocket.frames = [JSON.stringify({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }), JSON.stringify({ type: "complete", stream: "s", sequence: 2 })];
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket });
  const received = await collect(adapter.stream(operation("subscription")));
  equal(received.length, 2);
  equal(FakeWebSocket.last.closeCalls[0][0], 1000);

  FakeWebSocket.frames = null;
  const controller = new AbortController();
  const iterator = adapter.stream(operation("subscription"), { signal: controller.signal })[Symbol.asyncIterator]();
  const pending = iterator.next();
  await Promise.resolve();
  controller.abort();
  await rejectsCode(pending, "CLIENT_CANCELED");
  equal(FakeWebSocket.last.closeCalls[0][0], 1001);
});

await check("websocket-binary-frame-rejected", async () => {
  FakeWebSocket.frames = [new Uint8Array([123, 125])];
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket });
  await rejectsCode(collect(adapter.stream(operation("subscription"))), "CLIENT_STREAM_INVALID");
});

await check("websocket-queue-limit", async () => {
  FakeWebSocket.autoClose = false;
  FakeWebSocket.frames = [
    JSON.stringify({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }),
    JSON.stringify({ type: "data", stream: "s", sequence: 2, data: { value: 1 } }),
    JSON.stringify({ type: "complete", stream: "s", sequence: 3 }),
  ];
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket, maximumQueuedFrames: 1 });
  await rejectsCode(collect(adapter.stream(operation("subscription"))), "CLIENT_RESPONSE_LIMIT");
  equal(FakeWebSocket.last.closeCalls[0][0], 1009);
  FakeWebSocket.autoClose = true;
});

await check("stable-public-errors-no-protected-details", verifyStablePublicErrors);

return Object.freeze({ profile: "sdk.typescript.adapters-1", runtime, status: "passed", capabilities, dependencies, vectors: Object.freeze([...vectors]) });
}

async function runSSEFixtures() {
  await check("post-sse-terminal-and-truncation", verifySSETerminalAndTruncation);
  await check("post-sse-framing-split-utf8", verifySSEFraming);
  await check("post-sse-reconnect-resume", verifySSEReconnect);
  await check("post-sse-backpressure", verifySSEBackpressure);
  await check("post-sse-cancellation", verifySSECancellation);
  await check("post-sse-resource-limits", verifySSEResourceLimits);
  await check("post-sse-history-recovery-and-invalid-frame", verifySSEHistoryRecovery);
}

async function verifySSETerminalAndTruncation() {
  const completeBody = `${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "complete", stream: "s", sequence: 2 })}`;
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(completeBody, { headers: { "Content-Type": "text/event-stream" } }) });
  equal((await collect(await adapter.stream(operation("subscription")))).length, 2);
  const truncated = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }), { headers: { "Content-Type": "text/event-stream" } }) });
  await rejectsCode(collect(await truncated.stream(operation("subscription"))), "CLIENT_STREAM_TRUNCATED");
}

async function verifySSEFraming() {
  const open = { type: "open", stream: "s", sequence: 1, schemaRevision: "r1" };
  const data = { type: "data", stream: "s", sequence: 2, position: 1, cursor: "cursor-1", data: { text: "tēriņš" } };
  const complete = { type: "complete", stream: "s", sequence: 3 };
  const body = `: heartbeat\r\n\r\n${sse(open)}${sseMultiline(data)}${sse(complete)}`;
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(oneByteChunks(body), { headers: { "Content-Type": "text/event-stream; charset=utf-8" } }) });
  const received = await collect(await adapter.stream(operation("subscription")));
  equal(received.map((frame) => frame.type).join(","), "open,data,complete");
  equal(received[1].data.text, "tēriņš");
}

async function verifySSEReconnect() {
  let authenticationContext;
  let headers;
  const body = [
    { type: "open", stream: "s", sequence: 1, schemaRevision: "r1" },
    { type: "resume", stream: "s", sequence: 2, cursor: "cursor-1" },
    { type: "data", stream: "s", sequence: 3, position: 2, cursor: "cursor-2", data: { value: 2 } },
    { type: "complete", stream: "s", sequence: 4 },
  ].map(sse).join("");
  const adapter = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    authenticate: (context) => { authenticationContext = context; return { Authorization: "Bearer matrix-secret" }; },
    fetch: async (_url, init) => { headers = init.headers; return new Response(body, { headers: { "Content-Type": "text/event-stream" } }); },
  });
  const received = await collect(await adapter.stream(operation("subscription"), { lastEventId: "cursor-1" }));
  equal(received.map((frame) => frame.type).join(","), "open,resume,data,complete");
  equal(authenticationContext.lastEventId, "cursor-1");
  equal(headers.get("last-event-id"), "cursor-1");
  await rejectsCode(adapter.stream(operation("subscription"), { lastEventId: "protected\nmetadata" }), "CLIENT_STREAM_INVALID", ["protected"]);
}

async function verifySSEBackpressure() {
  const chunks = [
    new TextEncoder().encode(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })),
    new TextEncoder().encode(sse({ type: "complete", stream: "s", sequence: 2 })),
  ];
  let pulls = 0;
  const body = new ReadableStream({
    pull(controller) {
      pulls += 1;
      controller.enqueue(chunks.shift());
      if (chunks.length === 0) controller.close();
    },
  }, { highWaterMark: 0 });
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(body, { headers: { "Content-Type": "text/event-stream" } }) });
  const iterator = (await adapter.stream(operation("subscription")))[Symbol.asyncIterator]();
  equal((await iterator.next()).value.type, "open");
  equal(pulls, 1);
  equal((await iterator.next()).value.type, "complete");
  equal(pulls, 2);
}

async function verifySSECancellation() {
  let canceled = false;
  let reads = 0;
  let release;
  const body = {
    getReader() {
      return {
        read() {
          reads += 1;
          if (reads === 1) return Promise.resolve({ done: false, value: new TextEncoder().encode(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })) });
          return new Promise((resolve) => { release = resolve; });
        },
        cancel() {
          canceled = true;
          release?.({ done: true });
          return Promise.resolve();
        },
      };
    },
  };
  const controller = new AbortController();
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => ({ body, headers: new Headers({ "Content-Type": "text/event-stream" }), ok: true, status: 200 }) });
  const iterator = (await adapter.stream(operation("subscription"), { signal: controller.signal }))[Symbol.asyncIterator]();
  equal((await iterator.next()).value.type, "open");
  const pending = iterator.next();
  controller.abort();
  await rejectsCode(pending, "CLIENT_CANCELED");
  equal(canceled, true);
}

async function verifySSEResourceLimits() {
  const oversized = createFetchAdapter({ endpoint: "https://api.example/v1/execute", frameBytes: 64, fetch: async () => new Response(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "x".repeat(128) }), { headers: { "Content-Type": "text/event-stream" } }) });
  await rejectsCode(collect(await oversized.stream(operation("subscription"))), "CLIENT_FRAME_LIMIT");
}

async function verifySSEHistoryRecovery() {
  const recoveryBody = `${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "history-unavailable", stream: "s", sequence: 2, recovery: "refetch" })}`;
  const recovery = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(recoveryBody, { headers: { "Content-Type": "text/event-stream" } }) });
  equal((await collect(await recovery.stream(operation("subscription"))))[1].type, "history-unavailable");
  const invalidBody = `${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "history-unavailable", stream: "s", sequence: 2, recovery: "private-detail" })}`;
  const invalid = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(invalidBody, { headers: { "Content-Type": "text/event-stream" } }) });
  await rejectsCode(collect(await invalid.stream(operation("subscription"))), "CLIENT_STREAM_INVALID");
}

async function verifyStablePublicErrors() {
  const adapter = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    authenticate: () => { throw new Error("matrix-auth-private"); },
    fetch: async () => { throw new Error("matrix-transport-private"); },
  });
  await rejectsCode(adapter.stream(operation("subscription")), "CLIENT_AUTHENTICATION_FAILED", ["matrix-auth-private", "matrix-transport-private"]);

  FakeWebSocket.frames = ["{private-frame-detail"];
  const websocket = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket });
  await rejectsCode(collect(websocket.stream(operation("subscription"))), "CLIENT_PROTOCOL_INVALID", ["private-frame-detail"]);
}

if (!globalThis.NAATRE_ADAPTER_NO_AUTORUN) await publish(await runAdapterRuntime());

async function publish(result) {
  globalThis.NAATRE_ADAPTER_RESULT = result;
  if (globalThis.process?.stdout?.write) globalThis.process.stdout.write(`${JSON.stringify(result)}\n`);
  else if (globalThis.Deno?.stdout?.write) await globalThis.Deno.stdout.write(new TextEncoder().encode(`${JSON.stringify(result)}\n`));
}

async function check(name, callback) {
  await callback();
  vectors.push(name);
}

function operation(kind = "query") {
  return createOperation({ name: "Matrix", kind, persisted, variables: [{ name: "id", type: "ID", required: true, nullable: false }] }, { id: "matrix" }, (value) => value);
}

function response(body) {
  return new Response(body, { headers: { "Content-Type": "application/vnd.naatre.response+json;version=1" } });
}

function sse(frame) {
  const lines = [`event: naatre.${frame.type}`];
  if (frame.cursor !== undefined) lines.push(`id: ${frame.cursor}`);
  lines.push(`data: ${JSON.stringify(frame)}`, "", "");
  return lines.join("\n");
}

function sseMultiline(frame) {
  const payload = JSON.stringify(frame);
  const split = payload.indexOf('"data"');
  const lines = [`event: naatre.${frame.type}`];
  if (frame.cursor !== undefined) lines.push(`id: ${frame.cursor}`);
  lines.push(`data: ${payload.slice(0, split)}`, `data: ${payload.slice(split)}`, "", "");
  return lines.join("\n");
}

function oneByteChunks(value) {
  const bytes = new TextEncoder().encode(value);
  let offset = 0;
  return new ReadableStream({
    pull(controller) {
      controller.enqueue(bytes.slice(offset, offset + 1));
      offset += 1;
      if (offset === bytes.length) controller.close();
    },
  });
}

async function collect(stream) {
  const values = [];
  for await (const value of stream) values.push(value);
  return values;
}

function equal(actual, expected) {
  if (!Object.is(actual, expected)) throw new Error("matrix assertion failed");
}

function throwsCode(callback, code) {
  try { callback(); } catch (error) {
    if (error instanceof NaatreClientError && error.code === code && !error.message.includes("matrix-secret")) return;
  }
  throw new Error("matrix assertion failed");
}

async function rejectsCode(promise, code, forbidden = []) {
  try { await promise; } catch (error) {
    const rendered = String(error?.stack ?? error);
    if (error instanceof NaatreClientError && error.code === code && error.message === `Naatre client failed: ${code}` && error.cause === undefined && !rendered.includes("matrix-secret") && forbidden.every((value) => !rendered.includes(value))) return;
  }
  throw new Error("matrix assertion failed");
}

function messageEvent(data) {
  const event = new Event("message");
  Object.defineProperty(event, "data", { value: data });
  return event;
}
