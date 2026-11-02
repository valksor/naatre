//! M10b suite: redaction + erasure end-state. The plan's named e2e lives
//! here (SQLite+fs, production derive kinds registered): the universal gate
//! on every serving path (by-id fetch → 410 `object_redacted`, `/log`
//! excludes the tombstoned node immediately, diff/file → structured
//! degradation, blame/path-history re-derived without the object, snapshot
//! containing the blob → 410 overriding the TTL, 3-way base → structured
//! 409 `redacted_base` on the land path, merge-base never names a tombstoned
//! node), the shared-subtree pin (redact in one repo, the other repo's
//! reads keep working), reference-aware purge (only when no repo references
//! remain; the tombstoned changeset's exclusively-referenced blobs follow
//! in the same pass; idempotent re-runs), the tombstone-meta rule (path +
//! date only, never author — inspected in the DB), principal deletion
//! (tokens + principal rows gone, audit actor pseudonymized, history
//! attribution survives), and fsck's redaction consistency (tombstoned-but-
//! unpurged normal; purged-but-still-referenced is drift).
//!
//! The GC/redaction passes are driven directly with the injected clock; the
//! serve-side background tasks are off-switched for determinism.

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use axum::Router;
use axum::http::StatusCode;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::tree::{self, EntryKind, Tree, TreeEntry};
use naatre_server::derive::derive_pass;
use naatre_server::fsck;
use naatre_server::gc::gc_pass;
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;

#[path = "m4b_common/mod.rs"]
mod m4b_common;
use m4b_common::{REPO, Resp, blob_hex, cs_body, get_on, post_json_on, seed_repo_at};

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;

fn urlencode(path: &str) -> String {
    path.replace('/', "%2F")
}

fn blob_id(content: &[u8]) -> Id {
    object_id(Kind::Blob, content)
}

fn hex_id(hex: &str) -> Id {
    Id::from_hex(hex).unwrap()
}

// ---- harness (the m10a shape, plus the production derive kinds) -------------

struct TestApp {
    _dir: tempfile::TempDir,
    app: Router,
    db_path: PathBuf,
    state: AppState,
    clock: Arc<AtomicU64>,
}

fn build_app_with(configure: impl FnOnce(&mut ServerState)) -> TestApp {
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
    // Every background task off: the tests drive the passes directly.
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    state.derive_queue_enabled = false;
    state.derive_backfill_enabled = false;
    for deriver in naatre_server::derived::production_kinds() {
        state.register_deriver(deriver).unwrap();
    }
    configure(&mut state);
    let db_path = dir.path().join("naatre.db");
    let state = Arc::new(state);
    let app = naatre_server::router(Arc::clone(&state));
    TestApp {
        _dir: dir,
        app,
        db_path,
        state,
        clock,
    }
}

struct Fixture {
    t: TestApp,
    /// The admin principal's token (redaction is admin-only).
    admin: String,
    writer: String,
    repo_id: i64,
}

fn fixture() -> Fixture {
    fixture_with(|_| {})
}

fn fixture_with(configure: impl FnOnce(&mut ServerState)) -> Fixture {
    let t = build_app_with(configure);
    let admin = seed_admin(&t.db_path);
    let writer = m4b_common::seed_token_at(&t.db_path, "writer-p", "writer", None);
    let repo_id = seed_repo_at(&t.db_path, REPO);
    Fixture {
        t,
        admin,
        writer,
        repo_id,
    }
}

/// Seeds an `admin` principal + token the way `naatre init` does; returns
/// the token string.
fn seed_admin(db_path: &std::path::Path) -> String {
    let (token, hash) = naatre_server::auth::mint_token();
    let conn = m4b_common::conn_at(db_path);
    conn.execute(
        "INSERT INTO principals(name, role, created_ns) VALUES ('admin', 'admin', 1) \
         ON CONFLICT(name) DO NOTHING",
        [],
    )
    .unwrap();
    conn.execute(
        "INSERT INTO tokens(principal, blake3, expires_ns, revoked) VALUES ('admin', ?1, NULL, 0)",
        [&hash[..]],
    )
    .unwrap();
    token
}

impl Fixture {
    async fn put_blob(&self, content: &[u8]) -> String {
        m4b_common::put_blob_on(&self.t.app, &self.writer, content).await
    }

    async fn create_ok(&self, body: &Value) -> (String, String) {
        let resp = post_json_on(
            &self.t.app,
            &format!("/v1/repos/{REPO}/changesets"),
            &self.writer,
            body,
        )
        .await;
        assert_eq!(resp.status, StatusCode::CREATED, "body: {}", resp.body);
        (
            resp.body["id"].as_str().unwrap().to_string(),
            resp.body["root_tree_id"].as_str().unwrap().to_string(),
        )
    }

    async fn commit(&self, parents: Value, path: &str, content: &[u8]) -> String {
        let blob = self.put_blob(content).await;
        let (id, _) = self
            .create_ok(&cs_body(
                parents,
                json!([m4b_common::add(path, &blob, content.len())]),
                "commit",
            ))
            .await;
        id
    }

    async fn land(&self, head: &str, key: &str) -> Resp {
        post_json_on(
            &self.t.app,
            &format!("/v1/repos/{REPO}/land"),
            &self.writer,
            &json!({ "head": head, "key": key }),
        )
        .await
    }

    /// Redacts `id` (kind `kind`) through the admin route; asserts 201.
    async fn redact(&self, kind: &str, id: &str, reason: &str) -> Resp {
        let resp = post_json_on(
            &self.t.app,
            &format!("/v1/repos/{REPO}/redactions"),
            &self.admin,
            &json!({ "id": id, "kind": kind, "reason": reason }),
        )
        .await;
        assert_eq!(
            resp.status,
            StatusCode::CREATED,
            "redact {id}: {}",
            resp.body
        );
        resp
    }

    async fn get(&self, uri: &str) -> Resp {
        get_on(&self.t.app, uri, &self.writer).await
    }

    fn count(&self, sql: &str) -> i64 {
        m4b_common::count_at(&self.t.db_path, sql)
    }

    fn count_params(&self, sql: &str, params: &[&dyn rusqlite::ToSql]) -> i64 {
        m4b_common::count_params_at(&self.t.db_path, sql, params)
    }

    fn seed_main(&self, target: &str) {
        m4b_common::seed_main_at(&self.t.db_path, self.repo_id, target);
    }

    fn tick_to(&self, now: u64) {
        self.t.clock.store(now, Ordering::Relaxed);
    }

    /// Drives the derive queue to quiescence and marks the production
    /// kinds' boot backfills complete (the settled state `serve` models).
    async fn drain(&self) {
        for _ in 0..100 {
            derive_pass(&self.t.state).await.expect("derive pass");
            let pending = self.count("SELECT COUNT(*) FROM derive_state WHERE state = 'pending'");
            if pending == 0 {
                break;
            }
        }
        assert_eq!(
            self.count("SELECT COUNT(*) FROM derive_state WHERE state = 'pending'"),
            0,
            "queue drained"
        );
        for kind in [
            naatre_server::derived::KIND_BLAME,
            naatre_server::derived::KIND_PATH_HISTORY,
            naatre_server::derived::KIND_COMMIT_GRAPH,
        ] {
            m4b_common::conn_at(&self.t.db_path)
                .execute(
                    "INSERT INTO meta(key, value) VALUES (?1, '0') \
                     ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                    [&format!("derive_backfill_done:{kind}")],
                )
                .unwrap();
        }
    }

    /// Raw blob bytes of `path` at `at` through /file + /objects.
    async fn cat(&self, at: &str, path: &str) -> Vec<u8> {
        let resp = self
            .get(&format!(
                "/v1/repos/{REPO}/file?at={at}&path={}",
                urlencode(path)
            ))
            .await;
        assert_eq!(resp.status, StatusCode::OK, "cat {path}: {}", resp.body);
        let blob = resp.body["blob_id"].as_str().unwrap().to_string();
        let req = axum::http::Request::builder()
            .method("GET")
            .uri(format!("/v1/repos/{REPO}/objects/{blob}"))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", self.writer),
            )
            .body(axum::body::Body::empty())
            .unwrap();
        let res = self.t.app.clone().oneshot(req).await.unwrap();
        assert!(res.status().is_success());
        http_body_util::BodyExt::collect(res.into_body())
            .await
            .unwrap()
            .to_bytes()
            .to_vec()
    }

    async fn get_raw_object(&self, id_hex: &str) -> Resp {
        let req = axum::http::Request::builder()
            .method("GET")
            .uri(format!("/v1/repos/{REPO}/objects/{id_hex}"))
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", self.writer),
            )
            .body(axum::body::Body::empty())
            .unwrap();
        let res = self.t.app.clone().oneshot(req).await.unwrap();
        let status = res.status();
        let body = http_body_util::BodyExt::collect(res.into_body())
            .await
            .unwrap()
            .to_bytes();
        Resp {
            status,
            body: serde_json::from_slice(&body)
                .unwrap_or_else(|_| json!({ "raw": String::from_utf8_lossy(&body) })),
        }
    }
}

/// The base every test starts from: a ROOT commit landed on main (one file
/// + one nested file, so subtrees exist to be shared).
async fn seed_base(f: &Fixture) -> String {
    let a = f.put_blob(b"live-content-A").await;
    let x = f.put_blob(b"nested-x").await;
    let (root, _) = f
        .create_ok(&cs_body(
            json!([]),
            json!([
                m4b_common::add("a.txt", &a, b"live-content-A".len()),
                m4b_common::add("dir/x.txt", &x, b"nested-x".len()),
            ]),
            "root",
        ))
        .await;
    f.seed_main(&root);
    let resp = f.land(&root, "k0").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    root
}

// ---- 1. the universal gate on the changeset surface --------------------------

