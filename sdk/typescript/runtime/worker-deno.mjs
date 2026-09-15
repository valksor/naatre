import { createWorkerRuntimeAdapter } from "./worker-runtime.mjs";

export function createDenoWorkerAdapter(worker) {
  return createWorkerRuntimeAdapter(worker, "deno");
}
