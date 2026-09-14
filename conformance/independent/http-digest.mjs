import { createHash } from "node:crypto";
import { gunzipSync } from "node:zlib";
import { readFileSync } from "node:fs";

const fixture = JSON.parse(readFileSync(new URL("../v1/http-digest.json", import.meta.url), "utf8"));
const preference = ["sha-512", "sha-256"];

class DigestError extends Error {
  constructor(code) {
    super(code);
    this.code = code;
  }
}

function splitDictionary(value) {
  if (typeof value !== "string" || value.length === 0 || Buffer.byteLength(value) > 4096 || /[\r\n]/u.test(value)) {
    throw new DigestError("MALFORMED_DIGEST");
  }
  const members = value.split(",").map((member) => member.replace(/^[ \t]+|[ \t]+$/gu, ""));
  if (members.length === 0 || members.length > 2 || members.some((member) => member.length === 0 || /[ \t]/u.test(member))) {
    throw new DigestError("MALFORMED_DIGEST");
  }
  return members;
}

function validKey(value) {
  return /^[a-z][a-z0-9_.*-]*$/u.test(value);
}

function parseMembers(value) {
  const members = [];
  const seen = new Set();
  for (const member of splitDictionary(value)) {
    const equals = member.indexOf("=");
    const key = equals < 0 ? "" : member.slice(0, equals);
    const raw = equals < 0 ? "" : member.slice(equals + 1);
    if (!preference.includes(key)) {
      throw new DigestError(equals >= 0 && validKey(key) ? "UNSUPPORTED_DIGEST_ALGORITHM" : "MALFORMED_DIGEST");
    }
    if (seen.has(key)) throw new DigestError("DUPLICATE_DIGEST_ALGORITHM");
    seen.add(key);
    members.push({ key, raw });
  }
  return members;
}

function parseDigestFields(fields) {
  if (fields.length === 0) throw new DigestError("DIGEST_REQUIRED");
  if (fields.length !== 1) throw new DigestError("DUPLICATE_DIGEST_FIELD");
  const values = new Map();
  for (const { key, raw } of parseMembers(fields[0])) {
    if (!/^:[A-Za-z0-9+/]*={0,2}:$/u.test(raw)) throw new DigestError("MALFORMED_DIGEST");
    const encoded = raw.slice(1, -1);
    const decoded = Buffer.from(encoded, "base64");
    const size = key === "sha-512" ? 64 : 32;
    if (decoded.toString("base64") !== encoded || decoded.length !== size) throw new DigestError("MALFORMED_DIGEST");
    values.set(key, decoded);
  }
  return values;
}

function verify(input, fields, maximum, expectedLength) {
  const values = parseDigestFields(fields);
  if (input.length > maximum) throw new DigestError("DIGEST_LIMIT_EXCEEDED");
  if (input.length < expectedLength) throw new DigestError("DIGEST_TRUNCATED");
  if (input.length > expectedLength) throw new DigestError("DIGEST_LENGTH_MISMATCH");
  for (const algorithm of preference) {
    const expected = values.get(algorithm);
    if (expected === undefined) continue;
    const actual = createHash(algorithm.replace("-", "")).update(input).digest();
    if (!actual.equals(expected)) throw new DigestError("DIGEST_MISMATCH");
  }
}

function negotiate(fields) {
  if (fields.length === 0) return "sha-256";
  if (fields.length !== 1) throw new DigestError("DUPLICATE_DIGEST_FIELD");
  const weights = new Map();
  for (const { key, raw } of parseMembers(fields[0])) {
    if (!/^(?:[0-9]|10)$/u.test(raw)) throw new DigestError("MALFORMED_DIGEST");
    weights.set(key, Number(raw));
  }
  let selected = "";
  let weight = 0;
  for (const algorithm of preference) {
    if ((weights.get(algorithm) ?? 0) > weight) {
      selected = algorithm;
      weight = weights.get(algorithm);
    }
  }
  if (selected === "") throw new DigestError("NO_ACCEPTABLE_DIGEST");
  return selected;
}

function require(condition, message) {
  if (!condition) throw new Error(message);
}

function semanticIdentity(input) {
  return {
    algorithm: "sha-256",
    canonicalVersion: "c14n-1",
    digest: createHash("sha256").update("naatre:document:c14n-1\n").update(input).digest("hex"),
  };
}

