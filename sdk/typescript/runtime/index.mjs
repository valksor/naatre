export { NaatreClientError } from "./error.mjs";
export { canonicalStringify, hasOwn, parseJSON, safeObject } from "./json.mjs";
export { createOperation, loadManifest } from "./operation.mjs";
export { decodeBoolean, decodeList, decodeNumber, decodeOperationResult, decodeSelected, decodeString, failed, missing, nullValue, pending, present, skipped } from "./result.mjs";
export { createScalarCodecs, decodeBytes, decodeScalar, encodeBigInt, encodeBytes, encodeDecimal, encodeDuration, encodeInt64, encodeScalar, encodeTimestamp, encodeUInt64, encodeUUID } from "./scalars.mjs";
export { createWebSocketAdapter, decodeSSEStream } from "./stream.mjs";
export { createFetchAdapter } from "./transport.mjs";

export const runtimeVersion = "naatre.typescript.runtime-1";
