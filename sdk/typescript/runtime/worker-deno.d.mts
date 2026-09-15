import type { NaatreWorker } from "./server.mjs";
import type { WorkerRuntimeAdapter } from "./worker-runtime.mjs";

export function createDenoWorkerAdapter(worker: NaatreWorker): WorkerRuntimeAdapter<"deno">;
