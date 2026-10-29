//! Property tests: re-encode identity, normalization round-trips, decoder
//! fuzzing, and the author/message character rule.

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::error::CoreError;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::{self, EntryKind, Tree, TreeEntry};
use proptest::prelude::*;
use unicode_normalization::UnicodeNormalization;

proptest! {
    #![proptest_config(ProptestConfig::with_cases(64))]

    /// canonical -> decode -> canonical is the identity, and the id is stable.
    #[test]
    fn changeset_reencode_identity(cs in arb_changeset()) {
        let bytes = changeset::encode(&cs).expect("encode valid changeset");
        let id = changeset::changeset_id(&cs).expect("id");
        let back = changeset::decode(&bytes).expect("decode own canonical bytes");
        prop_assert_eq!(&back, &cs);
        prop_assert_eq!(changeset::encode(&back).expect("re-encode"), bytes);
        prop_assert_eq!(changeset::changeset_id(&back).expect("id"), id);
    }

    /// Same identity property for trees.
    #[test]
    fn tree_reencode_identity(t in arb_tree()) {
        let bytes = tree::encode(&t).expect("encode valid tree");
        let id = tree::tree_id(&t).expect("id");
        let back = tree::decode(&bytes).expect("decode own canonical bytes");
        prop_assert_eq!(&back, &t);
        prop_assert_eq!(tree::encode(&back).expect("re-encode"), bytes);
        prop_assert_eq!(tree::tree_id(&back).expect("id"), id);
    }

    /// Composition after decomposition is the identity for NFC.
    #[test]
    fn nfc_nfd_roundtrip(s in arb_unicode_text()) {
        let nfc_once = s.nfc().collect::<String>();
        let nfd_then_nfc = s.nfd().nfc().collect::<String>();
        prop_assert_eq!(nfc_once, nfd_then_nfc);
    }

    /// Mutations of canonical bytes never panic: truncation and appended
    /// garbage always error; a flipped byte either errors, decodes back to
    /// exactly the mutated bytes, or breaks path sort order — which
    /// re-encoding catches as EncodeUnsorted.
    #[test]
    fn changeset_decode_fuzz(
        cs in arb_changeset(),
        mutation in 0u8..3,
        i in any::<usize>(),
        flip in any::<u8>(),
        garbage in proptest::collection::vec(any::<u8>(), 1..=16),
    ) {
        let bytes = changeset::encode(&cs).expect("encode valid changeset");
        let n = bytes.len();
        let mutated: Vec<u8> = match mutation {
            0 => bytes[..i % n].to_vec(),
            1 => {
                let mut m = bytes.clone();
                let j = i % n;
                m[j] ^= flip | 1;
                m
            }
            _ => {
                let mut m = bytes.clone();
                m.extend_from_slice(&garbage);
                m
            }
        };
        if let Ok(back) = changeset::decode(&mutated) {
            match changeset::encode(&back) {
                Ok(reencoded) => prop_assert_eq!(reencoded.as_slice(), mutated.as_slice()),
                // A flip inside a path byte can break sort order; the decode
                // is structural, and re-encoding must catch that.
                Err(e) => {
                    prop_assert_eq!(mutation, 1);
                    prop_assert!(
                        matches!(e, CoreError::EncodeUnsorted),
                        "unexpected re-encode error {e:?}",
                    );
                }
            }
        }
        if mutation != 1 {
            prop_assert!(changeset::decode(&mutated).is_err());
        }
    }

    /// Same fuzz guarantee for trees.
    #[test]
    fn tree_decode_fuzz(
        t in arb_tree(),
        mutation in 0u8..3,
        i in any::<usize>(),
        flip in any::<u8>(),
        garbage in proptest::collection::vec(any::<u8>(), 1..=16),
    ) {
        let bytes = tree::encode(&t).expect("encode valid tree");
        let n = bytes.len();
        let mutated: Vec<u8> = match mutation {
            0 => bytes[..i % n].to_vec(),
            1 => {
                let mut m = bytes.clone();
                let j = i % n;
                m[j] ^= flip | 1;
                m
            }
            _ => {
                let mut m = bytes.clone();
                m.extend_from_slice(&garbage);
                m
            }
        };
        if let Ok(back) = tree::decode(&mutated) {
            match tree::encode(&back) {
                Ok(reencoded) => prop_assert_eq!(reencoded.as_slice(), mutated.as_slice()),
                Err(e) => {
                    prop_assert_eq!(mutation, 1);
                    prop_assert!(
                        matches!(e, CoreError::EncodeUnsorted),
                        "unexpected re-encode error {e:?}",
                    );
                }
            }
        }
        if mutation != 1 {
            prop_assert!(tree::decode(&mutated).is_err());
        }
    }

    /// '\n' and '\t' are legal in author/message; '\r' (Cc) and U+202E (Cf,
    /// right-to-left override) are rejected wherever they appear.
    #[test]
    fn author_message_charset(
        mut cs in arb_changeset(),
        field in 0u8..2,
        pos in any::<usize>(),
        ch in 0usize..SPECIAL_CHARS.len(),
    ) {
        cs.changes.push(FileChange {
            path: "z.txt".into(),
            op: Op::Add,
            blob: Some(object_id(Kind::Blob, b"z")),
            size: Some(1),
            exec: Some(false),
        });
        let target = if field == 0 { &mut cs.author } else { &mut cs.message };
        let ch = SPECIAL_CHARS[ch];
        *target = insert_at(target, pos, ch);
        let result = changeset::validate(&cs);
        match ch {
            '\n' | '\t' => prop_assert!(result.is_ok(), "expected accept, got {result:?}"),
            _ => prop_assert!(
                matches!(
                    result,
                    Err(CoreError::AuthorChar { .. } | CoreError::MessageChar { .. })
                ),
                "expected char rejection, got {result:?}",
            ),
        }
    }
}

