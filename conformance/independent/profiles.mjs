#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const suite = readJSON(new URL("../v1/suite.json", import.meta.url));
const registry = readJSON(new URL("../v1/profiles.json", import.meta.url));
const matrix = readJSON(new URL("../v1/compatibility.json", import.meta.url));
const reportSchema = readJSON(new URL("../profile-report.schema.json", import.meta.url));
const requiredFixtureClasses = ["positive", "negative", "malformed", "limit", "cancellation", "security"];
const resultStatuses = ["passed", "failed", "unsupported", "invalid-skip", "infrastructure-failure"];
const cborCapability = "transport.cbor.unary-1";
const cborCodecRevision = "cbor-det-1";
const cborReportBinding = `${cborCapability}@${cborCodecRevision}`;
const profilesByID = new Map(registry.profiles.map((profile) => [profile.id, profile]));

function readJSON(location) {
  return JSON.parse(readFileSync(location, "utf8"));
}

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function sortedUnique(values) {
  return [...new Set(values)].sort();
}

function sameSet(left, right) {
  return JSON.stringify(sortedUnique(left)) === JSON.stringify(sortedUnique(right));
}

function profileByID(id) {
  return profilesByID.get(id);
}

function requiredClauses(profile) {
  return sortedUnique(suite.normativeSources
    .filter((source) => profile.requiredNormativeSources.includes(source.caseId))
    .flatMap((source) => source.clauses));
}

function requiredFixtures(profile) {
  const paths = sortedUnique(suite.normativeSources
    .filter((source) => profile.requiredNormativeSources.includes(source.caseId))
    .flatMap((source) => source.fixtureRefs.map((reference) => reference.path))
    .filter((path) => path !== "v1/suite.json"));
  const files = new Map(suite.files.map((file) => [file.path, file]));
  return paths.map((path) => {
    const file = files.get(path);
    requireValue(file, `profile ${profile.id} references an unpinned fixture: ${path}`);
    return { path, profile: file.profile, sha256: file.sha256 };
  });
}

function validateRegistry() {
  requireValue(registry.profile === "suite.profiles-1", "invalid profile registry identity");
  requireValue(registry.specVersion === suite.specVersion, "profile/spec version skew");
  requireValue(registry.fixtureVersion === suite.fixtureVersion, "profile/fixture version skew");
  requireValue(registry.runnerProtocol === suite.runnerProtocol, "profile/runner version skew");
  requireValue(registry.reportProtocol === "naatre.conformance.report-1", "invalid report protocol");
  requireValue(reportSchema.$id === "https://naatre.dev/conformance/profile-report-1.schema.json", "invalid report schema identity");
  const schemaBytes = readFileSync(new URL("../profile-report.schema.json", import.meta.url));
  requireValue(createHash("sha256").update(schemaBytes).digest("hex") === registry.reportSchemaSHA256, "profile report schema digest mismatch");
  requireValue(sameSet(registry.resultStatuses.map((entry) => entry.status), resultStatuses), "incomplete result status vocabulary");
  requireValue(registry.resultStatuses.find((entry) => entry.status === "unsupported")?.scope === "optional-capability-only", "unsupported must be optional-capability-only");
  for (const rule of ["exactProfileExecutionRequired", "exactClauseExecutionRequired", "exactFixtureDigestsRequired", "unsupportedRequiredTestsFail", "partialRunsCannotCertify", "delegatedExecutionDoesNotGrantRuntimeEvidence", "experimentalProfilesDoNotReduceRoadmapScope"]) {
    requireValue(registry.claimRules[rule] === true, `profile claim rule is disabled: ${rule}`);
  }
  requireValue(registry.claimRules.versionSkewStatus === "infrastructure-failure", "version skew must be an infrastructure failure");

  const ids = new Set();
  for (const profile of registry.profiles) {
    requireValue(!ids.has(profile.id), `duplicate profile ${profile.id}`);
    ids.add(profile.id);
    requireValue(profile.stability === "stable" || profile.stability === "experimental", `invalid stability for ${profile.id}`);
    requireValue(profile.evidenceRole && profile.requiredNormativeSources.length > 0 && profile.requiredSpecs.length > 0, `incomplete profile ${profile.id}`);
    const selectedSources = suite.normativeSources.filter((source) => profile.requiredNormativeSources.includes(source.caseId));
    requireValue(selectedSources.length === profile.requiredNormativeSources.length, `unknown normative source in ${profile.id}`);
    requireValue(sameSet(profile.requiredSpecs, selectedSources.map((source) => source.spec)), `incomplete normative source inventory for ${profile.id}`);
    requireValue(sameSet(profile.requiredClauses, requiredClauses(profile)), `incomplete normative clause inventory for ${profile.id}`);
    requireValue(JSON.stringify(profile.requiredFixtures) === JSON.stringify(requiredFixtures(profile)), `incomplete fixture inventory for ${profile.id}`);
    requireValue(sameSet(profile.requiredFixtureClasses, requiredFixtureClasses), `incomplete fixture classes for ${profile.id}`);
    for (const capability of profile.optionalCapabilities) {
      requireValue(capability.required === false, `optional capability is marked required: ${capability.id}`);
      requireValue(profile.requiredFixtures.some((fixture) => fixture.path === capability.fixture), `optional capability fixture is not pinned: ${capability.id}`);
    }
  }

  const stable = profileByID("release.stable-1");
  requireValue(stable?.certificationExecutionOwnerIssue === 69 && stable.publicationOwnerIssue === 69, "stable release ownership must remain with issue 69");
  requireValue(stable.requiredEvidence.some((entry) => entry.language === "go" && entry.profile === "runtime.execution-1"), "stable release omits Go runtime evidence");
  requireValue(stable.requiredEvidence.some((entry) => entry.language === "javascript-typescript" && entry.profile === "sdk.client-1"), "stable release omits JavaScript/TypeScript client evidence");
}

