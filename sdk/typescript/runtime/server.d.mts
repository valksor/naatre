import type { ScalarCodecs } from "./index.mjs";

export type WorkerEffect = "query" | "mutation" | "transaction" | "subscription";
export type WorkerExecutionProfile = "inline" | "worker" | "process";

export type ValueSchema =
  | Readonly<{ kind: "scalar"; type: string }>
  | Readonly<{ kind: "list"; element: ValueSchema; elementNullable?: boolean }>
  | Readonly<{ kind: "enum"; values: readonly string[]; open?: boolean }>
  | Readonly<{ kind: "object"; fields: readonly Readonly<{ name: string; schema: ValueSchema; required: boolean; nullable: boolean }>[]; additionalProperties: false }>
  | Readonly<{ kind: "union"; tag: string; value: string; variants: readonly Readonly<{ tag: string; schema: ValueSchema }>[]; open?: boolean }>;

export interface HandlerContext<TState extends object = Record<string, unknown>> {
  readonly requestId: string;
  readonly invocationId: string;
  readonly attemptId: string;
  readonly principal: string;
  readonly tenant: string;
  readonly cache: Map<unknown, unknown>;
  readonly loaders: Map<unknown, unknown>;
  readonly state: TState;
  readonly signal: AbortSignal;
  readonly executionProfile: WorkerExecutionProfile;
  onCleanup(callback: () => void | Promise<void>): void;
}

export type Handler<TInput, TOutput, TState extends object = Record<string, unknown>> =
  (input: TInput, context: HandlerContext<TState>) => TOutput | PromiseLike<TOutput>;

export type StreamHandler<TInput, TOutput, TState extends object = Record<string, unknown>> =
  (input: TInput, context: HandlerContext<TState>) => AsyncIterable<TOutput> | PromiseLike<AsyncIterable<TOutput>>;

export interface HandlerDefinition<TInput, TOutput> {
  readonly id: string;
  readonly inputSchema: string;
  readonly outputSchema: string;
  readonly codec: "naatre.json-1";
  readonly effect: WorkerEffect;
  readonly requiredCapabilities: readonly string[];
  readonly executionProfile: WorkerExecutionProfile;
  readonly input: ValueSchema;
  readonly output: ValueSchema;
  readonly codecs?: ScalarCodecs;
}

export interface AnyDefinedHandler {
  readonly __naatreDefinedHandler?: true;
}

export interface DefinedHandler<TInput, TOutput> extends AnyDefinedHandler {
  readonly __input?: TInput;
  readonly __output?: TOutput;
}

export interface WorkerRegistration {
  readonly protocol: "naatre.remote-worker.v1";
  readonly workerId: string;
  readonly serviceIdentity: string;
  readonly audience: string;
  readonly endpoint: string;
  readonly schemaRevision: string;
  readonly schemaDigest: string;
  readonly capabilities: readonly string[];
  readonly limits: Readonly<{ maxInFlight: number; maxRequestBytes: number; maxResponseBytes: number; maxStreamFrames: number; maxStreamBytes: number }>;
  readonly handlers: readonly Readonly<{ id: string; inputSchema: string; outputSchema: string; codec: "naatre.json-1"; effect: WorkerEffect; requiredCapabilities: readonly string[] }>[];
}

export interface WorkerInvocation {
  readonly protocol: "naatre.remote-worker.v1";
  readonly requestId: string;
  readonly invocationId: string;
  readonly attemptId: string;
  readonly handlerId: string;
  readonly schemaRevision: string;
  readonly deadlineUnixMilli: number;
  readonly delegatedContext: string;
  readonly input: unknown;
  readonly parent?: unknown;
  readonly idempotencyKey?: string;
  readonly resumeCursor?: string;
}

export interface NaatreWorker {
  registration(): WorkerRegistration;
  register(candidate: unknown): Promise<Readonly<Record<string, unknown>>>;
  invoke(candidate: WorkerInvocation | unknown, options?: Readonly<{ signal?: AbortSignal }>): Promise<Readonly<{ protocol: "naatre.remote-worker.v1"; invocationId: string; attemptId: string; schemaRevision: string; data: unknown; errors: readonly Readonly<Record<string, unknown>>[] }>>;
  stream(candidate: WorkerInvocation | unknown, options?: Readonly<{ signal?: AbortSignal }>): Promise<AsyncIterable<unknown>>;
  cancel(candidate: unknown): Promise<Readonly<{ protocol: "naatre.remote-worker.v1"; invocationId: string; disposition: "acknowledged" }>>;
  metrics(): Readonly<{ activeInvocations: number; cancellationRequested: number; activeStreams: number }>;
}

export interface WorkerConfiguration {
  readonly workerId: string;
  readonly serviceIdentity: string;
  readonly audience: string;
  readonly endpoint: string;
  readonly schemaRevision: string;
  readonly schemaDigest: string;
  readonly capabilities: readonly string[];
  readonly limits: WorkerRegistration["limits"];
  readonly handlers: readonly AnyDefinedHandler[];
  readonly authenticate: (context: Readonly<{ delegatedContext: string; requestId: string; invocationId: string; handlerId: string; schemaRevision: string; signal: AbortSignal }>) => Readonly<{ principal: string; tenant: string }> | PromiseLike<Readonly<{ principal: string; tenant: string }>>;
  readonly createRequestState?: (context: Readonly<{ principal: string; tenant: string; requestId: string; signal: AbortSignal }>) => Readonly<{ cache?: Map<unknown, unknown>; loaders?: Map<unknown, unknown>; state?: object; cleanup?: (() => void | Promise<void>) | readonly (() => void | Promise<void>)[] }> | PromiseLike<Readonly<{ cache?: Map<unknown, unknown>; loaders?: Map<unknown, unknown>; state?: object; cleanup?: (() => void | Promise<void>) | readonly (() => void | Promise<void>)[] }>>;
  readonly executors?: Readonly<Partial<Record<"worker" | "process", (handler: Handler<unknown, unknown>, input: unknown, context: HandlerContext) => unknown>>>;
  readonly now?: () => number;
}

export class NaatreWorkerError extends Error {
  readonly code: string;
  readonly status: number;
  readonly applicationError: boolean;
  readonly retryable: boolean;
  static application(code: string, message: string, options?: Readonly<{ retryable?: boolean; details?: unknown }>): NaatreWorkerError;
}

export function defineHandler<TInput, TOutput>(definition: HandlerDefinition<TInput, TOutput>, handler: Handler<TInput, TOutput> | StreamHandler<TInput, TOutput>): DefinedHandler<TInput, TOutput>;
export function createWorker(configuration: WorkerConfiguration): NaatreWorker;
export function createFetchWorkerAdapter(worker: NaatreWorker, options?: Readonly<{ signal?: AbortSignal }>): (request: Request) => Promise<Response>;
export const workerProtocol: "naatre.remote-worker.v1";
export const workerRuntimeVersion: "naatre.typescript.worker-1";
