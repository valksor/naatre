//! M4a suite: the merge primitives (table + the two plan proptests) and
//! land end-to-end — happy land, rebase with the exact-content oracle,
//! conflict 409 + recipe + same-key re-land, key lifecycle (settled /
//! writer_busy contention), containment, scope gating, rebase across an
//! imported 2-parent merge node, and the crash-point smoke.
//!
//! `main` is seeded directly (the way land leaves it) because the refs
//! route refuses it — land is its only writer.

use std::path::PathBuf;
use std::process::Command;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use proptest::prelude::*;
use rusqlite::Connection;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::merge::{MergeError, MergeOutcome, PathConflict, merge_lines, merge_trees};
use naatre_server::state::{AppState, ServerState};
use naatre_server::tree_derive::MemNode;
use naatre_store::DataDir;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m4a";

// ---- harness ---------------------------------------------------------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
    /// Kept for the write-slot hook (`hold_write_txn`).
    state: AppState,
}

fn build_app() -> TestApp {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let dir = tempfile::tempdir().unwrap();
    let data = DataDir::create(dir.path()).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(dir.path()).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    let db_path = dir.path().join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    TestApp {
        _dir: dir,
        app,
        db_path,
        state,
    }
}

impl TestApp {
    fn db(&self) -> Connection {
        let conn = Connection::open(&self.db_path).unwrap();
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        conn
    }

    fn seed_token(&self, principal: &str, role: &str, scope_path: Option<&str>) -> String {
        let (token, hash) = naatre_server::auth::mint_token();
        let conn = self.db();
        conn.execute(
            "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3) \
             ON CONFLICT(name) DO UPDATE SET role = excluded.role",
            rusqlite::params![principal, role, BASE_NS as i64],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO tokens(principal, blake3, expires_ns, revoked, scope_path) \
             VALUES (?1, ?2, ?3, 0, ?4)",
            rusqlite::params![principal, hash, Option::<i64>::None, scope_path],
        )
        .unwrap();
        token
    }

    fn seed_repo(&self, name: &str) -> i64 {
        let conn = self.db();
        conn.execute(
            "INSERT INTO repos(name, created_ns) VALUES (?1, ?2)",
            rusqlite::params![name, BASE_NS as i64],
        )
        .unwrap();
        conn.last_insert_rowid()
    }

    /// Seeds the `main` ref the way land leaves it (version 1).
    fn seed_main(&self, repo_id: i64, target_hex: &str) {
        let target = Id::from_hex(target_hex).unwrap();
        let conn = self.db();
        conn.execute(
            "INSERT INTO refs(repo_id, name, target, version, last_principal) \
             VALUES (?1, 'main', ?2, 1, 'seed') \
             ON CONFLICT(repo_id, name) DO UPDATE SET target = excluded.target",
            rusqlite::params![repo_id, target.0.as_slice()],
        )
        .unwrap();
    }

    /// Moves main without a version bump (test scaffolding for "main
    /// advanced while we worked").
    fn move_main(&self, repo_id: i64, target_hex: &str) {
        let target = Id::from_hex(target_hex).unwrap();
        let conn = self.db();
        conn.execute(
            "UPDATE refs SET target = ?2 WHERE repo_id = ?1 AND name = 'main'",
            rusqlite::params![repo_id, target.0.as_slice()],
        )
        .unwrap();
    }

    fn count(&self, sql: &str) -> i64 {
        self.db()
            .query_row(sql, [], |r| r.get(0))
            .expect("count query")
    }

    /// Direct-DB seed of an imported-style 2-parent changeset: canonical
    /// bytes from core, tree derived from `base_tree` + the delta, all rows
    /// in place (changesets, 2 parents, file_changes, trees, repo_blobs).
    #[allow(clippy::too_many_arguments)] // direct-DB seeding fixture
    fn seed_merge_node(
        &self,
        repo_id: i64,
        parents: [&str; 2],
        base_tree_hex: &str,
        delta: &FileChange,
        author: &str,
        message: &str,
        generation: i64,
    ) -> String {
        let cs = Changeset {
            repo_id: repo_id as u64,
            parents: parents.iter().map(|h| Id::from_hex(h).unwrap()).collect(),
            author: author.into(),
            message: message.into(),
            timestamp_ns: BASE_NS + 7,
            changes: vec![delta.clone()],
            conflicts: Vec::new(),
        };
        let canonical = changeset::encode(&cs).unwrap();
        let id = object_id(Kind::Changeset, &canonical);
        let base_tree = Id::from_hex(base_tree_hex).unwrap();
        let node = {
            let conn = self.db();
            let mut node = MemNode::load(&naatre_store::Db::Sqlite(conn), Some(base_tree)).unwrap();
            node.apply(std::slice::from_ref(delta)).unwrap();
            node
        };
        let (root, _, trees) = node.collect().unwrap();

        let conn = self.db();
        conn.execute(
            "INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, \
             generation, principal, root_tree_id) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
            rusqlite::params![
                repo_id,
                id.0.as_slice(),
                canonical,
                cs.author,
                cs.message,
                cs.timestamp_ns as i64,
                generation,
                "git-import",
                root.0.as_slice(),
            ],
        )
        .unwrap();
        for (ord, p) in cs.parents.iter().enumerate() {
            conn.execute(
                "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, ?4)",
                rusqlite::params![repo_id, id.0.as_slice(), p.0.as_slice(), ord as i64],
            )
            .unwrap();
        }
        conn.execute(
            "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            rusqlite::params![
                repo_id,
                id.0.as_slice(),
                delta.path,
                1i64, // Mod
                delta.blob.unwrap().0.as_slice(),
                delta.size.unwrap() as i64,
                0i64,
            ],
        )
        .unwrap();
        conn.execute(
            "INSERT OR IGNORE INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)",
            rusqlite::params![repo_id, delta.blob.unwrap().0.as_slice()],
        )
        .unwrap();
        for t in &trees {
            conn.execute(
                "INSERT OR REPLACE INTO trees(id, repo_id, canonical) VALUES (?1, ?2, ?3)",
                rusqlite::params![t.id.0.as_slice(), repo_id, t.canonical],
            )
            .unwrap();
            for e in &t.entries {
                conn.execute(
"INSERT OR REPLACE INTO tree_entries(tree_id, name, kind, object_id, size, exec) \
                     VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
                    rusqlite::params![
                        t.id.0.as_slice(),
                        e.name,
                        e.kind,
                        e.object_id,
                        e.size,
                        e.exec as i64,
                    ],
                )
                .unwrap();
            }
        }
        id.to_hex()
    }
}

