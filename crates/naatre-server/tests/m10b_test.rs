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
/// still exists IS drift (fabricated here by hand-stamping, the only way a
/// correct purge could leave one).
#[tokio::test]
async fn fsck_reports_purged_but_still_referenced_as_drift() {
    let f = fixture();
    seed_base(&f).await;
    let b_hex = blob_hex(b"live-content-A");
    f.redact("blob", &b_hex, "drift seed").await;
    // Hand-stamp purged_ns while the objects row still exists.
    m4b_common::conn_at(&f.t.db_path)
        .execute(
            "UPDATE redactions SET purged_ns = 1 WHERE kind = 'blob'",
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
    assert_eq!(resp.body["tokens_deleted"], 1, "{}", resp.body);

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

    // The audit ACTOR is pseudonymized; the deletion is audited.
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE principal = 'alice'"),
        0
    );
    assert!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE principal = 'deleted:alice'") > 0,
        "the pseudonymized rows carry the actor history"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'principal_delete'"),
        1
    );

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
