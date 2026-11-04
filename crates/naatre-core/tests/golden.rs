//! Golden fixtures: deterministic objects pinned byte-exactly on disk.
//!
//! Each fixture lives as `tests/golden/<name>.bin` (canonical bytes) and
//! `tests/golden/<name>.json` (fields + hex ids). Normal runs assert that the
//! code-built object encodes to exactly the committed bytes, decodes back
//! equal, and that its id matches the committed JSON. Truncated input must
//! decode to an error, never panic.
//!
//! Regenerate the files after an intentional format change with:
//!
//! ```text
//! NAATRE_UPDATE_GOLDEN=1 cargo test -p naatre-core --test golden
//! ```
//!
//! and review the diff in the same change that alters the format.

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::conflict::{self, ConflictRep};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::{self, EntryKind, Tree, TreeEntry};
use serde_json::{Value, json};
use std::fs;
use std::path::{Path, PathBuf};

fn update_mode() -> bool {
    std::env::var("NAATRE_UPDATE_GOLDEN").is_ok_and(|v| v == "1")
}

fn golden_path(name: &str, ext: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/golden")
        .join(format!("{name}.{ext}"))
}

fn blob_main() -> Id {
    object_id(Kind::Blob, b"fn main() {}\n")
}

fn empty_tree() -> Tree {
    Tree { entries: vec![] }
}

fn changeset_basic() -> Changeset {
    Changeset {
        repo_id: 1,
        parents: vec![],
        author: "Ada <ada@example.com>".into(),
        message: "first".into(),
        timestamp_ns: 1_000_000_001,
        changes: vec![FileChange {
            path: "src/main.rs".into(),
            op: Op::Add,
            blob: Some(blob_main()),
            size: Some(13),
            exec: Some(false),
        }],
        conflicts: vec![],
    }
}

fn changeset_two_parents() -> Changeset {
    Changeset {
        repo_id: 7,
        parents: vec![
            object_id(Kind::Changeset, b"p1"),
            object_id(Kind::Changeset, b"p2"),
        ],
        author: "Bob <bob@example.com>".into(),
        message: "merge-ish\nsecond line".into(),
        timestamp_ns: 2_000_000_002,
        changes: vec![
            FileChange {
                path: "docs/a.txt".into(),
                op: Op::Mod,
                blob: Some(object_id(Kind::Blob, b"docs a\n")),
                size: Some(7),
                exec: Some(false),
            },
            FileChange {
                path: "old.txt".into(),
                op: Op::Del,
                blob: None,
                size: None,
                exec: None,
            },
        ],
        conflicts: vec![],
    }
}

fn changeset_exec_mod() -> Changeset {
    Changeset {
        repo_id: 1,
        parents: vec![],
        author: "Ada <ada@example.com>".into(),
        message: "flip exec".into(),
        timestamp_ns: 3_000_000_003,
        changes: vec![FileChange {
            path: "src/main.rs".into(),
            op: Op::Mod,
            blob: Some(blob_main()),
            size: Some(13),
            exec: Some(true),
        }],
        conflicts: vec![],
    }
}

/// The blob ids a conflict rep references: content-addressed pseudo blobs,
/// the same trick the v1 fixtures use.
fn conflict_blob(content: &[u8]) -> Id {
    object_id(Kind::Blob, content)
}

/// One format-v2 fixture: a changeset carrying a two-side conflict with a
/// common base (the modify/modify shape the recording land mints).
fn changeset_conflict_v2() -> Changeset {
    let sides = {
        let mut s = vec![conflict_blob(b"ours\n"), conflict_blob(b"theirs\n")];
        s.sort_by_key(|id| id.0);
        s
    };
    Changeset {
        repo_id: 1,
        parents: vec![object_id(Kind::Changeset, b"p1")],
        author: "Ada <ada@example.com>".into(),
        message: "conflicted\n".into(),
        timestamp_ns: 4_000_000_004,
        changes: vec![FileChange {
            path: "src/main.rs".into(),
            op: Op::Mod,
            blob: Some(conflict_blob(b"<<<<<<< naatre side 1/2\n")),
            size: Some(24),
            exec: Some(false),
        }],
        conflicts: vec![ConflictRep {
            path: "src/main.rs".into(),
            base: Some(conflict_blob(b"base\n")),
            base_exec: false,
            sides_exec: vec![false; sides.len()],
            sides,
            deleted: false,
        }],
    }
}

