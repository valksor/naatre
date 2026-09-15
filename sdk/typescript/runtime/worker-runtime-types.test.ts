import { createBunWorkerAdapter } from "@naatre/sdk/worker/bun";
import { createDenoWorkerAdapter } from "@naatre/sdk/worker/deno";
import { createEdgeWorkerAdapter } from "@naatre/sdk/worker/edge";
import { createNodeWorkerAdapter } from "@naatre/sdk/worker/node";
import type { NaatreWorker } from "@naatre/sdk/worker";
import type { WorkerRuntimeAdapter } from "./worker-runtime.mjs";

declare const worker: NaatreWorker;

const nodeAdapter = createNodeWorkerAdapter(worker) satisfies WorkerRuntimeAdapter<"node">;
const bunAdapter = createBunWorkerAdapter(worker) satisfies WorkerRuntimeAdapter<"bun">;
const denoAdapter = createDenoWorkerAdapter(worker) satisfies WorkerRuntimeAdapter<"deno">;
const edgeAdapter = createEdgeWorkerAdapter(worker) satisfies WorkerRuntimeAdapter<"edge">;

void [nodeAdapter, bunAdapter, denoAdapter, edgeAdapter];