struct Resp {
    status: StatusCode,
    body: Value,
}

async fn send(
    app: &Router,
    method: &str,
    uri: &str,
    token: Option<&str>,
    body: Option<Value>,
) -> Resp {
    let mut builder = axum::http::Request::builder().method(method).uri(uri);
    if let Some(t) = token {
        builder = builder.header(header::AUTHORIZATION, format!("Bearer {t}"));
    }
    let req = builder
        .header(
            header::CONTENT_TYPE,
            if body.is_some() {
                "application/json"
            } else {
                "application/octet-stream"
            },
        )
        .body(match body {
            Some(v) => Body::from(v.to_string()),
            None => Body::empty(),
        })
        .unwrap();
    let res = app.clone().oneshot(req).await.expect("in-memory request");
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body())
        .await
        .expect("body")
        .to_bytes()
        .to_vec();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Resp { status, body }
}

async fn get(app: &Router, uri: &str, token: &str) -> Resp {
    send(app, "GET", uri, Some(token), None).await
}

async fn post_json(app: &Router, uri: &str, token: &str, value: &Value) -> Resp {
    send(app, "POST", uri, Some(token), Some(value.clone())).await
}

fn code_of(r: &Resp) -> String {
    r.body["error"]["code"].as_str().unwrap_or("").to_string()
}

fn blob_hex(content: &[u8]) -> String {
    object_id(Kind::Blob, content).to_hex()
}

/// The shared fixture: app + admin/writer/reader tokens + repo "m4a".
struct Fixture {
    t: TestApp,
    writer: String,
    reader: String,
    repo_id: i64,
}

fn fixture() -> Fixture {
    let t = build_app();
    let writer = t.seed_token("writer-p", "writer", None);
    let reader = t.seed_token("reader-p", "reader", None);
    let repo_id = t.seed_repo(REPO);
    Fixture {
        t,
        writer,
        reader,
        repo_id,
    }
}

/// Uploads one blob through the raw PUT, returning its hex id.
async fn put_blob(f: &Fixture, content: &[u8]) -> String {
    let id = blob_hex(content);
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{id}"))
        .header(header::AUTHORIZATION, format!("Bearer {}", f.writer))
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .body(Body::from(content.to_vec()))
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert!(
        res.status().is_success(),
        "blob PUT failed: {}",
        res.status()
    );
    id
}

/// Creates a changeset, asserting success, returning (id, root_tree_id).
async fn create_ok(f: &Fixture, body: Value) -> (String, String) {
    let resp = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &body,
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "body: {}", resp.body);
    (
        resp.body["id"].as_str().unwrap().to_string(),
        resp.body["root_tree_id"].as_str().unwrap().to_string(),
    )
}

fn add(path: &str, blob: &str, size: usize) -> Value {
    json!({ "path": path, "op": "add", "blob": blob, "size": size, "exec": false })
}

fn mod_(path: &str, blob: &str, size: usize) -> Value {
    json!({ "path": path, "op": "mod", "blob": blob, "size": size, "exec": false })
}

fn cs_body(parents: Value, changes: Value, message: &str) -> Value {
    json!({
        "parents": parents,
        "author": "Agent <agent@naatre.dev>",
        "message": message,
        "changes": changes,
    })
}

async fn land(f: &Fixture, head: &str, key: &str) -> Resp {
    post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land"),
        &f.writer,
        &json!({ "head": head, "key": key }),
    )
    .await
}

/// Fetches raw blob bytes at `at`/`path` through /file + /objects — the
/// content oracle's read path.
async fn cat(f: &Fixture, at: &str, path: &str) -> Vec<u8> {
    let resp = get(
        &f.t.app,
        &format!(
            "/v1/repos/{REPO}/file?at={at}&path={}",
            path.replace('/', "%2F")
        ),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "cat {path}: {}", resp.body);
    let blob = resp.body["blob_id"].as_str().unwrap().to_string();
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/{REPO}/objects/{blob}"))
        .header(header::AUTHORIZATION, format!("Bearer {}", f.reader))
        .body(Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert!(res.status().is_success());
    BodyExt::collect(res.into_body())
        .await
        .unwrap()
        .to_bytes()
        .to_vec()
}

/// The m2a derivation oracle: replay every changeset's delta from an empty
/// tree and compare the canonical root with the head's root_tree_id.
async fn replay_root_equals(f: &Fixture, chain: &[String]) -> String {
    let mut node = MemNode::empty();
    for cs in chain {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/{REPO}/changesets/{cs}"),
            &f.reader,
        )
        .await;
        let changes: Vec<FileChange> = resp.body["changes"]
            .as_array()
            .unwrap()
            .iter()
            .map(|c| FileChange {
                path: c["path"].as_str().unwrap().to_string(),
                op: match c["op"].as_str().unwrap() {
                    "add" => Op::Add,
                    "mod" => Op::Mod,
                    _ => Op::Del,
                },
                blob: c["blob"].as_str().map(|h| Id::from_hex(h).unwrap()),
                size: c["size"].as_u64(),
                exec: c["exec"].as_bool(),
            })
            .collect();
        node.apply(&changes).unwrap();
    }
    let (root, _, _) = node.collect().unwrap();
    root.to_hex()
}

// ---- 1. merge primitives: tables -------------------------------------------