function validateMatrix() {
  requireValue(matrix.profile === "suite.compatibility-1", "invalid compatibility matrix identity");
  requireValue(matrix.profileRegistryVersion === registry.registryVersion, "matrix/profile version skew");
  requireValue(matrix.specVersion === registry.specVersion && matrix.fixtureVersion === registry.fixtureVersion, "matrix suite version skew");
  requireValue(matrix.executionOwnerIssue === 69 && matrix.publicationOwnerIssue === 69, "matrix ownership must remain with issue 69");
  requireValue(matrix.releaseStatus === "awaiting-complete-evidence", "matrix cannot advertise a release before issue 69 evidence exists");
  requireValue(sameSet(matrix.integrationPaths.map((path) => path.destination.language), ["php", "python", "javascript-typescript", "rust"]), "remote-worker integration path inventory is incomplete");
  for (const path of matrix.integrationPaths) {
    requireValue(path.source.implementation === "naatre-go" && path.source.language === "go" && path.source.kind === "gateway" && path.destination.kind === "worker" && path.destination.implementation.endsWith("-worker"), "remote-worker evidence path is not Go-gateway-to-worker");
    requireValue(path.profile === "worker.remote-1" && path.status === "planned" && path.executionOwnerIssue === 69 && path.report === null, "remote-worker path is advertised without issue 69 evidence");
  }
  requireValue(sameSet(matrix.implementations.map((entry) => entry.language), suite.languages), "compatibility matrix language inventory is incomplete");
  for (const entry of matrix.implementations) {
    requireValue(entry.status === "planned" && entry.claims.length === 0 && entry.reports.length === 0, `unevidenced implementation is advertised: ${entry.implementation}`);
    requireValue(!entry.plannedProfiles.includes("worker.remote-1"), `SDK/runtime row conflates remote-worker evidence: ${entry.implementation}`);
    for (const profile of entry.plannedProfiles) requireValue(profileByID(profile), `matrix references unknown profile ${profile}`);
    for (const field of ["operatingSystems", "architectures", "runtimeVersions", "featureFlags", "wireTransports", "streamTransports", "scalarPrecision", "cancellationCapabilities"]) {
      requireValue(Array.isArray(entry.environment[field]), `matrix entry ${entry.implementation} omits ${field}`);
    }
  }
  requireValue(Array.isArray(matrix.thirdPartyImplementations) && matrix.thirdPartySubmission.reportSchema === registry.reportSchema, "third-party evidence policy is incomplete");
}

