import { NaatreClientError } from "./error.mjs";
import { canonicalStringify, parseJSON, safeObject } from "./json.mjs";

const frameTypes = new Set(["open", "data", "patch", "error", "complete", "keepalive", "resume", "history-unavailable"]);

export async function* decodeSSEStream(body, options = {}) {
  const maximumFrameBytes = positiveLimit(options.maximumFrameBytes, 1 << 20);
  const maximumResponseBytes = positiveLimit(options.maximumResponseBytes, 16 << 20);
  const signal = options.signal;
  let reader;
  try {
    reader = body?.getReader?.();
    if (!reader) transportFail("CLIENT_STREAM_INVALID");
  } catch (error) {
    try { await body?.cancel?.(); } catch {}
    if (error instanceof NaatreClientError) throw error;
    transportFail("CLIENT_STREAM_INVALID");
  }
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let buffered = "";
  let received = 0;
  let ended = false;
  const state = createFrameState();
  const cancel = () => { void reader.cancel(); };
  signal?.addEventListener("abort", cancel, { once: true });
  try {
    for (;;) {
      if (signal?.aborted) transportFail("CLIENT_CANCELED");
      const { done, value } = await reader.read();
      if (done) break;
      if (!(value instanceof Uint8Array)) transportFail("CLIENT_STREAM_INVALID");
      received += value.byteLength;
      if (received > maximumResponseBytes) transportFail("CLIENT_RESPONSE_LIMIT");
      buffered += decoder.decode(value, { stream: true });
      const events = takeSSEEvents(buffered);
      buffered = events.remainder;
      if (new TextEncoder().encode(buffered).byteLength > maximumFrameBytes) transportFail("CLIENT_FRAME_LIMIT");
      for (const event of events.complete) {
        if (new TextEncoder().encode(event).byteLength > maximumFrameBytes) transportFail("CLIENT_FRAME_LIMIT");
        const decoded = decodeSSEEvent(event, maximumFrameBytes);
        if (decoded === null) continue;
        const frame = acceptFrame(state, decoded);
        if (frame === null) continue;
        ended ||= isStreamEnd(frame);
        yield frame;
        if (ended) {
          await reader.cancel();
          return;
        }
      }
    }
    buffered += decoder.decode();
  } catch (error) {
    if (error instanceof NaatreClientError) throw error;
    if (signal?.aborted || error?.name === "AbortError") transportFail("CLIENT_CANCELED");
    transportFail("CLIENT_STREAM_INVALID");
  } finally {
    signal?.removeEventListener("abort", cancel);
    try { await reader.cancel(); } catch {}
  }
  if (signal?.aborted) transportFail("CLIENT_CANCELED");
  if (buffered !== "" || !ended) transportFail("CLIENT_STREAM_TRUNCATED");
}

export function createWebSocketAdapter(configuration) {
  const endpoint = parseWebSocketEndpoint(configuration?.endpoint);
  const WebSocketConstructor = configuration?.WebSocket ?? globalThis.WebSocket;
  if (typeof WebSocketConstructor !== "function") transportFail("CLIENT_UNSUPPORTED");
  const maximumFrameBytes = positiveLimit(configuration?.maximumFrameBytes, 1 << 20);
  const maximumQueuedBytes = positiveLimit(configuration?.maximumQueuedBytes, 4 << 20);
  const maximumQueuedFrames = positiveLimit(configuration?.maximumQueuedFrames, 64);
  const protocols = validateProtocols(configuration?.protocols ?? []);
  return Object.freeze({
    stream(operation, options = {}) {
      return websocketFrames({ endpoint, WebSocketConstructor, maximumFrameBytes, maximumQueuedBytes, maximumQueuedFrames, protocols, operation, signal: options.signal });
    },
  });
}

