//! The production derived-data kinds (M6b): blame, path_history, and
//! commit_graph, built on the M6a framework. All three are DEFERRED — the
//! queue derives them; a write only enqueues (and the serve-time backfill
//! enqueues everything a registry-less process wrote, see
//! [`crate::derive::backfill`]).
//!
//! Per kind, the split between the framework handoff output
//! ([`crate::derive::DerivedOut`], capped at 16 MiB, read by CHILD
//! derivations) and the kind's own served-surface storage (its own keyed,
//! idempotent writes, read by the routes):
//!
//! - **blame** — the handoff output stays EMPTY (children need nothing; a
//!   whole-file annotation would be the 16 MiB hazard). The served surface
//!   is the per-changeset DIFF-STATE in `blame_state` (migration 5): for
//!   every path the changeset touched, its blob id and the FIRST parent's
//!   blob id. A blame read is then two point lookups per visited chain
//!   node — never a whole-file annotation, never a tree walk at read time.
//! - **path_history** — the handoff output stays EMPTY (a flat per-node
//!   computation; nothing flows between nodes). The served surface is the
//!   by-path inverted index in `path_history` (migration 5): the
//!   changed-path list re-keyed as (repo, path) → (gen, cs, op) touches.
//! - **commit_graph** — the handoff output IS the state: binary-lifting
//!   skip entries over the first-parent chain, (target, hop, hop-target
//!   generation, span-purity) with hops 1, 2, 4, … Children build theirs
//!   from the parent's output (plus the target's own output for the
//!   doubling step), so the inter-node channel is exactly the framework's.
//!
//! Every deriver charges its row/output visits through the
//! [`crate::derive::DeriveCtx`] walk budget — a COOPERATIVE per-kind cap
//! (it binds derivers that charge; raw SQL past the ctx is not capped) in
//! place of the (wrong, absolute-generation) depth cap it replaced. A
//! node whose derivation consumed a poisoned parent's empty marker
//! records done WITH poison taint ([`crate::derive::ParentOut`] / the
//! `derive_state` taint columns) — the read surfaces flag such reads
//! degraded instead of presenting them as exact.

use std::sync::Arc;

use rusqlite::Connection;

use naatre_core::ids::Id;

use crate::db;
use crate::derive::{DeriveCtx, DeriveFailure, DerivedOut, Deriver, ParentOut, PoisonReason};

/// Registry name of the blame kind.
pub const KIND_BLAME: &str = "blame";
/// Registry name of the path-history kind.
pub const KIND_PATH_HISTORY: &str = "path_history";
/// Registry name of the commit-graph kind.
pub const KIND_COMMIT_GRAPH: &str = "commit_graph";

/// The production registrations, in registry order. `naatre serve` (the
/// CLI's host verb) registers these before the accept loop; the offline
/// `import` process registers nothing — its backlog backfills here through
/// the queue on the serving side.
pub fn production_kinds() -> Vec<Arc<dyn Deriver>> {
    vec![
        Arc::new(BlameDeriver),
        Arc::new(PathHistoryDeriver),
        Arc::new(CommitGraphDeriver),
    ]
}

/// A rusqlite failure is a deriver `Err` — a failed attempt (the queue
/// retries, then quarantines). SQL failures are usually transient
/// (busy-slot shapes), not structural.
fn sql_fail(e: rusqlite::Error) -> DeriveFailure {
    DeriveFailure::Fail(PoisonReason::new(e.to_string()))
}

fn blob_of(bytes: &[u8]) -> Id {
    Id(bytes.try_into().expect("blob id is 32 bytes"))
}

// ---- blame ------------------------------------------------------------------

/// The blame DIFF-STATE deriver: one `blame_state` row per touched path
/// (this changeset's blob id, the first parent's blob id), resolved by
/// walking the parent's tree once per touched path. Charges one visit per
/// file-change row plus the ROW-PRICED cost of each parent-tree walk
/// ([`crate::db::tree_lookup_path_charged`]: per component lookup and per
/// entry row materialized — the raw
/// [`crate::db::tree_lookup_path`] loads every entry of every node and
/// would let a crafted wide-and-deep changeset stall the write slot with
/// the budget untripped).
pub struct BlameDeriver;