require(fixture.profile === "core.http.digest-1", "unexpected digest profile");
require(JSON.stringify(fixture.policy.allowedAlgorithms) === JSON.stringify(preference), "unexpected algorithm policy");
require(fixture.policy.trailerVerificationSupported === false && fixture.policy.requiresTrailerPreservation === false, "trailer capability overclaim");

for (const vector of fixture.exactByteVectors) {
  const representation = Buffer.from(vector.representationBase64, "base64");
  const content = Buffer.from(vector.contentBase64, "base64");
  verify(content, [vector.contentDigest], content.length, content.length);
  verify(representation, [vector.reprDigest], representation.length, representation.length);
  if (vector.name === "gzip-json-response") {
    require(gunzipSync(content).equals(representation), "gzip representation mismatch");
    require(vector.contentDigest !== vector.reprDigest, "gzip digests were interchanged");
  }
  if (vector.name === "range-response") {
    require(content.equals(representation.subarray(10, 20)) && vector.contentDigest !== vector.reprDigest, "range scope mismatch");
  }
  if (vector.name === "chunked-transfer-response") {
    const framed = Buffer.from(vector.framedBodyBase64, "base64");
    let rejected = false;
    try { verify(framed, [vector.contentDigest], framed.length, framed.length); } catch (error) { rejected = error.code === "DIGEST_MISMATCH"; }
    require(rejected, "transfer framing entered Content-Digest input");
  }
}

for (const vector of fixture.negotiationCases) {
  try {
    const selected = negotiate(vector.fields);
    require(vector.accepted && selected === vector.algorithm, `${vector.name}: negotiation mismatch`);
  } catch (error) {
    require(!vector.accepted && error.code === vector.code, `${vector.name}: unsafe negotiation failure`);
  }
}

for (const vector of fixture.failureCases) {
  let code = "";
  try {
    verify(Buffer.from(vector.inputBase64, "base64"), vector.fields, vector.maximumBytes, vector.expectedLength);
  } catch (error) {
    code = error.code;
  }
  require(code === vector.code, `${vector.name}: failure code ${code}, want ${vector.code}`);
}

const body = Buffer.from(fixture.webhook.bodyBase64, "base64");
verify(body, [fixture.webhook.contentDigest], body.length, body.length);
const signatureBase = Buffer.from(fixture.webhook.signatureBaseBase64, "base64");
require(createHash("sha256").update(signatureBase).digest("base64") === fixture.webhook.signatureBaseSHA256, "webhook signature input mismatch");
for (const modification of fixture.webhook.modifications) {
  if (modification.detectedBy === "signature") {
    const changed = Buffer.from(signatureBase.toString().replace(modification.find, modification.replace));
    require(!changed.equals(signatureBase) && createHash("sha256").update(changed).digest("base64") !== fixture.webhook.signatureBaseSHA256, `${modification.name}: signature did not change`);
  } else {
    const changed = Buffer.from(body.toString().replace(modification.find, modification.replace));
    let rejected = false;
    try { verify(changed, [fixture.webhook.contentDigest], changed.length, changed.length); } catch (error) { rejected = error.code === "DIGEST_MISMATCH"; }
    require(rejected, `${modification.name}: body digest did not change`);
  }
}

const semanticIdentities = fixture.semanticIdentity.transportCases.map((test) => {
  const content = Buffer.from(test.contentBase64, "base64");
  verify(content, [test.contentDigest], content.length, content.length);
  const representation = test.contentCoding === "gzip" ? gunzipSync(content) : content;
  require(representation.equals(Buffer.from(test.semanticInput)), `${test.name}: semantic input mismatch`);
  const actual = semanticIdentity(representation);
  require(JSON.stringify(actual) === JSON.stringify(test.expectedIdentity), `${test.name}: semantic identity mismatch`);
  return actual.digest;
});
require(new Set(semanticIdentities).size === 1, "transport metadata altered semantic identity");
require(new Set(fixture.semanticIdentity.transportCases.map((test) => test.contentDigest)).size === 2, "transport digest did not vary");

const changedSemantic = fixture.semanticIdentity.changedSemanticCase;
const changedContent = Buffer.from(changedSemantic.contentBase64, "base64");
verify(changedContent, [changedSemantic.contentDigest], changedContent.length, changedContent.length);
require(changedContent.equals(Buffer.from(changedSemantic.semanticInput)), "changed semantic input mismatch");
const changedIdentity = semanticIdentity(changedContent);
require(JSON.stringify(changedIdentity) === JSON.stringify(changedSemantic.expectedIdentity), "changed semantic identity mismatch");
require(changedIdentity.digest !== semanticIdentities[0], "changed semantic input retained identity");