#[test]
fn merge_lines_table() {
    let one_side = |b: &[u8], o: &[u8], t: &[u8]| match merge_lines(b, o, t) {
        MergeOutcome::Merged(v) => v,
        MergeOutcome::Conflict => panic!("expected merged"),
    };
    let conflict = |b: &[u8], o: &[u8], t: &[u8]| {
        assert_eq!(merge_lines(b, o, t), MergeOutcome::Conflict);
    };

    // Both sides made the same edit.
    assert_eq!(
        one_side(b"a\nb\n", b"X\nb\n", b"X\nb\n"),
        b"X\nb\n".to_vec()
    );
    // One-sided edits take the changed side whole (no merge, so binary
    // content is fine here).
    assert_eq!(
        one_side(b"a\nb\n", b"a\nb\n", b"a\nB\nbinary\xff\n"),
        b"a\nB\nbinary\xff\n".to_vec()
    );
    assert_eq!(
        one_side(b"a\nb\n", b"A\nb\n", b"a\nb\n"),
        b"A\nb\n".to_vec()
    );

    // Disjoint hunks (different regions) merge to the union.
    assert_eq!(
        one_side(
            b"a\nb\nc\nd\ne\n",
            b"A\nb\nc\nd\ne\n", // line 1
            b"a\nb\nc\nD\ne\n", // line 4
        ),
        b"A\nb\nc\nD\ne\n".to_vec()
    );
    // Adjacent regions (touching lines) still merge: line 1 edit + insertion
    // before line 2.
    assert_eq!(
        one_side(b"a\nb\nc\n", b"A\nb\nc\n", b"a\nNEW\nb\nc\n",),
        b"A\nNEW\nb\nc\n".to_vec()
    );
    // Identical same-point insertions from both sides apply once.
    assert_eq!(
        one_side(b"a\nb\nc\n", b"a\nX\nb\nc\n", b"a\nX\nb\nc\n",),
        b"a\nX\nb\nc\n".to_vec()
    );
    // Overlapping hunks conflict (lines 2-3 vs lines 3-4).
    conflict(
        b"l1\nl2\nl3\nl4\nl5\n",
        b"l1\nO2\nO3\nl4\nl5\n",
        b"l1\nl2\nT3\nT4\nl5\n",
    );
    // Same-line divergent edits conflict.
    conflict(b"l1\nl2\n", b"O\nl2\n", b"T\nl2\n");
    // Both-modified binary content conflicts (never a garbage merge).
    conflict(b"text\n", b"\xff\xfe\x01\n", b"\x00\x01\x02\n");
}

/// The path conflicts of a refused merge (a fetch failure is not one).
fn conflicts_of(e: MergeError) -> Vec<PathConflict> {
    match e {
        MergeError::Conflicts(c) => c,
        MergeError::Fetch(_, m) => panic!("expected path conflicts, got a fetch failure: {m}"),
    }
}

#[test]
fn merge_trees_table() {
    use naatre_server::merge::flatten_files;

    let blob = |c: &[u8]| object_id(Kind::Blob, c);
    // Node from add-changes; fetcher resolves from a fixed table.
    let node = |files: &[(&str, &[u8], bool)]| -> MemNode {
        let mut n = MemNode::empty();
        let adds: Vec<FileChange> = files
            .iter()
            .map(|(p, c, e)| FileChange {
                path: (*p).into(),
                op: Op::Add,
                blob: Some(blob(c)),
                size: Some(c.len() as u64),
                exec: Some(*e),
            })
            .collect();
        n.apply(&adds).unwrap();
        n
    };
    let table: Vec<(Id, Vec<u8>)> = [
        b"A\n".to_vec(),
        b"A-mine\n".to_vec(),
        b"A-theirs\n".to_vec(),
    ]
    .into_iter()
    .map(|c| (blob(&c), c))
    .collect();
    let fetch = move |id: &Id| -> Result<Vec<u8>, String> {
        table
            .iter()
            .find(|(i, _)| i == id)
            .map(|(_, c)| c.clone())
            .ok_or_else(|| "missing".into())
    };

    // add/add same → ok.
    let base = node(&[]);
    let ours = node(&[("f", b"A\n", false)]);
    let theirs = node(&[("f", b"A\n", false)]);
    let merged = merge_trees(&base, &ours, &theirs, &fetch).unwrap();
    assert_eq!(flatten_files(&merged.node)["f"].blob, blob(b"A\n"));

    // add/add different → conflict.
    let theirs = node(&[("f", b"A-theirs\n", false)]);
    let errs = conflicts_of(merge_trees(&base, &ours, &theirs, &fetch).unwrap_err());
    assert_eq!(errs.len(), 1);
    assert_eq!(errs[0].path, "f");
    assert_eq!(errs[0].ours_blob, Some(blob(b"A\n")));
    assert_eq!(errs[0].theirs_blob, Some(blob(b"A-theirs\n")));

    // del/mod → conflict.
    let base = node(&[("f", b"A\n", false)]);
    let ours = node(&[]);
    let theirs = node(&[("f", b"A-theirs\n", false)]);
    let errs = conflicts_of(merge_trees(&base, &ours, &theirs, &fetch).unwrap_err());
    assert_eq!(errs[0].path, "f");
    assert!(errs[0].reason.contains("deleted by one side"));

    // Exec flips merge when the content agrees; an exec flip on one side
    // wins when the other side only changed content. (A pure exec-only
    // both-diverged conflict cannot exist with a single exec bit — the
    // nearest real conflict is an exec flip combined with a divergent
    // content edit, covered by the next case.)
    let base = node(&[("f", b"A\n", false)]);
    let ours = node(&[("f", b"A\n", true)]); // exec-only flip
    let theirs = node(&[("f", b"A-theirs\n", false)]); // content-only change
    let merged = merge_trees(&base, &ours, &theirs, &fetch).unwrap();
    let f_meta = flatten_files(&merged.node)["f"];
    assert_eq!(f_meta.blob, blob(b"A-theirs\n"));
    assert!(f_meta.exec, "the flipped exec bit wins");

    // Exec flip + divergent content edit on both sides → conflict.
    let table2: Vec<(Id, Vec<u8>)> = [
        b"l1\nl2\n".to_vec(),
        b"O1\nl2\n".to_vec(),
        b"T1\nl2\n".to_vec(),
    ]
    .into_iter()
    .map(|c| (blob(&c), c))
    .collect();
    let fetch2 = move |id: &Id| -> Result<Vec<u8>, String> {
        table2
            .iter()
            .find(|(i, _)| i == id)
            .map(|(_, c)| c.clone())
            .ok_or_else(|| "missing".into())
    };
    let base = node(&[("f", b"l1\nl2\n", false)]);
    let ours = node(&[("f", b"O1\nl2\n", true)]);
    let theirs = node(&[("f", b"T1\nl2\n", false)]);
    let errs = conflicts_of(merge_trees(&base, &ours, &theirs, fetch2).unwrap_err());
    assert_eq!(errs[0].path, "f");
    assert!(errs[0].reason.contains("line merge conflict"));

    // One-sided del + no touch → del.
    let base = node(&[("gone", b"A\n", false), ("kept", b"A\n", false)]);
    let ours = node(&[("kept", b"A\n", false)]);
    let theirs = node(&[("gone", b"A\n", false), ("kept", b"A\n", false)]);
    let merged = merge_trees(&base, &ours, &theirs, &fetch).unwrap();
    let files = flatten_files(&merged.node);
    assert!(!files.contains_key("gone"));
    assert!(files.contains_key("kept"));

    // Line-merge inside a tree: disjoint edits of one file produce a NEW
    // blob carrying the union, reported in new_blobs.
    let base = node(&[("f", b"l1\nl2\nl3\nl4\n", false)]);
    let ours = node(&[("f", b"O1\nl2\nl3\nl4\n", false)]);
    let theirs = node(&[("f", b"l1\nl2\nl3\nT4\n", false)]);
    let table3 = [
        (blob(b"l1\nl2\nl3\nl4\n"), b"l1\nl2\nl3\nl4\n".to_vec()),
        (blob(b"O1\nl2\nl3\nl4\n"), b"O1\nl2\nl3\nl4\n".to_vec()),
        (blob(b"l1\nl2\nl3\nT4\n"), b"l1\nl2\nl3\nT4\n".to_vec()),
    ];
    let merged = merge_trees(&base, &ours, &theirs, move |id| {
        table3
            .iter()
            .find(|(i, _)| i == id)
            .map(|(_, c)| c.clone())
            .ok_or_else(|| "missing".into())
    })
    .unwrap();
    let expected = b"O1\nl2\nl3\nT4\n".to_vec();
    assert_eq!(flatten_files(&merged.node)["f"].blob, blob(&expected));
    assert_eq!(merged.new_blobs.len(), 1);
    assert_eq!(merged.new_blobs[0], (blob(&expected), expected));
}