impl Deriver for BlameDeriver {
    fn kind(&self) -> &'static str {
        KIND_BLAME
    }

    /// One visit per touched file-change row plus the tree rows each
    /// row's parent-tree walk loads (see the pricing convention on
    /// [`crate::derive::DEFAULT_WALK_BUDGET`]). The worst admitted node is
    /// a MAX_ENTRIES changeset (100,000 rows) whose per-row walk loads
    /// ≤ ~90 tree entries (e.g. depth 8 through dirs averaging ~11
    /// entries) — 10 million covers it; anything wider trips the budget
    /// and is quarantined immediately (seconds), which is the designed
    /// outcome: unpriced, that same shape held the write slot for
    /// minutes-to-hours.
    fn walk_budget(&self) -> u64 {
        10_000_000
    }

    fn derive(
        &self,
        ctx: &DeriveCtx,
        conn: &Connection,
        repo_id: i64,
        cs: Id,
        _parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        let parent_root: Option<Id> =
            match db::changeset_graph(conn, repo_id, &cs).map_err(sql_fail)? {
                Some(graph) => match graph.parents.first() {
                    Some(p1) => {
                        // The parent row is append-only and was present at
                        // enqueue time; a missing row is a broken store — a
                        // plain failure (retried, then quarantined), never a
                        // silent half-state.
                        let p1g = db::changeset_graph(conn, repo_id, p1)
                            .map_err(sql_fail)?
                            .ok_or_else(|| {
                                DeriveFailure::Fail(PoisonReason::new(
                                    "first-parent row vanished mid-derivation",
                                ))
                            })?;
                        p1g.root_tree_id.map(|b| blob_of(&b))
                    }
                    None => None,
                },
                None => {
                    return Err(DeriveFailure::Fail(PoisonReason::new(
                        "changeset row vanished mid-derivation",
                    )));
                }
            };

        let changes = db::file_changes_list(conn, repo_id, &cs).map_err(sql_fail)?;
        let mut entries = Vec::with_capacity(changes.len());
        for c in &changes {
            ctx.charge(1)?;
            let parent_blob_id = match &parent_root {
                Some(root) => db::tree_lookup_path_charged(conn, ctx, root, &c.path)?,
                None => None,
            };
            entries.push(db::BlameStateEntry {
                path: c.path.clone(),
                blob_id: c.blob_id.as_deref().map(blob_of),
                parent_blob_id,
            });
        }
        db::blame_state_put(conn, repo_id, &cs, &entries).map_err(sql_fail)?;
        // The handoff output stays empty: children resolve their own
        // parents' trees, and whole-file annotations are exactly what this
        // representation avoids.
        Ok(DerivedOut::empty())
    }
}

// ---- path_history -----------------------------------------------------------

/// The path-history deriver: re-keys this changeset's changed-path list
/// into the by-path inverted index (`path_history` rows keyed
/// (repo, path, gen, cs)). Charges one visit per file-change row.
pub struct PathHistoryDeriver;

impl Deriver for PathHistoryDeriver {
    fn kind(&self) -> &'static str {
        KIND_PATH_HISTORY
    }

    /// One visit per file-change row — the worst legitimate node is a
    /// MAX_ENTRIES changeset (100,000 rows), so exactly 2× the bootstrap
    /// default bounds it with headroom and nothing more.
    fn walk_budget(&self) -> u64 {
        400_000
    }

    fn derive(
        &self,
        ctx: &DeriveCtx,
        conn: &Connection,
        repo_id: i64,
        cs: Id,
        _parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        let generation = db::changeset_graph(conn, repo_id, &cs)
            .map_err(sql_fail)?
            .ok_or_else(|| {
                DeriveFailure::Fail(PoisonReason::new("changeset row vanished mid-derivation"))
            })?
            .generation;
        let changes = db::file_changes_list(conn, repo_id, &cs).map_err(sql_fail)?;
        for _ in &changes {
            ctx.charge(1)?;
        }
        db::path_history_put(conn, repo_id, generation, &cs, &changes).map_err(sql_fail)?;
        Ok(DerivedOut::empty())
    }
}

