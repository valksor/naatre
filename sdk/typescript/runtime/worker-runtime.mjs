import { canonicalStringify } from "./json.mjs";
import { createFetchWorkerAdapter } from "./server.mjs";

const workerMediaType = "application/naatre-worker+json";
const runtimeNames = new Set(["node", "bun", "deno", "edge"]);

export function createWorkerRuntimeAdapter(worker, runtime) {
  if (!runtimeNames.has(runtime)) throw new TypeError("worker runtime is unsupported");
  const lifecycle = new AbortController();
  const dispatch = createFetchWorkerAdapter(worker, { signal: lifecycle.signal });
  const active = new Set();
  let phase = "ready";
  let closing;

  const adapter = Object.freeze({
    profile: `worker.javascript-typescript.${runtime}-1`,
    runtime,
    fetch(request) {
      if (phase !== "ready") return Promise.resolve(unavailable());
      const operation = Promise.resolve().then(() => dispatch(request));
      active.add(operation);
      return operation.finally(() => active.delete(operation));
    },
    status() {
      return Object.freeze({ phase });
    },
    shutdown() {
      if (closing) return closing;
      phase = "draining";
      lifecycle.abort(new DOMException("worker runtime is shutting down", "AbortError"));
      closing = settle(active).then(() => { phase = "stopped"; });
      return closing;
    },
  });
  return adapter;
}

async function settle(active) {
  while (active.size !== 0) await Promise.allSettled([...active]);
}

function unavailable() {
  return new Response(canonicalStringify({ code: "OVERLOADED", message: "worker runtime is not accepting requests" }), {
    status: 503,
    headers: { "content-type": workerMediaType },
  });
}