// ---- 2. merge properties ----------------------------------------------------

fn join_lines(lines: &[String]) -> String {
    if lines.is_empty() {
        return String::new();
    }
    let mut out = lines.join("\n");
    out.push('\n');
    out
}

fn splice(base_lines: &[String], at: usize, len: usize, lines: &[String]) -> String {
    let mut all: Vec<String> = Vec::new();
    all.extend_from_slice(&base_lines[..at]);
    all.extend_from_slice(lines);
    all.extend_from_slice(&base_lines[at + len..]);
    join_lines(&all)
}

proptest! {
    /// Property (a), plan "Merge-content oracle": random disjoint line
    /// edits to different regions merge to the exact union.
    #[test]
    fn disjoint_edits_merge_to_the_union(
        n in 12usize..40,
        sa in 0usize..4,
        la in 1usize..3usize,
        gap in 1usize..4usize,
        lt in 1usize..3usize,
        ra in 0usize..3usize,
        rt in 0usize..3usize,
    ) {
        let base_lines: Vec<String> = (0..n).map(|i| format!("L{i}")).collect();
        let sb = sa + la + gap; // theirs' region starts strictly after ours'
        prop_assert!(sb + lt <= n);

        let ours_lines: Vec<String> = (0..ra).map(|i| format!("OA{i}")).collect();
        let theirs_lines: Vec<String> = (0..rt).map(|i| format!("OT{i}")).collect();

        let base = join_lines(&base_lines);
        let ours = splice(&base_lines, sa, la, &ours_lines);
        let theirs = splice(&base_lines, sb, lt, &theirs_lines);

        // The exact union: both regions replaced in place (single pass over
        // the ORIGINAL base lines — indexes never shift).
        let mut exp: Vec<String> = Vec::new();
        exp.extend_from_slice(&base_lines[..sa]);
        exp.extend_from_slice(&ours_lines);
        exp.extend_from_slice(&base_lines[sa + la..sb]);
        exp.extend_from_slice(&theirs_lines);
        exp.extend_from_slice(&base_lines[sb + lt..]);
        let expected = join_lines(&exp);

        match merge_lines(base.as_bytes(), ours.as_bytes(), theirs.as_bytes()) {
            MergeOutcome::Merged(v) => prop_assert_eq!(v, expected.into_bytes()),
            MergeOutcome::Conflict => panic!("disjoint edits must merge"),
        }
    }

    /// Property (b): an identical edit on both sides is a no-op (the merged
    /// result equals the edit), never a conflict.
    #[test]
    fn identical_edit_is_a_no_op(
        n in 0usize..20,
        at in 0usize..6,
        len in 0usize..4usize,
        r in 0usize..3usize,
    ) {
        let base_lines: Vec<String> = (0..n).map(|i| format!("L{i}")).collect();
        let at = at.min(n);
        let len = len.min(n - at);
        let edit_lines: Vec<String> = (0..r).map(|i| format!("E{i}")).collect();
        let base = join_lines(&base_lines);
        let edited = splice(&base_lines, at, len, &edit_lines);
        match merge_lines(base.as_bytes(), edited.as_bytes(), edited.as_bytes()) {
            MergeOutcome::Merged(v) => prop_assert_eq!(v, edited.into_bytes()),
            MergeOutcome::Conflict => panic!("identical edits must not conflict"),
        }
    }
}

// ---- 3. happy land ----------------------------------------------------------

