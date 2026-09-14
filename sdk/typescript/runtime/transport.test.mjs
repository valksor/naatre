import assert from "node:assert/strict";
import test from "node:test";

import { NaatreClientError, createFetchAdapter, createOperation, createWebSocketAdapter, decodeSSEStream } from "./index.mjs";

const persisted = Object.freeze({ algorithm: "sha-256", canonicalVersion: "c14n-1", digest: "a".repeat(64) });

test("Fetch unary sends canonical POST and preserves partial data with errors", async () => {
  let captured;
  const adapter = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    authenticate: () => ({ Authorization: "Bearer local-test-value" }),
    fetch: async (url, init) => {
      captured = { url, init };
      return jsonResponse('{"complete":false,"data":{"name":"Ada"},"errors":[{"code":"PARTIAL"}]}');
    },
  });
  const result = await adapter.execute(operation());
  assert.equal(result.data.name, "Ada");
  assert.equal(result.errors[0].code, "PARTIAL");
  assert.equal(captured.url, "https://api.example/v1/execute");
  assert.equal(captured.init.method, "POST");
  assert.equal(captured.init.headers.get("authorization"), "Bearer local-test-value");
  assert.equal(captured.init.headers.get("content-type"), "application/vnd.naatre.request+json;version=1");
  assert.equal(captured.init.body, operation().canonicalRequest());
});

test("Fetch redirects reject by default and strip credentials when explicitly allowed", async () => {
  const rejected = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    authenticate: () => ({ Authorization: "Bearer local-test-value", "Naatre-Tenant": "tenant" }),
    fetch: async () => new Response(null, { status: 307, headers: { Location: "https://other.example/v1/execute" } }),
  });
  await assert.rejects(rejected.execute(operation()), clientCode("CLIENT_REDIRECT_REJECTED"));

  const calls = [];
  const allowed = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    redirectOrigins: ["https://other.example"],
    authenticate: () => ({ Authorization: "Bearer local-test-value", "Naatre-Tenant": "tenant", "X-Public": "safe" }),
    fetch: async (url, init) => {
      calls.push({ url, authorization: init.headers.get("authorization"), tenant: init.headers.get("naatre-tenant"), public: init.headers.get("x-public"), credentials: init.credentials });
      return calls.length === 1
        ? new Response(null, { status: 307, headers: { Location: "https://other.example/v1/execute" } })
        : jsonResponse('{"complete":true,"data":null,"errors":[]}');
    },
  });
  await allowed.execute(operation());
  assert.deepEqual(calls, [
    { url: "https://api.example/v1/execute", authorization: "Bearer local-test-value", tenant: "tenant", public: "safe", credentials: "omit" },
    { url: "https://other.example/v1/execute", authorization: null, tenant: null, public: "safe", credentials: "omit" },
  ]);
});

test("Fetch closes rejected redirect and response bodies", async () => {
  let opaqueCanceled = false;
  const opaqueBody = new ReadableStream({ cancel() { opaqueCanceled = true; } });
  const opaque = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    fetch: async () => ({ type: "opaqueredirect", status: 0, body: opaqueBody }),
  });
  await assert.rejects(opaque.execute(operation()), clientCode("CLIENT_REDIRECT_UNSUPPORTED"));
  assert.equal(opaqueCanceled, true);

  let mediaCanceled = false;
  const mediaBody = new ReadableStream({ cancel() { mediaCanceled = true; } });
  const media = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    fetch: async () => new Response(mediaBody, { headers: { "Content-Type": "text/plain" } }),
  });
  await assert.rejects(media.execute(operation()), clientCode("CLIENT_UNSUPPORTED_MEDIA_TYPE"));
  assert.equal(mediaCanceled, true);
});

test("Fetch normalizes invalid authentication headers without invoking transport", async () => {
  let invoked = false;
  const adapter = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    authenticate: () => ({ "invalid header": "private-auth-detail" }),
    fetch: async () => {
      invoked = true;
      return jsonResponse("{}");
    },
  });
  await assert.rejects(adapter.execute(operation()), clientCode("CLIENT_AUTHENTICATION_FAILED"));
  assert.equal(invoked, false);
});

