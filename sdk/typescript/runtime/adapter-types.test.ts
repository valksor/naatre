import { createFetchAdapter, type FetchAdapter } from "@naatre/sdk/fetch";
import { createWebSocketAdapter, decodeSSEStream, type WebSocketAdapter } from "@naatre/sdk/websocket";
import type { Operation } from "@naatre/sdk";

declare const operation: Operation<{ readonly id: string }, { readonly name: string }>;
declare const body: ReadableStream<Uint8Array>;

const fetchAdapter: FetchAdapter = createFetchAdapter({ endpoint: "https://api.example/v1/execute" });
const websocketAdapter: WebSocketAdapter = createWebSocketAdapter({ endpoint: "wss://api.example/v1/stream" });
const unary = fetchAdapter.execute(operation);
const fetchStream = fetchAdapter.stream(operation);
const websocketStream = websocketAdapter.stream(operation);
const decoded = decodeSSEStream(body);

void unary;
void fetchStream;
void websocketStream;
void decoded;
