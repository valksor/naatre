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