test("Fetch unary enforces encoding, compressed declaration, decompressed bytes, and active abort", async () => {
  const unsupported = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => jsonResponse("{}", { "Content-Encoding": "br" }) });
  await assert.rejects(unsupported.execute(operation()), clientCode("CLIENT_UNSUPPORTED_ENCODING"));

  let declaredCanceled = false;
  const declaredBody = new ReadableStream({ cancel() { declaredCanceled = true; } });
  const declared = createFetchAdapter({ endpoint: "https://api.example/v1/execute", compressedBytes: 2, fetch: async () => new Response(declaredBody, { headers: { "Content-Type": "application/vnd.naatre.response+json;version=1", "Content-Length": "3" } }) });
  await assert.rejects(declared.execute(operation()), clientCode("CLIENT_RESPONSE_LIMIT"));
  assert.equal(declaredCanceled, true);

  let canceled = false;
  const oversizedBody = new ReadableStream({
    start(controller) { controller.enqueue(new TextEncoder().encode("123456789")); },
    cancel() { canceled = true; },
  });
  const expanded = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    decompressedBytes: 8,
    fetch: async () => new Response(oversizedBody, { headers: { "Content-Type": "application/vnd.naatre.response+json;version=1", "Content-Encoding": "gzip", "Content-Length": "1" } }),
  });
  await assert.rejects(expanded.execute(operation()), clientCode("CLIENT_RESPONSE_LIMIT"));
  assert.equal(canceled, true);

  const controller = new AbortController();
  const aborted = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    fetch: (_url, init) => new Promise((_resolve, reject) => init.signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true })),
  });
  const pending = aborted.execute(operation(), { signal: controller.signal });
  controller.abort();
  await assert.rejects(pending, clientCode("CLIENT_CANCELED"));
});

test("POST SSE decodes bounded frames, ignores comments, and skips equivalent duplicates", async () => {
  const frames = [
    { type: "open", stream: "s", sequence: 1, schemaRevision: "r1" },
    { type: "data", stream: "s", sequence: 2, position: 1, cursor: "cursor-1", data: { value: 1 } },
    { type: "data", stream: "s", sequence: 2, position: 1, cursor: "cursor-1", data: { value: 1 } },
    { type: "complete", stream: "s", sequence: 3 },
  ];
  const body = `: heartbeat\n\n${sse(frames[0])}${sse(frames[1])}${sse(frames[2])}${sse(frames[3])}`;
  let headers;
  const adapter = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    fetch: async (_url, init) => {
      headers = init.headers;
      return new Response(chunked(body, 3), { headers: { "Content-Type": "text/event-stream; charset=utf-8" } });
    },
  });
  const stream = await adapter.stream(operation("subscription"));
  const received = [];
  for await (const frame of stream) received.push(frame);
  assert.deepEqual(received.map((frame) => frame.type), ["open", "data", "complete"]);
  assert.equal(headers.get("accept"), "text/event-stream");
  assert.equal(headers.get("accept-encoding"), "identity");
});

test("POST SSE rejects truncation and cancellation actively closes the body", async () => {
  const truncated = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    fetch: async () => new Response(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }), { headers: { "Content-Type": "text/event-stream" } }),
  });
  await assert.rejects(collect(await truncated.stream(operation("subscription"))), clientCode("CLIENT_STREAM_TRUNCATED"));

  let canceled = false;
  const body = new ReadableStream({
    start(streamController) { streamController.enqueue(new TextEncoder().encode(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }))); },
    cancel() { canceled = true; },
  });
  const controller = new AbortController();
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(body, { headers: { "Content-Type": "text/event-stream" } }) });
  const iterator = (await adapter.stream(operation("subscription"), { signal: controller.signal }))[Symbol.asyncIterator]();
  assert.equal((await iterator.next()).value.type, "open");
  const pending = iterator.next();
  controller.abort();
  await assert.rejects(pending, clientCode("CLIENT_CANCELED"));
  assert.equal(canceled, true);
});

test("POST SSE normalizes reader acquisition failures and closes the body", async () => {
  let canceled = false;
  const body = {
    getReader() { throw new Error("private-reader-detail"); },
    async cancel() { canceled = true; },
  };
  await assert.rejects(collect(decodeSSEStream(body)), clientCode("CLIENT_STREAM_INVALID"));
  assert.equal(canceled, true);
});

test("POST SSE closes a history-unavailable recovery attempt and rejects malformed or oversized frames", async () => {
  const recovery = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    fetch: async () => new Response(`${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "history-unavailable", stream: "s", sequence: 2, recovery: "refetch" })}`, { headers: { "Content-Type": "text/event-stream" } }),
  });
  const recovered = await collect(await recovery.stream(operation("subscription")));
  assert.deepEqual(recovered.map((frame) => frame.type), ["open", "history-unavailable"]);

  const malformed = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    fetch: async () => new Response(`${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "history-unavailable", stream: "s", sequence: 2, recovery: "leak-details" })}`, { headers: { "Content-Type": "text/event-stream" } }),
  });
  await assert.rejects(collect(await malformed.stream(operation("subscription"))), clientCode("CLIENT_STREAM_INVALID"));

  const oversized = createFetchAdapter({
    endpoint: "https://api.example/v1/execute",
    frameBytes: 64,
    fetch: async () => new Response(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "x".repeat(128) }), { headers: { "Content-Type": "text/event-stream" } }),
  });
  await assert.rejects(collect(await oversized.stream(operation("subscription"))), clientCode("CLIENT_FRAME_LIMIT"));
});

