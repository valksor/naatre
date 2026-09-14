import assert from "node:assert/strict";
import test from "node:test";

test("package adapter subpaths expose only their declared runtime surface", async () => {
  const fetchAdapter = await import("@naatre/sdk/fetch");
  const websocketAdapter = await import("@naatre/sdk/websocket");
  assert.deepEqual(Object.keys(fetchAdapter), ["createFetchAdapter"]);
  assert.deepEqual(Object.keys(websocketAdapter), ["createWebSocketAdapter", "decodeSSEStream"]);
});
