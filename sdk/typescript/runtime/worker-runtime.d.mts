import type { NaatreWorker } from "./server.mjs";

export type WorkerRuntimeName = "node" | "bun" | "deno" | "edge";
export type WorkerRuntimePhase = "ready" | "draining" | "stopped";

export interface WorkerRuntimeAdapter<TRuntime extends WorkerRuntimeName = WorkerRuntimeName> {
  readonly profile: `worker.javascript-typescript.${TRuntime}-1`;
  readonly runtime: TRuntime;
  fetch(request: Request): Promise<Response>;
  status(): Readonly<{ phase: WorkerRuntimePhase }>;
  shutdown(): Promise<void>;
}

export function createWorkerRuntimeAdapter<TRuntime extends WorkerRuntimeName>(worker: NaatreWorker, runtime: TRuntime): WorkerRuntimeAdapter<TRuntime>;
