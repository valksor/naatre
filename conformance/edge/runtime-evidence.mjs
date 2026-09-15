export function createEvidenceWorker({ runtimeKey, noAutorunKey, runtime, run }) {
  return Object.freeze({
    async fetch() {
      globalThis[runtimeKey] = runtime;
      globalThis[noAutorunKey] = true;
      try {
        return Response.json(await run());
      } catch (error) {
        return Response.json({ status: "failed", error: String(error?.stack ?? error) }, { status: 500 });
      }
    },
  });
}