#[tokio::test]
async fn happy_land_replays_and_moves_main() {
    let f = fixture();
    let b1 = put_blob(&f, b"alpha\n").await;
    let (c1, _) = create_ok(&f, cs_body(json!([]), json!([add("a.txt", &b1, 6)]), "c1")).await;
    f.t.seed_main(f.repo_id, &c1);
    let b2 = put_blob(&f, b"beta\n").await;
    let (c2, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("b.txt", &b2, 5)]), "c2"),
    )
    .await;

    let cs_before = f.t.count("SELECT COUNT(*) FROM changesets");
    let resp = land(&f, &c2, "k-happy").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(resp.body["landed"], c2.as_str());
    assert_eq!(resp.body["main"], c2.as_str());
    assert_eq!(resp.body["rebased"], 0);
    assert_eq!(resp.body["note"], Value::Null);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), cs_before);

    // main moved, key done, audited exactly once.
    let resp = get(&f.t.app, &format!("/v1/repos/{REPO}/refs/main"), &f.reader).await;
    assert_eq!(resp.body["target"], c2.as_str());
    assert_eq!(resp.body["version"], 2);
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM lands WHERE state = 'done'"),
        1
    );
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM audit_log WHERE action = 'land'"),
        1
    );

    // Replay: same key + same head → the recorded result, no second audit.
    let resp = land(&f, &c2, "k-happy").await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["landed"], c2.as_str());
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM audit_log WHERE action = 'land'"),
        1
    );

    // Status read by key (own principal).
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land/k-happy"),
        &f.writer,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["state"], "done");
    assert_eq!(resp.body["head"], c2.as_str());
    assert_eq!(resp.body["result"]["landed"], c2.as_str());
    assert_eq!(resp.body["result"]["rebased"], 0);

    // Reader: 403 on both routes (writer+).
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land/k-happy"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN);
    let resp = post_json(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land"),
        &f.reader,
        &json!({ "head": c2, "key": "k-reader" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN);
    assert_eq!(code_of(&resp), "forbidden");
}

// ---- 4. rebase land: content oracle -----------------------------------------

#[tokio::test]
async fn rebase_land_content_oracle() {
    let f = fixture();

    // c1 (the fork point): one.txt + other.txt.
    let one_base = b"alpha\nbeta\ngamma\ndelta\nepsilon\nfoxtrot\n".to_vec();
    let other_base = b"one\ntwo\n".to_vec();
    let b1 = put_blob(&f, &one_base).await;
    let b2 = put_blob(&f, &other_base).await;
    let (c1, _) = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([
                add("one.txt", &b1, one_base.len()),
                add("other.txt", &b2, other_base.len())
            ]),
            "c1",
        ),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);

    // head side (c2): line 1 of one.txt + a new file.
    let one_ours = b"ALPHA-2\nbeta\ngamma\ndelta\nepsilon\nfoxtrot\n".to_vec();
    let fresh = b"fresh\n".to_vec();
    let bo = put_blob(&f, &one_ours).await;
    let bf = put_blob(&f, &fresh).await;
    let (c2, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([
                add("new.txt", &bf, fresh.len()),
                mod_("one.txt", &bo, one_ours.len())
            ]),
            "c2",
        ),
    )
    .await;

    // main side (c2'): ADJACENT line 2 of one.txt + line 1 of other.txt.
    let one_theirs = b"alpha\nBETA-3\ngamma\ndelta\nepsilon\nfoxtrot\n".to_vec();
    let other_theirs = b"ONE-1\ntwo\n".to_vec();
    let bt = put_blob(&f, &one_theirs).await;
    let bot = put_blob(&f, &other_theirs).await;
    let (c2p, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([
                mod_("one.txt", &bt, one_theirs.len()),
                mod_("other.txt", &bot, other_theirs.len()),
            ]),
            "c2-prime",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c2p);

    let resp = land(&f, &c2, "k-rebase").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_ne!(landed, c2, "rebase rewrites the id");
    assert_eq!(resp.body["main"], landed.as_str());
    assert_eq!(resp.body["rebased"], 1);

    // CONTENT ORACLE: exact expected union content at the new main.
    assert_eq!(
        cat(&f, &landed, "one.txt").await,
        b"ALPHA-2\nBETA-3\ngamma\ndelta\nepsilon\nfoxtrot\n".to_vec(),
        "both adjacent-region edits must survive the rebase"
    );
    assert_eq!(cat(&f, &landed, "other.txt").await, other_theirs.to_vec());
    assert_eq!(cat(&f, &landed, "new.txt").await, fresh.to_vec());

    // Linear history: landed's first-parent chain runs through main's tip
    // (c2') down to the fork point.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/log?from={landed}"),
        &f.reader,
    )
    .await;
    let ids: Vec<String> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap().to_string())
        .collect();
    assert_eq!(ids, vec![landed.clone(), c2p.clone(), c1.clone()]);

    // The rebased changeset: parent = main's tip, original
    // author/message/timestamp, landing principal, bumped generation,
    // re-merged delta.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{landed}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["parents"], json!([c2p]));
    assert_eq!(resp.body["author"], "Agent <agent@naatre.dev>");
    assert_eq!(resp.body["message"], "c2");
    let orig = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{c2}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["timestamp_ns"], orig.body["timestamp_ns"]);
    assert_eq!(resp.body["principal"], "writer-p");
    // main (c2') was generation 1; the rebased node bumps to 2.
    assert_eq!(resp.body["generation"], 2);
    let paths: Vec<String> = resp.body["changes"]
        .as_array()
        .unwrap()
        .iter()
        .map(|c| c["path"].as_str().unwrap().to_string())
        .collect();
    assert_eq!(paths, vec!["new.txt", "one.txt"]);
    let merged_one = resp.body["changes"][1]["blob"]
        .as_str()
        .unwrap()
        .to_string();
    assert_ne!(merged_one, bo, "the re-merged blob differs from both sides");

    // root_tree_id oracle: replaying [c1, landed]'s deltas from an empty
    // tree equals the rebased node's recorded root.
    let replayed = replay_root_equals(&f, &[c1.clone(), c2p.clone(), landed.clone()]).await;
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{landed}/root"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["root_tree_id"], replayed.as_str());

    // The pre-rebase original stays fetchable by id.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{c2}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
}

// ---- 5b. every conflicting path is named, not just the first ---------------