/// A >=3-side conflict (the proptest generator's shape, pinned in bytes):
/// three sides, no common base (the add/add composition shape).
fn changeset_conflict_v2_three_sides() -> Changeset {
    let sides = {
        let mut s = vec![
            conflict_blob(b"one\n"),
            conflict_blob(b"two\n"),
            conflict_blob(b"three\n"),
        ];
        s.sort_by_key(|id| id.0);
        s
    };
    Changeset {
        repo_id: 7,
        parents: vec![],
        author: "Bob <bob@example.com>".into(),
        message: "three-way conflict".into(),
        timestamp_ns: 5_000_000_005,
        changes: vec![FileChange {
            path: "notes.txt".into(),
            op: Op::Add,
            blob: Some(conflict_blob(b"<<<<<<< naatre side 1/3\n")),
            size: Some(24),
            exec: Some(false),
        }],
        conflicts: vec![ConflictRep {
            path: "notes.txt".into(),
            base: None,
            base_exec: false,
            sides_exec: vec![false; sides.len()],
            sides,
            deleted: false,
        }],
    }
}

/// The deletion-side rep: one blob side, a common base, the other side
/// deleted the path (the modify/delete shape F2 records).
fn deletion_side_rep() -> ConflictRep {
    ConflictRep {
        path: "src/main.rs".into(),
        base: Some(conflict_blob(b"base\n")),
        base_exec: false,
        sides: vec![conflict_blob(b"ours\n")],
        sides_exec: vec![false],
        deleted: true,
    }
}

/// The exec-bit rep: an executable base, two blob sides (one executable) —
/// the shape of a chmod-vs-edit conflict (flags byte 5).
fn exec_rep() -> ConflictRep {
    let mut pairs = [
        (conflict_blob(b"ours\n"), false),
        (conflict_blob(b"theirs\n"), true),
    ];
    pairs.sort_by_key(|(id, e)| (id.0, *e));
    ConflictRep {
        path: "bin/run.sh".into(),
        base: Some(conflict_blob(b"base\n")),
        base_exec: true,
        sides: pairs.iter().map(|(id, _)| *id).collect(),
        sides_exec: pairs.iter().map(|(_, e)| *e).collect(),
        deleted: false,
    }
}

/// Format v2 with exec bits (flags byte 5).
fn changeset_conflict_v2_exec() -> Changeset {
    Changeset {
        repo_id: 1,
        parents: vec![object_id(Kind::Changeset, b"p1")],
        author: "Ada <ada@example.com>".into(),
        message: "exec conflict".into(),
        timestamp_ns: 7_000_000_007,
        changes: vec![FileChange {
            path: "bin/run.sh".into(),
            op: Op::Mod,
            blob: Some(conflict_blob(b"<<<<<<< naatre side 1/2\n")),
            size: Some(24),
            exec: Some(false),
        }],
        conflicts: vec![exec_rep()],
    }
}

/// Format v2 with a deletion side (flags byte 3).
fn changeset_conflict_v2_deletion_side() -> Changeset {
    Changeset {
        repo_id: 1,
        parents: vec![object_id(Kind::Changeset, b"p1")],
        author: "Ada <ada@example.com>".into(),
        message: "modify/delete conflict".into(),
        timestamp_ns: 6_000_000_006,
        changes: vec![FileChange {
            path: "src/main.rs".into(),
            op: Op::Mod,
            blob: Some(conflict_blob(b"<<<<<<< naatre side 1/2\n")),
            size: Some(24),
            exec: Some(false),
        }],
        conflicts: vec![deletion_side_rep()],
    }
}

fn tree_basic() -> Tree {
    Tree {
        entries: vec![
            TreeEntry {
                name: "main.rs".into(),
                kind: EntryKind::File,
                object: blob_main(),
                size: 13,
                exec: false,
            },
            TreeEntry {
                name: "src".into(),
                kind: EntryKind::Dir,
                object: tree::tree_id(&empty_tree()).expect("empty tree encodes"),
                size: 0,
                exec: false,
            },
        ],
    }
}

fn tree_nested() -> Tree {
    let inner = tree_basic();
    Tree {
        entries: vec![
            TreeEntry {
                name: "a.txt".into(),
                kind: EntryKind::File,
                object: object_id(Kind::Blob, b"a\n"),
                size: 2,
                exec: false,
            },
            TreeEntry {
                name: "sub".into(),
                kind: EntryKind::Dir,
                object: tree::tree_id(&inner).expect("tree-basic encodes"),
                size: tree::encode(&inner).expect("tree-basic encodes").len() as u64,
                exec: false,
            },
        ],
    }
}

