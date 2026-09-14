# Decision 0002: queue-neutral asynchronous operations

Status: accepted for v1.

The asynchronous core is a revisioned state machine over an application-owned
durable store. Durable acceptance, bounded pending discovery, and compare-and-swap are the portability
boundary; a broker or queue is not. This lets an application use a database,
an outbox, a queue-backed store, or another durable scheduler while preserving
one handle and one race contract.

The public coordinator refuses process-local stores. It never turns the
accepting HTTP request into worker ownership and never launches accepted work
as an attached goroutine. Workers claim pending work through an atomic state
revision. Optional subscription waits only for revision notifications and then
loads the same authorized record as polling, so a notification channel cannot
become a second source of truth.

Concrete durable providers, leases, dispatch scanners, and queue adapters are
separated into issue #89. That boundary prevents a nominal reference queue
from becoming a hidden platform requirement while keeping crash recovery,
duplicate delivery, and late completion behavior normative in the core.
