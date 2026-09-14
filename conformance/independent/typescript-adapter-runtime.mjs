import { NaatreClientError, canonicalStringify, createFetchAdapter, createOperation, createWebSocketAdapter, parseJSON } from "../../sdk/typescript/runtime/index.mjs";

const vectors = [];
const persisted = Object.freeze({ algorithm: "sha-256", canonicalVersion: "c14n-1", digest: "b".repeat(64) });

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
const runtime = runtimeOverride ?? runtimeID();
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

await check("post-sse-terminal-and-truncation", async () => {
  const completeBody = `${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "complete", stream: "s", sequence: 2 })}`;
  const adapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(completeBody, { headers: { "Content-Type": "text/event-stream" } }) });
  const received = await collect(await adapter.stream(operation("subscription")));
  equal(received.length, 2);
  const truncated = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" }), { headers: { "Content-Type": "text/event-stream" } }) });
  await rejectsCode(collect(await truncated.stream(operation("subscription"))), "CLIENT_STREAM_TRUNCATED");
});

await check("post-sse-history-recovery-and-invalid-frame", async () => {
  const recoveryBody = `${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "history-unavailable", stream: "s", sequence: 2, recovery: "refetch" })}`;
  const recovery = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(recoveryBody, { headers: { "Content-Type": "text/event-stream" } }) });
  const received = await collect(await recovery.stream(operation("subscription")));
  equal(received[1].type, "history-unavailable");
  const invalidBody = `${sse({ type: "open", stream: "s", sequence: 1, schemaRevision: "r1" })}${sse({ type: "history-unavailable", stream: "s", sequence: 2, recovery: "private-detail" })}`;
  const invalid = createFetchAdapter({ endpoint: "https://api.example/v1/execute", fetch: async () => new Response(invalidBody, { headers: { "Content-Type": "text/event-stream" } }) });
  await rejectsCode(collect(await invalid.stream(operation("subscription"))), "CLIENT_STREAM_INVALID");
});

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

return Object.freeze({ profile: "sdk.typescript.adapters-1", runtime, status: "passed", vectors: Object.freeze([...vectors]) });
}

if (!globalThis.NAATRE_ADAPTER_NO_AUTORUN) await publish(await runAdapterRuntime());

async function publish(result) {
  globalThis.NAATRE_ADAPTER_RESULT = result;
  if (globalThis.process?.stdout?.write) globalThis.process.stdout.write(`${JSON.stringify(result)}\n`);
  else if (globalThis.Deno?.stdout?.write) await globalThis.Deno.stdout.write(new TextEncoder().encode(`${JSON.stringify(result)}\n`));
}

function runtimeID() {
  if (typeof globalThis.NAATRE_RUNTIME_ID === "string") return globalThis.NAATRE_RUNTIME_ID;
  if (globalThis.Deno?.version?.deno) return `deno-${globalThis.Deno.version.deno}`;
  if (globalThis.Bun?.version) return `bun-${globalThis.Bun.version}`;
  if (globalThis.process?.versions?.node) return `node-${globalThis.process.versions.node}`;
  return "browser-web";
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
  return `event: naatre.${frame.type}\ndata: ${JSON.stringify(frame)}\n\n`;
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

async function rejectsCode(promise, code) {
  try { await promise; } catch (error) {
    if (error instanceof NaatreClientError && error.code === code && !error.message.includes("matrix-secret")) return;
  }
  throw new Error("matrix assertion failed");
}

function messageEvent(data) {
  const event = new Event("message");
  Object.defineProperty(event, "data", { value: data });
  return event;
}
