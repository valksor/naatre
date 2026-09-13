# Request-scoped batching and caching

This document defines the `core.batch-cache-1` profile. It permits bounded
request-local optimization without changing selection ordering, authorization,
effects, completion, or application ownership of cross-request data.

## Registration and dispatch

- **BATCH-100 Explicit registration:** A batch handler is registered against
  one normal operation or member descriptor with a typed input, deterministic
  key function, and finite maximum dispatch size. Batch eligibility,
  cacheability, effect, thread safety, authorization, and transaction metadata
  remain independent assertions. A batch handler requires a batch-eligible,
  thread-safe read descriptor.
- **BATCH-101 Deterministic windows:** A dispatch window is formed only by an
  explicit parallel group or a mapped collection region whose complete selected
  work is read-only, thread-safe, and not transaction-required. The window is
  bounded by the registered maximum and split in declaration/input order. Host
  event-loop ticks, timers, and goroutine scheduling are not semantic inputs.
- **BATCH-102 Barriers:** A serial-only handler, write, dependency, transaction,
  authorization failure, completion boundary, or execution wrapper closes the
  current window. A strictly serial loop may therefore dispatch one item at a
  time. No handler is speculatively moved across such a barrier.
- **BATCH-103 Authorization before grouping:** Every logical input is authorized
  before it joins a dispatch, including duplicates. Group identity includes the
  operation and authorization scope. Equal provider keys never merge
  incompatible principals, tenants, authorization revisions, expiries, or
  cache scopes.

## Results, errors, and cancellation

- **BATCH-200 Indexed results:** Each deduplicated input has a stable dispatch
  index. Backends may return indexed results in any order. The runtime restores
  original input and response order; a duplicate or unknown result index makes
  the dispatch malformed, while an omitted index produces a per-input missing
  result error. Duplicate logical inputs receive the same indexed result only
  when the descriptor explicitly permits request memoization.
- **BATCH-201 Completion and partial errors:** Each returned value is completed
  independently against the registered output type. Handler, missing-result,
  and completion failures retain the original response path and null
  placeholder rules. One item failure does not shift later items.
- **BATCH-202 Cancellation and permits:** A cancelled cache waiter returns
  without spawning a waiter goroutine. Batch handlers receive the request
  context and use the normal bounded abandonment policy. Waiting for a dispatch
  or in-flight deduplicated result never holds a handler concurrency permit; the
  dispatch itself holds one permit and releases it on every return path.
- **BATCH-203 Accounting and telemetry:** Runtime work is charged once per
  logical selected handler before dispatch. Dispatch telemetry contains only
  operation, handler identity, bounded size, stage, and failure/cancellation
  flags; it contains no typed inputs, raw keys, principal claims, or backend
  errors.

## Request and application caches

- **CACHE-100 Explicit safety:** Only a descriptor explicitly marked cacheable
  may be deduplicated or memoized. Query/read classification alone does not
  establish purity. Execution wrappers and interceptors disable core
  memoization because their cache semantics are not declared by the descriptor.
- **CACHE-101 Complete identity:** A handler cache key binds the operation kind
  and name, handler identity, document digest, schema digest and revision,
  canonical coerced variables, typed source/input semantics, authorization
  identity and cache scope, negotiated requirements, relevant application
  context, and the current request invalidation generation. Raw claims,
  variables, and typed inputs are represented only by digests in application
  hooks.
- **CACHE-102 Scope and policy:** Current authorization runs before every hit.
  A dynamic `no-store` decision disables reuse. Errors and null/negative values
  are not retained unless the request explicitly enables their independent
  policies. Request memoization can be disabled without enabling any other
  storage.
- **CACHE-103 Application ownership:** Core stores entries only inside one
  execution. Cross-request lookup, store, and invalidation require an explicit
  application-owned hook and non-empty current schema revision. Hook failures
  never silently widen scope; lookup/store failures fall back to execution,
  while a failed invalidation is a visible execution failure.
- **CACHE-104 Mutation invalidation:** Every successfully completed write first
  advances the request generation and clears request entries, then calls the
  application invalidation hook before a dependent read can be admitted.
  Concurrent pre-write fills cannot republish after generation change.

Valid: 64 independent mapped reads become one bounded batch, a missing backend
result fails only its original item path, and a following mutation write forces
the next identical read to observe committed application state.

Invalid: schedule a batch on a timer tick, hold all handler permits while
waiting for that batch, reuse a principal entry for another tenant, cache an
error by default, or publish a stale fill after mutation invalidation.
