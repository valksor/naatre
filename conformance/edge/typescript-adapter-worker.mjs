import { createEvidenceWorker } from "./runtime-evidence.mjs";

export default createEvidenceWorker({
  runtimeKey: "NAATRE_RUNTIME_ID",
  noAutorunKey: "NAATRE_ADAPTER_NO_AUTORUN",
  runtime: "edge-workerd-2026-09-14",
  run: async () => (await import("../independent/typescript-adapter-runtime.mjs")).runAdapterRuntime(),
});
