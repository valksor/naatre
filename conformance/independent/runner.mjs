import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import { arch, platform } from "node:process";
import { fileURLToPath } from "node:url";

import { hasUnpairedSurrogate, hasUnpairedSurrogateValue } from "./unicode.mjs";

const protocol = "naatre.conformance.runner-1";
const maxInputBytes = 1024 * 1024;
const suiteURL = new URL("../v1/suite.json", import.meta.url);
const conformanceURL = new URL("../", import.meta.url);
const suite = JSON.parse(readFileSync(suiteURL, "utf8"));
const implementations = new Map([
  ["suite.contract-1", null],
  ["suite.profiles-1", "profiles.mjs"],
  ["suite.supervision-1", "supervise.mjs"],
  ["core.scalar.c14n-1", "scalars.mjs"],
  ["core.interop.c14n-1", "canonical.mjs"],
  ["collection.query.codegen-1", "verify-collection-query-generation.mjs"],
  ["core.validation-1", "validation.mjs"],
  ["sdk.dart.core-1", "verify-dart-sdk.mjs"],
  ["sdk.generation-1", "verify-generator.mjs"],
  ["sdk.php.core-1", "verify-php-sdk.mjs"],
  ["sdk.python.core-1", "verify-python-sdk.py"],
  ["sdk.rust.core-1", "verify-rust-sdk.mjs"],
  ["sdk.jvm.core-1", "verify-jvm-sdk.mjs"],
  ["sdk.dotnet.core-1", "verify-dotnet-sdk.mjs"],
  ["sdk.typescript.adapters-1", "verify-typescript-adapters.mjs"],
  ["sdk.typescript.core-1", "verify-typescript-sdk.mjs"],
  ["sdk.swift.core-1", "verify-swift-sdk.mjs"],
]);
const profileTimeouts = new Map([["sdk.jvm.core-1", 240_000]]);

class ProtocolError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

function runnerIdentity() {
  return {
    name: "naatre-independent-js",
    version: "1.0.0",
    language: "javascript-typescript",
    platform: `${platform}-${arch}-node-${process.versions.node}`,
  };
}

function profileInventory() {
  const profiles = new Set(suite.files.map((file) => file.profile));
  for (const profile of implementations.keys()) profiles.add(profile);
  return [...profiles].sort().map((profile) => ({
    profile,
    status: implementations.has(profile) ? "supported" : "unsupported",
  }));
}

function response(id, path, results = []) {
  const value = {
    protocol,
    id,
    fixtureVersion: suite.fixtureVersion,
    runner: runnerIdentity(),
    profiles: profileInventory(),
    results,
  };
  if (path) value.path = path;
  return value;
}

function resultShape(profile, status, fields = {}) {
  return {
    profile,
    status,
    capabilities: fields.capabilities ?? [],
    phase: fields.phase ?? "",
    code: fields.code ?? "",
    source: fields.source ?? {},
    path: fields.path ?? [],
    data: fields.data ?? null,
    errors: fields.errors ?? [],
    canonicalBytes: fields.canonicalBytes ?? "",
    diagnostics: fields.diagnostics ?? [],
    evidence: fields.evidence ?? [],
    ...(fields.reason ? { reason: fields.reason } : {}),
  };
}

function failureStatus(error) {
  return error instanceof ProtocolError ? "failed" : "infrastructure-failure";
}

function validateRequest(request) {
  requireObject(request, "request");
  requireKeys(request, ["protocol", "id", "command", "path", "profiles"], "request");
  if (request.protocol !== protocol) throw new ProtocolError("UNSUPPORTED_RUNNER_PROTOCOL", "unsupported runner protocol");
  if (!boundedString(request.id, 128)) throw new ProtocolError("INVALID_RUNNER_REQUEST", "id must be a bounded non-empty string");
  if (request.command !== "discover" && request.command !== "run") throw new ProtocolError("INVALID_RUNNER_REQUEST", "command must be discover or run");
  if (request.path !== undefined) validatePath(request.path);
  if (request.profiles !== undefined) {
    if (!Array.isArray(request.profiles) || request.profiles.length === 0 || request.profiles.length > 128 || new Set(request.profiles).size !== request.profiles.length || request.profiles.some((entry) => !boundedString(entry, 128))) {
      throw new ProtocolError("INVALID_RUNNER_REQUEST", "profiles must be a bounded unique string array");
    }
  }
  if (request.command === "run" && (request.path === undefined || request.profiles === undefined)) {
    throw new ProtocolError("INVALID_RUNNER_REQUEST", "run requires a path and profiles");
  }
}

function validatePath(path) {
  requireObject(path, "path");
  requireKeys(path, ["source", "destination"], "path");
  for (const name of ["source", "destination"]) {
    const endpoint = path[name];
    requireObject(endpoint, `path.${name}`);
    requireKeys(endpoint, ["kind", "language", "profile"], `path.${name}`);
    if (!["sdk", "codec", "gateway", "worker", "http-server", "native-runtime"].includes(endpoint.kind) || !boundedString(endpoint.language, 64)) {
      throw new ProtocolError("INVALID_RUNNER_PATH", `invalid ${name} endpoint`);
    }
    if (endpoint.profile !== undefined && !boundedString(endpoint.profile, 128)) {
      throw new ProtocolError("INVALID_RUNNER_PATH", `invalid ${name} profile`);
    }
  }
}