#[tokio::test]
async fn conflict_lists_every_conflicting_path() {
    let f = fixture();
    let base_a = b"a1\na2\n".to_vec();
    let base_b = b"b1\nb2\n".to_vec();
    let ba = put_blob(&f, &base_a).await;
    let bb2 = put_blob(&f, &base_b).await;
    let (c1, _) = create_ok(
        &f,
        cs_body(
            json!([]),
            json!([
                add("a.txt", &ba, base_a.len()),
                add("b.txt", &bb2, base_b.len()),
            ]),
            "c1",
        ),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);

    // Both files divergently edited on both sides: two independent conflicts.
    let ours_a = b"A-ours\na2\n".to_vec();
    let theirs_a = b"A-main\na2\n".to_vec();
    let ours_b = b"b1\nB-ours\n".to_vec();
    let theirs_b = b"b1\nB-main\n".to_vec();
    let bo_a = put_blob(&f, &ours_a).await;
    let bt_a = put_blob(&f, &theirs_a).await;
    let bo_b = put_blob(&f, &ours_b).await;
    let bt_b = put_blob(&f, &theirs_b).await;
    let (c2, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([
                mod_("a.txt", &bo_a, ours_a.len()),
                mod_("b.txt", &bo_b, ours_b.len()),
            ]),
            "c2",
        ),
    )
    .await;
    let (c2p, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([
                mod_("a.txt", &bt_a, theirs_a.len()),
                mod_("b.txt", &bt_b, theirs_b.len()),
            ]),
            "c2-prime",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c2p);

    let resp = land(&f, &c2, "k-multi").await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "body: {}", resp.body);
    let conflicts = resp.body["error"]["details"]["conflicts"]
        .as_array()
        .expect("conflicts array");
    assert_eq!(
        conflicts.len(),
        2,
        "every conflicting path is named: {conflicts:?}"
    );
    let paths: Vec<&str> = conflicts
        .iter()
        .filter_map(|c| c["path"].as_str())
        .collect();
    assert!(
        paths.contains(&"a.txt") && paths.contains(&"b.txt"),
        "{paths:?}"
    );
}

// ---- 5. conflict: recipe + unsettled key ------------------------------------

#[tokio::test]
async fn conflict_leaves_key_unsettled_and_same_key_relands() {
    let f = fixture();
    let base = b"l1\nl2\nl3\n".to_vec();
    let bb = put_blob(&f, &base).await;
    let (c1, _) = create_ok(
        &f,
        cs_body(json!([]), json!([add("same.txt", &bb, base.len())]), "c1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &c1);

    let ours = b"L1-ours\nl2\nl3\n".to_vec();
    let bo = put_blob(&f, &ours).await;
    let (c2, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("same.txt", &bo, ours.len())]),
            "c2",
        ),
    )
    .await;

    let theirs = b"X1-main\nl2\nl3\n".to_vec();
    let bt = put_blob(&f, &theirs).await;
    let (c2p, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([mod_("same.txt", &bt, theirs.len())]),
            "c2-prime",
        ),
    )
    .await;
    f.t.move_main(f.repo_id, &c2p);

    let resp = land(&f, &c2, "k-conflict").await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "ref_conflict");
    let conflicts = resp.body["error"]["details"]["conflicts"]
        .as_array()
        .expect("conflicts array");
    assert_eq!(conflicts.len(), 1);
    assert_eq!(conflicts[0]["path"], "same.txt");
    assert_eq!(conflicts[0]["base"], bb.as_str());
    // ours = main's current content, theirs = the landing head's content.
    assert_eq!(conflicts[0]["ours"], bt.as_str());
    assert_eq!(conflicts[0]["theirs"], bo.as_str());
    let recipe = resp.body["error"]["details"]["recipe"]
        .as_str()
        .unwrap()
        .to_string();
    assert!(recipe.contains("re-land with the same key"), "{recipe}");

    // Nothing persisted: no land row at all (key unsettled), main unmoved.
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM lands WHERE key = 'k-conflict'"),
        0
    );
    let resp = get(&f.t.app, &format!("/v1/repos/{REPO}/refs/main"), &f.reader).await;
    assert_eq!(resp.body["target"], c2p.as_str());

    // The plan's recipe path: resolution changeset built on head's parent
    // (c1), re-land with the SAME key → success.
    let resolved = b"resolved\n".to_vec();
    let br = put_blob(&f, &resolved).await;
    let (res_cs, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([add("resolved.txt", &br, resolved.len())]),
            "resolve",
        ),
    )
    .await;
    let resp = land(&f, &res_cs, "k-conflict").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_eq!(resp.body["rebased"], 1);
    let resp = get(&f.t.app, &format!("/v1/repos/{REPO}/refs/main"), &f.reader).await;
    assert_eq!(resp.body["target"], landed.as_str());
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM lands WHERE key = 'k-conflict' AND state = 'done'"),
        1
    );
}

// ---- 6. key lifecycle --------------------------------------------------------

#[tokio::test]
async fn settled_key_rejects_different_head() {
    let f = fixture();
    let b1 = put_blob(&f, b"a\n").await;
    let (c1, _) = create_ok(&f, cs_body(json!([]), json!([add("a.txt", &b1, 2)]), "c1")).await;
    f.t.seed_main(f.repo_id, &c1);
    let b2 = put_blob(&f, b"b\n").await;
    let (c2, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("b.txt", &b2, 2)]), "c2"),
    )
    .await;
    let b3 = put_blob(&f, b"c\n").await;
    let (c3, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("c.txt", &b3, 2)]), "c3"),
    )
    .await;

    let resp = land(&f, &c2, "k-life").await;
    assert_eq!(resp.status, StatusCode::OK);
    let resp = land(&f, &c3, "k-life").await;
    assert_eq!(resp.status, StatusCode::CONFLICT);
    assert_eq!(code_of(&resp), "idempotency_key_settled");
    assert_eq!(resp.body["error"]["details"]["settled_head"], c2.as_str());
    assert_eq!(
        resp.body["error"]["details"]["land_status_pointer"],
        format!("/v1/repos/{REPO}/land/k-life")
    );
}