const NAMES: &[&str] = &["a", "bc", "src", "docs", "main.rs", "café", "naïve", "x1"];

const TEXT_CHARS: &[&str] = &["a", "B", "9", " ", "<", ">", "@", ".", "é", "ü"];

const SPECIAL_CHARS: &[char] = &['\n', '\t', '\r', '\u{202E}'];

const NORMALIZATION_SEEDS: &[&str] = &[
    "e", "é", "e\u{301}", "u", "ü", "u\u{308}", "ss", "ß", "ﬁ", "i", "x", "a",
];

fn pick_name() -> impl Strategy<Value = &'static str> {
    (0usize..NAMES.len()).prop_map(|i| NAMES[i])
}

fn arb_path() -> impl Strategy<Value = String> {
    proptest::collection::vec(pick_name(), 1..=3).prop_map(|parts| parts.join("/"))
}

fn arb_sorted_paths() -> impl Strategy<Value = Vec<String>> {
    proptest::collection::vec(arb_path(), 0..=8).prop_map(|mut paths| {
        paths.sort();
        paths.dedup();
        paths
    })
}

fn arb_text() -> impl Strategy<Value = String> {
    proptest::collection::vec(0usize..TEXT_CHARS.len(), 0..=24)
        .prop_map(|idxs| idxs.into_iter().map(|i| TEXT_CHARS[i]).collect())
}

fn arb_id() -> impl Strategy<Value = Id> {
    proptest::array::uniform32(any::<u8>()).prop_map(Id)
}

fn arb_changeset() -> impl Strategy<Value = Changeset> {
    (
        any::<u64>(),
        proptest::collection::vec(arb_id(), 0..=2),
        arb_text(),
        arb_text(),
        any::<u64>(),
        arb_sorted_paths(),
    )
        .prop_flat_map(|(repo_id, parents, author, message, timestamp_ns, paths)| {
            let changes = paths
                .into_iter()
                .map(|path| {
                    (Just(path.clone()), 0u8..3, any::<bool>(), any::<u64>()).prop_map(
                        move |(path, op, exec, size)| {
                            let (op, blob, size, exec) = match op {
                                0 => (
                                    Op::Add,
                                    Some(object_id(Kind::Blob, path.as_bytes())),
                                    Some(size),
                                    Some(exec),
                                ),
                                1 => (
                                    Op::Mod,
                                    Some(object_id(Kind::Blob, path.as_bytes())),
                                    Some(size),
                                    Some(exec),
                                ),
                                _ => (Op::Del, None, None, None),
                            };
                            FileChange {
                                path,
                                op,
                                blob,
                                size,
                                exec,
                            }
                        },
                    )
                })
                .collect::<Vec<_>>();
            (
                Just(repo_id),
                Just(parents),
                Just(author),
                Just(message),
                Just(timestamp_ns),
                changes,
            )
                .prop_map(
                    |(repo_id, parents, author, message, timestamp_ns, changes)| Changeset {
                        repo_id,
                        parents,
                        author,
                        message,
                        timestamp_ns,
                        changes,
                    },
                )
        })
}

fn arb_tree() -> impl Strategy<Value = Tree> {
    proptest::collection::vec(pick_name(), 0..=8)
        .prop_map(|mut names| {
            names.sort();
            names.dedup();
            names
        })
        .prop_flat_map(|names| {
            names
                .into_iter()
                .map(|name| {
                    (Just(name), 0u8..2, any::<bool>(), any::<u64>()).prop_map(
                        move |(name, kind, exec, size)| {
                            let kind = if kind == 0 {
                                EntryKind::File
                            } else {
                                EntryKind::Dir
                            };
                            TreeEntry {
                                name: name.to_string(),
                                kind,
                                object: object_id(Kind::Blob, name.as_bytes()),
                                size,
                                exec: if kind == EntryKind::File { exec } else { false },
                            }
                        },
                    )
                })
                .collect::<Vec<_>>()
        })
        .prop_map(|entries| Tree { entries })
}

/// Strings guaranteed to contain normalization-sensitive characters
/// ('é', 'ü', 'ß', 'ﬁ' or combining marks).
fn arb_unicode_text() -> impl Strategy<Value = String> {
    (
        proptest::collection::vec(0usize..NORMALIZATION_SEEDS.len(), 0..=6),
        (0usize..5),
    )
        .prop_map(|(idxs, special)| {
            let mut s: String = idxs.into_iter().map(|i| NORMALIZATION_SEEDS[i]).collect();
            s.push_str(["é", "ü", "ß", "ﬁ", "e\u{301}"][special]);
            s
        })
}

/// Inserts `ch` at the nearest char boundary at or before `pos`.
fn insert_at(s: &str, pos: usize, ch: char) -> String {
    let mut p = pos.min(s.len());
    while !s.is_char_boundary(p) {
        p -= 1;
    }
    let mut out = String::with_capacity(s.len() + ch.len_utf8());
    out.push_str(&s[..p]);
    out.push(ch);
    out.push_str(&s[p..]);
    out
}
