import { createWorkerRuntimeAdapter } from "./worker-runtime.mjs";

export function createNodeWorkerAdapter(worker) {
  return createWorkerRuntimeAdapter(worker, "node");
}