function validateReport(report) {
  requireValue(report && typeof report === "object" && !Array.isArray(report), "report must be an object");
  validateReportShape(report);
  rejectSecrets(report);
  requireValue(report.protocol === registry.reportProtocol, "report protocol mismatch");
  requireValue(report.versions?.spec === registry.specVersion, "report spec version skew");
  requireValue(report.versions?.fixtures === registry.fixtureVersion, "report fixture version skew");
  requireValue(report.versions?.profiles === registry.registryVersion, "report profile version skew");
  requireValue(report.versions?.runnerProtocol === registry.runnerProtocol, "report runner version skew");
  requireValue(report.versions?.canonicalization === "c14n-1", "report canonicalization version skew");
  requireValue(report.implementation?.specVersion === report.versions.spec, "implementation/spec version skew");
  requireValue(report.implementation?.fixtureVersion === report.versions.fixtures, "implementation/fixture version skew");
  requireValue(report.implementation?.profileVersion === report.versions.profiles, "implementation/profile version skew");
  requireValue(report.implementation?.schemaRevision === report.versions.schemaRevision, "implementation/schema version skew");
  requireValue(suite.languages.includes(report.implementation?.language) || report.implementation?.language === "other", "unknown implementation language");
  requireValue(Array.isArray(report.run?.command) && report.run.command.length > 0 && Array.isArray(report.run?.artifacts) && report.run.artifacts.length > 0, "report lacks reproducible command or artifacts");
  requireValue(report.path?.source?.kind && report.path?.destination?.kind, "report path is incomplete");
  requireValue(Array.isArray(report.claims) && Array.isArray(report.results), "report claims/results are missing");

  const results = new Map();
  for (const result of report.results) {
    const profile = profileByID(result.profile);
    requireValue(profile, `result references unknown profile ${result.profile}`);
    requireValue(!results.has(result.profile), `duplicate result ${result.profile}`);
    results.set(result.profile, result);
    requireValue(result.evidenceRole === profile.evidenceRole, `evidence role mismatch for ${result.profile}`);
    requireValue(resultStatuses.includes(result.status), `invalid result status ${result.status}`);
    validateStatus(result);
  }

  const claims = new Set();
  for (const claim of report.claims) {
    requireValue(!claims.has(claim.profile), `duplicate claim ${claim.profile}`);
    claims.add(claim.profile);
    const profile = profileByID(claim.profile);
    const result = results.get(claim.profile);
    requireValue(profile && result?.status === "passed", `profile was claimed without a passing execution: ${claim.profile}`);
    requireValue(result.evidenceRole === profile.evidenceRole, `evidence role mismatch for ${claim.profile}`);
    requireValue(sameSet(result.executedClauses, profile.requiredClauses), `partial clause execution cannot certify ${claim.profile}`);
    const evidence = result.evidence.map((entry) => ({ path: entry.fixture, profile: profile.requiredFixtures.find((fixture) => fixture.path === entry.fixture)?.profile, sha256: entry.sha256 }));
    requireValue(JSON.stringify(evidence) === JSON.stringify(profile.requiredFixtures), `partial or skewed fixture evidence cannot certify ${claim.profile}`);
    requireValue(profile.eligiblePaths.some((path) => path.source === report.path.source.kind && path.destination === report.path.destination.kind), `execution path cannot certify ${claim.profile}`);
    const optional = new Set(profile.optionalCapabilities.map((capability) => capability.id));
    requireValue(claim.capabilities.every((capability) => optional.has(capability) && result.capabilities.includes(capability)), `unexecuted capability claimed for ${claim.profile}`);
  }
  return true;
}

function validateReportShape(report) {
  closedObject(report, ["protocol", "run", "versions", "implementation", "environment", "path", "claims", "results"], "report");
  closedObject(report.run, ["id", "operator", "command", "artifacts"], "run");
  boundedArray(report.run.command, 1, 32, "run.command");
  for (const argument of report.run.command) {
    boundedString(argument, 512, "run.command argument");
    requireValue(!/(?:^|[-_])(?:token|secret|password|authorization|cookie)(?:[=_-]|$)/i.test(argument), "publish-unsafe command argument");
  }
  boundedArray(report.run.artifacts, 1, 128, "run.artifacts");
  for (const artifact of report.run.artifacts) {
    closedObject(artifact, ["name", "sha256"], "run artifact");
    requireDigest(artifact.sha256, "run artifact");
  }
  closedObject(report.versions, ["spec", "fixtures", "profiles", "runnerProtocol", "canonicalization", "schemaRevision"], "versions");
  requireDigest(report.versions.schemaRevision, "versions.schemaRevision");
  closedObject(report.implementation, ["name", "version", "language", "runtimeVersion", "specVersion", "fixtureVersion", "profileVersion", "schemaRevision"], "implementation");
  requireDigest(report.implementation.schemaRevision, "implementation.schemaRevision");
  closedObject(report.environment, ["os", "architecture", "featureFlags", "wireTransports", "streamTransports", "scalarPrecision", "cancellationCapabilities"], "environment");
  for (const field of ["featureFlags", "wireTransports", "streamTransports", "scalarPrecision", "cancellationCapabilities"]) boundedArray(report.environment[field], 0, 1024, `environment.${field}`);
  closedObject(report.path, ["source", "destination"], "path");
  for (const endpoint of [report.path.source, report.path.destination]) closedObject(endpoint, ["kind", "language"], "endpoint");
  boundedArray(report.claims, 0, 128, "claims");
  for (const claim of report.claims) {
    closedObject(claim, ["profile", "capabilities"], "claim");
    boundedArray(claim.capabilities, 0, 1024, "claim.capabilities");
  }
  boundedArray(report.results, 0, 128, "results");
  for (const result of report.results) {
    closedObject(result, ["profile", "evidenceRole", "status", "capabilities", "capability", "executedClauses", "evidence", "diagnostics", "skip"], "result");
    boundedArray(result.capabilities, 0, 1024, "result.capabilities");
    boundedArray(result.executedClauses, 0, 1024, "result.executedClauses");
    boundedArray(result.evidence, 0, 256, "result.evidence");
    for (const evidence of result.evidence) {
      closedObject(evidence, ["fixture", "sha256"], "evidence");
      requireDigest(evidence.sha256, "fixture evidence");
    }
    boundedArray(result.diagnostics, 0, 128, "result.diagnostics");
    for (const diagnostic of result.diagnostics) closedObject(diagnostic, ["code", "message"], "diagnostic");
    if (result.skip !== undefined) closedObject(result.skip, ["fixture", "reason"], "skip");
  }
  const claimsCBOR = report.results.some((result) => result.status === "passed" && result.evidence.some((entry) => entry.fixture === "v1/cbor.json"));
  if (claimsCBOR) {
    requireValue(report.environment.wireTransports.includes(cborReportBinding), "CBOR conformance claim lacks exact profile and codec revision binding");
  }
}