async function* websocketFrames(configuration) {
  const { endpoint, WebSocketConstructor, maximumFrameBytes, maximumQueuedBytes, maximumQueuedFrames, protocols, operation, signal } = configuration;
  if (operation?.kind !== "subscription" || typeof operation.canonicalRequest !== "function") transportFail("CLIENT_OPERATION_INVALID");
  if (signal?.aborted) transportFail("CLIENT_CANCELED");
  let socket;
  let queue;
  try {
    socket = new WebSocketConstructor(endpoint.href, protocols);
    queue = createSocketQueue(socket, signal, { maximumFrameBytes, maximumQueuedBytes, maximumQueuedFrames });
  } catch {
    transportFail("CLIENT_TRANSPORT_ERROR");
  }
  let ended = false;
  const state = createFrameState();
  try {
    await queue.opened;
    if (signal?.aborted) transportFail("CLIENT_CANCELED");
    socket.send(operation.canonicalRequest());
    for (;;) {
      const item = await queue.next();
      if (item.kind === "close") {
        if (signal?.aborted) transportFail("CLIENT_CANCELED");
        if (!ended) transportFail("CLIENT_STREAM_TRUNCATED");
        return;
      }
      if (typeof item.data !== "string") transportFail("CLIENT_STREAM_INVALID");
      if (new TextEncoder().encode(item.data).byteLength > maximumFrameBytes) {
        socket.close(1009, "frame limit");
        transportFail("CLIENT_FRAME_LIMIT");
      }
      const frame = acceptFrame(state, validateFrame(parseJSON(item.data, maximumFrameBytes)));
      if (frame === null) continue;
      ended ||= isStreamEnd(frame);
      yield frame;
      if (ended) {
        socket.close(1000, frame.type === "history-unavailable" ? "history unavailable" : "complete");
        return;
      }
    }
  } catch (error) {
    if (error instanceof NaatreClientError) throw error;
    if (signal?.aborted || error?.name === "AbortError") transportFail("CLIENT_CANCELED");
    transportFail("CLIENT_STREAM_INVALID");
  } finally {
    queue.dispose();
    if (!ended && socket.readyState < 2) socket.close(1001, "client closed");
  }
}

function createSocketQueue(socket, signal, limits) {
  const values = [];
  const waiters = [];
  let queuedBytes = 0;
  let failure;
  let openResolve;
  let openReject;
  const opened = new Promise((resolve, reject) => { openResolve = resolve; openReject = reject; });
  const deliver = (value) => {
    const waiter = waiters.shift();
    if (waiter) waiter.resolve(value);
    else {
      values.push(value);
      queuedBytes += value.bytes ?? 0;
    }
  };
  const failQueue = (error) => {
    if (failure) return;
    failure = error;
    values.length = 0;
    queuedBytes = 0;
    openReject(error);
    while (waiters.length > 0) waiters.shift().reject(error);
  };
  const onOpen = () => openResolve();
  const onMessage = (event) => {
    if (failure) return;
    if (typeof event.data !== "string") {
      failQueue(new NaatreClientError("CLIENT_STREAM_INVALID"));
      socket.close(1003, "text required");
      return;
    }
    const bytes = new TextEncoder().encode(event.data).byteLength;
    if (bytes > limits.maximumFrameBytes) {
      failQueue(new NaatreClientError("CLIENT_FRAME_LIMIT"));
      socket.close(1009, "frame limit");
      return;
    }
    if (waiters.length === 0 && (values.length >= limits.maximumQueuedFrames || queuedBytes + bytes > limits.maximumQueuedBytes)) {
      failQueue(new NaatreClientError("CLIENT_RESPONSE_LIMIT"));
      socket.close(1009, "queue limit");
      return;
    }
    deliver({ kind: "message", data: event.data, bytes });
  };
  const onClose = () => { if (!failure) deliver({ kind: "close", bytes: 0 }); };
  const onError = () => failQueue(new Error("socket failed"));
  const onAbort = () => {
    failQueue(new Error("socket canceled"));
    socket.close(1001, "canceled");
  };
  socket.addEventListener("open", onOpen);
  socket.addEventListener("message", onMessage);
  socket.addEventListener("close", onClose);
  socket.addEventListener("error", onError);
  signal?.addEventListener("abort", onAbort, { once: true });
  return {
    opened,
    next() {
      if (values.length > 0) {
        const value = values.shift();
        queuedBytes -= value.bytes ?? 0;
        return Promise.resolve(value);
      }
      if (failure) return Promise.reject(failure);
      return new Promise((resolve, reject) => waiters.push({ resolve, reject }));
    },
    dispose() {
      signal?.removeEventListener("abort", onAbort);
      socket.removeEventListener("open", onOpen);
      socket.removeEventListener("message", onMessage);
      socket.removeEventListener("close", onClose);
      socket.removeEventListener("error", onError);
    },
  };
}

