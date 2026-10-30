# Reference-clone research digest

During deep research (October 2026), six independent "Scion-style" source
control implementations were studied as **read-only reference material**
(local clones under `.research/`, gitignored; never built, never
executed). This digest records each clone's license and the one thing it
taught this project.

**No code was copied from any of them.** naatre is original code; the
clones informed design choices and pitfalls only, and their licenses are
recorded here because the plan requires it (`plan.md`: "`.research/`
clones are read-only study material — no code is copied; the research
digest records each clone's license").

| Clone | Upstream | License | What it taught |
|---|---|---|---|
| tekeburak/scion | <https://github.com/tekeburak/scion> | Apache-2.0 (+NOTICE) | Demands Postgres + S3 to run; its measurements of trees-in-DB informed naatre's split — derived tree rows in SQLite, blob bytes in a filesystem CAS. |
| CarterT27/scion | <https://github.com/CarterT27/scion> | Apache-2.0 | CockroachDB as the metadata store — a distributed database as a *prerequisite* is exactly the deployment weight naatre's embedded-SQLite default exists to avoid. |
| bnsd55/openscion | <https://github.com/bnsd55/openscion> | MIT OR Apache-2.0 | The most complete clone (~123k LOC, protoc + Postgres/SQLite); its server-side land flow was the closest precedent for naatre's pushrebase-style land and its idempotency-key handling. |
| tinyhumansai/openscion | <https://github.com/tinyhumansai/openscion> | GPL-3.0-only | RAM-only storage: a toy that demonstrates the model collapses without durable blobs and an authoritative server. |
| fenwii/openscion | <https://github.com/fenwii/openscion> | MIT | Reportedly never compiled — a reminder that a design document alone is not an implementation, and that compile-ability is itself a claim to verify. |
| feder-cr/git-ai | <https://github.com/feder-cr/git-ai> | MIT (fork + upstream Gitaly); NOTICE aggregates third-party component notices | A fork of Gitaly in Go: process, container, and storage-server infrastructure as the substrate — the contrast case that anchors naatre's single-binary goal. |

Compatibility note: one clone is GPL-3.0-only (tinyhumansai). Since no
code was copied from any clone, naatre's GPL-3.0-only licensing is
unaffected; the no-copy rule is what keeps that true.