function boundedString(value, maximum) {
  const length = typeof value === "string" ? Array.from(value).length : 0;
  return length > 0 && length <= maximum && !hasUnpairedSurrogate(value);
}

function handle(request) {
  try {
    validateRequest(request);
    if (request.command === "discover") return response(request.id);
    return response(request.id, request.path, request.profiles.map(runProfile));
  } catch (error) {
    const code = error.code ?? "RUNNER_INTERNAL";
    return response(boundedString(request?.id, 128) ? request.id : "invalid", undefined, [
      resultShape("runner", failureStatus(error), {
        phase: "runner",
        code,
        diagnostics: [{ phase: "runner", code, message: safeMessage(error), source: {}, path: [] }],
      }),
    ]);
  }
}

function runProfile(profile) {
  if (!implementations.has(profile)) {
    return resultShape(profile, "unsupported", { reason: "profile is not implemented by this runner" });
  }
  if (profile === "suite.contract-1") return verifySuite();
  const script = implementations.get(profile);
  const executable = script.endsWith(".py") ? (process.env.PYTHON ?? "python3") : process.execPath;
  const executed = spawnSync(executable, [fileURLToPath(new URL(script, import.meta.url))], {
    encoding: "utf8",
    timeout: profileTimeouts.get(profile) ?? 30_000,
    timeout: profile === "sdk.dotnet.core-1" ? 120_000 : 30_000,
  });
  const evidence = evidenceForProfile(profile);
  if (executed.error || executed.status !== 0) {
    return resultShape(profile, executed.error ? "infrastructure-failure" : "failed", {
      phase: "runner",
      code: "PROFILE_FAILED",
      diagnostics: [{
        phase: "runner",
        code: "PROFILE_FAILED",
        message: "independent profile execution failed",
        source: {},
        path: [],
      }],
      evidence,
    });
  }
  return resultShape(profile, "passed", { capabilities: [profile], evidence });
}

function verifySuite() {
  const diagnostics = [];
  const evidence = [];
  for (const file of suite.files) {
    try {
      const content = readFileSync(new URL(file.path, conformanceURL));
      const digest = createHash("sha256").update(content).digest("hex");
      evidence.push({ fixture: file.path, sha256: digest });
      if (digest !== file.sha256) {
        diagnostics.push({ phase: "runner", code: "FIXTURE_DIGEST_MISMATCH", message: `fixture digest mismatch: ${file.path}`, source: {}, path: [] });
      }
    } catch {
      diagnostics.push({ phase: "runner", code: "FIXTURE_UNAVAILABLE", message: `fixture unavailable: ${file.path}`, source: {}, path: [] });
    }
  }
  if (diagnostics.length > 0) {
    return resultShape("suite.contract-1", "failed", { phase: "runner", code: diagnostics[0].code, diagnostics, evidence });
  }
  return resultShape("suite.contract-1", "passed", { capabilities: ["suite.contract-1"], evidence });
}

function evidenceForProfile(profile) {
  const fixtureProfiles = profile === "suite.supervision-1"
    ? new Set(["suite.interactions-1"])
    : profile === "suite.profiles-1"
      ? new Set(["suite.profiles-1", "suite.compatibility-1"])
    : new Set([profile]);
  return suite.files
    .filter((file) => fixtureProfiles.has(file.profile))
    .map((file) => ({ fixture: file.path, sha256: file.sha256 }));
}

function requireObject(value, label) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new ProtocolError("INVALID_RUNNER_REQUEST", `${label} must be an object`);
}

function requireKeys(value, allowed, label) {
  const unknown = Object.keys(value).find((key) => !allowed.includes(key));
  if (unknown !== undefined) throw new ProtocolError("INVALID_RUNNER_REQUEST", `${label} contains unknown member`);
}

function safeMessage(error) {
  return error?.code ? String(error.message).slice(0, 1024) : "runner failed";
}

function parseRequest(input) {
  if (Buffer.byteLength(input) > maxInputBytes) throw new ProtocolError("RUNNER_REQUEST_TOO_LARGE", "runner request exceeds one MiB");
  let text;
  try {
    text = Buffer.isBuffer(input) ? new TextDecoder("utf-8", { fatal: true }).decode(input) : input;
  } catch {
    throw new ProtocolError("MALFORMED_RUNNER_JSON", "runner request is not valid UTF-8");
  }
  try {
    const request = JSON.parse(text);
    if (hasUnpairedSurrogateValue(request)) {
      throw new ProtocolError("MALFORMED_RUNNER_JSON", "runner request contains an unpaired Unicode surrogate");
    }
    return request;
  } catch {
    throw new ProtocolError("MALFORMED_RUNNER_JSON", "runner request is not valid JSON");
  }
}

