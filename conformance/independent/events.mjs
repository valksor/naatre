import { createHash, createHmac, timingSafeEqual } from "node:crypto";
import { gunzipSync } from "node:zlib";
import { readFileSync } from "node:fs";

const fixture = JSON.parse(readFileSync(new URL("../v1/events.json", import.meta.url), "utf8"));
const components = ["@method", "@target-uri", "content-type", "content-encoding", "content-digest", "naatre-webhook-id", "naatre-webhook-timestamp", "naatre-webhook-audience"];
const inputPattern = /^naatre=\("@method" "@target-uri" "content-type" "content-encoding" "content-digest" "naatre-webhook-id" "naatre-webhook-timestamp" "naatre-webhook-audience"\);created=([0-9]+);expires=([0-9]+);keyid="([A-Za-z0-9][A-Za-z0-9._:-]{0,127})";alg="hmac-sha256"$/u;

function require(condition, message) {
  if (!condition) throw new Error(message);
}

function digest(body) {
  return `sha-256=:${createHash("sha256").update(body).digest("base64")}:`;
}

function signatureBase(message) {
  return Buffer.from([
    `"@method": ${message.method}`,
    `"@target-uri": ${message.targetUri}`,
    `"content-type": ${message.contentType}`,
    `"content-encoding": ${message.contentEncoding}`,
    `"content-digest": ${message.contentDigest}`,
    `"naatre-webhook-id": ${message.deliveryId}`,
    `"naatre-webhook-timestamp": ${message.timestamp}`,
    `"naatre-webhook-audience": ${message.audience}`,
    `"@signature-params": ${message.signatureInput.slice("naatre=".length)}`,
  ].join("\n"));
}

function verify(message, key, now, replay) {
  require(message.method === "POST", "method rejected");
  require(new URL(message.targetUri).protocol === "https:", "target rejected");
  require(message.contentEncoding === "identity" || message.contentEncoding === "gzip", "content encoding rejected");
  require(digest(message.body) === message.contentDigest, "digest rejected");
  const parsed = inputPattern.exec(message.signatureInput);
  require(parsed !== null, "signature input rejected");
  const created = Number(parsed[1]);
  const expires = Number(parsed[2]);
  require(Number(message.timestamp) === created && now >= created && now < expires && expires - created <= fixture.signaturePolicy.maximumValiditySeconds, "freshness rejected");
  require(parsed[3] === key.id && message.audience === key.audience && created >= Date.parse(key.notBefore) / 1000 && expires <= Date.parse(key.notAfter) / 1000 && key.state !== "revoked", "key rejected");
  const match = /^naatre=:([A-Za-z0-9+/]+={0,2}):$/u.exec(message.signature);
  require(match !== null, "signature field rejected");
  const actual = Buffer.from(match[1], "base64");
  const expected = createHmac("sha256", Buffer.from(key.secretBase64, "base64")).update(signatureBase(message)).digest();
  require(actual.length === expected.length && timingSafeEqual(actual, expected), "signature rejected");
  const replayKey = `${message.audience}\0${message.deliveryId}`;
  const duplicate = replay.has(replayKey);
  replay.add(replayKey);
  return duplicate;
}

function messageFrom(vector) {
  return {
    ...vector,
    body: Buffer.from(vector.bodyBase64, "base64"),
  };
}

require(fixture.profile === "core.events-1", "unexpected event profile");
require(fixture.specifications.cloudEvents === "1.0.2" && fixture.specifications.httpMessageSignatures === "RFC 9421" && fixture.specifications.digestFields === "RFC 9530", "specification pins are incomplete");
require(JSON.stringify(fixture.signaturePolicy.coveredComponents) === JSON.stringify(components), "covered components changed");
require(fixture.signaturePolicy.exactTransmittedContent && fixture.signaturePolicy.digestBeforeSignature && fixture.signaturePolicy.keySelection === "exact-keyid-only", "signature policy weakened");

const keys = new Map(fixture.keys.map((key) => [key.id, key]));
require(keys.size === fixture.keys.length && fixture.keys.length >= 2 && new Set(fixture.keys.map((key) => key.id)).size === fixture.keys.length, "rotation key IDs are ambiguous");
for (const vector of fixture.signatureVectors) {
  const message = messageFrom(vector);
  const base = signatureBase(message);
  require(base.equals(Buffer.from(vector.signatureBaseBase64, "base64")), `${vector.name}: signature base mismatch`);
  require(verify(message, keys.get(vector.keyId), Date.parse(vector.now) / 1000, new Set()) === false, `${vector.name}: fresh delivery reported duplicate`);
  if (vector.contentEncoding === "gzip") {
    require(gunzipSync(message.body).equals(Buffer.from(vector.decodedBodyBase64, "base64")), `${vector.name}: gzip representation mismatch`);
  } else {
    require(message.body.includes(Buffer.from("\n  \"id\"")), `${vector.name}: whitespace vector was normalized`);
  }
}