function closedObject(value, allowed, label) {
  requireValue(value && typeof value === "object" && !Array.isArray(value), `${label} must be an object`);
  requireValue(allowed.every((key) => Object.hasOwn(value, key)) || label === "result" || label === "skip", `${label} is missing a required member`);
  requireValue(Object.keys(value).every((key) => allowed.includes(key)), `${label} contains an unknown member`);
}

function boundedArray(value, minimum, maximum, label) {
  requireValue(Array.isArray(value) && value.length >= minimum && value.length <= maximum, `${label} is not a bounded array`);
}

function boundedString(value, maximum, label) {
  requireValue(typeof value === "string" && value.length > 0 && [...value].length <= maximum, `${label} is not a bounded string`);
}

function requireDigest(value, label) {
  requireValue(typeof value === "string" && /^[0-9a-f]{64}$/.test(value), `${label} is not a SHA-256 digest`);
}

function validateStatus(result) {
  requireValue(Array.isArray(result.capabilities) && Array.isArray(result.executedClauses) && Array.isArray(result.evidence) && Array.isArray(result.diagnostics), `incomplete result ${result.profile}`);
  if (result.status === "unsupported") {
    const profile = profileByID(result.profile);
    requireValue(result.capability && profile.optionalCapabilities.some((entry) => entry.id === result.capability), `unsupported is only valid for an optional capability: ${result.profile}`);
  }
  if (result.status === "invalid-skip") requireValue(result.skip?.fixture && result.skip?.reason, `invalid-skip requires skip evidence: ${result.profile}`);
  if (result.status === "failed" || result.status === "infrastructure-failure") requireValue(result.diagnostics.length > 0, `${result.status} requires diagnostics: ${result.profile}`);
}

function rejectSecrets(value, key = "") {
  requireValue(!/(?:token|secret|password|authorization|cookie)/i.test(key), `publish-unsafe metadata key: ${key}`);
  if (typeof value === "string") {
    requireValue(!/(?:gh[pousr]_|github_pat_|bearer\s+|https?:\/\/[^/\s:@]+:[^/\s@]+@)/i.test(value), "publish-unsafe credential-shaped value");
    requireValue([...value].length <= 4096, "publish-unsafe unbounded string");
    return;
  }
  if (Array.isArray(value)) {
    requireValue(value.length <= 2048, "publish-unsafe unbounded array");
    for (const entry of value) rejectSecrets(entry);
    return;
  }
  if (value && typeof value === "object") {
    requireValue(Object.keys(value).length <= 128, "publish-unsafe unbounded object");
    for (const [name, entry] of Object.entries(value)) rejectSecrets(entry, name);
  }
}

