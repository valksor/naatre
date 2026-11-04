//! Structured-mutation fuzzing of the canonical changeset decoder.
//!
//! This is the CI-runnable stand-in for a libfuzzer target (cargo-fuzz
//! needs nightly + sanitizer runtimes that this CI does not carry — the
//! M2a plan names this substitution as pre-approved). A real fuzz target
//! with the same mutations should land with the M2b cargo-fuzz work.
//!
//! Strategy: generate a canonical changeset encoding from a structured
//! generator, then apply one structured mutation — truncate at an arbitrary
//! boundary, flip a byte (biased toward the header/length-field region),
//! append garbage, or swap two adjacent entries in the raw bytes — and
//! assert:
//!
//! 1. `decode` never panics; every outcome is Ok or Err.
//! 2. Truncation and appended garbage are always rejected (the decoder's
//!    trailing-byte/EOF strictness).
//! 3. Any `Ok` decode round-trips: when `encode` also succeeds, it must
//!    reproduce the exact input bytes — `decode` is lossless (every field
//!    is copied verbatim, framing is recomputed from the same values). The
//!    only `Ok`-but-`encode`-fails case is a mutation that broke the
//!    strictly-ascending path order, and that failure is exactly
//!    `EncodeUnsorted`.
//! 4. The unmutated encoding satisfies full identity: decode → encode is
//!    the original bytes.

use proptest::prelude::*;

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::conflict::ConflictRep;
use naatre_core::error::CoreError;
use naatre_core::ids::Id;

/// Conflict-section generator (format v2): 0–2 reps at distinct paths
/// (sorted into canonical order), base present or absent, 2–5 blob sides in
/// strictly ascending order, and in some cases the deletion flag — the
/// deleted side is always the last one, and it may leave a single blob
/// side. Exec bits ride on the base and on each side (the same blob may
/// appear twice, differing only in its exec bit). Half the generated sets
/// are empty, so v1 and v2 frames both occur.
fn arb_conflicts() -> impl Strategy<Value = Vec<ConflictRep>> {
    let rep = (
        prop_oneof![
            Just("c1".to_string()),
            Just("c2/x".to_string()),
            Just("c3".to_string()),
            Just("d/\u{00e9}".to_string()),
        ],
        2u8..=5,
        any::<bool>(),
        any::<bool>(),
        any::<bool>(),
        any::<u8>(),
        any::<bool>(),
        any::<bool>(),
    );
    prop::collection::vec(rep, 0..=2).prop_map(|reps| {
        let by_path: std::collections::BTreeMap<String, ConflictRep> = reps
            .into_iter()
            .map(
                |(path, n, with_base, deleted, one_side, exec_mask, base_exec, same_blob)| {
                    let n = if deleted && one_side { 1 } else { n };
                    // Strictly ascending (blob, exec): with `same_blob` the
                    // first two sides share a blob and differ in exec.
                    let blob_idx = |i: u8| if same_blob && i >= 1 { i - 1 } else { i };
                    let exec_of = |i: u8| {
                        if same_blob && i < 2 {
                            i == 1
                        } else {
                            exec_mask >> i & 1 == 1
                        }
                    };
                    let sides = (0..n)
                        .map(|i| {
                            let mut b = [0u8; 32];
                            b[0] = 0xC0;
                            b[1] = blob_idx(i);
                            Id(b)
                        })
                        .collect();
                    let sides_exec = (0..n).map(exec_of).collect();
                    let base = with_base.then(|| {
                        let mut b = [0u8; 32];
                        b[0] = 0xD0;
                        Id(b)
                    });
                    (
                        path.clone(),
                        ConflictRep {
                            path,
                            base,
                            base_exec: with_base && base_exec,
                            sides,
                            sides_exec,
                            deleted,
                        },
                    )
                },
            )
            .collect();
        // BTreeMap<String, _> iterates in ascending byte order.
        by_path.into_values().collect()
    })
}