#[tokio::test]
async fn writer_busy_then_retry_replays() {
    let f = fixture();
    let b1 = put_blob(&f, b"a\n").await;
    let (c1, _) = create_ok(&f, cs_body(json!([]), json!([add("a.txt", &b1, 2)]), "c1")).await;
    f.t.seed_main(f.repo_id, &c1);
    let b2 = put_blob(&f, b"b\n").await;
    let (c2, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("b.txt", &b2, 2)]), "c2"),
    )
    .await;

    // Deterministic 503 through the real land handler: the full router's
    // auth middleware does its own store read before the handler, which
    // would block on the held slot and mask the handler's try_lock (the
    // reason the M1b HTTP 503 test tolerates both outcomes). A minimal
    // router without the auth layer — principal extension attached
    // directly — leaves `state.write`'s try_lock as the first store touch.
    let bare: Router = axum::Router::new()
        .route(
            "/v1/repos/{r}/land",
            axum::routing::post(naatre_server::routes::land::land),
        )
        .with_state(Arc::clone(&f.t.state));
    let bare_land = |key: &str| {
        let req = axum::http::Request::builder()
            .method("POST")
            .uri(format!("/v1/repos/{REPO}/land"))
            .header(header::CONTENT_TYPE, "application/json")
            .extension(naatre_server::auth::AuthedPrincipal {
                principal: "writer-p".into(),
                role: naatre_server::auth::Role::Writer,
                scope_path: None,
            })
            .body(Body::from(
                json!({ "head": c2.clone(), "key": key }).to_string(),
            ))
            .unwrap();
        bare.clone().oneshot(req)
    };

    let holder = {
        let state = Arc::clone(&f.t.state);
        tokio::spawn(async move { state.hold_write_txn(1000).await })
    };
    // Bounded wait for the holder to take the store mutex.
    tokio::time::sleep(Duration::from_millis(100)).await;

    let resp = bare_land("k-slow").await.unwrap();
    let (status, headers) = (resp.status(), resp.headers().clone());
    let bytes = BodyExt::collect(resp.into_body())
        .await
        .unwrap()
        .to_bytes()
        .to_vec();
    let body: Value = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE, "body: {body}");
    assert_eq!(body["error"]["code"], "writer_busy");
    assert_eq!(body["error"]["retryable"], true);
    assert_eq!(headers[header::RETRY_AFTER], "1");
    // Nothing settled while busy.
    assert_eq!(
        f.t.count("SELECT COUNT(*) FROM lands WHERE key = 'k-slow'"),
        0
    );
    holder.await.expect("holder task");

    // Retry after the slot frees: settles, then replays idempotently.
    let resp = land(&f, &c2, "k-slow").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let first = resp.body.clone();
    let resp = land(&f, &c2, "k-slow").await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body, first);

    // Concurrent identical (key, head): the second request JOINS the
    // in-flight land (identical 200) or, arriving after deregistration,
    // replays the settled row — an idempotent 200 either way. The 503
    // tolerance below is defense in depth (a join wait that outlives the
    // drain timeout), not the expected path; see land_join.rs.
    let (r1, r2) = tokio::join!(land(&f, &c2, "k-race"), land(&f, &c2, "k-race"));
    let (winner, loser) = if r1.status == StatusCode::OK {
        (r1, r2)
    } else {
        (r2, r1)
    };
    assert_eq!(winner.status, StatusCode::OK, "body: {}", winner.body);
    assert!(
        loser.status == StatusCode::OK || loser.status == StatusCode::SERVICE_UNAVAILABLE,
        "loser status {}",
        loser.status
    );
    if loser.status == StatusCode::SERVICE_UNAVAILABLE {
        let retry = land(&f, &c2, "k-race").await;
        assert_eq!(retry.status, StatusCode::OK, "body: {}", retry.body);
        assert_eq!(retry.body, winner.body);
    } else {
        assert_eq!(
            loser.body, winner.body,
            "joined lands return identical bodies"
        );
    }
}

// ---- 7. containment ----------------------------------------------------------

#[tokio::test]
async fn contained_heads_settle_without_new_changesets() {
    let f = fixture();
    let b1 = put_blob(&f, b"a\n").await;
    let (c1, _) = create_ok(&f, cs_body(json!([]), json!([add("a.txt", &b1, 2)]), "c1")).await;
    let b2 = put_blob(&f, b"b\n").await;
    let (c2, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("b.txt", &b2, 2)]), "c2"),
    )
    .await;

    // main = c2: landing c1 (an ancestor) is contained → done, main stays.
    f.t.seed_main(f.repo_id, &c2.clone());
    let cs_before = f.t.count("SELECT COUNT(*) FROM changesets");
    let resp = land(&f, &c1, "k-contained").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    assert_eq!(resp.body["landed"], c2.as_str());
    assert_eq!(resp.body["rebased"], 0);
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), cs_before);

    // head == main → idempotent done.
    let resp = land(&f, &c2, "k-is-main").await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["landed"], c2.as_str());
    assert_eq!(f.t.count("SELECT COUNT(*) FROM changesets"), cs_before);

    // No repo main → 422 validation.
    let t2 = build_app();
    let writer2 = t2.seed_token("writer-p", "writer", None);
    let _reader2 = t2.seed_token("reader-p", "reader", None);
    t2.seed_repo(REPO);
    let f2 = Fixture {
        t: t2,
        writer: writer2,
        reader: String::new(),
        repo_id: 0,
    };
    let b = put_blob(&f2, b"x\n").await;
    let (x1, _) = create_ok(&f2, cs_body(json!([]), json!([add("x.txt", &b, 2)]), "x1")).await;
    let resp = land(&f2, &x1, "k-nomain").await;
    assert_eq!(resp.status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(code_of(&resp), "validation");

    // Unknown head → 404.
    let ghost = object_id(Kind::Changeset, b"ghost").to_hex();
    let resp = land(&f, &ghost, "k-ghost").await;
    assert_eq!(resp.status, StatusCode::NOT_FOUND);
}

// ---- 8. scope gating -----------------------------------------------------------

#[tokio::test]
async fn scope_gates_the_rebase_chain() {
    let t = build_app();
    let scoped = t.seed_token("scoped-w", "writer", Some("src"));
    let unscoped = t.seed_token("free-w", "writer", None);
    t.seed_token("reader-p", "reader", None);
    let repo_id = t.seed_repo(REPO);
    let f = Fixture {
        t,
        writer: unscoped.clone(),
        reader: String::new(),
        repo_id,
    };

    let b1 = put_blob(&f, b"a\n").await;
    let (c1, _) = create_ok(
        &f,
        cs_body(json!([]), json!([add("src/in.txt", &b1, 2)]), "c1"),
    )
    .await;
    f.t.seed_main(repo_id, &c1);

    // An out-of-scope chain (created by the unscoped writer).
    let b2 = put_blob(&f, b"docs\n").await;
    let (c2_docs, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([add("docs/out.txt", &b2, 5)]),
            "docs",
        ),
    )
    .await;
    // An in-scope chain.
    let b3 = put_blob(&f, b"more\n").await;
    let (c2_src, _) = create_ok(
        &f,
        cs_body(
            json!([c1.clone()]),
            json!([add("src/more.txt", &b3, 5)]),
            "src",
        ),
    )
    .await;

    let scoped_land = async |head: &str, key: &str| {
        post_json(
            &f.t.app,
            &format!("/v1/repos/{REPO}/land"),
            &scoped,
            &json!({ "head": head, "key": key }),
        )
        .await
    };

    let resp = scoped_land(&c2_docs, "k-scope").await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "body: {}", resp.body);
    assert_eq!(code_of(&resp), "forbidden");
    assert_eq!(resp.body["error"]["details"]["path"], "docs/out.txt");
    assert_eq!(f.t.count("SELECT COUNT(*) FROM lands"), 0);

    // In-scope chain lands fine with the scoped token.
    let resp = scoped_land(&c2_src, "k-scope-ok").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
}

