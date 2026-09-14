import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const interactions = JSON.parse(readFileSync(new URL("../v1/interactions.json", import.meta.url), "utf8"));
const fixture = interactions.cases.find((entry) => entry.name === "supervised-uncooperative-process");
if (!fixture) throw new Error("supervised fixture is missing");

const { deadlineMs, killAfterMs, requireExit } = fixture.supervision;
const child = spawn(process.execPath, [fileURLToPath(new URL("uncooperative-child.mjs", import.meta.url))], {
  stdio: ["ignore", "pipe", "inherit"],
});
let admitted = false;
let forced = false;
let settled = false;

const deadline = setTimeout(() => {
  if (!settled) {
    child.kill("SIGKILL");
    process.stderr.write("supervision deadline exceeded\n");
    process.exitCode = 1;
  }
}, deadlineMs);

child.stdout.setEncoding("utf8");
child.stdout.on("data", (chunk) => {
  if (admitted || !chunk.includes("admitted\n")) return;
  admitted = true;
  child.kill("SIGTERM");
  setTimeout(() => {
    forced = child.kill("SIGKILL");
  }, killAfterMs);
});

child.on("error", (error) => {
  clearTimeout(deadline);
  settled = true;
  process.stderr.write(String(error.message) + "\n");
  process.exitCode = 1;
});

child.on("exit", (code, signal) => {
  clearTimeout(deadline);
  settled = true;
  const passed = admitted && forced && signal === "SIGKILL" && (!requireExit || code === null);
  process.stdout.write(JSON.stringify({
    profile: "suite.supervision-1",
    status: passed ? "passed" : "failed",
    states: ["starting", "ready", "draining", "forced"],
    code: passed ? "RESOURCE_EXHAUSTED" : "PROFILE_FAILED",
    admitted,
    gracefulSignal: "SIGTERM",
    forcedSignal: signal,
    exited: code !== null || signal !== null,
  }) + "\n");
  if (!passed) process.exitCode = 1;
});
