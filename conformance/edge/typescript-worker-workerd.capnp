using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (name = "main", worker = (
      compatibilityDate = "2026-09-14",
      modules = [
        (name = "conformance/edge/typescript-worker.mjs", esModule = embed "typescript-worker.mjs"),
        (name = "conformance/edge/runtime-evidence.mjs", esModule = embed "runtime-evidence.mjs"),
        (name = "conformance/independent/typescript-worker-runtime.mjs", esModule = embed "../independent/typescript-worker-runtime.mjs"),
        (name = "conformance/independent/runtime-id.mjs", esModule = embed "../independent/runtime-id.mjs"),
        (name = "sdk/typescript/runtime/error.mjs", esModule = embed "../../sdk/typescript/runtime/error.mjs"),
        (name = "sdk/typescript/runtime/json.mjs", esModule = embed "../../sdk/typescript/runtime/json.mjs"),
        (name = "sdk/typescript/runtime/scalars.mjs", esModule = embed "../../sdk/typescript/runtime/scalars.mjs"),
        (name = "sdk/typescript/runtime/server.mjs", esModule = embed "../../sdk/typescript/runtime/server.mjs"),
        (name = "sdk/typescript/runtime/worker-runtime.mjs", esModule = embed "../../sdk/typescript/runtime/worker-runtime.mjs"),
        (name = "sdk/typescript/runtime/worker-node.mjs", esModule = embed "../../sdk/typescript/runtime/worker-node.mjs"),
        (name = "sdk/typescript/runtime/worker-bun.mjs", esModule = embed "../../sdk/typescript/runtime/worker-bun.mjs"),
        (name = "sdk/typescript/runtime/worker-deno.mjs", esModule = embed "../../sdk/typescript/runtime/worker-deno.mjs"),
        (name = "sdk/typescript/runtime/worker-edge.mjs", esModule = embed "../../sdk/typescript/runtime/worker-edge.mjs"),
        (name = "sdk/typescript/runtime/fixtures/plain-worker-handler.mjs", esModule = embed "../../sdk/typescript/runtime/fixtures/plain-worker-handler.mjs"),
        (name = "sdk/typescript/runtime/fixtures/worker-test-contract.mjs", esModule = embed "../../sdk/typescript/runtime/fixtures/worker-test-contract.mjs")
      ]
    ))
  ],
  sockets = [
    (name = "http", address = "127.0.0.1:0", http = (), service = "main")
  ]
);
