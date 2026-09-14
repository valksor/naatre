export default {
  async fetch() {
    globalThis.NAATRE_RUNTIME_ID = "edge-workerd-2026-09-14";
    globalThis.NAATRE_ADAPTER_NO_AUTORUN = true;
    try {
      const { runAdapterRuntime } = await import("../independent/typescript-adapter-runtime.mjs");
      return Response.json(await runAdapterRuntime());
    } catch (error) {
      return Response.json({ status: "failed", error: String(error?.stack ?? error) }, { status: 500 });
    }
  },
};