fn cs_json(cs: &Changeset, id: &Id) -> Value {
    json!({
        "id": id.to_hex(),
        // The id domain the content selects: v2 iff conflict reps are
        // carried (format v2 — see docs/format.md §8).
        "domain": if cs.conflicts.is_empty() { "v1" } else { "v2" },
        "repo_id": cs.repo_id,
        "parents": cs.parents.iter().map(|p| json!(p.to_hex())).collect::<Vec<_>>(),
        "author": cs.author,
        "message": cs.message,
        "timestamp_ns": cs.timestamp_ns,
        "changes": cs.changes.iter().map(|c| json!({
            "path": c.path,
            "op": match c.op { Op::Add => "add", Op::Mod => "mod", Op::Del => "del" },
            "blob": c.blob.as_ref().map(Id::to_hex),
            "size": c.size,
            "exec": c.exec,
        })).collect::<Vec<_>>(),
        "conflicts": cs.conflicts.iter().map(|r| {
            let mut rep = json!({
                "path": r.path,
                "base": r.base.as_ref().map(Id::to_hex),
                "sides": r.sides.iter().map(Id::to_hex).collect::<Vec<_>>(),
            });
            // Exec bits are listed only when some bit is set (the pre-F2
            // fixture files stay byte-identical).
            if r.base_exec || r.sides_exec.iter().any(|&e| e) {
                rep["base_exec"] = json!(r.base_exec);
                rep["sides_exec"] = json!(r.sides_exec);
            }
            // Only a deletion side adds the key, so the pre-F2 fixture
            // files stay byte-identical.
            if r.deleted {
                rep["deleted"] = json!(true);
            }
            rep
        }).collect::<Vec<_>>(),
    })
}

fn tree_json(t: &Tree, id: &Id) -> Value {
    json!({
        "id": id.to_hex(),
        "entries": t.entries.iter().map(|e| json!({
            "name": e.name,
            "kind": match e.kind { EntryKind::File => "file", EntryKind::Dir => "dir" },
            "object": e.object.to_hex(),
            "size": e.size,
            "exec": e.exec,
        })).collect::<Vec<_>>(),
    })
}

fn check_bytes(name: &str, canonical: &[u8], json: Value, decode_fn: &dyn Fn(&[u8]) -> bool) {
    let bin_path = golden_path(name, "bin");
    let json_path = golden_path(name, "json");
    if update_mode() {
        fs::create_dir_all(bin_path.parent().unwrap()).unwrap();
        fs::write(&bin_path, canonical).unwrap();
        fs::write(
            &json_path,
            format!("{}\n", serde_json::to_string_pretty(&json).unwrap()),
        )
        .unwrap();
        return;
    }
    let disk_bin = fs::read(&bin_path).unwrap_or_else(|e| panic!("read {name}.bin: {e}"));
    assert_eq!(
        canonical, disk_bin,
        "{name}: canonical bytes diverged from {name}.bin"
    );
    let disk_json: Value = serde_json::from_str(
        &fs::read_to_string(&json_path).unwrap_or_else(|e| panic!("read {name}.json: {e}")),
    )
    .unwrap_or_else(|e| panic!("{name}.json is not valid JSON: {e}"));
    assert_eq!(
        json, disk_json,
        "{name}: JSON fixture diverged from {name}.json"
    );

    // decode(.bin) equals the built object and re-encodes to identical bytes
    // (asserted by the callers via PartialEq on the decoded value); here we
    // additionally require that a truncated encoding errors instead of
    // panicking.
    let half = &disk_bin[..disk_bin.len() / 2];
    assert!(
        decode_fn(half),
        "{name}: truncated decode must error, not panic"
    );
}

#[test]
fn changeset_basic_fixture() {
    let cs = changeset_basic();
    let bytes = changeset::encode(&cs).expect("encode");
    let id = changeset::changeset_id(&cs).expect("id");
    check_bytes("changeset-basic", &bytes, cs_json(&cs, &id), &|b| {
        changeset::decode(b).is_err()
    });
    assert_eq!(changeset::decode(&bytes).as_ref(), Ok(&cs));
}