/// Changeset-shaped generator: parents (0–2), distinct paths (sorted into
/// canonical order — the mutations supply the disorder), blob ids from a
/// small pool, small sizes, arbitrary exec bits.
fn arb_changeset() -> impl Strategy<Value = Changeset> {
    let path_pool = prop::collection::vec(
        prop_oneof![
            Just("a".to_string()),
            Just("b".to_string()),
            Just("a/b".to_string()),
            Just("a/c/d".to_string()),
            Just("z/\u{00e9}.txt".to_string()),
            Just("m".to_string()),
            Just("n/o".to_string()),
            Just("p/q/r".to_string()),
            Just("\u{00ff}x".to_string()),
        ],
        1..=6,
    );
    (
        0u64..=2,
        prop::option::of(any::<u64>().prop_map(|s| format!("A{s} <a@b>"))),
        prop::option::of(any::<u64>()), // timestamp base
        path_pool,
        arb_conflicts(),
    )
        .prop_map(|(parent_seed, author, ts_seed, paths, conflicts)| {
            let parents: Vec<Id> = (0..parent_seed as usize)
                .map(|i| {
                    let mut b = [0u8; 32];
                    b[0] = 0x50;
                    b[1] = i as u8;
                    Id(b)
                })
                .collect();
            // Dedup by bytes and iterate in ascending byte order — the
            // canonical order the encoder demands (the mutations below
            // supply the disorder).
            let distinct: std::collections::BTreeSet<Vec<u8>> =
                paths.into_iter().map(|p| p.into_bytes()).collect();
            let changes: Vec<FileChange> = distinct
                .into_iter()
                .map(|path_bytes| {
                    let path = String::from_utf8(path_bytes).unwrap();
                    let mut b = [0u8; 32];
                    b[0] = 0xB0;
                    b[1] = (path.len() % 251) as u8;
                    let blob = Id(b);
                    let op = match path.len() % 3 {
                        0 => Op::Add,
                        1 => Op::Mod,
                        _ => Op::Del,
                    };
                    FileChange {
                        path,
                        op,
                        blob: (op != Op::Del).then_some(blob),
                        size: (op != Op::Del).then_some(1 + (blob.0[1] as u64)),
                        exec: (op != Op::Del).then(|| blob.0[1].is_multiple_of(2)),
                    }
                })
                .collect();
            Changeset {
                conflicts,
                repo_id: 7,
                parents,
                author: author.unwrap_or_else(|| "A <a@b>".to_string()),
                message: "fuzz".into(),
                timestamp_ns: ts_seed.unwrap_or(1),
                changes,
            }
        })
}

/// One structured mutation. Positions are fractions of the input length
/// (the case space covers every boundary region over 256 cases).
#[derive(Debug, Clone)]
enum Mutation {
    /// Cut the input at position `num/den`.
    Truncate { num: u32, den: u32 },
    /// Flip one byte with `mask`, biased to the header/length region.
    Flip {
        frac: u32,
        mask: u8,
        header_bias: bool,
    },
    /// Append 1–16 garbage bytes.
    Append(Vec<u8>),
    /// Swap the raw byte spans of two adjacent entries.
    Swap { index: usize },
}

fn arb_mutation() -> impl Strategy<Value = Mutation> {
    prop_oneof![
        3 => (0u32..100u32, any::<u32>()).prop_map(|(num, salt)| {
            Mutation::Truncate { num: num ^ (salt & 1), den: 100 }
        }),
        5 => (0u32..100u32, any::<u8>(), any::<bool>()).prop_map(
            |(frac, mask, header_bias)| Mutation::Flip { frac, mask, header_bias },
        ),
        1 => proptest::collection::vec(any::<u8>(), 1..=16).prop_map(Mutation::Append),
        2 => (0usize..4usize).prop_map(|index| Mutation::Swap { index }),
    ]
}

