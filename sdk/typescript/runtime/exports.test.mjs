import assert from "node:assert/strict";
import test from "node:test";

test("package adapter subpaths expose only their declared runtime surface", async () => {
  const fetchAdapter = await import("@naatre/sdk/fetch");
  const websocketAdapter = await import("@naatre/sdk/websocket");
  const worker = await import("@naatre/sdk/worker");
  const nodeWorker = await import("@naatre/sdk/worker/node");
  const bunWorker = await import("@naatre/sdk/worker/bun");
  const denoWorker = await import("@naatre/sdk/worker/deno");
  const edgeWorker = await import("@naatre/sdk/worker/edge");
  assert.deepEqual(Object.keys(fetchAdapter), ["createFetchAdapter"]);
  assert.deepEqual(Object.keys(websocketAdapter), ["createWebSocketAdapter", "decodeSSEStream"]);
  assert.deepEqual(Object.keys(worker), ["NaatreWorkerError", "createFetchWorkerAdapter", "createWorker", "defineHandler", "workerProtocol", "workerRuntimeVersion"]);
  assert.deepEqual(Object.keys(nodeWorker), ["createNodeWorkerAdapter"]);
  assert.deepEqual(Object.keys(bunWorker), ["createBunWorkerAdapter"]);
  assert.deepEqual(Object.keys(denoWorker), ["createDenoWorkerAdapter"]);
  assert.deepEqual(Object.keys(edgeWorker), ["createEdgeWorkerAdapter"]);
});