/// Redact a landed changeset → fetch 410 `object_redacted` on every
/// changeset-naming surface, `/log` drops it immediately (and the walk
/// continues past it), diff/file degrade, path-history drops it, and the
/// land path answers the structured 409 `redacted_base`.
#[tokio::test]
async fn redacted_changeset_gates_every_serving_path() {
    let f = fixture();
    let root = seed_base(&f).await;
    f.drain().await;
    let c2_orig = f
        .commit(json!([root.clone()]), "a.txt", b"second-version")
        .await;
    let resp = f.land(&c2_orig, "k1").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    // Landing REWRITES ids (the server-side rebase): main points at the
    // rebased changeset, and that is the node whose attribution the
    // tombstone must gate.
    let c2 = resp.body["landed"].as_str().unwrap().to_string();
    assert_ne!(c2, c2_orig, "a land onto a moved tip rebases");
    f.drain().await;
    assert_eq!(f.cat(&c2, "a.txt").await, b"second-version".to_vec());

    // Mark. The audit row carries the acting admin and the reason — never
    // the redacted content's author.
    let author: String = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT author FROM changesets WHERE id = ?1",
            [&hex_id(&c2).0[..]],
            |r| r.get(0),
        )
        .unwrap();
    assert!(author.contains("Agent"), "seeded author: {author}");
    f.redact("changeset", &c2, "takedown request 7").await;

    // Tombstone metadata: the ROW's column set is EXACTLY the clause's
    // path + date set — repo target, object, kind, path, mark date, purge
    // date. NO reason column, NO principal column (free text can carry
    // personal data; the actor is the accountability record's, not the
    // tombstone's), and never an author column.
    let columns: String = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT group_concat(name, ',') \
             FROM (SELECT name FROM pragma_table_info('redactions') ORDER BY cid)",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        columns,
        "repo_id,object_id,kind,path,marked_ns,purged_ns".to_string(),
        "the tombstone column set is exactly the plan's path + date set"
    );
    let (kind, path, purged): (String, Option<String>, Option<i64>) =
        m4b_common::conn_at(&f.t.db_path)
            .query_row(
                "SELECT kind, path, purged_ns FROM redactions WHERE object_id = ?1",
                rusqlite::params![hex_id(&c2).0.as_slice()],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
            )
            .unwrap();
    assert_eq!(kind, "changeset");
    assert_eq!(path.as_deref(), Some("a.txt"), "the tombstone's path");
    assert!(
        purged.is_none(),
        "tombstoned-but-unpurged is the mark state"
    );
    // Reason + acting admin live ONLY in the audited `redact` action.
    let (audit_principal, details): (String, String) = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT principal, details_json FROM audit_log WHERE action = 'redact'",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert_eq!(audit_principal, "admin", "the audit row's actor");
    assert!(
        details.contains("takedown request 7"),
        "the reason lives in the audit details: {details}"
    );
    assert!(
        !details.contains(&author),
        "audit leaks the author: {details}"
    );
    assert!(details.contains(&c2), "audit names the target: {details}");

    // The mark poisoned the tombstoned changeset's derive rows (the
    // re-derivation taint machinery's source).
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM derive_state WHERE cs = ?1 AND state = 'poison'",
            &[&hex_id(&c2).0.as_slice()]
        ),
        3,
        "all three production kinds' rows invalidated"
    );

    // By-id fetches: 410 object_redacted (both the changeset and its root).
    let resp = f.get(&format!("/v1/repos/{REPO}/changesets/{c2}")).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
    let resp = f
        .get(&format!("/v1/repos/{REPO}/changesets/{c2}/root"))
        .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);

    // /log: the tombstoned node drops out immediately; the walk continues
    // past it to the root.
    let resp = f.get(&format!("/v1/repos/{REPO}/log")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(
        ids,
        vec![root.as_str()],
        "only the root serves: {}",
        resp.body
    );

    // /log from the tombstoned id directly: the same 410 as the fetch.
    let resp = f.get(&format!("/v1/repos/{REPO}/log?from={c2}")).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);

    // The log cursor continues past it: resuming after the root (the last
    // SERVED entry) ends the page cleanly, and no tombstoned entry ever
    // appears under any from.
    let resp = f.get(&format!("/v1/repos/{REPO}/log?from={root}")).await;
    assert_eq!(resp.status, StatusCode::OK);
    assert_eq!(resp.body["entries"].as_array().unwrap().len(), 1);
    assert_eq!(resp.body["has_more"], false);

    // diff: naming the tombstoned endpoint is 410.
    let resp = f
        .get(&format!("/v1/repos/{REPO}/diff?a={root}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");

    // /file at the tombstoned changeset is 410.
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/file?at={c2}&path={}",
            urlencode("a.txt")
        ))
        .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);

    // Path history: the tombstoned node's touch drops out of both the
    // derived index and the scan fallback (the page also carries the
    // degraded flag — the mark poisoned its queue row).
    let resp = f
        .get(&format!("/v1/repos/{REPO}/log?path={}", urlencode("a.txt")))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert!(
        !ids.contains(&c2.as_str()),
        "tombstoned node in path history: {}",
        resp.body
    );
    assert_eq!(resp.body["degraded"], true, "{}", resp.body);

    // Blame at the tombstoned changeset: 410.
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/blame?at={c2}&path={}",
            urlencode("a.txt")
        ))
        .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);

    // The land path: a child of the tombstoned tip lands → 409
    // `redacted_base` naming the offending id, and the key stays unsettled.
    let c3 = f
        .commit(json!([c2.clone()]), "a.txt", b"third-version")
        .await;
    let resp = f.land(&c3, "k2").await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "redacted_base");
    assert_eq!(resp.body["error"]["details"]["redacted"], c2.as_str());
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands WHERE key = 'k2'"),
        0,
        "the refused land left zero rows"
    );
    // A redacted MAIN tip conflicts on the head probe the same way.
    let resp = f.land(&c2, "k3").await;
    assert_eq!(resp.status, StatusCode::CONFLICT, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "redacted_base");

    // The audit trail: mark rows redact + (no purge yet) — one redact row.
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact'"),
        1
    );
}

/// Blame across a tombstoned chain node degrades without the author: the
/// node's lines carry its id (poisoned_cs names it, the taint pointer) and
/// the `(redacted)` placeholder, never the real author.
#[tokio::test]
async fn blame_across_a_tombstoned_node_withholds_the_author() {
    let f = fixture();
    let root = seed_base(&f).await;
    // c2 carries a DISTINCT author (the default harness author is
    // "Agent <agent@naatre.dev>"): the pin is observable only if the
    // redacted node's author is unique.
    let c2 = {
        let blob = f.put_blob(b"line-one\nline-two\n").await;
        let resp = post_json_on(
            &f.t.app,
            &format!("/v1/repos/{REPO}/changesets"),
            &f.writer,
            &json!({
                "parents": [root],
                "author": "Bob <bob@evil.example>",
                "message": "bob edits",
                "changes": [m4b_common::mod_("a.txt", &blob, b"line-one\nline-two\n".len())],
            }),
        )
        .await;
        assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
        resp.body["id"].as_str().unwrap().to_string()
    };
    let c3 = f
        .commit(
            json!([c2.clone()]),
            "a.txt",
            b"line-one\nline-two\nline-three\n",
        )
        .await;
    f.drain().await;

    // Pre-redaction: c2's lines attribute to Bob.
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/blame?at={c3}&path={}",
            urlencode("a.txt")
        ))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], false, "{}", resp.body);
    let authors: Vec<&str> = resp.body["lines"]
        .as_array()
        .unwrap()
        .iter()
        .map(|l| l["author"].as_str().unwrap())
        .collect();
    assert!(authors.iter().any(|a| a.contains("Bob")), "{authors:?}");

    f.redact("changeset", &c2, "erasure").await;
    f.drain().await;

    // Post-redaction: degraded, the tombstoned node named in poisoned_cs,
    // and NO line carries a real author for it.
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/blame?at={c3}&path={}",
            urlencode("a.txt")
        ))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["degraded"], true, "{}", resp.body);
    let poisoned: Vec<&str> = resp.body["poisoned_cs"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap())
        .collect();
    assert_eq!(poisoned, vec![c2.as_str()], "{}", resp.body);
    let body = resp.body.to_string();
    assert!(!body.contains("Bob"), "author leaked: {body}");
    // Every line attributes to a live changeset (root / c3, real author)
    // or to the redacted boundary (its id + the placeholder author).
    for line in resp.body["lines"].as_array().unwrap() {
        let cs = line["cs"].as_str().unwrap();
        let author = line["author"].as_str().unwrap();
        assert!(
            (cs == root || cs == c3) && author.contains("Agent")
                || (cs == c2 && author == "(redacted)"),
            "line {line}"
        );
    }
}

/// Merge-base never NAMES a tombstoned node: the walk traverses through it
/// but an excluded node cannot win the best-selection.
#[tokio::test]
async fn merge_base_never_names_a_tombstoned_node() {
    let f = fixture();
    let root = seed_base(&f).await;
    let c2a = f.commit(json!([root.clone()]), "b.txt", b"branch-a").await;
    let c2b = f.commit(json!([root.clone()]), "c.txt", b"branch-b").await;

    let resp = f
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c2a}&b={c2b}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["merge_base"], root.as_str(), "{}", resp.body);

    f.redact("changeset", &root, "history rewrite").await;

    // The only common ancestor is tombstoned → null (never the tombstoned
    // id), and the endpoints themselves still answer 404-free.
    let resp = f
        .get(&format!("/v1/repos/{REPO}/merge-base?a={c2a}&b={c2b}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["merge_base"], Value::Null, "{}", resp.body);
}

// ---- 2. the blob surface + the shared-subtree pin ----------------------------

/// Redact a blob → by-id fetch 410, batch fetch carries it in `redacted`,
/// /file at a path resolving to it is 410, diff masks the pointers with
/// `redacted: true`.
#[tokio::test]
async fn redacted_blob_gates_the_object_surface() {
    let f = fixture();
    let root = seed_base(&f).await;
    let b_blob = blob_hex(b"secret-content");
    let c2 = f
        .commit(json!([root.clone()]), "b.txt", b"secret-content")
        .await;
    f.drain().await;

    f.redact("blob", &b_blob, "leaked key").await;

    // By-id fetch: 410 (the tombstone wins over membership).
    let resp = f.get_raw_object(&b_blob).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");

    // Batch fetch: redacted list, never in objects.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/objects/get"),
        &f.writer,
        &json!({ "ids": [blob_hex(b"live-content-A"), b_blob] }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let served: Vec<&str> = resp.body["objects"]
        .as_array()
        .unwrap()
        .iter()
        .map(|o| o["id"].as_str().unwrap())
        .collect();
    assert_eq!(served, vec![blob_hex(b"live-content-A")]);
    assert_eq!(resp.body["redacted"], json!([b_blob]), "{}", resp.body);

    // /file at a path resolving to the redacted blob: 410.
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/file?at={c2}&path={}",
            urlencode("b.txt")
        ))
        .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");

    // Diff between root and c2 masks the redacted pointer.
    let resp = f
        .get(&format!("/v1/repos/{REPO}/diff?a={root}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let changes = resp.body["changes"].as_array().unwrap();
    assert_eq!(changes.len(), 1, "{}", resp.body);
    assert_eq!(changes[0]["path"], "b.txt");
    assert_eq!(changes[0]["redacted"], true);
    assert_eq!(changes[0]["b_blob"], Value::Null, "{}", resp.body);

    // Blame's assembly gate: blaming b.txt at c2 names the redacted blob.
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/blame?at={c2}&path={}",
            urlencode("b.txt")
        ))
        .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
}

