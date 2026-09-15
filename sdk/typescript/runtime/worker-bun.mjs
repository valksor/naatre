import { createWorkerRuntimeAdapter } from "./worker-runtime.mjs";

export function createBunWorkerAdapter(worker) {
  return createWorkerRuntimeAdapter(worker, "bun");
}
