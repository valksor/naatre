using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (name = "main", worker = (
      compatibilityDate = "2026-09-14",
      modules = [
        (name = "conformance/edge/typescript-adapter-worker.mjs", esModule = embed "typescript-adapter-worker.mjs"),
        (name = "conformance/independent/typescript-adapter-runtime.mjs", esModule = embed "../independent/typescript-adapter-runtime.mjs"),
        (name = "sdk/typescript/runtime/error.mjs", esModule = embed "../../sdk/typescript/runtime/error.mjs"),
        (name = "sdk/typescript/runtime/index.mjs", esModule = embed "../../sdk/typescript/runtime/index.mjs"),
        (name = "sdk/typescript/runtime/json.mjs", esModule = embed "../../sdk/typescript/runtime/json.mjs"),
        (name = "sdk/typescript/runtime/operation.mjs", esModule = embed "../../sdk/typescript/runtime/operation.mjs"),
        (name = "sdk/typescript/runtime/result.mjs", esModule = embed "../../sdk/typescript/runtime/result.mjs"),
        (name = "sdk/typescript/runtime/scalars.mjs", esModule = embed "../../sdk/typescript/runtime/scalars.mjs"),
        (name = "sdk/typescript/runtime/stream.mjs", esModule = embed "../../sdk/typescript/runtime/stream.mjs"),
        (name = "sdk/typescript/runtime/transport.mjs", esModule = embed "../../sdk/typescript/runtime/transport.mjs")
      ]
    ))
  ],
  sockets = [
    (name = "http", address = "127.0.0.1:0", http = (), service = "main")
  ]
);