/// The shared-subtree pin: two repos reference the same content; redacting
/// the blob in one repo gates that repo's reads and leaves the other
/// repo's reads working byte-identically.
#[tokio::test]
async fn shared_subtree_redact_in_one_repo_other_keeps_serving() {
    let f = fixture();
    let root = seed_base(&f).await;
    // A second repo committing the SAME content (same blob ids).
    m4b_common::seed_repo_at(&f.t.db_path, "other");
    let other_token = m4b_common::seed_token_at(&f.t.db_path, "other-p", "writer", None);
    let shared = b"shared-subtree-content".to_vec();
    let shared_hex = blob_hex(&shared);
    // Both repos PUT the same bytes (content-addressed: one CAS object, two
    // membership rows).
    let _ = m4b_common::put_blob_on(&f.t.app, &other_token, &shared).await;
    let _ = m4b_common::put_blob_on(&f.t.app, &f.writer, &shared).await;
    let resp = post_json_on(
        &f.t.app,
        "/v1/repos/other/changesets",
        &other_token,
        &cs_body(
            json!([]),
            json!([m4b_common::add("s.txt", &shared_hex, shared.len())]),
            "root",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    f.redact("blob", &shared_hex, "redact in the first repo only")
        .await;

    // First repo: 410.
    let resp = f.get_raw_object(&shared_hex).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    // Other repo: byte-identical bytes still serve.
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/other/objects/{shared_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {other_token}"),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(res.status(), StatusCode::OK);
    let served = http_body_util::BodyExt::collect(res.into_body())
        .await
        .unwrap()
        .to_bytes()
        .to_vec();
    assert_eq!(served, shared, "the other repo's reads keep working");
    // The unredacted sibling blob of the same repo still serves.
    assert_eq!(f.cat(&root, "a.txt").await, b"live-content-A".to_vec());
}

/// A snapshot containing a redacted blob becomes unrestorable immediately:
/// the snapshot blob fetch answers 410 `object_redacted` while the
/// snapshot is still inside its TTL (redaction overrides the TTL root).
#[tokio::test]
async fn snapshot_containing_a_redacted_blob_is_unrestorable() {
    let f = fixture();
    seed_base(&f).await;
    // A snapshot whose closure references the (soon redacted) blob — a
    // HISTORY blob (put through the objects route and referenced), so the
    // tombstone has a repo-visible target.
    let secret = b"snapshot-secret".to_vec();
    let secret_hex = blob_hex(&secret);
    let history = m4b_common::put_blob_on(&f.t.app, &f.writer, &secret).await;
    let c = {
        let resp = post_json_on(
            &f.t.app,
            &format!("/v1/repos/{REPO}/changesets"),
            &f.writer,
            &cs_body(
                json!([]),
                json!([m4b_common::add("s.txt", &history, secret.len())]),
                "with secret",
            ),
        )
        .await;
        assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
        resp.body["id"].as_str().unwrap().to_string()
    };
    f.seed_main(&c);
    let file = |name: &str, blob: Id, size: u64| TreeEntry {
        name: name.to_string(),
        kind: EntryKind::File,
        object: blob,
        size,
        exec: false,
    };
    let t = Tree {
        entries: vec![file("s.txt", hex_id(&secret_hex), secret.len() as u64)],
    };
    let tree_id = tree::tree_id(&t).unwrap();
    let snap_id = format!("{:062x}{:02x}", 0u128, 0xBE);
    let put = axum::http::Request::builder()
        .method("PUT")
        .uri(format!(
            "/v1/repos/{REPO}/snapshots/{snap_id}/objects/{secret_hex}"
        ))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(secret.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(put).await.unwrap();
    assert_eq!(res.status(), StatusCode::CREATED);
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.writer,
        &json!({
            "snap_id": snap_id,
            "tree_id": tree_id.to_hex(),
            "trees": [STANDARD.encode(tree::encode(&t).unwrap())],
            "ttl_secs": 3_600u64,
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Inside the TTL the bytes serve…
    let get = axum::http::Request::builder()
        .method("GET")
        .uri(format!(
            "/v1/repos/{REPO}/snapshots/{snap_id}/objects/{secret_hex}"
        ))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(get).await.unwrap();
    assert_eq!(res.status(), StatusCode::OK);

    // …until the redaction: 410 immediately, TTL irrelevant.
    f.redact("blob", &secret_hex, "snapshot erasure").await;
    let get = axum::http::Request::builder()
        .method("GET")
        .uri(format!(
            "/v1/repos/{REPO}/snapshots/{snap_id}/objects/{secret_hex}"
        ))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(get).await.unwrap();
    assert_eq!(res.status(), StatusCode::GONE);
    let body = http_body_util::BodyExt::collect(res.into_body())
        .await
        .unwrap()
        .to_bytes();
    let body: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(body["error"]["code"], "object_redacted", "{body}");
}

// ---- 3. the redactions route contract ----------------------------------------

#[tokio::test]
async fn redaction_route_is_admin_only_idempotent_and_lists() {
    let f = fixture();
    let root = seed_base(&f).await;
    let b_hex = blob_hex(b"live-content-A");

    // Non-admin POST/GET → 403.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &f.writer,
        &json!({ "id": b_hex, "kind": "blob", "reason": "nope" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);
    let resp = f.get(&format!("/v1/repos/{REPO}/redactions")).await;
    assert_eq!(resp.status, StatusCode::FORBIDDEN, "{}", resp.body);

    // Validation: bad kind, empty reason, unknown id.
    for (body, code) in [
        (
            json!({ "id": b_hex, "kind": "tree", "reason": "x" }),
            "validation",
        ),
        (
            json!({ "id": b_hex, "kind": "blob", "reason": "  " }),
            "validation",
        ),
        (
            json!({ "id": format!("{:064x}", 0xEEu64), "kind": "blob", "reason": "x" }),
            "object_not_found",
        ),
    ] {
        let resp = post_json_on(
            &f.t.app,
            &format!("/v1/repos/{REPO}/redactions"),
            &f.admin,
            &body,
        )
        .await;
        assert_eq!(resp.body["error"]["code"], code, "{}", resp.body);
    }

    // Mark: 201 with marked_ns; re-POST: 200, the earliest mark stands, no
    // second audit row.
    let first = f.redact("blob", &b_hex, "first reason").await;
    let marked_ns = first.body["redaction"]["marked_ns"].clone();
    assert!(!marked_ns.is_null());
    let again = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &f.admin,
        &json!({ "id": b_hex, "kind": "blob", "reason": "second reason" }),
    )
    .await;
    assert_eq!(again.status, StatusCode::OK, "{}", again.body);
    assert_eq!(again.body["redaction"]["marked_ns"], marked_ns);
    assert_eq!(again.body["redaction"]["path"], "a.txt", "{}", again.body);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact'"),
        1,
        "the audit row rides the 0→1 transition only"
    );

    // The list (admin): paginated, purged state included.
    let c_hex = {
        let c = f.commit(json!([root.clone()]), "z.txt", b"z").await;
        f.redact("changeset", &c, "second tombstone").await;
        c
    };
    let resp = get_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions?limit=1"),
        &f.admin,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["redactions"].as_array().unwrap().len(), 1);
    assert_eq!(resp.body["has_more"], true);
    let cursor = resp.body["next_cursor"].as_str().unwrap().to_string();
    let resp = get_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions?limit=10&cursor={cursor}"),
        &f.admin,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK);
    let ids: Vec<&str> = resp.body["redactions"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids.len(), 1);
    assert_eq!(ids[0], c_hex, "{}", resp.body);
    assert_eq!(resp.body["has_more"], false);
}

// ---- 4. reference-aware purge -------------------------------------------------

/// The purge reclaims a tombstoned changeset + its exclusively-referenced
/// blob when nothing references them; tombstoned-but-unpurged (still under
/// a ref) is normal and survives a pass; re-runs are idempotent and each
/// purging pass rows ONE `redact_purge` audit row.
#[tokio::test]
async fn purge_is_reference_aware_and_idempotent() {
    let f = fixture();
    let root = seed_base(&f).await;

    // An unlanded commit: unreachable-from-birth → purgeable the moment it
    // is tombstoned (no ref, no lands, no git_map, no children).
    let orphan_content = b"orphan-redact-content".to_vec();
    let orphan_blob = blob_id(&orphan_content);
    let orphan = f
        .commit(json!([root.clone()]), "o.txt", &orphan_content)
        .await;
    f.redact("changeset", &orphan, "unlanded takedown").await;
    // …and its blob, tombstoned separately (still referenced by the
    // changeset's file_changes until the changeset purge drops them).
    f.redact("blob", &orphan_blob.to_hex(), "same content")
        .await;

    // A still-referenced blob tombstone: referenced by the LIVE root's
    // file_changes → never purgeable while the reference stands.
    f.redact("blob", &blob_id(b"live-content-A").to_hex(), "live ref")
        .await;

    // Pass 1: the orphan changeset purges (rows, changeset last). Its
    // exclusively-referenced blob does NOT follow yet — the orphan's root
    // TREE still carries the blob in its entries, and tree collection is
    // the reachability sweep's grace-gated business (never dangling, one
    // phase behind). The live-referenced blob tombstone stays.
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_cs, 1, "{report:?}");
    assert_eq!(report.redactions_purged_blobs, 0, "{report:?}");
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM tree_entries WHERE kind = 0 AND object_id = ?1",
            &[&orphan_blob.0.as_slice()]
        ),
        1,
        "the dying tree still references the blob — it spares"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM changesets WHERE id = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        0,
        "the tombstoned changeset row is gone"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM file_changes WHERE cs = ?1",
            &[&hex_id(&orphan).0.as_slice()]
        ),
        0
    );
    // The tombstoned changeset's stamp is in.
    let cs_purged: Option<i64> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT purged_ns FROM redactions WHERE kind = 'changeset' AND object_id = ?1",
            [&hex_id(&orphan).0[..]],
            |r| r.get(0),
        )
        .unwrap();
    assert!(cs_purged.is_some());
    // …the live-referenced one does not (tombstoned-but-unpurged, normal).
    let live_purged: Option<i64> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT purged_ns FROM redactions WHERE kind = 'blob' AND object_id = ?1",
            [&blob_id(b"live-content-A").0[..]],
            |r| r.get(0),
        )
        .unwrap();
    assert!(live_purged.is_none());
    // The gated read still answers 410 for the live-referenced blob.
    let resp = f.get_raw_object(&blob_id(b"live-content-A").to_hex()).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);

    // ONE purge audit row for the pass.
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact_purge'"),
        1
    );

    // Pass 2, past the grace window: the reachability sweep collects the
    // orphan's dying tree, and the redaction phase reclaims the blob the
    // tree was holding (the never-dangling convergence).
    f.tick_to(BASE_NS + 31 * 24 * 3600 * 1_000_000_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_blobs, 1, "{report:?}");
    assert_eq!(
        f.t.state.cas.has(&[orphan_blob]).unwrap(),
        vec![false],
        "the exclusively-referenced blob's bytes are reclaimed"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&orphan_blob.0.as_slice()]
        ),
        0
    );
    let blob_purged: Option<i64> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT purged_ns FROM redactions WHERE kind = 'blob' AND object_id = ?1",
            [&orphan_blob.0[..]],
            |r| r.get(0),
        )
        .unwrap();
    assert!(blob_purged.is_some());
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact_purge'"),
        2,
        "each purging pass rows its own"
    );

    // Re-run: idempotent — nothing left to purge, no further audit row.
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.redactions_purged_cs + report.redactions_purged_blobs,
        0
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact_purge'"),
        2
    );

    // fsck clean: tombstoned-but-unpurged is not drift, and the purge left
    // no dangling reference behind.
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(report.clean(), "{report:?}");
    assert_eq!(report.redactions_active, 1, "{report:?}");
    assert_eq!(report.redactions_purged, 2, "{report:?}");
    assert_eq!(report.redaction_purge_drift, 0);

    // The live chain serves byte-identically after the purge; the gated
    // a.txt blob stays 410 (its still-referenced tombstone stands).
    assert_eq!(f.cat(&root, "dir/x.txt").await, b"nested-x".to_vec());
    let resp = f.get_raw_object(&blob_id(b"live-content-A").to_hex()).await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
}

/// fsck's drift direction: a tombstone stamped purged whose object row
/// still exists FROM BEFORE the stamp IS drift (fabricated here by
/// hand-stamping, the only way a correct purge could leave one — a row
/// whose touch postdates the stamp is a never-redacted repo's legitimate
/// re-upload, not drift).
#[tokio::test]
async fn fsck_reports_purged_but_still_referenced_as_drift() {
    let f = fixture();
    seed_base(&f).await;
    let b_hex = blob_hex(b"live-content-A");
    f.redact("blob", &b_hex, "drift seed").await;
    // Hand-stamp purged_ns AFTER the objects row's last touch while the
    // row still exists (the drift state the temporal arm hunts).
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "UPDATE redactions SET purged_ns = 9223372036854775807 WHERE kind = 'blob'",
            [],
        )
        .unwrap();
    let report = fsck::fsck(&f.t.state).await.unwrap();
    assert!(!report.clean(), "{report:?}");
    assert_eq!(report.redaction_purge_drift, 1, "{report:?}");
}

// ---- 5. principal deletion (the erasure end-state) ----------------------------