function takeSSEEvents(input) {
  const normalized = input.replace(/\r\n/gu, "\n").replace(/\r/gu, "\n");
  const complete = [];
  let start = 0;
  for (;;) {
    const boundary = normalized.indexOf("\n\n", start);
    if (boundary < 0) break;
    complete.push(normalized.slice(start, boundary));
    start = boundary + 2;
  }
  return { complete, remainder: normalized.slice(start) };
}

function decodeSSEEvent(event, maximumFrameBytes) {
  let eventName;
  let eventID;
  const data = [];
  for (const line of event.split("\n")) {
    if (line === "" || line.startsWith(":")) continue;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /u, "");
    if (field === "event" && eventName === undefined) eventName = value;
    else if (field === "id" && eventID === undefined && !value.includes("\0")) eventID = value;
    else if (field === "data") data.push(value);
    else transportFail("CLIENT_STREAM_INVALID");
  }
  if (eventName === undefined && eventID === undefined && data.length === 0) return null;
  if (!eventName?.startsWith("naatre.") || data.length === 0) transportFail("CLIENT_STREAM_INVALID");
  const payload = data.join("\n");
  const frame = validateFrame(parseJSON(payload, maximumFrameBytes));
  if (eventName !== `naatre.${frame.type}` || (eventID === undefined) !== (frame.cursor === undefined) || eventID !== frame.cursor) transportFail("CLIENT_STREAM_INVALID");
  return frame;
}

function validateFrame(value) {
  const frame = safeObject(value, "CLIENT_STREAM_INVALID");
  if (!frameTypes.has(frame.type) || !validIdentifier(frame.stream)) transportFail("CLIENT_STREAM_INVALID");
  if (frame.type === "keepalive") {
    exactKeys(frame, ["stream", "type"]);
    return Object.freeze(frame);
  }
  if (!Number.isSafeInteger(frame.sequence) || frame.sequence < 1) transportFail("CLIENT_STREAM_INVALID");
  switch (frame.type) {
    case "open":
      exactKeys(frame, ["schemaRevision", "sequence", "stream", "type"]);
      if (frame.sequence !== 1 || !validIdentifier(frame.schemaRevision)) transportFail("CLIENT_STREAM_INVALID");
      break;
    case "data":
      allowedKeys(frame, ["cursor", "data", "eventId", "position", "sequence", "stream", "type"]);
      if (!Object.hasOwn(frame, "data")) transportFail("CLIENT_STREAM_INVALID");
      validateDeliveryMetadata(frame, false);
      break;
    case "patch":
      allowedKeys(frame, ["cursor", "data", "eventId", "path", "position", "sequence", "stream", "type"]);
      if (!Object.hasOwn(frame, "data") || !validPosition(frame.position) || !validPath(frame.path, false)) transportFail("CLIENT_STREAM_INVALID");
      validateDeliveryMetadata(frame, true);
      break;
    case "error":
      allowedKeys(frame, ["error", "eventId", "final", "path", "position", "sequence", "stream", "type"]);
      if (!validFrameError(frame.error) || frame.final !== undefined && typeof frame.final !== "boolean" || !validPath(frame.path, frame.final === true)) transportFail("CLIENT_STREAM_INVALID");
      validateOptionalPositionAndEvent(frame);
      break;
    case "complete":
      exactKeys(frame, ["sequence", "stream", "type"]);
      break;
    case "resume":
      exactKeys(frame, ["cursor", "sequence", "stream", "type"]);
      if (!validIdentifier(frame.cursor)) transportFail("CLIENT_STREAM_INVALID");
      break;
    case "history-unavailable":
      exactKeys(frame, ["recovery", "sequence", "stream", "type"]);
      if (frame.recovery !== "restart" && frame.recovery !== "refetch") transportFail("CLIENT_STREAM_INVALID");
      break;
  }
  return Object.freeze(frame);
}

function createFrameState() {
  return { stream: "", sequence: 0, position: 0, frames: new Map(), closed: false };
}