#[test]
fn changeset_two_parents_fixture() {
    let cs = changeset_two_parents();
    let bytes = changeset::encode(&cs).expect("encode");
    let id = changeset::changeset_id(&cs).expect("id");
    check_bytes("changeset-two-parents", &bytes, cs_json(&cs, &id), &|b| {
        changeset::decode(b).is_err()
    });
    assert_eq!(changeset::decode(&bytes).as_ref(), Ok(&cs));
}

#[test]
fn changeset_exec_mod_fixture() {
    let cs = changeset_exec_mod();
    let bytes = changeset::encode(&cs).expect("encode");
    let id = changeset::changeset_id(&cs).expect("id");
    check_bytes("changeset-exec-mod", &bytes, cs_json(&cs, &id), &|b| {
        changeset::decode(b).is_err()
    });
    assert_eq!(changeset::decode(&bytes).as_ref(), Ok(&cs));
}

/// Format v2: a conflicted changeset's bytes and its v2-domain id. The v1
/// fixtures above stay byte-identical — v1 ids are never recomputed.
#[test]
fn changeset_conflict_v2_fixture() {
    let cs = changeset_conflict_v2();
    let bytes = changeset::encode(&cs).expect("encode");
    let id = changeset::changeset_id(&cs).expect("id");
    // The v2 domain: a different id than the v1 hash of the same bytes.
    assert_ne!(id, object_id(Kind::Changeset, &bytes));
    assert_eq!(id, naatre_core::ids::object_id_v2(Kind::Changeset, &bytes));
    check_bytes("changeset-conflict-v2", &bytes, cs_json(&cs, &id), &|b| {
        changeset::decode(b).is_err()
    });
    assert_eq!(changeset::decode(&bytes).as_ref(), Ok(&cs));
}

#[test]
fn changeset_conflict_v2_three_sides_fixture() {
    let cs = changeset_conflict_v2_three_sides();
    let bytes = changeset::encode(&cs).expect("encode");
    let id = changeset::changeset_id(&cs).expect("id");
    assert_eq!(id, naatre_core::ids::object_id_v2(Kind::Changeset, &bytes));
    check_bytes(
        "changeset-conflict-v2-three-sides",
        &bytes,
        cs_json(&cs, &id),
        &|b| changeset::decode(b).is_err(),
    );
    assert_eq!(changeset::decode(&bytes).as_ref(), Ok(&cs));
}

/// P1: the deletion-side encoding is pinned in bytes (flags byte 3) and
/// the rendering equals the committed text byte for byte. The pre-F2 v2
/// fixtures above are untouched.
#[test]
fn marker_bytes_deletion_side_golden() {
    let cs = changeset_conflict_v2_deletion_side();
    let bytes = changeset::encode(&cs).expect("encode");
    let id = changeset::changeset_id(&cs).expect("id");
    assert_eq!(id, naatre_core::ids::object_id_v2(Kind::Changeset, &bytes));
    check_bytes(
        "changeset-conflict-v2-deletion-side",
        &bytes,
        cs_json(&cs, &id),
        &|b| changeset::decode(b).is_err(),
    );
    assert_eq!(changeset::decode(&bytes).as_ref(), Ok(&cs));

    let rep = deletion_side_rep();
    let rendered = conflict::marker_bytes(&rep, |id| {
        [b"ours\n".as_slice(), b"base\n".as_slice()]
            .into_iter()
            .find(|c| conflict_blob(c) == *id)
            .map(<[u8]>::to_vec)
            .ok_or_else(|| "unknown blob".to_string())
    })
    .expect("render");
    assert!(conflict::marker_shaped(&rendered));
    let path = golden_path("conflict-markers-deletion-side", "txt");
    if update_mode() {
        fs::write(&path, &rendered).unwrap();
        return;
    }
    let disk = fs::read(&path).unwrap_or_else(|e| panic!("read the rendering golden: {e}"));
    assert_eq!(rendered, disk, "the deletion-side rendering diverged");
}

