import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const manifest = JSON.parse(readFileSync(resolve(root, "conformance/v1/http-digest-integration.json"), "utf8"));

function require(condition, message) {
  if (!condition) throw new Error(message);
}

function verifyFiles(entries) {
  for (const entry of entries) {
    require(Object.keys(entry).length === 2 && typeof entry.path === "string" && /^[0-9a-f]{64}$/u.test(entry.sha256), "invalid evidence entry");
    const content = readFileSync(resolve(root, entry.path));
    require(createHash("sha256").update(content).digest("hex") === entry.sha256, `evidence mismatch: ${entry.path}`);
  }
}

require(manifest.profile === "implementation.go.http-digest-1", "unexpected implementation profile");
require(manifest.contract.profile === "core.http.digest-1" && manifest.contract.ownerIssue === 63, "contract authority drift");
require(/^[0-9a-f]{40}$/u.test(manifest.contract.gitCommit), "dependency commit is not exact");
require(manifest.implementation.ownerIssue === 104 && manifest.implementation.minimumGo === "1.27", "implementation boundary drift");
verifyFiles(manifest.contract.files);
verifyFiles(manifest.implementation.files);

const requiredClasses = ["positive", "negative", "malformed", "boundary", "cancellation", "limit", "security"];
const classes = new Set(manifest.cases.map((test) => test.class));
for (const fixtureClass of requiredClasses) require(classes.has(fixtureClass), `missing fixture class: ${fixtureClass}`);
for (const test of manifest.cases) {
  require(["headers", "content", "representation", "completion"].includes(test.phase), `invalid phase: ${test.name}`);
  require(test.code === "" || /^[A-Z][A-Z0-9_]+$/u.test(test.code), `unstable code: ${test.name}`);
}

require(manifest.runtimeBoundary.unsupported.includes("digest-only trailers"), "trailer boundary omitted");
require(manifest.runtimeBoundary.unsupported.includes("non-Go SDK transports"), "SDK boundary omitted");
require(manifest.verificationCommands.every((command) => Array.isArray(command) && command.length > 1 && command.every((argument) => typeof argument === "string" && !/token|secret|password/iu.test(argument))), "unsafe verification command");