// ---- 9. imported merge node -----------------------------------------------------

#[tokio::test]
async fn rebase_across_imported_merge_node() {
    let f = fixture();

    let ba = put_blob(&f, b"A\n").await;
    let (c1, _) = create_ok(&f, cs_body(json!([]), json!([add("a.txt", &ba, 2)]), "c1")).await;
    let bc = put_blob(&f, b"C\n").await;
    let (c2, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("c.txt", &bc, 2)]), "c2"),
    )
    .await;
    // main side: m1.
    let bb = put_blob(&f, b"B\n").await;
    let (m1, _) = create_ok(
        &f,
        cs_body(json!([c1.clone()]), json!([add("b.txt", &bb, 2)]), "m1"),
    )
    .await;
    f.t.seed_main(f.repo_id, &m1);

    // Imported-style 2-parent merge node M2 (parents [c2, m1]) with a delta
    // on a.txt, seeded the way the importer seeds it: tree = c2's tree plus
    // the delta.
    let ba_m = put_blob(&f, b"A-merged\n").await;
    let delta = FileChange {
        path: "a.txt".into(),
        op: Op::Mod,
        blob: Some(Id::from_hex(&ba_m).unwrap()),
        size: Some(9),
        exec: Some(false),
    };
    let c2_root = {
        let resp = get(
            &f.t.app,
            &format!("/v1/repos/{REPO}/changesets/{c2}/root"),
            &f.reader,
        )
        .await;
        resp.body["root_tree_id"].as_str().unwrap().to_string()
    };
    let m2 = f.t.seed_merge_node(
        f.repo_id,
        [&c2, &m1],
        &c2_root,
        &delta,
        "Git Import <git@naatre.dev>",
        "merge commit",
        2,
    );

    // head: c3 on top of the merge.
    let bd = put_blob(&f, b"D\n").await;
    let (c3, _) = create_ok(
        &f,
        cs_body(json!([m2.clone()]), json!([add("d.txt", &bd, 2)]), "c3"),
    )
    .await;

    let resp = land(&f, &c3, "k-merge").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_eq!(resp.body["rebased"], 3, "c2, M2, c3 all rebase");

    // Linear first-parent history across the former merge: landed → M2' →
    // c2' → c1.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/log?from={landed}&limit=10"),
        &f.reader,
    )
    .await;
    let entries: Vec<(String, String)> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| {
            (
                e["id"].as_str().unwrap().to_string(),
                e["message"].as_str().unwrap().to_string(),
            )
        })
        .collect();
    let msgs: Vec<String> = entries.iter().map(|(_, m)| m.clone()).collect();
    // The rebased chain sits on top of main's tip (m1), so the linear
    // first-parent walk is c3' → M2' → c2' → m1 → c1.
    assert_eq!(msgs, vec!["c3", "merge commit", "c2", "m1", "c1"]);
    let m2_rebased = entries[1].0.clone();
    // The rebased merge is single-parent now.
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{m2_rebased}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["parents"].as_array().unwrap().len(), 1);

    // Content survives the traverse: all four files correct at the new main.
    assert_eq!(cat(&f, &landed, "a.txt").await, b"A-merged\n".to_vec());
    assert_eq!(cat(&f, &landed, "b.txt").await, b"B\n".to_vec());
    assert_eq!(cat(&f, &landed, "c.txt").await, b"C\n".to_vec());
    assert_eq!(cat(&f, &landed, "d.txt").await, b"D\n".to_vec());

    // root_tree_id oracle: replay ALL ancestors of the landed node (the
    // rebased chain plus the second branch the merge absorbed) in
    // topological order from an empty tree — every delta is accounted for.
    let c2r_id = entries[2].0.clone();
    let chain = vec![
        c1.clone(),
        m1.clone(),
        c2r_id,
        entries[1].0.clone(),
        landed.clone(),
    ];
    let replayed = replay_root_equals(&f, &chain).await;
    let resp = get(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{landed}/root"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.body["root_tree_id"], replayed.as_str());
}

// ---- 10. crash-point smoke ------------------------------------------------------

/// The child process: reads NAATRE_CRASH_POINT (unset in the normal suite
/// run → trivial pass). An enum name hits its call site (panics); any other
/// value must never fire a call site.
#[test]
fn land_crash_hook_child() {
    let Ok(name) = std::env::var("NAATRE_CRASH_POINT") else {
        return;
    };
    if naatre_server::land::CRASH_POINTS.contains(&name.as_str()) {
        naatre_server::land::crash_point(&name);
    } else {
        naatre_server::land::crash_point("after_commit");
    }
}

/// Each name of the closed crash-point enum panics when set; a junk name
/// does not. Each check runs in a fresh child process of this binary
/// because the env var is read exactly once per process.
#[cfg(debug_assertions)]
#[test]
fn crash_hook_smoke_each_name_panics() {
    let exe = std::env::current_exe().unwrap();
    for name in naatre_server::land::CRASH_POINTS {
        let out = Command::new(&exe)
            .args(["--exact", "land_crash_hook_child", "--nocapture"])
            .env("NAATRE_CRASH_POINT", name)
            .output()
            .expect("spawn child");
        assert!(
            !out.status.success(),
            "crash point {name} must panic (exit {:?})",
            out.status.code()
        );
        let stderr = String::from_utf8_lossy(&out.stderr);
        assert!(
            stderr.contains(&format!("crash point {name} triggered")),
            "stderr of {name}: {stderr}"
        );
    }
    // A name outside the enum never fires.
    let out = Command::new(&exe)
        .args(["--exact", "land_crash_hook_child", "--nocapture"])
        .env("NAATRE_CRASH_POINT", "not-a-crash-point")
        .output()
        .expect("spawn child");
    assert!(out.status.success(), "junk name must not panic");
}