function acceptFrame(state, frame) {
  if (state.closed || state.stream !== "" && frame.stream !== state.stream) transportFail("CLIENT_STREAM_INVALID");
  if (frame.type === "keepalive") {
    if (state.sequence === 0) transportFail("CLIENT_STREAM_INVALID");
    return frame;
  }
  const canonical = canonicalStringify(frame);
  if (frame.sequence <= state.sequence) {
    if (state.frames.get(frame.sequence) === canonical) return null;
    transportFail("CLIENT_STREAM_INVALID");
  }
  if (frame.sequence !== state.sequence + 1 || state.sequence === 0 && frame.type !== "open") transportFail("CLIENT_STREAM_INVALID");
  if ((frame.type === "resume" || frame.type === "history-unavailable") && state.sequence !== 1) transportFail("CLIENT_STREAM_INVALID");
  if (frame.position !== undefined && frame.position <= state.position) transportFail("CLIENT_STREAM_INVALID");
  state.stream ||= frame.stream;
  state.sequence = frame.sequence;
  if (frame.position !== undefined) state.position = frame.position;
  state.frames.set(frame.sequence, canonical);
  if (state.frames.size > 64) state.frames.delete(state.sequence - 64);
  state.closed = isStreamEnd(frame);
  return frame;
}

function isTerminal(frame) {
  return frame.type === "complete" || frame.type === "error" && frame.final === true;
}

function isStreamEnd(frame) {
  return isTerminal(frame) || frame.type === "history-unavailable";
}

function exactKeys(frame, keys) {
  if (Object.keys(frame).sort().join(",") !== [...keys].sort().join(",")) transportFail("CLIENT_STREAM_INVALID");
}

function allowedKeys(frame, keys) {
  const allowed = new Set(keys);
  if (Object.keys(frame).some((key) => !allowed.has(key))) transportFail("CLIENT_STREAM_INVALID");
}

function validateDeliveryMetadata(frame, requirePosition) {
  if (requirePosition && !validPosition(frame.position)) transportFail("CLIENT_STREAM_INVALID");
  validateOptionalPositionAndEvent(frame);
  if (frame.cursor !== undefined && (!validIdentifier(frame.cursor) || frame.position === undefined)) transportFail("CLIENT_STREAM_INVALID");
}

function validateOptionalPositionAndEvent(frame) {
  if (frame.position !== undefined && !validPosition(frame.position)) transportFail("CLIENT_STREAM_INVALID");
  if (frame.eventId !== undefined && !validIdentifier(frame.eventId)) transportFail("CLIENT_STREAM_INVALID");
}

function validPosition(value) {
  return Number.isSafeInteger(value) && value > 0;
}

function validIdentifier(value) {
  return typeof value === "string" && value !== "" && new TextEncoder().encode(value).byteLength <= 512 && !/[\u0000-\u001f\u007f-\u009f]/u.test(value);
}

function validPath(value, allowEmpty) {
  if (value === undefined) return allowEmpty;
  if (!Array.isArray(value) || value.length > 64 || !allowEmpty && value.length === 0) return false;
  return value.every((part) => typeof part === "string" ? validIdentifier(part) : Number.isSafeInteger(part) && part >= 0);
}

function validFrameError(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value) || Object.getPrototypeOf(value) !== null) return false;
  if (Object.keys(value).sort().join(",") !== "code,message") return false;
  return typeof value.code === "string" && /^[A-Z0-9_]+$/u.test(value.code) && new TextEncoder().encode(value.code).byteLength <= 512 && typeof value.message === "string" && value.message !== "" && new TextEncoder().encode(value.message).byteLength <= 32768;
}

function parseWebSocketEndpoint(value) {
  let endpoint;
  try { endpoint = new URL(value); } catch { transportFail("CLIENT_CONFIG_INVALID"); }
  if (endpoint.protocol !== "wss:" || endpoint.username || endpoint.password || endpoint.hash) transportFail("CLIENT_CONFIG_INVALID");
  return endpoint;
}

function validateProtocols(value) {
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== "string" || entry === "" || /[\s,]/u.test(entry))) transportFail("CLIENT_CONFIG_INVALID");
  return Object.freeze([...value]);
}

function positiveLimit(value, fallback) {
  if (value === undefined) return fallback;
  if (!Number.isSafeInteger(value) || value < 1) transportFail("CLIENT_CONFIG_INVALID");
  return value;
}

function transportFail(code) {
  throw new NaatreClientError(code);
}