#[tokio::test]
async fn principal_deletion_pseudonymizes_the_actor_and_keeps_history() {
    let f = fixture();
    let root = seed_base(&f).await;
    // Alice writes a commit through her own token.
    let alice = m4b_common::seed_token_at(&f.t.db_path, "alice", "writer", None);
    // token_create audit rows through the route's own write path — one
    // for alice (whose details carry her name) and one for an unrelated
    // principal the deletion must not touch.
    let conn = naatre_store::Db::Sqlite(rusqlite::Connection::open(&f.t.db_path).unwrap());
    let (_, alice2_hash) = naatre_server::auth::mint_token();
    naatre_server::db::token_create(
        &conn,
        "alice",
        "writer",
        &alice2_hash,
        None,
        None,
        BASE_NS as i64,
    )
    .unwrap();
    let (_, bob_hash) = naatre_server::auth::mint_token();
    naatre_server::db::token_create(
        &conn,
        "bob",
        "reader",
        &bob_hash,
        None,
        None,
        BASE_NS as i64,
    )
    .unwrap();
    let blob = m4b_common::put_blob_on(&f.t.app, &alice, b"alice-content").await;
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &alice,
        &cs_body(
            json!([root.clone()]),
            json!([m4b_common::add("alice.txt", &blob, b"alice-content".len())]),
            "alice was here",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    let alice_cs = resp.body["id"].as_str().unwrap().to_string();

    // Admin deletes alice through the route.
    let resp = {
        let req = axum::http::Request::builder()
            .method("DELETE")
            .uri("/v1/principals/alice")
            .header(
                axum::http::header::AUTHORIZATION,
                format!("Bearer {}", f.admin),
            )
            .body(axum::body::Body::empty())
            .unwrap();
        let res = f.t.app.clone().oneshot(req).await.unwrap();
        let status = res.status();
        let body = http_body_util::BodyExt::collect(res.into_body())
            .await
            .unwrap()
            .to_bytes();
        Resp {
            status,
            body: serde_json::from_slice(&body).unwrap(),
        }
    };
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(
        resp.body["tokens_deleted"], 2,
        "the SQL-seeded row and the token_create row: {}",
        resp.body
    );

    // Alice's token no longer authenticates; the rows are gone.
    let resp = get_on(&f.t.app, "/v1/whoami", &alice).await;
    assert_eq!(resp.status, StatusCode::UNAUTHORIZED, "{}", resp.body);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM principals WHERE name = 'alice'"),
        0
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM tokens WHERE principal = 'alice'"),
        0
    );

    // The audit ACTOR is pseudonymized to an OPAQUE token (never
    // `deleted:<name>` — the name would survive the erasure); the token is
    // stable across every row of this deletion (linkage preserved) and
    // matches the `deleted:<16 hex>` shape; the deletion is audited.
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE principal = 'alice'"),
        0
    );
    let pseudonyms: Vec<String> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT group_concat(DISTINCT principal) FROM audit_log \
             WHERE principal LIKE 'deleted:%'",
            [],
            |r| {
                r.get::<_, String>(0)
                    .map(|s| s.split(',').map(str::to_string).collect())
            },
        )
        .unwrap();
    assert_eq!(pseudonyms.len(), 1, "one token per deletion event");
    let pseudo = &pseudonyms[0];
    let token = pseudo.strip_prefix("deleted:").unwrap();
    assert_eq!(token.len(), 16, "opaque token shape: {pseudo}");
    assert!(
        token.bytes().all(|b| b.is_ascii_hexdigit()),
        "opaque token is hex, not the name: {pseudo}"
    );
    assert!(
        f.count(&format!(
            "SELECT COUNT(*) FROM audit_log WHERE principal = '{pseudo}'"
        )) > 0,
        "the pseudonymized rows carry the actor history"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'principal_delete'"),
        1
    );
    // The erasure receipt carries the per-event pseudonym token, never the
    // plaintext name (fix-m10b2): the token links the receipt to the
    // rewritten actor rows.
    let receipt: String = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT details_json FROM audit_log WHERE action = 'principal_delete'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert!(
        receipt.contains(pseudo.as_str()),
        "the receipt carries the pseudonym token: {receipt}"
    );
    assert!(
        !receipt.contains("alice"),
        "the receipt never names the deleted principal: {receipt}"
    );

    // No token_create row names alice any more — checked STRUCTURALLY:
    // every details_json parses, and no string value anywhere in it is
    // the deleted name. The role and every other key survive; bob's row
    // is byte-for-byte untouched in meaning.
    fn contains_string_value(v: &Value, needle: &str) -> bool {
        match v {
            Value::String(s) => s == needle,
            Value::Array(a) => a.iter().any(|x| contains_string_value(x, needle)),
            Value::Object(o) => o.values().any(|x| contains_string_value(x, needle)),
            _ => false,
        }
    }
    let create_rows: Vec<(String, String)> = conn
        .query_all_map(
            &naatre_store::Sql::plain(
                "SELECT principal, details_json FROM audit_log                  WHERE action = 'token_create' ORDER BY id ASC",
            ),
            &naatre_store::pv![],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert_eq!(create_rows.len(), 2, "alice's and bob's rows only");
    for (principal, details) in &create_rows {
        let v: Value = serde_json::from_str(details).expect("details parse");
        assert!(
            !contains_string_value(&v, "alice"),
            "no token_create row names alice: {details}"
        );
        if principal == "bob" {
            assert_eq!(v["principal"], json!("bob"), "bob's row untouched");
            assert_eq!(v["role"], json!("reader"), "bob's role untouched");
        } else {
            assert!(
                principal.starts_with("deleted:"),
                "alice's actor column carries the token: {principal}"
            );
            assert_eq!(
                v["principal"].as_str().unwrap().len(),
                "deleted:0123456789abcdef".len(),
                "the name field's value is the per-event token: {details}"
            );
            assert_eq!(v["role"], json!("writer"), "the role survives");
            assert!(v["token_id"].is_u64(), "the other keys stay: {details}");
        }
    }

    // History attribution survives (the honest whole-changeset rule): the
    // changeset still serves under alice's principal.
    let resp = f
        .get(&format!("/v1/repos/{REPO}/changesets/{alice_cs}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["principal"], "alice");
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM changesets WHERE principal = 'alice'",
            &[]
        ),
        1,
        "changesets.principal is left as-is"
    );

    // Unknown principal → 404.
    let req = axum::http::Request::builder()
        .method("DELETE")
        .uri("/v1/principals/nobody")
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.admin),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(res.status(), StatusCode::NOT_FOUND);
}

// ---- 6. the tombstone write boundary (the M10b lens blocker) ------------------

/// Redacted content never resurrects: after a full purge, a same-content
/// re-PUT is 410 (bytes never re-enter the CAS), the batch PUT rejects the
/// entry, present reports the id `redacted` (never `missing` — no
/// re-upload invitation), a changeset referencing the blob is refused, the
/// identical purged changeset id cannot be recreated, a re-mark on the
/// purged tombstone RE-ARMS it with a new audit row, and a kind-mismatch
/// re-mark is refused. While a tombstone is ACTIVE the same refusals hold
/// (QA C4).
#[tokio::test]
async fn redacted_content_never_resurrects_and_remark_rearms() {
    let f = fixture();
    let root = seed_base(&f).await;

    // An orphan changeset + its blob, both tombstoned, purged to the end.
    let secret = b"resurrect-me".to_vec();
    let secret_hex = blob_hex(&secret);
    let orphan = f.commit(json!([root.clone()]), "s.txt", &secret).await;
    f.redact("changeset", &orphan, "orphan takedown").await;
    f.redact("blob", &secret_hex, "same content").await;
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_cs, 1, "{report:?}");
    f.tick_to(BASE_NS + 31 * 24 * 3600 * 1_000_000_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_blobs, 1, "{report:?}");
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&secret)]).unwrap(),
        vec![false],
        "the purge reclaimed the bytes"
    );

    // Raw re-PUT of the purged content: 410, and the bytes stay gone.
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{secret_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .body(axum::body::Body::from(secret.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(res.status(), StatusCode::GONE,);
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&secret)]).unwrap(),
        vec![false],
        "the refused re-PUT wrote no bytes"
    );

    // An ACTIVE tombstone refuses the re-PUT the same way (QA C4).
    let live_hex = blob_hex(b"live-content-A");
    f.redact("blob", &live_hex, "active tombstone").await;
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/{REPO}/objects/{live_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .body(axum::body::Body::from(b"live-content-A".to_vec()))
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(res.status(), StatusCode::GONE);

    // present: tombstoned ids are `redacted`, NEVER `missing` (the purged
    // id's bytes are gone — without the gate it would be invited back).
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/objects/present"),
        &f.writer,
        &json!({ "ids": [secret_hex, live_hex] }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["missing"], json!([]), "{}", resp.body);
    assert_eq!(
        resp.body["redacted"],
        json!([secret_hex, live_hex]),
        "{}",
        resp.body
    );

    // Batch PUT: the tombstoned entry is rejected per entry.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/objects"),
        &f.writer,
        &json!({ "objects": [ { "id": secret_hex, "data_b64": STANDARD.encode(&secret) } ] }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["stored"], json!([]), "{}", resp.body);
    assert_eq!(
        resp.body["rejected"][0]["code"],
        json!("redacted"),
        "{}",
        resp.body
    );

    // A changeset referencing the tombstoned blob: refused at create.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &cs_body(
            json!([root.clone()]),
            json!([m4b_common::add("s.txt", &secret_hex, secret.len())]),
            "reference the redacted blob",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");

    // The identical purged changeset id cannot be recreated (same parents +
    // same content hash to the same id).
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &cs_body(
            json!([root.clone()]),
            json!([m4b_common::add("s.txt", &secret_hex, secret.len())]),
            "commit",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");

    // Re-mark the purged blob tombstone: 200 `rearmed`, fresh marked_ns,
    // purged cleared, and a NEW audit row (never a silent no-op).
    let audits_before = f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact'");
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &f.admin,
        &json!({ "id": secret_hex, "kind": "blob", "reason": "re-arm after purge" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["rearmed"], true, "{}", resp.body);
    assert_eq!(resp.body["redaction"]["purged_ns"], Value::Null);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact'"),
        audits_before + 1,
        "the re-arm rows its own audit entry"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM redactions WHERE object_id = ?1 AND purged_ns IS NULL",
            &[&blob_id(&secret).0.as_slice()]
        ),
        1,
        "the re-armed tombstone stands active"
    );

    // A kind-mismatch re-mark is refused (the row stands as a blob).
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &f.admin,
        &json!({ "id": secret_hex, "kind": "changeset", "reason": "wrong kind" }),
    )
    .await;
    assert_eq!(
        resp.status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "{}",
        resp.body
    );

    // The re-armed tombstone re-enters the purge work list and re-purges
    // (nothing left to reclaim — the stamp converges), and fsck stays clean.
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_blobs, 1, "{report:?}");
    let fs = fsck::fsck(&f.t.state).await.unwrap();
    assert!(fs.clean(), "{fs:?}");
}

/// The mark's check-then-insert is ATOMIC (the M10b closure advisory): two
/// CONCURRENT POSTs racing the same fresh id yield exactly one 201, ONE
/// tombstone row, and ONE audit row — never a duplicate, never a
/// unique-violation 500. The loser either reached the freed write slot and
/// stood idempotent (200) or hit the busy slot's retryable `writer_busy`
/// (503, the designed contention answer); the statement-level conflict
/// clause ([`Sql::ignore`] in `redaction_mark`) closes the probe-to-insert
/// race for any caller outside the slot regardless.
#[tokio::test]
async fn concurrent_marks_yield_one_row_and_one_audit() {
    let f = fixture();
    let _root = seed_base(&f).await;
    let content = b"concurrent-mark-target".to_vec();
    let hex = m4b_common::put_blob_on(&f.t.app, &f.writer, &content).await;

    let mark = |hex: String, admin: String| {
        let app = f.t.app.clone();
        async move {
            let req = axum::http::Request::builder()
                .method("POST")
                .uri(format!("/v1/repos/{REPO}/redactions"))
                .header(axum::http::header::AUTHORIZATION, format!("Bearer {admin}"))
                .header(axum::http::header::CONTENT_TYPE, "application/json")
                .body(axum::body::Body::from(
                    json!({ "id": hex, "kind": "blob", "reason": "race" }).to_string(),
                ))
                .unwrap();
            app.oneshot(req).await.expect("in-memory request").status()
        }
    };
    let (a, b) = tokio::join!(
        mark(hex.clone(), f.admin.clone()),
        mark(hex.clone(), f.admin.clone())
    );
    let statuses = [a, b];
    assert_eq!(
        statuses
            .iter()
            .filter(|s| **s == StatusCode::CREATED)
            .count(),
        1,
        "exactly one mark inserts: {statuses:?}"
    );
    for s in statuses {
        assert!(
            [
                StatusCode::CREATED,
                StatusCode::OK,
                StatusCode::SERVICE_UNAVAILABLE
            ]
            .contains(&s),
            "each mark inserts, stands idempotent, or retries busy: {statuses:?}"
        );
    }
    assert_eq!(
        f.count("SELECT COUNT(*) FROM redactions"),
        1,
        "exactly one tombstone row"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact'"),
        1,
        "the audit row rides the 0→1 transition only"
    );
}