/// The exec-bit encoding (flags bit 2: an exec byte after the base id and
/// after each side id) and its rendering (` +x` after the blob id) are
/// pinned in bytes; the pre-F2 fixtures are untouched.
#[test]
fn marker_bytes_exec_golden() {
    let cs = changeset_conflict_v2_exec();
    let bytes = changeset::encode(&cs).expect("encode");
    let id = changeset::changeset_id(&cs).expect("id");
    assert_eq!(id, naatre_core::ids::object_id_v2(Kind::Changeset, &bytes));
    check_bytes(
        "changeset-conflict-v2-exec",
        &bytes,
        cs_json(&cs, &id),
        &|b| changeset::decode(b).is_err(),
    );
    assert_eq!(changeset::decode(&bytes).as_ref(), Ok(&cs));

    let rep = exec_rep();
    let rendered = conflict::marker_bytes(&rep, |id| {
        [
            b"ours\n".as_slice(),
            b"theirs\n".as_slice(),
            b"base\n".as_slice(),
        ]
        .into_iter()
        .find(|c| conflict_blob(c) == *id)
        .map(<[u8]>::to_vec)
        .ok_or_else(|| "unknown blob".to_string())
    })
    .expect("render");
    assert!(conflict::marker_shaped(&rendered));
    assert!(conflict::marker_opener(
        &rendered[..rendered.len().min(conflict::MARKER_PREFIX_BYTES)]
    ));
    let path = golden_path("conflict-markers-exec", "txt");
    if update_mode() {
        fs::write(&path, &rendered).unwrap();
        return;
    }
    let disk = fs::read(&path).unwrap_or_else(|e| panic!("read the exec rendering golden: {e}"));
    assert_eq!(rendered, disk, "the exec rendering diverged");
}

#[test]
fn tree_basic_fixture() {
    let t = tree_basic();
    let bytes = tree::encode(&t).expect("encode");
    let id = tree::tree_id(&t).expect("id");
    check_bytes("tree-basic", &bytes, tree_json(&t, &id), &|b| {
        tree::decode(b).is_err()
    });
    assert_eq!(tree::decode(&bytes).as_ref(), Ok(&t));
}

#[test]
fn tree_nested_fixture() {
    let t = tree_nested();
    let bytes = tree::encode(&t).expect("encode");
    let id = tree::tree_id(&t).expect("id");
    check_bytes("tree-nested", &bytes, tree_json(&t, &id), &|b| {
        tree::decode(b).is_err()
    });
    assert_eq!(tree::decode(&bytes).as_ref(), Ok(&t));
}

/// LIMIT, stated: this pin proves only that the v2 frame magic differs
/// from v1's and that a v1-guard EMULATION refuses the frame — it does
/// NOT cover the schema guard. The actual old-binary lockout is
/// store-level at open (migration 012 bumps every store this binary
/// touches to schema_version 12, so a pre-M12 binary's `migrate` refuses
/// the whole store before reading any row — pinned store-side as
/// `future_by_one_refuses_at_open_the_11_binary_12_store_path` in
/// naatre-store's integration suite). The magic check here is
/// defense-in-depth for bytes that would get past that guard.
#[test]
fn v2_frames_are_refused_by_the_v1_magic_guard() {
    const MAGIC_V1: &[u8] = b"naatre-changeset\x01";
    // The guard a pre-M12 binary ships: magic equality, then decode.
    let v1_guard =
        |frame: &[u8]| frame.len() >= MAGIC_V1.len() && &frame[..MAGIC_V1.len()] == MAGIC_V1;

    // The committed golden frames; in update mode the builders stand in
    // (the files may not exist yet).
    let frame = |name: &str, build: fn() -> Changeset| -> Vec<u8> {
        if update_mode() {
            changeset::encode(&build()).expect("encode")
        } else {
            fs::read(golden_path(name, "bin")).expect("read the committed frame")
        }
    };

    let v2 = frame("changeset-conflict-v2", changeset_conflict_v2);
    assert!(!v1_guard(&v2), "the v2 frame must fail the v1 magic guard");
    // The refusal is exactly the version byte (the v1 decoder's bad-magic
    // arm): the parser below the guard never sees the frame's body, so a
    // misparse is unreachable for old binaries.
    assert_eq!(v2[MAGIC_V1.len() - 1], 0x02, "v2 magic version byte");

    let v2b = frame(
        "changeset-conflict-v2-three-sides",
        changeset_conflict_v2_three_sides,
    );
    assert!(!v1_guard(&v2b), "the >=3-side v2 frame must fail the guard");
    assert_eq!(v2b[MAGIC_V1.len() - 1], 0x02);

    let v1 = frame("changeset-basic", changeset_basic);
    assert!(v1_guard(&v1), "a genuine v1 frame passes the same guard");
    assert_eq!(v1[MAGIC_V1.len() - 1], 0x01);
    // Then the real decode, for the frame the guard admits.
    assert!(changeset::decode(&v1).is_ok());
}
