import { createWorkerRuntimeAdapter } from "./worker-runtime.mjs";

export function createEdgeWorkerAdapter(worker) {
  return createWorkerRuntimeAdapter(worker, "edge");
}