// ---- commit_graph -----------------------------------------------------------

/// One commit-graph skip entry: the first-parent ancestor `hop` levels up
/// from a node, its generation, and whether the SPAN in between is free of
/// branching nodes (every spanned changeset has at most one parent). Only
/// pure spans may be jumped without losing a spanned node's non-first
/// parents — the merge-base alignment jumps pure entries only.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SkipEntry {
    pub target: Id,
    pub hop: u64,
    pub generation: i64,
    pub pure: bool,
}

/// Encodes a node's skip entries: count u32 LE, then per entry the 32-byte
/// target id, hop u64 LE, generation i64 LE, purity byte. Hops follow the
/// chained-doubling sequence 1, 3, 7, 15, … (2^(k+1) − 1) — each entry
/// composes from the parent's and the midpoint's previous entries with two
/// committed-output reads, and any climb distance is coverable greedily in
/// O(log) jumps. A node carries O(log chain) entries: orders of magnitude
/// under the 16 MiB output cap for any real chain.
pub fn encode_skip_entries(entries: &[SkipEntry]) -> Vec<u8> {
    let mut out = Vec::with_capacity(4 + entries.len() * 49);
    out.extend_from_slice(&(entries.len() as u32).to_le_bytes());
    for e in entries {
        out.extend_from_slice(&e.target.0);
        out.extend_from_slice(&e.hop.to_le_bytes());
        out.extend_from_slice(&e.generation.to_le_bytes());
        out.push(u8::from(e.pure));
    }
    out
}

/// Decodes [`encode_skip_entries`] bytes; `None` on any shape violation
/// (the read side treats an undecodable output as "no skips" and falls
/// back — never guesses from corrupt bytes).
pub fn decode_skip_entries(bytes: &[u8]) -> Option<Vec<SkipEntry>> {
    if bytes.len() < 4 {
        return None;
    }
    let n = u32::from_le_bytes(bytes[..4].try_into().expect("4 bytes")) as usize;
    let mut rest = &bytes[4..];
    let mut out = Vec::with_capacity(n.min(1024));
    for _ in 0..n {
        if rest.len() < 49 {
            return None;
        }
        let target = Id(rest[..32].try_into().expect("32 bytes"));
        let hop = u64::from_le_bytes(rest[32..40].try_into().expect("8 bytes"));
        let generation = i64::from_le_bytes(rest[40..48].try_into().expect("8 bytes"));
        let pure = rest[48] == 1;
        rest = &rest[49..];
        out.push(SkipEntry {
            target,
            hop,
            generation,
            pure,
        });
    }
    if !rest.is_empty() {
        return None;
    }
    Some(out)
}

/// The commit-graph deriver: chained doubling over the first-parent chain.
/// Entry 0 is the first parent (hop 1). Entry k composes the parent's
/// entry k−1 (its target is the hop's MIDPOINT, 1 + d(k−1) levels up) with
/// the midpoint's own entry k−1 — two committed-output reads (the parent
/// and the midpoint both derived before this node) — giving hop
/// d(k) = 2·d(k−1) + 1: 1, 3, 7, 15, … A span's purity needs every spanned
/// node single-parent: the span of entry k is {p1} ∪ span(parent, k−1) ∪
/// {midpoint} ∪ span(midpoint, k−1), so the parent and the midpoint must
/// be single-parent and both sub-spans pure.
pub struct CommitGraphDeriver;

impl CommitGraphDeriver {
    /// A done node's decoded skip output: `None` when the node has no row
    /// (not registered — impossible for a registered kind past backfill),
    /// is not done (poisoned first parent — the child builds on fewer
    /// jumps), or its bytes do not decode.
    fn skips_of(
        conn: &Connection,
        repo_id: i64,
        cs: &Id,
    ) -> Result<Option<Vec<SkipEntry>>, DeriveFailure> {
        let Some(kind_id) = db::derive_kind_id(conn, KIND_COMMIT_GRAPH).map_err(sql_fail)? else {
            return Ok(None);
        };
        let Some(row) = db::derive_get(conn, kind_id, repo_id, cs).map_err(sql_fail)? else {
            return Ok(None);
        };
        if row.state != "done" {
            return Ok(None);
        }
        Ok(row.output.as_deref().and_then(decode_skip_entries))
    }
}