function writeNDJSON(value) {
  process.stdout.write(`${JSON.stringify(value)}\n`);
}

function responsePassed(value) {
  return value.results.every((result) => result.status === "passed");
}

async function startStdio(requirePass) {
  let pending = Buffer.alloc(0);
  let oversized = false;
  const processLine = (line) => {
    if (line.length === 0) return;
    let value;
    let request;
    try {
      request = parseRequest(line);
      value = handle(request);
    } catch (error) {
      const code = error.code ?? "RUNNER_INTERNAL";
      const id = boundedString(request?.id, 128) ? request.id : "invalid";
      value = response(id, undefined, [resultShape("runner", failureStatus(error), {
        phase: "runner",
        code,
        diagnostics: [{ phase: "runner", code, message: safeMessage(error), source: {}, path: [] }],
      })]);
    }
    writeNDJSON(value);
    if (requirePass && !responsePassed(value)) process.exitCode = 1;
  };
  for await (const chunk of process.stdin) {
    let offset = 0;
    while (offset < chunk.length) {
      const newline = chunk.indexOf(0x0a, offset);
      const end = newline < 0 ? chunk.length : newline;
      const fragment = chunk.subarray(offset, end);
      if (!oversized) {
        if (pending.length + fragment.length > maxInputBytes) {
          oversized = true;
          pending = Buffer.alloc(0);
        } else {
          pending = Buffer.concat([pending, fragment]);
        }
      }
      if (newline < 0) break;
      if (oversized) {
        processLine(Buffer.alloc(maxInputBytes + 1));
        oversized = false;
      } else {
        if (pending.at(-1) === 0x0d) pending = pending.subarray(0, pending.length - 1);
        processLine(pending);
      }
      pending = Buffer.alloc(0);
      offset = newline + 1;
    }
  }
  if (oversized) processLine(Buffer.alloc(maxInputBytes + 1));
  else processLine(pending);
}

function startHTTP(address) {
  const separator = address.lastIndexOf(":");
  const host = separator < 0 ? "127.0.0.1" : address.slice(0, separator);
  const port = Number(separator < 0 ? address : address.slice(separator + 1));
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new ProtocolError("INVALID_LISTEN_ADDRESS", "invalid HTTP runner port");
  const server = createServer((request, reply) => {
    if (request.method === "GET" && request.url === "/v1/conformance/profiles") {
      return writeHTTP(reply, 200, response("http-discover"));
    }
    if (request.method !== "POST" || request.url !== "/v1/conformance/run") {
      return writeHTTP(reply, 404, { code: "NOT_FOUND" });
    }
    if (!validJSONMediaType(request.headers["content-type"])) {
      return writeHTTP(reply, 415, { code: "UNSUPPORTED_MEDIA_TYPE" });
    }
    let size = 0;
    const chunks = [];
    request.on("data", (chunk) => {
      size += chunk.length;
      if (size <= maxInputBytes) chunks.push(chunk);
    });
    request.on("end", () => {
      if (size > maxInputBytes) return writeHTTP(reply, 413, { code: "RUNNER_REQUEST_TOO_LARGE" });
      let decoded;
      try {
        decoded = parseRequest(Buffer.concat(chunks));
        validateRequest(decoded);
        if (decoded.command !== "run") throw new ProtocolError("INVALID_RUNNER_REQUEST", "HTTP run endpoint requires the run command");
        return writeHTTP(reply, 200, handle(decoded));
      } catch (error) {
        const code = error.code ?? "RUNNER_INTERNAL";
        const id = boundedString(decoded?.id, 128) ? decoded.id : "invalid";
        return writeHTTP(reply, 400, response(id, undefined, [resultShape("runner", failureStatus(error), {
          phase: "runner",
          code,
          diagnostics: [{ phase: "runner", code, message: safeMessage(error), source: {}, path: [] }],
        })]));
      }
    });
  });
  server.listen(port, host, () => {
    const bound = server.address();
    process.stderr.write("naatre-conformance-listening " + host + ":" + bound.port + "\n");
  });
}

function validJSONMediaType(value) {
  if (typeof value !== "string") return false;
  const parts = value.split(";").map((part) => part.trim());
  if (parts.shift()?.toLowerCase() !== "application/json") return false;
  return parts.every((part) => {
    const match = /^charset=(?:"utf-8"|utf-8)$/i.exec(part);
    return match !== null;
  }) && parts.length <= 1;
}

function writeHTTP(reply, status, value) {
  const body = Buffer.from(JSON.stringify(value));
  reply.writeHead(status, { "Content-Type": "application/json; charset=utf-8", "Content-Length": body.length });
  reply.end(body);
}

const httpArgument = process.argv.find((argument) => argument.startsWith("--http="));
const requirePass = process.argv.includes("--require-pass");
if (httpArgument && requirePass) throw new ProtocolError("INVALID_RUNNER_ARGUMENT", "--require-pass is only valid for the NDJSON binding");
if (httpArgument) startHTTP(httpArgument.slice("--http=".length));
else await startStdio(requirePass);