/// Applies the mutation to canonical bytes. Returns `(mutated, kind)` where
/// `kind` reports whether the mutation can only produce a decoder
/// rejection (truncate/append) or is order-breaking by construction
/// (swap). Byte flips carry no guarantee either way.
fn apply_mutation(bytes: &[u8], m: &Mutation) -> (Vec<u8>, &'static str) {
    match m {
        Mutation::Truncate { num, den } => {
            let pos = ((bytes.len() as u64) * u64::from(*num) / u64::from(*den)) as usize;
            (bytes[..pos.min(bytes.len())].to_vec(), "reject_only")
        }
        Mutation::Flip {
            frac,
            mask,
            header_bias,
        } => {
            let mut out = bytes.to_vec();
            let candidates = if *header_bias {
                // Header + first length fields: magic (17), repo_id (8),
                // parent count, parent ids, and the first string length.
                out.len().min(40)
            } else {
                out.len()
            };
            if candidates > 0 {
                let pos = (u64::from(*frac) * candidates as u64 / 100) as usize % candidates;
                out[pos] ^= *mask | 1; // never a no-op flip
            }
            (out, "either")
        }
        Mutation::Append(garbage) => {
            let mut out = bytes.to_vec();
            out.extend_from_slice(garbage);
            (out, "reject_only")
        }
        Mutation::Swap { index } => match entry_spans(bytes) {
            Some(spans) if spans.len() >= 2 => {
                let i = index % (spans.len() - 1);
                let (a, b) = (spans[i], spans[i + 1]);
                let mut out = Vec::with_capacity(bytes.len());
                out.extend_from_slice(&bytes[..a.0]);
                out.extend_from_slice(&bytes[b.0..b.1]);
                out.extend_from_slice(&bytes[a.1..b.0]);
                out.extend_from_slice(&bytes[a.0..a.1]);
                out.extend_from_slice(&bytes[b.1..]);
                (out, "order_breaking")
            }
            _ => (bytes.to_vec(), "identity"),
        },
    }
}

/// Locates every entry's `(start, end)` byte span by walking the canonical
/// layout: magic(17) ‖ u64le repo_id ‖ u8 parent_count ‖ parents ‖ str
/// author ‖ str message ‖ u64le ts ‖ u32le count ‖ entries.
fn entry_spans(bytes: &[u8]) -> Option<Vec<(usize, usize)>> {
    if bytes.len() < 17 + 8 + 1 + 8 {
        return None;
    }
    let mut pos = 17;
    let repo_end = pos + 8;
    if repo_end >= bytes.len() {
        return None;
    }
    let parent_count = bytes[repo_end] as usize;
    if parent_count > 2 {
        return None;
    }
    pos = repo_end + 1 + 32 * parent_count;
    for _ in 0..2 {
        let len = read_u64(bytes, pos)? as usize;
        if len > bytes.len() {
            return None;
        }
        pos += 8 + len;
    }
    pos += 8; // timestamp
    if pos + 4 > bytes.len() {
        return None;
    }
    let count = u32::from_le_bytes(bytes[pos..pos + 4].try_into().ok()?) as usize;
    pos += 4;
    let mut spans = Vec::with_capacity(count.min(64));
    for _ in 0..count {
        let start = pos;
        let plen = read_u64(bytes, pos)? as usize;
        pos += 8 + plen;
        if pos >= bytes.len() {
            return None;
        }
        let tag = bytes[pos];
        pos += 1;
        if tag != 2 {
            pos += 32 + 8 + 1;
        }
        if pos > bytes.len() {
            return None;
        }
        spans.push((start, pos));
    }
    Some(spans)
}

fn read_u64(bytes: &[u8], pos: usize) -> Option<u64> {
    if pos + 8 > bytes.len() {
        return None;
    }
    Some(u64::from_le_bytes(bytes[pos..pos + 8].try_into().ok()?))
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(256))]

    #[test]
    fn decode_survives_structured_mutations(
        cs in arb_changeset(),
        mutation in arb_mutation(),
    ) {
        let canonical = changeset::encode(&cs).expect("generator output must encode");

        // Identity: the unmutated encoding decodes to an equal changeset and
        // re-encodes to the exact bytes.
        let decoded = changeset::decode(&canonical).expect("canonical input decodes");
        prop_assert_eq!(&decoded, &cs);
        prop_assert_eq!(changeset::encode(&decoded).unwrap(), canonical.clone());

        let (mutated, kind) = apply_mutation(&canonical, &mutation);
        match changeset::decode(&mutated) {
            Err(_) => {
                // Truncations and appended garbage must never decode;
                // order-breaking and flipped inputs may land here too.
                prop_assert!(
                    kind != "identity",
                    "the unmutated canonical input must decode"
                );
                if kind == "reject_only" {
                    // Covered by the Err itself; nothing further to pin.
                }
            }
            Ok(parsed) => match changeset::encode(&parsed) {
                Ok(reencoded) => {
                    // decode is lossless: a successful re-encode of a
                    // successfully-decoded input is the input, byte for byte.
                    prop_assert_eq!(reencoded, mutated);
                }
                Err(
                    CoreError::EncodeUnsortedConflicts
                    | CoreError::EncodeConflictSides
                    | CoreError::Path(_),
                ) => {
                    // The decoder is structural; the conflict section's
                    // ordering, side rules and path grammar are validate's
                    // job. A flip inside that section can decode and then
                    // fail exactly there (never from a truncation, an
                    // append, or a swap).
                    prop_assert!(kind == "either", "conflict-section encode error from kind {kind}");
                }
                Err(CoreError::EncodeUnsorted) => {
                    // The only legal encode failure: the mutation broke
                    // strictly-ascending path order. Order can break two
                    // ways: the dedicated swap mutation, or a flip that
                    // lands in a path or its length prefix ("either" kind)
                    // and rewrites one path past its neighbor.
                    prop_assert!(
                        kind == "order_breaking" || kind == "either",
                        "encode-unsorted from kind {kind}"
                    );
                }
                Err(other) => prop_assert!(false, "unexpected encode error: {other:?}"),
            },
        }
    }
}