impl Deriver for CommitGraphDeriver {
    fn kind(&self) -> &'static str {
        KIND_COMMIT_GRAPH
    }

    /// Two charges per skip entry and O(log chain) entries per node: even
    /// a chain spanning the whole u64 generation space needs ~120 charges,
    /// so 1,000 is orders of magnitude above any real node.
    fn walk_budget(&self) -> u64 {
        1_000
    }

    fn derive(
        &self,
        ctx: &DeriveCtx,
        conn: &Connection,
        repo_id: i64,
        cs: Id,
        _parents_done: &[ParentOut],
    ) -> Result<DerivedOut, DeriveFailure> {
        let graph = db::changeset_graph(conn, repo_id, &cs)
            .map_err(sql_fail)?
            .ok_or_else(|| {
                DeriveFailure::Fail(PoisonReason::new("changeset row vanished mid-derivation"))
            })?;
        let Some(p1) = graph.parents.first().copied() else {
            // Root: no first parent, no jumps.
            return Ok(DerivedOut::empty());
        };
        let p1_gen = db::changeset_graph(conn, repo_id, &p1)
            .map_err(sql_fail)?
            .ok_or_else(|| {
                DeriveFailure::Fail(PoisonReason::new(
                    "first-parent row vanished mid-derivation",
                ))
            })?
            .generation;
        let mut entries = vec![SkipEntry {
            target: p1,
            hop: 1,
            generation: p1_gen,
            // Hop 1 skips nothing (the jump source is processed), so the
            // span is trivially pure.
            pure: true,
        }];

        let p1_skips = Self::skips_of(conn, repo_id, &p1)?;
        if let Some(p1_skips) = p1_skips {
            for k in 1.. {
                ctx.charge(1)?;
                // The parent's entry k-1 targets the hop's midpoint.
                let Some(mid) = p1_skips.get(k - 1) else {
                    break;
                };
                let mid_skips = Self::skips_of(conn, repo_id, &mid.target)?;
                let Some(far) = mid_skips.as_ref().and_then(|s| s.get(k - 1)) else {
                    break;
                };
                ctx.charge(1)?;
                let p1_single =
                    db::changeset_parent_count(conn, repo_id, &p1).map_err(sql_fail)? <= 1;
                let mid_single =
                    db::changeset_parent_count(conn, repo_id, &mid.target).map_err(sql_fail)? <= 1;
                // Span of entry k = {p1} ∪ span(parent, k-1) ∪ {midpoint} ∪
                // span(midpoint, k-1): every member single-parent.
                let pure = p1_single && mid_single && mid.pure && far.pure;
                entries.push(SkipEntry {
                    target: far.target,
                    hop: far.hop * 2 + 1,
                    generation: far.generation,
                    pure,
                });
            }
        }
        Ok(DerivedOut(encode_skip_entries(&entries)))
    }
}

// ---- blame assembly ---------------------------------------------------------

/// One line of an assembled blame: the changeset whose edit introduced the
/// line (first-parent semantics), its author, and the line text without
/// its trailing newline.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BlameLine {
    pub cs: Id,
    pub author: String,
    pub text: String,
}

/// One layer of a blame walk, oldest LAST (the walk collects newest-first
/// and the assembly reverses): the changeset, its author, and the path's
/// blob bytes at it.
pub struct BlameLayer<'a> {
    pub cs: Id,
    pub author: String,
    pub bytes: &'a [u8],
}

/// Assembles line attributions from the walked layers, one layer at a
/// time. The route streams blob loads through this (each layer's bytes are
/// fetched, diffed, and dropped before the next is read — never all
/// versions materialized at once) inside a blocking task, so the Myers
/// diffs never run on the async runtime. Feed the OLDEST layer to
/// [`BlameAssembler::new`] and the newer layers, oldest first, to
/// [`BlameAssembler::push_step`]; [`BlameAssembler::finish`] yields the
/// lines.
pub struct BlameAssembler {
    /// Raw (ending-kept) text of the newest layer fed so far; display
    /// strips the newline.
    current_raw: String,
    owners: Vec<(Id, String)>,
}