function exampleReport(profileID, source, destination) {
  const profile = profileByID(profileID);
  const schemaRevision = "0".repeat(64);
  const capabilities = [];
  return {
    protocol: registry.reportProtocol,
    run: { id: "independent-check", operator: "naatre-project", command: ["node", "conformance/independent/profiles.mjs"], artifacts: [{ name: "source-tree", sha256: "1".repeat(64) }] },
    versions: { spec: registry.specVersion, fixtures: registry.fixtureVersion, profiles: registry.registryVersion, runnerProtocol: registry.runnerProtocol, canonicalization: "c14n-1", schemaRevision },
    implementation: { name: "independent-example", version: "1.0.0", language: "javascript-typescript", runtimeVersion: process.versions.node, specVersion: registry.specVersion, fixtureVersion: registry.fixtureVersion, profileVersion: registry.registryVersion, schemaRevision },
    environment: { os: "linux", architecture: "amd64", featureFlags: [], wireTransports: profileID === "wire.codec-1" ? [cborReportBinding] : ["ndjson"], streamTransports: [], scalarPrecision: ["arbitrary-precision-decimal"], cancellationCapabilities: ["process-signal"] },
    path: { source: { kind: source, language: "javascript-typescript" }, destination: { kind: destination, language: "javascript-typescript" } },
    claims: [{ profile: profileID, capabilities }],
    results: [{ profile: profileID, evidenceRole: profile.evidenceRole, status: "passed", capabilities, executedClauses: [...profile.requiredClauses], evidence: profile.requiredFixtures.map((fixture) => ({ fixture: fixture.path, sha256: fixture.sha256 })), diagnostics: [] }],
  };
}

function expectRejected(name, mutate) {
  const report = exampleReport("sdk.client-1", "sdk", "native-runtime");
  mutate(report);
  try {
    validateReport(report);
  } catch {
    return;
  }
  throw new Error(`invalid report was accepted: ${name}`);
}

function selfTest() {
  const cborReport = exampleReport("wire.codec-1", "codec", "codec");
  validateReport(cborReport);
  const unboundCBORReport = structuredClone(cborReport);
  unboundCBORReport.environment.wireTransports = [cborCapability];
  try {
    validateReport(unboundCBORReport);
    throw new Error("unbound CBOR report was accepted");
  } catch (error) {
    if (error.message === "unbound CBOR report was accepted") throw error;
  }
  expectRejected("broader profile", (report) => { report.claims[0].profile = "runtime.execution-1"; });
  expectRejected("delegated runtime", (report) => {
    const runtime = profileByID("runtime.execution-1");
    report.claims[0].profile = runtime.id;
    report.results[0] = { ...report.results[0], profile: runtime.id, evidenceRole: runtime.evidenceRole, executedClauses: runtime.requiredClauses, evidence: runtime.requiredFixtures.map((fixture) => ({ fixture: fixture.path, sha256: fixture.sha256 })) };
    report.path.destination.kind = "gateway";
  });
  expectRejected("fixture skew", (report) => { report.versions.fixtures = "9.9.9"; });
  expectRejected("schema skew", (report) => { report.implementation.schemaRevision = "2".repeat(64); });
  expectRejected("partial fixtures", (report) => { report.results[0].evidence.pop(); });
  expectRejected("partial clauses", (report) => { report.results[0].executedClauses.pop(); });
  expectRejected("unsafe metadata", (report) => { report.run.authorization = "Bearer example"; });
  expectRejected("unsafe command", (report) => { report.run.command.push("--token=example"); });

  const statuses = exampleReport("transport.streaming-1", "sdk", "stream-transport");
  statuses.claims = [];
  statuses.results = [
    { profile: "transport.streaming-1", evidenceRole: "streaming-transport", status: "unsupported", capability: "stream.websocket-1", capabilities: [], executedClauses: [], evidence: [], diagnostics: [] },
    { profile: "wire.codec-1", evidenceRole: "wire-codec", status: "failed", capabilities: [], executedClauses: [], evidence: [], diagnostics: [{ code: "ASSERTION_FAILED", message: "expected mismatch" }] },
    { profile: "sdk.client-1", evidenceRole: "client", status: "invalid-skip", capabilities: [], executedClauses: [], evidence: [], diagnostics: [], skip: { fixture: "v1/http.json#/methodCases/0", reason: "required fixture cannot be skipped" } },
    { profile: "schema.tooling-1", evidenceRole: "schema-tooling", status: "infrastructure-failure", capabilities: [], executedClauses: [], evidence: [], diagnostics: [{ code: "TOOL_UNAVAILABLE", message: "runtime unavailable" }] },
  ];
  validateReport(statuses);
}

validateRegistry();
validateMatrix();
selfTest();
const reportArgument = process.argv.indexOf("--report");
if (reportArgument >= 0) {
  requireValue(process.argv[reportArgument + 1], "--report requires a file");
  validateReport(readJSON(resolve(process.argv[reportArgument + 1])));
}
process.stdout.write(JSON.stringify({ protocol: registry.reportProtocol, profiles: registry.profiles.length, matrixEntries: matrix.implementations.length, status: "passed" }) + "\n");
