# v1 asynchronous operations migration note

The `operations.async-1` profile adds durable operation handles and does not
change the meaning of an ordinary unary response. Hosts opt individual
operations into asynchronous acceptance and must persist ownership before
returning `202 Accepted`.

Applications adopting the profile provide a durable `AsyncOperationStore`, an
authorization callback, stable ID and Location builders, retention and progress
limits, and workers. Process-local idempotency stores do not satisfy durable
acceptance. Existing background goroutines that return a handle before durable
persistence must move the persistence barrier ahead of the response.

Clients should follow Location, retain ETag and revision, honor Retry-After,
and treat `requested` cancellation as non-terminal. They must handle
`indeterminate` without automatically retrying a mutation. Optional
subscription is an optimization over the same authorized durable record;
polling remains the portable baseline.