impl BlameAssembler {
    /// Seeds the assembly with the OLDEST layer (an add: every line of its
    /// blob starts attributed to it — or, in a degraded read, the
    /// quarantined boundary changeset: the oldest resolved point, flagged
    /// by the caller). A non-UTF-8 layer is opaque: its content diffs as
    /// empty, so the next-newer layer absorbs the attribution (containment
    /// still holds — the layer's own blob is never claimed to contain
    /// lines it cannot show).
    pub fn new(base: BlameLayer<'_>) -> Self {
        let current_raw = raw_text(base.bytes);
        let owners = vec![(base.cs, base.author.clone()); line_count(&current_raw)];
        BlameAssembler {
            current_raw,
            owners,
        }
    }

    /// Folds one newer layer in: its diff against the previous layer keeps
    /// matching lines' attributions and attributes the rest to the step.
    pub fn push_step(&mut self, step: BlameLayer<'_>) {
        let new_raw = raw_text(step.bytes);
        let new_lines = line_count(&new_raw);
        let diff = similar::TextDiff::from_lines(&self.current_raw, &new_raw);
        let mut new_owners: Vec<(Id, String)> = Vec::with_capacity(new_lines);
        for op in diff.ops() {
            match op {
                similar::DiffOp::Equal { old_index, len, .. } => {
                    for j in 0..*len {
                        new_owners.push(self.owners[old_index + j].clone());
                    }
                }
                // Insert / Replace: the new-side lines are this step's
                // edit. Delete contributes no new-side lines.
                similar::DiffOp::Insert { new_len, .. }
                | similar::DiffOp::Replace { new_len, .. } => {
                    for _ in 0..*new_len {
                        new_owners.push((step.cs, step.author.clone()));
                    }
                }
                similar::DiffOp::Delete { .. } => {}
            }
        }
        // Every new line is covered by exactly one op (Equal, Insert, or
        // Replace); pad defensively rather than index-panic on a diff
        // implementation change.
        while new_owners.len() < new_lines {
            new_owners.push((step.cs, step.author.clone()));
        }
        new_owners.truncate(new_lines);
        self.owners = new_owners;
        self.current_raw = new_raw;
    }

    /// The attributed lines of the FINAL content (the last step's bytes
    /// when steps exist, the base's otherwise).
    pub fn finish(self) -> Vec<BlameLine> {
        let display: Vec<&str> = if self.current_raw.is_empty() {
            Vec::new()
        } else {
            self.current_raw
                .split_inclusive('\n')
                .map(|l| l.strip_suffix('\n').unwrap_or(l))
                .collect()
        };
        display
            .into_iter()
            .zip(self.owners)
            .map(|(text, (cs, author))| BlameLine {
                cs,
                author,
                text: text.to_string(),
            })
            .collect()
    }
}

/// Raw (ending-kept) view of a layer's bytes; invalid UTF-8 diffs as empty.
fn raw_text(bytes: &[u8]) -> String {
    std::str::from_utf8(bytes).unwrap_or("").to_string()
}

/// Assembles line attributions from the walked layers. `base` is the
/// OLDEST layer; `steps` are the NEWER layers, oldest first; the LAST
/// step's bytes are the file at the read's changeset. The streaming
/// equivalent is [`BlameAssembler`].
pub fn assemble_blame(base: BlameLayer<'_>, steps: &[BlameLayer<'_>]) -> Vec<BlameLine> {
    let mut asm = BlameAssembler::new(base);
    for step in steps {
        asm.push_step(BlameLayer {
            cs: step.cs,
            author: step.author.clone(),
            bytes: step.bytes,
        });
    }
    asm.finish()
}

/// `similar`-compatible line count: `split_inclusive('\n')` length.
fn line_count(text: &str) -> usize {
    if text.is_empty() {
        return 0;
    }
    text.split_inclusive('\n').count()
}