test("WebSocket adapter sends the canonical request, closes after terminal, and rejects truncation", async () => {
  FakeWebSocket.frames = [
    JSON.stringify({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }),
    JSON.stringify({ type: "complete", stream: "s", sequence: 2 }),
  ];
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket, protocols: ["naatre.v1"] });
  const received = await collect(adapter.stream(operation("subscription")));
  assert.deepEqual(received.map((frame) => frame.type), ["open", "complete"]);
  assert.equal(FakeWebSocket.last.sent[0], operation().canonicalRequest());
  assert.deepEqual(FakeWebSocket.last.closeCalls[0], [1000, "complete"]);

  FakeWebSocket.frames = [JSON.stringify({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })];
  await assert.rejects(collect(adapter.stream(operation("subscription"))), clientCode("CLIENT_STREAM_TRUNCATED"));
});

test("WebSocket abort actively closes a pending socket", async () => {
  FakeWebSocket.frames = null;
  const controller = new AbortController();
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket });
  const iterator = adapter.stream(operation("subscription"), { signal: controller.signal })[Symbol.asyncIterator]();
  const pending = iterator.next();
  await Promise.resolve();
  controller.abort();
  await assert.rejects(pending, clientCode("CLIENT_CANCELED"));
  assert.deepEqual(FakeWebSocket.last.closeCalls[0], [1001, "canceled"]);
});

test("WebSocket rejects binary, malformed, and oversized frames with bounded closes", async () => {
  FakeWebSocket.autoClose = false;
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket });

  FakeWebSocket.frames = [new Uint8Array([123, 125])];
  await assert.rejects(collect(adapter.stream(operation("subscription"))), clientCode("CLIENT_STREAM_INVALID"));
  assert.deepEqual(FakeWebSocket.last.closeCalls[0], [1003, "text required"]);

  FakeWebSocket.frames = ["{"];
  await assert.rejects(collect(adapter.stream(operation("subscription"))), clientCode("CLIENT_PROTOCOL_INVALID"));
  assert.deepEqual(FakeWebSocket.last.closeCalls[0], [1001, "client closed"]);

  FakeWebSocket.frames = ["x".repeat(65)];
  const bounded = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket, maximumFrameBytes: 64 });
  await assert.rejects(collect(bounded.stream(operation("subscription"))), clientCode("CLIENT_FRAME_LIMIT"));
  assert.deepEqual(FakeWebSocket.last.closeCalls[0], [1009, "frame limit"]);
  FakeWebSocket.autoClose = true;
});

test("WebSocket normalizes constructor failures", async () => {
  class FailingWebSocket {
    constructor() {
      throw new Error("private-websocket-detail");
    }
  }
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FailingWebSocket });
  await assert.rejects(collect(adapter.stream(operation("subscription"))), clientCode("CLIENT_TRANSPORT_ERROR"));
});

test("WebSocket bounds frames queued ahead of the consumer", async () => {
  FakeWebSocket.autoClose = false;
  FakeWebSocket.frames = [
    JSON.stringify({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }),
    JSON.stringify({ type: "data", stream: "s", sequence: 2, data: { value: 1 } }),
    JSON.stringify({ type: "complete", stream: "s", sequence: 3 }),
  ];
  const adapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream", WebSocket: FakeWebSocket, maximumQueuedFrames: 1 });
  await assert.rejects(collect(adapter.stream(operation("subscription"))), clientCode("CLIENT_RESPONSE_LIMIT"));
  assert.deepEqual(FakeWebSocket.last.closeCalls[0], [1009, "queue limit"]);
  FakeWebSocket.autoClose = true;
});

function operation(kind = "query") {
  return createOperation({ name: "GetAccount", kind, persisted, variables: [{ name: "id", type: "ID", required: true, nullable: false }] }, { id: "acct-1" }, (value) => value);
}

function jsonResponse(body, headers = {}) {
  return new Response(body, { headers: { "Content-Type": "application/vnd.naatre.response+json;version=1", ...headers } });
}

function sse(frame) {
  const cursor = frame.cursor === undefined ? "" : `id: ${frame.cursor}\n`;
  return `event: naatre.${frame.type}\n${cursor}data: ${JSON.stringify(frame)}\n\n`;
}

function chunked(value, size) {
  const bytes = new TextEncoder().encode(value);
  return new ReadableStream({
    start(controller) {
      for (let offset = 0; offset < bytes.length; offset += size) controller.enqueue(bytes.slice(offset, offset + size));
      controller.close();
    },
  });
}

async function collect(stream) {
  const values = [];
  for await (const value of stream) values.push(value);
  return values;
}

function clientCode(code) {
  return (error) => error instanceof NaatreClientError && error.code === code && !error.message.includes("local-test-value");
}

class FakeWebSocket extends EventTarget {
  static frames = [];
  static last;
  static autoClose = true;
  readyState = 0;
  sent = [];
  closeCalls = [];

  constructor(url, protocols) {
    super();
    this.url = url;
    this.protocols = protocols;
    FakeWebSocket.last = this;
    queueMicrotask(() => {
      this.readyState = 1;
      this.dispatchEvent(new Event("open"));
    });
  }

  send(value) {
    this.sent.push(value);
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

function messageEvent(data) {
  const event = new Event("message");
  Object.defineProperty(event, "data", { value: data });
  return event;
}
