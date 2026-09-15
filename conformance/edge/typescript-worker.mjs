import { createEvidenceWorker } from "./runtime-evidence.mjs";

export default createEvidenceWorker({
  runtimeKey: "NAATRE_WORKER_RUNTIME_ID",
  noAutorunKey: "NAATRE_WORKER_NO_AUTORUN",
  runtime: "workerd-2026-09-14",
  run: async () => (await import("../independent/typescript-worker-runtime.mjs")).runWorkerRuntime(undefined, { typescript: false }),
});