/// `/trees/{id}` masks a redacted entry (M10b SEC gate): the file name
/// serves, the blob pointer, size, and hash do not — including via a live
/// landed root tree. The untouched sibling subtree serves intact.
#[tokio::test]
async fn trees_route_masks_redacted_entries() {
    let f = fixture();
    let root = seed_base(&f).await;
    let resp = f
        .get(&format!("/v1/repos/{REPO}/changesets/{root}/root"))
        .await;
    let root_tree = resp.body["root_tree_id"].as_str().unwrap().to_string();

    f.redact("blob", &blob_hex(b"live-content-A"), "mask in trees")
        .await;

    // The live root tree: a.txt masked, dir intact.
    let resp = f.get(&format!("/v1/repos/{REPO}/trees/{root_tree}")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let entries = resp.body["entries"].as_array().unwrap();
    assert_eq!(entries.len(), 2, "{}", resp.body);
    let a = entries
        .iter()
        .find(|e| e["name"] == "a.txt")
        .expect("a.txt entry");
    assert_eq!(a["object_id"], Value::Null, "{}", resp.body);
    assert_eq!(a["size"], Value::Null, "{}", resp.body);
    assert_eq!(a["redacted"], true);
    let dir = entries
        .iter()
        .find(|e| e["name"] == "dir")
        .expect("dir entry");
    assert_eq!(dir["redacted"], false);
    let dir_id = dir["object_id"].as_str().unwrap().to_string();

    // The subtree below it never carried the redacted blob: intact.
    let resp = f.get(&format!("/v1/repos/{REPO}/trees/{dir_id}")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let x = &resp.body["entries"].as_array().unwrap()[0];
    assert_eq!(x["name"], "x.txt");
    assert_eq!(x["redacted"], false);
    assert!(x["object_id"].is_string(), "{}", resp.body);
    assert!(x["size"].is_u64(), "{}", resp.body);
}

/// The diff masks PER SIDE (the published per-field contract): a redacted
/// `a`-side blob nulls `a_blob` only; the `b` side keeps serving its
/// pointer. Both sides redacted → both null, one `redacted` flag.
#[tokio::test]
async fn diff_masks_only_the_redacted_side() {
    let f = fixture();
    let root = seed_base(&f).await;
    let v1 = blob_hex(b"live-content-A");
    let v2_content = b"second-version".to_vec();
    let v2 = blob_hex(&v2_content);
    let c2 = f.commit(json!([root.clone()]), "a.txt", &v2_content).await;

    // No tombstone: both pointers serve, not redacted.
    let resp = f
        .get(&format!("/v1/repos/{REPO}/diff?a={root}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let e = &resp.body["changes"].as_array().unwrap()[0];
    assert_eq!(e["a_blob"], json!(v1));
    assert_eq!(e["b_blob"], json!(v2));
    assert_eq!(e["redacted"], false);

    // Redact the a-side only: a_blob nulls, b_blob serves.
    f.redact("blob", &v1, "a-side takedown").await;
    let resp = f
        .get(&format!("/v1/repos/{REPO}/diff?a={root}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let e = &resp.body["changes"].as_array().unwrap()[0];
    assert_eq!(e["a_blob"], Value::Null, "{}", resp.body);
    assert_eq!(e["b_blob"], json!(v2), "{}", resp.body);
    assert_eq!(e["redacted"], true);

    // Redact the b-side too: both pointers withheld.
    f.redact("blob", &v2, "b-side takedown").await;
    let resp = f
        .get(&format!("/v1/repos/{REPO}/diff?a={root}&b={c2}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let e = &resp.body["changes"].as_array().unwrap()[0];
    assert_eq!(e["a_blob"], Value::Null);
    assert_eq!(e["b_blob"], Value::Null);
    assert_eq!(e["redacted"], true);
}

/// Cross-repo purge refcount, gc_pass DRIVEN (QA C1): a blob tombstoned in
/// repo A stays while repo B still references it (B serves byte-identically,
/// the CAS keeps the bytes, no stamp); once every repo's references are
/// gone the purge reclaims it — the exclusive-blob arm stamps the
/// self-tombstoned blob in the SAME pass, counted in the blob unit. The
/// write boundary is PER-REPO (the pinned decision): B re-PUTs the shared
/// content while A's tombstone stands ACTIVE, and a repo that never
/// tombstoned the id re-PUTs it after the purge reclaimed the bytes — while
/// A keeps refusing both the PUT and the GET, its purged tombstone gating.
#[tokio::test]
async fn cross_repo_purge_needs_every_reference_gone() {
    let f = fixture();
    let _root = seed_base(&f).await;
    m4b_common::seed_repo_at(&f.t.db_path, "other");
    let other_token = m4b_common::seed_token_at(&f.t.db_path, "other-p", "writer", None);
    let shared = b"cross-repo-shared".to_vec();
    let shared_hex = blob_hex(&shared);
    let _ = m4b_common::put_blob_on(&f.t.app, &other_token, &shared).await;
    let _ = m4b_common::put_blob_on(&f.t.app, &f.writer, &shared).await;
    // Repo A: an unlanded orphan carrying the shared blob.
    let a_orphan = f.commit(json!([]), "s.txt", &shared).await;
    // Repo B: a root commit carrying the same bytes.
    let resp = post_json_on(
        &f.t.app,
        "/v1/repos/other/changesets",
        &other_token,
        &cs_body(
            json!([]),
            json!([m4b_common::add("s.txt", &shared_hex, shared.len())]),
            "root",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    let b_root = resp.body["id"].as_str().unwrap().to_string();

    // Redact in A only (the changeset AND the blob).
    f.redact("changeset", &a_orphan, "a-side takedown").await;
    f.redact("blob", &shared_hex, "a-side takedown").await;
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_cs, 1, "{report:?}");
    assert_eq!(
        report.redactions_purged_blobs, 0,
        "repo B still references the blob: {report:?}"
    );
    let stamp: Option<i64> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT purged_ns FROM redactions WHERE kind = 'blob' AND object_id = ?1",
            [&blob_id(&shared).0[..]],
            |r| r.get(0),
        )
        .unwrap();
    assert!(stamp.is_none(), "the blob tombstone stays active");
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&shared)]).unwrap(),
        vec![true],
        "the bytes stay for repo B"
    );
    // Repo B's reads keep working.
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/other/objects/{shared_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {other_token}"),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(res.status(), StatusCode::OK);

    // The write boundary is PER-REPO: A's ACTIVE blob tombstone does not
    // bind B — B (no tombstone of its own) re-PUTs the same content and it
    // succeeds, never a 410.
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/other/objects/{shared_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {other_token}"),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(shared.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert!(
        res.status().is_success(),
        "B's re-PUT is not bound by A's tombstone: {}",
        res.status()
    );

    // Redact in B too — B's tombstones go through B's own admin surface
    // (the gate is repo-scoped) — then drive the purge past the upload
    // grace (the dying tree's collection may hold the blob one pass — the
    // same never-dangling lag the single-repo purge pin documents).
    for (kind, id) in [("changeset", b_root.clone()), ("blob", shared_hex.clone())] {
        let resp = post_json_on(
            &f.t.app,
            "/v1/repos/other/redactions",
            &f.admin,
            &json!({ "id": id, "kind": kind, "reason": "b-side takedown" }),
        )
        .await;
        assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    }
    let mut blob_purged_total = 0;
    for _ in 0..4 {
        f.tick_to(f.t.clock.load(Ordering::Relaxed) + 31 * 24 * 3600 * 1_000_000_000);
        let report = gc_pass(&f.t.state).await.unwrap();
        blob_purged_total += report.redactions_purged_blobs;
        if blob_purged_total >= 1 {
            break;
        }
    }
    assert_eq!(
        blob_purged_total, 1,
        "the blob reclaims exactly once, in the BLOB unit"
    );
    assert_eq!(
        f.t.state.cas.has(&[blob_id(&shared)]).unwrap(),
        vec![false],
        "the bytes are reclaimed"
    );
    // Repo B's read of the purged id: 410 (the gate covers purged rows).
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/other/objects/{shared_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {other_token}"),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(res.status(), StatusCode::GONE);

    // A repo that NEVER tombstoned the id re-PUTs the purged content (the
    // per-repo boundary again, now against PURGED tombstones): repo C's PUT
    // succeeds and the bytes serve there — while A still refuses both the
    // PUT and the GET, and A's present reports the id redacted, never
    // missing.
    m4b_common::seed_repo_at(&f.t.db_path, "third");
    let third_token = m4b_common::seed_token_at(&f.t.db_path, "third-p", "writer", None);
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/third/objects/{shared_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {third_token}"),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(shared.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(
        res.status(),
        StatusCode::CREATED,
        "C's re-PUT of purged content succeeds: {}",
        res.status()
    );
    // C even builds history on it — the changeset boundary is per-repo too:
    // a root commit carrying the reclaimed content is CREATED in C.
    let resp = post_json_on(
        &f.t.app,
        "/v1/repos/third/changesets",
        &third_token,
        &cs_body(
            json!([]),
            json!([m4b_common::add("s.txt", &shared_hex, shared.len())]),
            "root",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(format!("/v1/repos/third/objects/{shared_hex}"))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {third_token}"),
        )
        .body(axum::body::Body::empty())
        .unwrap();
    let res = f.t.app.clone().oneshot(req).await.unwrap();
    assert_eq!(res.status(), StatusCode::OK, "C serves the bytes");
    for (repo, token, status) in [
        (REPO, f.writer.as_str(), StatusCode::GONE),
        ("other", other_token.as_str(), StatusCode::GONE),
    ] {
        let req = axum::http::Request::builder()
            .method("PUT")
            .uri(format!("/v1/repos/{repo}/objects/{shared_hex}"))
            .header(axum::http::header::AUTHORIZATION, format!("Bearer {token}"))
            .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
            .body(axum::body::Body::from(shared.clone()))
            .unwrap();
        let res = f.t.app.clone().oneshot(req).await.unwrap();
        assert_eq!(res.status(), status, "{repo}'s re-PUT stays refused");
    }
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/objects/present"),
        &f.writer,
        &json!({ "ids": [shared_hex] }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["missing"], json!([]), "{}", resp.body);
    assert_eq!(resp.body["redacted"], json!([shared_hex]), "{}", resp.body);
    let resp = post_json_on(
        &f.t.app,
        "/v1/repos/third/objects/present",
        &third_token,
        &json!({ "ids": [shared_hex] }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["missing"], json!([]), "{}", resp.body);
    assert_eq!(resp.body["redacted"], json!([]), "{}", resp.body);

    // fsck stays clean over the whole cross-repo flow: C's post-purge
    // objects row (touched AFTER the stamp) is a legitimate re-upload, not
    // purge drift.
    let fs = fsck::fsck(&f.t.state).await.unwrap();
    assert!(fs.clean(), "{fs:?}");
}

/// /log cursor resume past a since-redacted node (QA C2): a stale cursor
/// naming a since-redacted id resumes at its parent; a valid cursor skips
/// an inner node redacted after the page was served. No hole is presented
/// as history.
#[tokio::test]
async fn log_cursor_resumes_past_a_since_redacted_node() {
    let f = fixture();
    let root = seed_base(&f).await;
    let c2 = f.commit(json!([root.clone()]), "b.txt", b"second").await;
    let c3 = f.commit(json!([c2.clone()]), "c.txt", b"third").await;

    // Page 1 from the tip, one entry: [c3], cursor after:c3.
    let resp = f
        .get(&format!("/v1/repos/{REPO}/log?from={c3}&limit=1"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, vec![c3.as_str()]);
    let cursor = resp.body["next_cursor"].as_str().unwrap().to_string();

    // The inner node goes redacted AFTER the page was served.
    f.redact("changeset", &c2, "inner takedown").await;

    // Resuming the valid cursor skips the since-redacted node: [root].
    let resp = f
        .get(&format!("/v1/repos/{REPO}/log?cursor={cursor}"))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, vec![root.as_str()], "{}", resp.body);

    // A STALE cursor naming the since-redacted id itself resumes cleanly
    // at that node's parent (it was a served entry once).
    let stale = format!("after%3A{c2}");
    let resp = f.get(&format!("/v1/repos/{REPO}/log?cursor={stale}")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(ids, vec![root.as_str()], "{}", resp.body);
}

/// path_history_scan's anti-join drops a tombstoned node with the
/// tombstone PRESENT (QA C3): with the derived kind unregistered the scan
/// answers the page, and the tombstoned touch never serves.
#[tokio::test]
async fn path_history_scan_drops_the_tombstoned_node() {
    let f = fixture();
    let root = seed_base(&f).await;
    let c2 = f.commit(json!([root.clone()]), "a.txt", b"v2").await;
    let c3 = f.commit(json!([c2.clone()]), "a.txt", b"v3").await;

    // Unregister the derived kind: the scan fallback answers ?path=.
    m4b_common::conn_at(&f.t.db_path)
        .execute("DELETE FROM derive_kinds WHERE name = 'path_history'", [])
        .unwrap();

    f.redact("changeset", &c2, "scan pin takedown").await;

    let resp = f
        .get(&format!("/v1/repos/{REPO}/log?path={}", urlencode("a.txt")))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let ids: Vec<&str> = resp.body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["id"].as_str().unwrap())
        .collect();
    assert_eq!(
        ids,
        vec![c3.as_str(), root.as_str()],
        "the scan drops the tombstoned touch: {}",
        resp.body
    );
    assert_eq!(resp.body["degraded"], false, "{}", resp.body);
}

// ---- 9. fix-m10b2 pins --------------------------------------------------------

/// The changeset-id resurrection pin, DRIVEN (fix-m10b2): recreating the
/// EXACT canonical body of a purged, blob-clean changeset — same parents,
/// author, message, an explicit `timestamp_ns` equal to the original's,
/// same changes — hashes to the same id, and the POST answers 410
/// `object_redacted` from the CS-ID arm: the refusal message names the
/// changeset id, and no rows are written. The prior form of this pin
/// omitted `timestamp_ns`, so the recreation hashed to a DIFFERENT id and
/// its 410 came from the blob-reference arm — a leg the pin never drove.
#[tokio::test]
async fn purged_changeset_id_resurrection_refuses_from_the_cs_id_arm() {
    let f = fixture();
    let root = seed_base(&f).await;
    let content = b"exact-body-target".to_vec();
    let blob = f.put_blob(&content).await;
    // The original: the fixture clock sits at BASE_NS, so the omitted
    // timestamp_ns is exactly BASE_NS.
    let (cs, _) = f
        .create_ok(&cs_body(
            json!([root.clone()]),
            json!([m4b_common::add("exact.txt", &blob, content.len())]),
            "exact body",
        ))
        .await;

    // Tombstone the CHANGESET only (the blob stays clean: the refusal
    // cannot be mistaken for the blob arm) and purge it to the end.
    f.redact("changeset", &cs, "exact takedown").await;
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_cs, 1, "{report:?}");
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM changesets WHERE id = ?1",
            &[&hex_id(&cs).0.as_slice()]
        ),
        0,
        "the purge removed the row"
    );

    // Recreate the exact canonical body with the explicit stamp: same id
    // → the cs-id arm refuses.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &m4b_common::cs_body_at(
            json!([root.clone()]),
            json!([m4b_common::add("exact.txt", &blob, content.len())]),
            "exact body",
            BASE_NS,
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
    assert!(
        resp.body["error"]["message"]
            .as_str()
            .unwrap()
            .contains(&cs),
        "the refusal names the changeset id (the cs-id arm): {}",
        resp.body
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM changesets WHERE id = ?1",
            &[&hex_id(&cs).0.as_slice()]
        ),
        0,
        "the refused create wrote no rows"
    );
}

/// Re-arm convergence for a FULLY purged changeset (fix-m10b2): the purge
/// deleted the changeset row, so the work-list selector (which JOINs
/// `changesets`) would never pick the re-armed tombstone again — the
/// re-arm stamps `purged_ns` to the re-arm call's time instead, the
/// tombstone reports purged, the audit row still rows, and the write gate
/// stays armed against the exact id.
#[tokio::test]
async fn rearm_of_a_fully_purged_changeset_stays_purged_and_gated() {
    let f = fixture();
    let root = seed_base(&f).await;
    let cs = f
        .commit(json!([root.clone()]), "t.txt", b"rearm-target")
        .await;
    f.redact("changeset", &cs, "purge me").await;
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_cs, 1, "{report:?}");

    f.tick_to(BASE_NS + 1_000);
    let audits_before = f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact'");
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &f.admin,
        &json!({ "id": cs, "kind": "changeset", "reason": "re-arm after purge" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["rearmed"], true, "{}", resp.body);
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'redact'"),
        audits_before + 1,
        "the re-arm rows its own audit entry"
    );
    // purged_ns stays NON-null, re-stamped to this call's time: nothing
    // remains to purge, and the state stays honest.
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM redactions \
             WHERE object_id = ?1 AND purged_ns IS NOT NULL AND purged_ns > ?2",
            &[&hex_id(&cs).0.as_slice(), &(BASE_NS as i64)]
        ),
        1,
        "the re-armed tombstone reports purged at the re-arm stamp"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM redactions WHERE object_id = ?1 AND purged_ns IS NULL",
            &[&hex_id(&cs).0.as_slice()]
        ),
        0,
        "the tombstone never poses as purge-pending"
    );

    // A later pass re-selects nothing for the id, and the write gate still
    // refuses the exact recreation (same body, explicit stamp).
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_cs, 0, "{report:?}");
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &m4b_common::cs_body_at(
            json!([root.clone()]),
            json!([m4b_common::add(
                "t.txt",
                &blob_hex(b"rearm-target"),
                b"rearm-target".len()
            )]),
            "commit",
            BASE_NS,
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
}