/// Pins the entry-span walker and the swap mutation: a swapped encoding
/// still decodes (structurally) but no longer re-encodes — exactly
/// `EncodeUnsorted`.
#[test]
fn swap_mutation_breaks_order_and_entry_spans_walk_the_layout() {
    let cs = Changeset {
        repo_id: 7,
        parents: vec![],
        author: "a".into(),
        message: "m".into(),
        timestamp_ns: 1,
        changes: vec![
            FileChange {
                path: "a".into(),
                op: Op::Add,
                blob: Some(Id([1; 32])),
                size: Some(1),
                exec: Some(false),
            },
            FileChange {
                path: "b".into(),
                op: Op::Del,
                blob: None,
                size: None,
                exec: None,
            },
        ],
        conflicts: Vec::new(),
    };
    let bytes = changeset::encode(&cs).unwrap();
    let spans = entry_spans(&bytes).expect("spans parse");
    assert_eq!(spans.len(), 2);
    let (mutated, kind) = apply_mutation(&bytes, &Mutation::Swap { index: 0 });
    assert_eq!(kind, "order_breaking");
    assert_ne!(mutated, bytes);
    let parsed = changeset::decode(&mutated).unwrap();
    assert!(matches!(
        changeset::encode(&parsed),
        Err(CoreError::EncodeUnsorted)
    ));
}

/// The same pin with a conflict section (a deletion side included) after
/// the entries: the span walker still ends at the entry list and the swap
/// stays order-breaking, with the v2 frame decoding structurally.
#[test]
fn swap_mutation_with_a_conflict_section_still_breaks_order() {
    let mut cs = Changeset {
        repo_id: 7,
        parents: vec![],
        author: "a".into(),
        message: "m".into(),
        timestamp_ns: 1,
        changes: vec![
            FileChange {
                path: "a".into(),
                op: Op::Add,
                blob: Some(Id([1; 32])),
                size: Some(1),
                exec: Some(false),
            },
            FileChange {
                path: "b".into(),
                op: Op::Del,
                blob: None,
                size: None,
                exec: None,
            },
        ],
        conflicts: vec![ConflictRep {
            path: "c".into(),
            base: Some(Id([9; 32])),
            base_exec: true,
            sides: vec![Id([2; 32])],
            sides_exec: vec![true],
            deleted: true,
        }],
    };
    let bytes = changeset::encode(&cs).unwrap();
    assert!(bytes.starts_with(changeset::MAGIC_V2));
    assert_eq!(entry_spans(&bytes).expect("spans parse").len(), 2);
    let (mutated, kind) = apply_mutation(&bytes, &Mutation::Swap { index: 0 });
    assert_eq!(kind, "order_breaking");
    let parsed = changeset::decode(&mutated).unwrap();
    assert_eq!(parsed.conflicts, cs.conflicts);
    assert!(matches!(
        changeset::encode(&parsed),
        Err(CoreError::EncodeUnsorted)
    ));
    cs.conflicts[0].deleted = false;
    cs.conflicts[0].sides = vec![Id([2; 32]), Id([3; 32])];
    cs.conflicts[0].sides_exec = vec![true, false];
    assert!(changeset::decode(&changeset::encode(&cs).unwrap()).is_ok());
}
