import { readFileSync } from "node:fs";

const fixture = JSON.parse(
  readFileSync(new URL("../v1/reliability.json", import.meta.url), "utf8"),
);

const exact = (actual, expected, label) => {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(`${label} mismatch`);
  }
};

if (fixture.profile !== "core.reliability-1") {
  throw new Error("invalid reliability profile");
}
exact(fixture.policies, ["non-idempotent", "idempotent", "conditionally-idempotent"], "policies");
exact(fixture.states, ["running", "completed", "indeterminate"], "states");
exact(fixture.durabilities, ["process-local", "durable"], "durabilities");

const required = new Set([
  "handler-failure-retries",
  "transport-interruption-is-indeterminate",
  "concurrent-waiter-replays",
  "completed-record-expires",
  "running-lease-expires-indeterminate",
  "store-outage-fails-closed",
  "crash-after-effect-before-result",
  "stale-owner-is-fenced",
  "process-local-provider-restart",
  "mismatched-fingerprint-conflicts",
  "replay-is-reauthorized",
  "shared-budget-bounds-layers",
  "deadline-cancels-scheduling",
  "retry-after-lower-bound",
]);
const names = new Set();
for (const vector of fixture.vectors) {
  if (!vector.name || names.has(vector.name) || !fixture.states.includes(vector.state)) {
    throw new Error(`invalid reliability vector ${vector.name}`);
  }
  if (!Number.isInteger(vector.attempts) || vector.attempts < 0) {
    throw new Error(`invalid attempt count for ${vector.name}`);
  }
  names.add(vector.name);
}
for (const name of required) {
  if (!names.has(name)) throw new Error(`missing reliability vector ${name}`);
}

console.log(`reliability conformance: ${fixture.vectors.length} vectors passed`);