for (const negative of fixture.negativeSignatures) {
  const vector = fixture.signatureVectors[negative.vector];
  const message = messageFrom(vector);
  let key = { ...keys.get(vector.keyId) };
  let now = Date.parse(vector.now) / 1000;
  if (negative.member === "body") message.body = Buffer.from(message.body.toString().replace(negative.find, negative.replace));
  else if (negative.member === "targetUri") message.targetUri = negative.replace;
  else if (negative.member === "method") message.method = negative.replace;
  else if (negative.member === "now") now = Date.parse(negative.replace) / 1000;
  else if (negative.member === "secret") key.secretBase64 = negative.replaceBase64;
  else throw new Error(`${negative.name}: unknown mutation`);
  let rejection = "";
  try { verify(message, key, now, new Set()); } catch (error) { rejection = error.message; }
  const expected = {
    body: ["digest rejected", "reject-digest"],
    targetUri: ["signature rejected", "reject-signature"],
    method: ["method rejected", "reject-message"],
    now: ["freshness rejected", "reject-freshness"],
    secret: ["signature rejected", "reject-signature"],
  }[negative.member];
  require(expected !== undefined && rejection === expected[0] && negative.outcome === expected[1], `${negative.name}: unexpected rejection ${rejection}`);
}

const replayVector = fixture.signatureVectors[fixture.replay.vector];
const replayMessage = messageFrom(replayVector);
const replay = new Set();
require(verify(replayMessage, keys.get(replayVector.keyId), Date.parse(replayVector.now) / 1000, replay) === false, "first delivery is not fresh");
require(verify(replayMessage, keys.get(replayVector.keyId), Date.parse(replayVector.now) / 1000, replay) === true, "duplicate delivery was not detected");
require(fixture.replay.stableDeliveryId === replayMessage.deliveryId && fixture.replay.stableEventId === JSON.parse(replayMessage.body).id, "duplicate identifiers are unstable");

let delay = fixture.retry.initialDelayMilliseconds;
for (const expected of fixture.retry.delaysBeforeAttemptsMilliseconds) {
  require(Math.min(delay, fixture.retry.maximumDelayMilliseconds) === expected, "retry backoff mismatch");
  delay = Math.min(delay * fixture.retry.multiplier, fixture.retry.maximumDelayMilliseconds);
}
for (const test of fixture.batch.accepted) require(test.events <= fixture.batch.maximumEvents && test.bytes <= fixture.batch.maximumBytes && test.decodedBytes <= fixture.batch.maximumBytes, "accepted batch exceeds limits");
for (const test of fixture.batch.rejected) require(test.events > fixture.batch.maximumEvents || test.bytes > fixture.batch.maximumBytes || test.decodedBytes > fixture.batch.maximumBytes, "rejected batch is within limits");

const skewOutcomes = new Set(fixture.versionSkew.map((test) => test.outcome));
for (const outcome of ["authenticate-acknowledge-ignore", "dead-letter-unsupported-type", "deliver", "dead-letter-schema-incompatible"]) require(skewOutcomes.has(outcome), `version skew outcome ${outcome} is missing`);
require(fixture.envelope.orderingDefault === "absent" && fixture.envelope.unknownTypeDefault === "authenticate-acknowledge-ignore", "envelope defaults are ambiguous");
require(fixture.recovery.length === 2 && fixture.recovery.every((test) => test.preserve.includes("tenant") && test.preserve.includes("endpointRevision") && test.preserve.includes("payloadReference")), "recovery loses delivery ownership");
require(fixture.recovery.some((test) => test.endpoint === "revoked" && test.after === "dead-lettered" && test.failureCode === "ENDPOINT_REVOKED"), "revocation is not safely dead-lettered");
require(fixture.redaction.forbiddenFields.every((field) => !fixture.redaction.safeFields.includes(field)) && fixture.redaction.deadLetterPayloadDefault === "opaque-reference-only", "redaction fields overlap");
