export type Presence = "missing" | "null" | "pending" | "present" | "failed" | "skipped";
export type Selected<T> =
  | { readonly state: "missing" }
  | { readonly state: "null" }
  | { readonly state: "pending" }
  | { readonly state: "present"; readonly value: T }
  | { readonly state: "failed"; readonly errors: readonly Readonly<Record<string, unknown>>[] }
  | { readonly state: "skipped"; readonly reason: string };

export interface PersistedReference {
  readonly algorithm: "sha-256";
  readonly canonicalVersion: "c14n-1";
  readonly digest: string;
}

export interface VariableDefinition {
  readonly name: string;
  readonly type: string;
  readonly required: boolean;
  readonly nullable: boolean;
}

export interface ScalarCodec<TInput = unknown, TOutput = unknown> {
  encode(value: TInput): unknown;
  decode(value: unknown): TOutput;
}

export type ScalarCodecs = Readonly<Record<string, ScalarCodec>>;
export interface OperationOptions { readonly codecs?: ScalarCodecs }

export interface OperationDefinition {
  readonly name: string;
  readonly kind: "query" | "mutation" | "subscription";
  readonly persisted: PersistedReference;
  readonly variables: readonly VariableDefinition[];
}

export interface Operation<TVariables extends object, TResult> {
  readonly kind: "query" | "mutation" | "subscription";
  readonly request: Readonly<{ version: "1"; operation: string; persisted: PersistedReference; variables: Readonly<Record<string, unknown>> }>;
  canonicalRequest(): string;
  decodeResult(input: string | Uint8Array | Readonly<Record<string, unknown>>): Readonly<{ data: TResult | null; errors: readonly Readonly<Record<string, unknown>>[]; complete: boolean }>;
}

export class NaatreClientError extends Error {
  readonly code: string;
  readonly status: number;
}

export function canonicalStringify(value: unknown): string;
export function parseJSON(input: string | Uint8Array, maximumBytes?: number): unknown;
export function hasOwn(value: object, key: PropertyKey): boolean;
export function safeObject(value: unknown, code?: string): Record<string, unknown>;
export function createOperation<TVariables extends object, TResult>(definition: OperationDefinition, variables: TVariables, decodeData: (value: unknown) => TResult, options?: OperationOptions): Operation<TVariables, TResult>;
export function loadManifest(input: string | Readonly<Record<string, unknown>>): Readonly<Record<string, Readonly<Record<string, unknown>>>>;
export function decodeSelected<T>(owner: Readonly<Record<string, unknown>>, key: string, pendingWhenMissing: boolean, decode: (value: unknown) => T): Selected<T>;
export function decodeString(value: unknown): string;
export function decodeNumber(value: unknown): number;
export function decodeBoolean(value: unknown): boolean;
export function decodeList<T>(value: unknown, decode: (value: unknown) => T): readonly T[];
export function decodeOperationResult<TResult>(input: string | Uint8Array | Readonly<Record<string, unknown>>, decodeData: (value: unknown) => TResult): Readonly<{ data: TResult | null; errors: readonly Readonly<Record<string, unknown>>[]; complete: boolean }>;
export const missing: Selected<never>;
export const nullValue: Selected<never>;
export const pending: Selected<never>;
export function present<T>(value: T): Selected<T>;
export function failed(errors: readonly Readonly<Record<string, unknown>>[]): Selected<never>;
export function skipped(reason: string): Selected<never>;
export function encodeInt64(value: string | bigint): string;
export function encodeUInt64(value: string | bigint): string;
export function encodeBigInt(value: string | bigint): string;
export function encodeDecimal(value: string): string;
export function encodeTimestamp(value: string): string;
export function encodeDuration(value: string | bigint): string;
export function encodeUUID(value: string): string;
export function encodeBytes(value: Uint8Array): string;
export function decodeBytes(value: string): Uint8Array;
export function createScalarCodecs(custom?: Readonly<Record<string, Readonly<{ encode(value: unknown): unknown; decode(value: unknown): unknown }>>>): Readonly<Record<string, Readonly<{ encode(value: unknown): unknown; decode(value: unknown): unknown }>>>;
export function encodeScalar(codecs: ScalarCodecs, type: string, value: unknown): unknown;
export function decodeScalar(codecs: ScalarCodecs, type: string, value: unknown): unknown;
export const runtimeVersion: "naatre.typescript.runtime-1";