/// Race-orphan reclamation (fix-m10b2): the forced upload race (probe
/// passes, mark lands, bytes written, gate 410s) leaves the CAS holding
/// bytes no row names. The refusal condemns them into the GC ledger
/// inside its own transaction, and a pass reclaims them. With another
/// repo holding a live reference, the same refusal does NOT reclaim that
/// repo's reachable copy.
#[tokio::test]
async fn race_orphan_bytes_are_condemned_at_the_refusal_and_reclaimed() {
    let f = fixture();
    let _root = seed_base(&f).await;
    let content = b"race-orphan".to_vec();
    let oid = blob_id(&content);

    // (a) The forced race: the bytes are in the CAS (an earlier upload's
    // step ran before the mark), the mark lands, and the upload's
    // in-transaction re-check refuses — `record_object` IS that re-check
    // (the M10a race pin's entry point; the route-level probe passed
    // before the mark landed).
    let hex = f.put_blob(&content).await;
    assert_eq!(hex, oid.to_hex());
    f.redact("blob", &oid.to_hex(), "race orphan").await;
    let err = naatre_server::routes::objects::record_object(
        &f.t.state,
        REPO,
        oid,
        content.len() as i64,
        1,
    )
    .await
    .unwrap_err();
    assert_eq!(
        err.code(),
        naatre_server::api_code::ApiCode::ObjectRedacted,
        "{err:?}"
    );
    assert!(
        err.to_string().contains(REPO),
        "the refusal names the real repo: {err}"
    );
    assert_eq!(
        f.t.state.cas.has(&[oid]).unwrap(),
        vec![true],
        "the orphan bytes sit in the CAS"
    );
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob' AND id = ?1",
            &[&oid.0.as_slice()]
        ),
        1,
        "the refusal condemned the orphan into the ledger"
    );

    // A GC pass past the upload grace reclaims the bytes (the redaction
    // purge phase's blob rule: active tombstone, no reference anywhere,
    // no upload touch inside the window — the objects row was touched at
    // BASE_NS, so the clock moves a grace past it).
    f.tick_to(BASE_NS + 31 * 24 * 3600 * 1_000_000_000);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(report.redactions_purged_blobs, 1, "{report:?}");
    assert_eq!(
        f.t.state.cas.has(&[oid]).unwrap(),
        vec![false],
        "the GC pass reclaimed the orphan bytes"
    );

    // (b) The same refusal with another repo holding a live reference: the
    // bytes are that repo's reachable copy and stay; the repo keeps
    // serving them. The blob re-enters through the other repo's own write
    // boundary (gate-clean there: CAS bytes + objects row).
    m4b_common::seed_repo_at(&f.t.db_path, "other-repo");
    let put = axum::http::Request::builder()
        .method("PUT")
        .uri(format!("/v1/repos/other-repo/objects/{}", oid.to_hex()))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(content.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(put).await.unwrap();
    assert_eq!(res.status(), StatusCode::CREATED);
    let resp = post_json_on(
        &f.t.app,
        "/v1/repos/other-repo/changesets",
        &f.writer,
        &cs_body(
            json!([]),
            json!([m4b_common::add("shared.txt", &oid.to_hex(), content.len())]),
            "reference the shared blob",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    // The forced race again, in REPO this time. The re-mark RE-ARMS the
    // tombstone (a)'s purge stamped it — 200 `rearmed`; the other repo's
    // copy is untouched (the gate is repo-scoped).
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/redactions"),
        &f.admin,
        &json!({ "id": oid.to_hex(), "kind": "blob", "reason": "orphan again" }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    assert_eq!(resp.body["rearmed"], true, "{}", resp.body);
    let err = naatre_server::routes::objects::record_object(
        &f.t.state,
        REPO,
        oid,
        content.len() as i64,
        1,
    )
    .await
    .unwrap_err();
    assert_eq!(err.code(), naatre_server::api_code::ApiCode::ObjectRedacted);
    let report = gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        report.redactions_purged_blobs, 0,
        "the live reference spares the bytes: {report:?}"
    );
    assert_eq!(
        f.t.state.cas.has(&[oid]).unwrap(),
        vec![true],
        "the other repo's reachable copy stays"
    );
    let resp = f
        .get(&format!("/v1/repos/other-repo/objects/{}", oid.to_hex()))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
}

/// Ref writes onto a tombstoned target answer 410 `object_redacted`
/// (fix-m10b2), create and move alike — a ref would stall that
/// changeset's purge forever — and a clean target still writes.
#[tokio::test]
async fn ref_writes_onto_a_tombstoned_target_answer_410() {
    let f = fixture();
    let root = seed_base(&f).await;
    let cs = f
        .commit(json!([root.clone()]), "r.txt", b"ref-target")
        .await;

    // Clean target: the create succeeds.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/refs/topic"),
        &f.admin,
        &json!({ "target": cs, "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);

    // Tombstone the target: the MOVE now refuses, and so does a CREATE
    // naming the same target.
    f.redact("changeset", &cs, "ref takedown").await;
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/refs/topic"),
        &f.admin,
        &json!({ "target": cs, "expected": { "version": 1 } }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/refs/other"),
        &f.admin,
        &json!({ "target": cs, "expected": null }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
    // The refused writes left no rows.
    assert_eq!(f.count("SELECT COUNT(*) FROM refs WHERE name = 'other'"), 0);
    let version: i64 = m4b_common::conn_at(&f.t.db_path)
        .query_row("SELECT version FROM refs WHERE name = 'topic'", [], |r| {
            r.get(0)
        })
        .unwrap();
    assert_eq!(version, 1, "the refused move did not bump the version");
}

/// The changeset fetch's delta mask (fix-m10b2): a change whose blob is
/// tombstoned in this repo serves with `redacted: true` and null
/// blob/size; unredacted entries serve byte-identically to the old
/// contract (plus `redacted: false`).
#[tokio::test]
async fn changeset_fetch_masks_tombstoned_delta_entries() {
    let f = fixture();
    let root = seed_base(&f).await;
    let secret = b"cs-delta-secret".to_vec();
    let secret_hex = f.put_blob(&secret).await;
    let clean = b"cs-delta-clean".to_vec();
    let clean_hex = f.put_blob(&clean).await;
    let (cs, _) = f
        .create_ok(&cs_body(
            json!([root.clone()]),
            json!([
                m4b_common::add("clean.txt", &clean_hex, clean.len()),
                m4b_common::add("s.txt", &secret_hex, secret.len()),
            ]),
            "two blobs",
        ))
        .await;
    f.redact("blob", &secret_hex, "delta takedown").await;

    let resp = f.get(&format!("/v1/repos/{REPO}/changesets/{cs}")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let changes = resp.body["changes"].as_array().unwrap();
    assert_eq!(changes.len(), 2, "{}", resp.body);
    // The masked entry: path-level fact serves, pointer and size do not —
    // nothing left to subtract.
    assert_eq!(changes[1]["path"], "s.txt");
    assert_eq!(changes[1]["blob"], Value::Null, "{}", resp.body);
    assert_eq!(changes[1]["size"], Value::Null);
    assert_eq!(changes[1]["exec"], Value::Null, "exec is withheld too");
    assert_eq!(changes[1]["redacted"], true);
    // The unredacted entry is unchanged.
    assert_eq!(changes[0]["path"], "clean.txt");
    assert_eq!(changes[0]["blob"], json!(clean_hex));
    assert_eq!(changes[0]["size"], json!(clean.len()));
    assert_eq!(changes[0]["exec"], json!(false));
    assert_eq!(changes[0]["redacted"], false);
}

/// The dir-aggregate mask (fix-m10b2): a dir entry's stored size is the
/// recursive total, so listing the parent and subtracting the children
/// would recover a redacted file's exact byte length. A dir with a
/// tombstoned descendant serves size-null (the name and the child tree
/// pointer still serve); sibling dirs keep their sizes; and the masked
/// file entry inside the dir keeps the file-level mask.
#[tokio::test]
async fn dir_aggregate_size_masks_redacted_descendants() {
    let f = fixture();
    let root = seed_base(&f).await;
    let secret = b"aggregate-secret!".to_vec();
    let secret_hex = f.put_blob(&secret).await;
    let clean = b"clean!".to_vec();
    let clean_hex = f.put_blob(&clean).await;
    let sib = b"sib!".to_vec();
    let sib_hex = f.put_blob(&sib).await;
    let (_cs, root_tree) = f
        .create_ok(&cs_body(
            json!([root.clone()]),
            json!([
                m4b_common::add("d/clean.txt", &clean_hex, clean.len()),
                m4b_common::add("d/secret.txt", &secret_hex, secret.len()),
                m4b_common::add("sib/other.txt", &sib_hex, sib.len()),
            ]),
            "dirs",
        ))
        .await;
    f.redact("blob", &secret_hex, "aggregate takedown").await;

    // The ROOT listing: `d` serves size-null (its aggregate embeds the
    // redacted file's length); `sib` keeps its size; the arithmetic
    // recovers nothing.
    let resp = f.get(&format!("/v1/repos/{REPO}/trees/{root_tree}")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let entries = resp.body["entries"].as_array().unwrap();
    let d = entries.iter().find(|e| e["name"] == "d").unwrap();
    assert_eq!(d["kind"], "dir");
    assert_eq!(d["size"], Value::Null, "the aggregate mask: {}", resp.body);
    assert_eq!(
        d["redacted"], false,
        "the dir's own tree id is not redacted"
    );
    assert!(
        d["object_id"].is_string(),
        "the child tree pointer serves: {}",
        resp.body
    );
    let sib_entry = entries.iter().find(|e| e["name"] == "sib").unwrap();
    assert_eq!(
        sib_entry["size"],
        json!(sib.len()),
        "sibling dirs keep their sizes"
    );
    assert_eq!(sib_entry["redacted"], false);

    // Inside `d`: the redacted file entry keeps the file-level mask; the
    // clean sibling file serves unchanged.
    let d_tree = d["object_id"].as_str().unwrap();
    let resp = f.get(&format!("/v1/repos/{REPO}/trees/{d_tree}")).await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let entries = resp.body["entries"].as_array().unwrap();
    let masked = entries.iter().find(|e| e["name"] == "secret.txt").unwrap();
    assert_eq!(masked["object_id"], Value::Null);
    assert_eq!(masked["size"], Value::Null);
    assert_eq!(masked["exec"], Value::Null, "exec is withheld too");
    assert_eq!(masked["redacted"], true);
    let ok = entries.iter().find(|e| e["name"] == "clean.txt").unwrap();
    assert_eq!(ok["size"], json!(clean.len()));
    assert_eq!(ok["exec"], json!(false));
    assert_eq!(ok["redacted"], false);
}

/// The three snapshot gates, both directions (fix-m10b2): a snapshot POST
/// whose closure carries a tombstoned blob answers 410 and writes no
/// rows; a snapshot blob PUT of a tombstoned id answers 410; the
/// snapshot-tree listing masks a tombstoned entry and size-nulls a dir
/// whose aggregate would embed a redacted size — and the clean closure
/// passes with 201/200 and unmasked entries.
#[tokio::test]
async fn snapshot_gates_refuse_redacted_and_pass_clean() {
    let f = fixture();
    let _root = seed_base(&f).await;
    let secret = b"snap-gate-secret".to_vec();
    let secret_hex = blob_hex(&secret);
    let clean = b"snap-gate-clean".to_vec();
    let clean_hex = blob_hex(&clean);
    // History-put so the tombstone has repo-visible targets (the mark
    // surface accepts them).
    m4b_common::put_blob_on(&f.t.app, &f.writer, &secret).await;
    m4b_common::put_blob_on(&f.t.app, &f.writer, &clean).await;
    f.redact("blob", &secret_hex, "snapshot gate").await;

    let file = |name: &str, blob: Id, size: u64| TreeEntry {
        name: name.to_string(),
        kind: EntryKind::File,
        object: blob,
        size,
        exec: false,
    };
    // PUT blob: the tombstoned id refuses BEFORE any ephemeral write…
    let put = axum::http::Request::builder()
        .method("PUT")
        .uri(format!(
            "/v1/repos/{REPO}/snapshots/{:062x}{:02x}/objects/{secret_hex}",
            0u128, 0xAA
        ))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(secret.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(put).await.unwrap();
    assert_eq!(res.status(), StatusCode::GONE);
    // …and the clean id PUTs fine.
    let clean_snap = format!("{:062x}{:02x}", 0u128, 0xBB);
    let put = axum::http::Request::builder()
        .method("PUT")
        .uri(format!(
            "/v1/repos/{REPO}/snapshots/{clean_snap}/objects/{clean_hex}"
        ))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(clean.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(put).await.unwrap();
    assert_eq!(res.status(), StatusCode::CREATED);

    // POST closure: a tree carrying the tombstoned blob refuses 410 with
    // zero rows written; the clean tree posts 201.
    let bad_tree = Tree {
        entries: vec![
            file("clean.txt", hex_id(&clean_hex), clean.len() as u64),
            file("secret.txt", hex_id(&secret_hex), secret.len() as u64),
        ],
    };
    let bad_id = tree::tree_id(&bad_tree).unwrap();
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.writer,
        &json!({
            "snap_id": format!("{:062x}{:02x}", 0u128, 0xCC),
            "tree_id": bad_id.to_hex(),
            "trees": [STANDARD.encode(tree::encode(&bad_tree).unwrap())],
            "ttl_secs": 3_600u64,
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");

    let good_tree = Tree {
        entries: vec![file("clean.txt", hex_id(&clean_hex), clean.len() as u64)],
    };
    let good_tree_id = tree::tree_id(&good_tree).unwrap();
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.writer,
        &json!({
            "snap_id": clean_snap,
            "tree_id": good_tree_id.to_hex(),
            "trees": [STANDARD.encode(tree::encode(&good_tree).unwrap())],
            "ttl_secs": 3_600u64,
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);

    // Snapshot-tree listing: the clean entry serves unmasked…
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/snapshots/{clean_snap}/trees/{}",
            good_tree_id.to_hex()
        ))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let entries = resp.body["entries"].as_array().unwrap();
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0]["redacted"], false);
    assert_eq!(entries[0]["size"], json!(clean.len()));

    // …and the masking arm drives a posted-BEFORE-the-mark ordering (a
    // snapshot blob PUT of redacted content is refused, so the tombstone
    // must postdate the POST): post the snapshot, mark both blobs, list
    // third. The closure carries a masked FILE at the root and a dir whose
    // subtree holds a second marked blob — the file-level mask and the
    // dir-aggregate mask in one listing.
    let pre = b"snap-pre-tombstone".to_vec();
    let pre_hex = blob_hex(&pre);
    m4b_common::put_blob_on(&f.t.app, &f.writer, &pre).await;
    // zoo's file is a SECOND redacted blob: the dir-aggregate direction
    // (the snapshot listing's dir entry serves size-null).
    let pre2 = b"snap-pre-zoo-blob".to_vec();
    let pre2_hex = blob_hex(&pre2);
    m4b_common::put_blob_on(&f.t.app, &f.writer, &pre2).await;
    let pre_snap = format!("{:062x}{:02x}", 0u128, 0xDD);
    let zoo_tree = Tree {
        entries: vec![TreeEntry {
            name: "inner.txt".into(),
            kind: EntryKind::File,
            object: hex_id(&pre2_hex),
            size: pre2.len() as u64,
            exec: false,
        }],
    };
    let zoo_id = tree::tree_id(&zoo_tree).unwrap();
    let dir_tree = zoo_tree;
    let dir_id = zoo_id;
    let outer_tree = Tree {
        entries: vec![
            TreeEntry {
                name: "pre.txt".into(),
                kind: EntryKind::File,
                object: hex_id(&pre_hex),
                size: pre.len() as u64,
                exec: false,
            },
            TreeEntry {
                name: "zoo".into(),
                kind: EntryKind::Dir,
                object: dir_id,
                size: pre2.len() as u64,
                exec: false,
            },
        ],
    };
    let outer_id = tree::tree_id(&outer_tree).unwrap();
    let put = axum::http::Request::builder()
        .method("PUT")
        .uri(format!(
            "/v1/repos/{REPO}/snapshots/{pre_snap}/objects/{pre_hex}"
        ))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(pre.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(put).await.unwrap();
    assert_eq!(res.status(), StatusCode::CREATED);
    let put = axum::http::Request::builder()
        .method("PUT")
        .uri(format!(
            "/v1/repos/{REPO}/snapshots/{pre_snap}/objects/{pre2_hex}"
        ))
        .header(
            axum::http::header::AUTHORIZATION,
            format!("Bearer {}", f.writer),
        )
        .header(axum::http::header::CONTENT_TYPE, "application/octet-stream")
        .body(axum::body::Body::from(pre2.clone()))
        .unwrap();
    let res = f.t.app.clone().oneshot(put).await.unwrap();
    assert_eq!(res.status(), StatusCode::CREATED);
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/snapshots"),
        &f.writer,
        &json!({
            "snap_id": pre_snap,
            "tree_id": outer_id.to_hex(),
            "trees": [
                STANDARD.encode(tree::encode(&outer_tree).unwrap()),
                STANDARD.encode(tree::encode(&dir_tree).unwrap()),
            ],
            "ttl_secs": 3_600u64,
        }),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    f.redact("blob", &pre_hex, "list takedown").await;
    f.redact("blob", &pre2_hex, "list takedown (zoo)").await;
    let resp = f
        .get(&format!(
            "/v1/repos/{REPO}/snapshots/{pre_snap}/trees/{}",
            outer_id.to_hex()
        ))
        .await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);
    let entries = resp.body["entries"].as_array().unwrap();
    let masked = entries.iter().find(|e| e["name"] == "pre.txt").unwrap();
    assert_eq!(masked["object_id"], Value::Null, "{}", resp.body);
    assert_eq!(masked["size"], Value::Null);
    assert_eq!(masked["exec"], Value::Null);
    assert_eq!(masked["redacted"], true);
    // The dir-aggregate direction: zoo's stored size is the recursive
    // total embedding the redacted inner.txt's exact length — it serves
    // size-null, name and child-tree pointer intact.
    let zoo = entries.iter().find(|e| e["name"] == "zoo").unwrap();
    assert_eq!(zoo["redacted"], false, "{}", resp.body);
    assert_eq!(
        zoo["size"],
        Value::Null,
        "the aggregate mask: {}",
        resp.body
    );
    assert!(
        zoo["object_id"].is_string(),
        "the child tree pointer serves"
    );
    assert_eq!(zoo["exec"], json!(false), "dirs are not redacted entries");
}

/// The land's rebase-body tombstone gate (fix-m10b2), both directions:
/// (a) a land whose rebase writes or references a tombstoned blob answers
/// 410 `object_redacted` and writes no rows (the pending row rolls back
/// with the transaction); (b) a byte-identical rebase whose rebased id is
/// tombstoned-purged (the exact id the rebase would reproduce — the same
/// canonical body POST /changesets refuses) answers 410 from the cs-id
/// arm. Before the gate the rebase wrote merged blobs and inserted the
/// rebased changeset ungated.
#[tokio::test]
async fn land_refuses_a_tombstoned_rebase_body() {
    let f = fixture();
    let root = seed_base(&f).await;

    // Fork: main advances to cx (fast-forward adopt — same parent, no
    // rewrite), while ch stays a side head rooted below main's tip, so
    // landing it REBASES.
    let x_content = b"branch-x".to_vec();
    let _x_blob = f.put_blob(&x_content).await;
    let cx = f.commit(json!([root.clone()]), "x.txt", &x_content).await;
    let resp = f.land(&cx, "k-main").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);

    let h_content = b"branch-h".to_vec();
    let h_blob = f.put_blob(&h_content).await;
    let ch = f.commit(json!([root.clone()]), "h.txt", &h_content).await;
    let changesets_before = f.count("SELECT COUNT(*) FROM changesets");

    // (a) Redact one of the land's blobs (active): the rebase references
    // it, so the land refuses before any write.
    f.redact("blob", &h_blob, "land blob takedown").await;
    let resp = f.land(&ch, "k-a").await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
    let msg = resp.body["error"]["message"].as_str().unwrap();
    assert!(msg.contains(REPO), "the refusal names the repo: {msg}");
    assert!(msg.contains(&h_blob), "it names the blob id: {msg}");
    assert!(
        msg.contains("referencing it is refused"),
        "singular blob-arm wording: {msg}"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM changesets"),
        changesets_before,
        "the refused land wrote no changeset rows"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands WHERE key = 'k-a'"),
        0,
        "the refused land left the key unsettled"
    );
    let main_target: Option<String> = m4b_common::conn_at(&f.t.db_path)
        .query_row(
            "SELECT lower(hex(target)) FROM refs WHERE name = 'main'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(main_target.as_deref(), Some(cx.as_str()), "main unmoved");

    // (b) The byte-identical rebase of a purged changeset: the rebase of
    // ch onto cx reproduces a DETERMINISTIC id (same parent, same author/
    // message/timestamp — the fixture clock sat at BASE_NS — same delta;
    // disjoint paths make the merged delta exactly ch's own changes).
    // Stamp the purged tombstone for that id directly (the state a purge
    // leaves: row gone, tombstone standing), then land: the cs-id arm
    // refuses.
    let canonical = naatre_core::changeset::encode(&naatre_core::changeset::Changeset {
        repo_id: f.repo_id as u64,
        parents: vec![hex_id(&cx)],
        author: "Agent <agent@naatre.dev>".into(),
        message: "commit".into(),
        timestamp_ns: BASE_NS,
        changes: vec![naatre_core::changeset::FileChange {
            path: "h.txt".into(),
            op: naatre_core::changeset::Op::Add,
            blob: Some(hex_id(&h_blob)),
            size: Some(h_content.len() as u64),
            exec: Some(false),
        }],
        conflicts: Vec::new(),
    })
    .unwrap();
    let rebased = object_id(Kind::Changeset, &canonical);
    assert_ne!(rebased, hex_id(&ch), "the rebase rewrites the id");
    {
        let conn = naatre_store::Db::Sqlite(rusqlite::Connection::open(&f.t.db_path).unwrap());
        let tx = conn.transaction_immediate().unwrap();
        match naatre_server::db::redaction_mark(
            &tx,
            f.repo_id,
            &rebased,
            "changeset",
            BASE_NS as i64,
        )
        .unwrap()
        {
            naatre_server::db::RedactionMarkOutcome::Inserted(_) => {}
            other => panic!("expected an insert, got {other:?}"),
        }
        assert_eq!(
            naatre_server::db::redaction_stamp_purged(&tx, "changeset", &rebased, BASE_NS as i64)
                .unwrap(),
            1,
            "the tombstone stands purged"
        );
        tx.commit().unwrap();
    }
    let resp = f.land(&ch, "k-b").await;
    assert_eq!(resp.status, StatusCode::GONE, "{}", resp.body);
    assert_eq!(resp.body["error"]["code"], "object_redacted");
    let msg = resp.body["error"]["message"].as_str().unwrap();
    assert!(
        msg.contains(&rebased.to_hex()),
        "the refusal names the rebased changeset id (the cs-id arm): {msg}"
    );
    assert!(msg.contains(REPO), "it names the repo: {msg}");
    assert!(
        msg.contains("recreating it is refused"),
        "singular cs-arm wording: {msg}"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM changesets"),
        changesets_before,
        "the refused rebase wrote no rows"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands WHERE key = 'k-b'"),
        0,
        "the key stays unsettled"
    );
}

/// The batch race-refusal condemns EVERY written id (round 2): the whole
/// batch's bytes are already in the CAS when one raced mark refuses the
/// record, and the all-or-nothing row side dies for the clean siblings
/// too — their bytes orphan identically. A later legitimate record of a
/// clean id self-heals: the row records, and once the blob is referenced
/// (reachable again) the sweep's repair clears its ledger row on the next
/// pass.
#[tokio::test]
async fn batch_race_refusal_condemns_every_written_id() {
    let f = fixture();
    let root = seed_base(&f).await;
    let clean1 = b"batch-clean-1".to_vec();
    let clean2 = b"batch-clean-2".to_vec();
    let marked = b"batch-marked".to_vec();
    let (c1, c2, m) = (blob_id(&clean1), blob_id(&clean2), blob_id(&marked));

    // The raced mark stands BEFORE the record step (the route-level probe
    // passed for the batch before it landed). The marked id goes through
    // the objects route once so the mark surface accepts it; the clean
    // ids stay rowless CAS bytes — exactly the orphan shape.
    let _ = f.put_blob(&marked).await;
    f.redact("blob", &m.to_hex(), "raced mark").await;
    // The CAS step already wrote all three payloads.
    f.t.state.cas.put(&clean1).unwrap();
    f.t.state.cas.put(&clean2).unwrap();
    f.t.state.cas.put(&marked).unwrap();

    let err = naatre_server::routes::objects::record_batch(
        &f.t.state,
        REPO,
        vec![
            (c1, clean1.len() as i64, 1),
            (c2, clean2.len() as i64, 1),
            (m, marked.len() as i64, 1),
        ],
    )
    .await
    .unwrap_err();
    assert_eq!(
        err.code(),
        naatre_server::api_code::ApiCode::ObjectRedacted,
        "{err:?}"
    );
    // Every written id carries a ledger condemnation, clean or not.
    for id in [&c1, &c2, &m] {
        assert_eq!(
            f.count_params(
                "SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob' AND id = ?1",
                &[&id.0.as_slice()]
            ),
            1,
            "the refusal condemned {}",
            id.to_hex()
        );
    }
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&c1.0.as_slice()]
        ),
        0,
        "no rows survived the refusal"
    );

    // The self-heal: a legitimate record of a clean id writes its row and
    // clears the condemnation mark.
    let existed =
        naatre_server::routes::objects::record_object(&f.t.state, REPO, c1, clean1.len() as i64, 1)
            .await
            .unwrap();
    assert!(!existed);
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
            &[&c1.0.as_slice()]
        ),
        1
    );
    // Reference it: reachable again, so the sweep's repair owns the row.
    let resp = post_json_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets"),
        &f.writer,
        &cs_body(
            json!([root.clone()]),
            json!([m4b_common::add("healed.txt", &c1.to_hex(), clean1.len())]),
            "reference the healed blob",
        ),
    )
    .await;
    assert_eq!(resp.status, StatusCode::CREATED, "{}", resp.body);
    // Land it: pre-rebase originals are NOT mark roots, so the reference
    // only makes the blob reachable once main names the changeset.
    let healed = resp.body["id"].as_str().unwrap().to_string();
    let resp = f.land(&healed, "k-heal").await;
    assert_eq!(resp.status, StatusCode::OK, "{}", resp.body);

    gc_pass(&f.t.state).await.unwrap();
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM gc_pending WHERE scope = 'blob' AND id = ?1",
            &[&c1.0.as_slice()]
        ),
        0,
        "the re-referenced id's ledger row cleared on the pass"
    );
}

/// repo_stats excludes THIS repo's tombstoned ids (round 2): polling the
/// stats across a mark must not recover the redacted blob's exact byte
/// length by subtraction — the sums equal the same repo's stats as if the
/// blob had never been added (only the changeset count moves).
#[tokio::test]
async fn repo_stats_exclude_this_repo_tombstones() {
    let f = fixture();
    let root = seed_base(&f).await;
    let before = {
        let conn = naatre_store::Db::Sqlite(rusqlite::Connection::open(&f.t.db_path).unwrap());
        stats_of(&conn, f.repo_id)
    };

    let secret = b"stats-secret!".to_vec();
    let secret_hex = f.put_blob(&secret).await;
    let cs = f.commit(json!([root.clone()]), "s.txt", &secret).await;
    let with_secret = {
        let conn = naatre_store::Db::Sqlite(rusqlite::Connection::open(&f.t.db_path).unwrap());
        stats_of(&conn, f.repo_id)
    };
    assert_eq!(with_secret.0, before.0 + 1, "the commit counted");
    assert!(with_secret.1 > before.1, "the blob's row counted");

    f.redact("blob", &secret_hex, "stats takedown").await;
    let after = {
        let conn = naatre_store::Db::Sqlite(rusqlite::Connection::open(&f.t.db_path).unwrap());
        stats_of(&conn, f.repo_id)
    };
    assert_eq!(
        after,
        (before.0 + 1, before.1, before.2, before.3),
        "the sums equal the never-added baseline (changesets still count the row)"
    );

    // The tombstone here is the normal tombstoned-but-unpurged state (the
    // blob stays referenced by the live commit — the purge waits for the
    // reference to vanish); the leak the pin closes is exactly that
    // window: the stats are already the never-added baseline.
    let _ = cs;
}

fn stats_of(conn: &naatre_store::Db, repo_id: i64) -> (i64, i64, i64, i64) {
    let changesets = naatre_server::db::repo_changeset_count(conn, repo_id).unwrap();
    let excluded: std::collections::HashSet<naatre_core::ids::Id> =
        naatre_server::db::redaction_ids_repo(conn)
            .unwrap()
            .into_iter()
            .collect();
    let members: Vec<naatre_core::ids::Id> = naatre_server::db::gc_repo_blobs(conn, repo_id)
        .unwrap()
        .into_iter()
        .filter(|id| !excluded.contains(id))
        .collect();
    let (objects, bytes, cbytes) = naatre_server::db::repo_object_sums(conn, &members).unwrap();
    (changesets, objects, bytes, cbytes)
}
