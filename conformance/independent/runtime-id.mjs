export function runtimeID(overrideName, fallback) {
  if (typeof globalThis[overrideName] === "string") return globalThis[overrideName];
  if (globalThis.Deno?.version?.deno) return `deno-${globalThis.Deno.version.deno}`;
  if (globalThis.Bun?.version) return `bun-${globalThis.Bun.version}`;
  if (globalThis.process?.versions?.node) return `node-${globalThis.process.versions.node}`;
  return fallback;
}
