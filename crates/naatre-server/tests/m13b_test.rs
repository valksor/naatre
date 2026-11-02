//! M13b suite, server side: the repo read ACLs (uniform 404 on every
//! repo-scoped route for a private repo's non-readers, the filtered
//! listing, the audited grant/revoke rows), the ticket mint surface
//! (private-repo refusal = no ticket through the cache, tree validation,
//! the mint-side lifetime cap, `cache_disabled` without keys), the
//! tombstone gate at mint time (the membership walk excludes active AND
//! purged tombstones; the epoch moves on every tombstone event), and the
//! origin's ticket-authenticated surfaces (the cache-support endpoints
//! are ticket-only; the by-id fetch's ticket arm enforces the repo
//! binding; every ticket failure is the credential codes).
//!
//! The tier's own end-to-end pins (cross-repo 404 through the cache,
//! warm-blob-outside-scope, corrupt-store fallback, fail-closed startup,
//! rotation) run against the REAL binary in the CLI suite
//! (`naatre-cli/tests/m13b_e2e_test.rs`).

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use http_body_util::BodyExt;
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::{AppState, ServerState};
use naatre_store::DataDir;
use naatre_store::Sql;

/// Fixed-step clock base: ns for 2023-11-14.
const BASE_NS: u64 = 1_700_000_000_000_000_000;

// ---- fixture ----------------------------------------------------------------

struct Fixture {
    _dir: tempfile::TempDir,
    data_path: std::path::PathBuf,
    state: AppState,
    app: Router,
    /// The fixture's injectable clock (store into it to move time).
    clock: Arc<std::sync::atomic::AtomicU64>,
    admin: String,
    agent: String,
    /// The fixture's blobs: [0] in-scope member, [1] in-scope (child dir),
    /// [2] member of the repo but OUTSIDE the ticket's root tree, [3]
    /// in-scope but TOMBSTONED.
    blobs: Vec<Id>,
    root_tree: Id,
}

async fn fixture() -> Fixture {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let dir = tempfile::tempdir().unwrap();
    let data_path = dir.path().to_path_buf();
    let data = DataDir::create(&data_path).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(&data_path).unwrap();
    let mut state = ServerState::open(data).unwrap();
    let clock = Arc::new(AtomicU64::new(BASE_NS));
    let clock_for_closure = Arc::clone(&clock);
    state.clock = Arc::new(move || clock_for_closure.load(Ordering::Relaxed));
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    let state = Arc::new(state);

    // Admin + a second principal `agent` (role writer — the pre-M13
    // read-by-default shape) with their tokens.
    let (admin, admin_hash) = naatre_server::auth::mint_token();
    let (agent, agent_hash) = naatre_server::auth::mint_token();
    state
        .write({
            move |store| {
                let conn = store.conn();
                let tx = conn.transaction().unwrap();
                naatre_server::db::principal_upsert(&tx, "admin", "admin", BASE_NS as i64).unwrap();
                naatre_server::db::principal_upsert(&tx, "agent", "writer", BASE_NS as i64)
                    .unwrap();
                naatre_server::db::principal_upsert(&tx, "alice", "reader", BASE_NS as i64)
                    .unwrap();
                naatre_server::db::token_insert(&tx, "admin", &admin_hash, None, None).unwrap();
                naatre_server::db::token_insert(&tx, "agent", &agent_hash, None, None).unwrap();
                naatre_server::db::audit_append(&tx, BASE_NS as i64, "admin", "init", "{}")
                    .unwrap();
                tx.commit().unwrap();
                Ok(())
            }
        })
        .await
        .unwrap();

    let _ = (admin_hash, agent_hash);

    // Repo `m13b-a` with a real tree structure in the repo DB (the
    // seeded-rows shape m13a uses — the membership walk reads trees /
    // tree_entries / repo_blobs directly):
    //
    //   t_root
    //   ├── f1.txt  -> blob0
    //   └── d/      (t_child)
    //       ├── g.txt -> blob1
    //       └── h.txt -> blob3  (tombstoned below)
    //   blob2 is in the CAS + repo_blobs but under NO tree of this ticket.
    let repo = "m13b-a".to_string();
    state
        .write({
            let repo = repo.clone();
            move |store| {
                let conn = store.conn();
                naatre_server::db::repos_create(conn, &repo, BASE_NS as i64).unwrap();
                Ok(())
            }
        })
        .await
        .unwrap();

    let contents: Vec<Vec<u8>> = (0..4)
        .map(|i| format!("m13b payload {i}\n").into_bytes())
        .collect();
    let blobs: Vec<Id> = contents.iter().map(|c| object_id(Kind::Blob, c)).collect();
    for c in &contents {
        state.cas.put(c).unwrap();
    }

    let tree_canonical = b"m13b-tree-canonical".to_vec();
    let root_tree = object_id(Kind::Tree, &tree_canonical);
    let child_tree = object_id(Kind::Tree, b"m13b-child-canonical");
    let state_for_seed = Arc::clone(&state);
    {
        let blobs = blobs.clone();
        let tree_canonical = tree_canonical.clone();
        state_for_seed
            .write_repo(repo.clone(), move |cx| {
                let db = cx.db;
                let repo_id = cx.id;
                for (id, c) in blobs.iter().zip(&contents) {
                    naatre_server::db::object_row_upsert(
                        &cx.cat(),
                        id,
                        c.len() as i64,
                        1,
                        BASE_NS as i64,
                    )
                    .unwrap();
                    db.execute(
                        &Sql::plain(
                            "INSERT OR IGNORE INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)",
                        ),
                        &naatre_store::pv![repo_id, id.0.as_slice()],
                    )
                    .unwrap();
                }
                db.execute(
                    &Sql::plain("INSERT INTO trees(id, repo_id, canonical) VALUES (?1, ?2, ?3)"),
                    &naatre_store::pv![root_tree.0.as_slice(), repo_id, tree_canonical.as_slice()],
                )
                .unwrap();
                db.execute(
                    &Sql::plain("INSERT INTO trees(id, repo_id, canonical) VALUES (?1, ?2, ?3)"),
                    &naatre_store::pv![
                        child_tree.0.as_slice(),
                        repo_id,
                        b"m13b-child-canonical".as_slice()
                    ],
                )
                .unwrap();
                for (tree, name, kind, oid) in [
                    (root_tree, "f1.txt", 0i64, blobs[0]),
                    (root_tree, "d", 1, child_tree),
                    (child_tree, "g.txt", 0, blobs[1]),
                    (child_tree, "h.txt", 0, blobs[3]),
                ] {
                    db.execute(
                        &Sql::plain(
                            "INSERT INTO tree_entries(tree_id, name, kind, object_id, size, exec) \
                             VALUES (?1, ?2, ?3, ?4, 5, 0)",
                        ),
                        &naatre_store::pv![tree.0.as_slice(), name, kind, oid.0.as_slice()],
                    )
                    .unwrap();
                }
                // The mint's tree validation reads the repo_trees junction.
                for tree in [root_tree, child_tree] {
                    db.execute(
                        &Sql::plain(
                            "INSERT OR IGNORE INTO repo_trees(repo_id, tree_id) VALUES (?1, ?2)",
                        ),
                        &naatre_store::pv![repo_id, tree.0.as_slice()],
                    )
                    .unwrap();
                }
                // file_changes rows so the tombstone's path derivation and
                // the redaction gate have the M10 shapes.
                for (name, blob) in [
                    ("f1.txt", blobs[0]),
                    ("d/g.txt", blobs[1]),
                    ("d/h.txt", blobs[3]),
                ] {
                    db.execute(
                        &Sql::plain(
                            "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
                             VALUES (?1, ?2, ?3, 0, ?4, 5, 0)",
                        ),
                        &naatre_store::pv![repo_id, blob.0.as_slice(), name, blob.0.as_slice()],
                    )
                    .unwrap();
                }
                Ok(())
            })
            .await
            .unwrap();
    }

    // Tombstone blob3 NOW (before any ticket/membership): the mint-time
    // gate — the membership walk must exclude it.
    {
        let s = Arc::clone(&state);
        let blob = blobs[3];
        let repo_for_mark = repo.clone();
        s.write_repo(repo_for_mark, move |cx| {
            let tx = cx.db.transaction_immediate().unwrap();
            naatre_server::db::redaction_mark(&tx, cx.id, &blob, "blob", BASE_NS as i64).unwrap();
            tx.commit().unwrap();
            Ok(())
        })
        .await
        .unwrap();
    }

    // The origin's ticket key store: initialized in the fixture data dir
    // (the mint + the ticket-authed middleware read it from there).
    let (der, public_hex) = naatre_server::ticket::generate_keypair().unwrap();
    let key_id = naatre_server::ticket::key_id_from_hex_checked(&public_hex).unwrap();
    let keys = naatre_server::ticket::VerifyKeys::parse(&format!(
        r#"{{"keys":[{{"id":"{key_id}","public":"{public_hex}","current":true}}]}}"#
    ))
    .unwrap();
    naatre_server::ticket::write_signing_file(&data_path, &der).unwrap();
    naatre_server::ticket::write_verify_file(&data_path, &keys).unwrap();

    let app = naatre_server::router(Arc::clone(&state));
    Fixture {
        _dir: dir,
        data_path,
        state,
        app,
        clock,
        admin,
        agent,
        blobs,
        root_tree,
    }
}

/// An in-memory request with an ARBITRARY Authorization header value
/// (Bearer tokens and NaatreTicket tickets share the helper).
async fn send(
    app: &Router,
    method: &str,
    uri: &str,
    auth: Option<&str>,
    body_bytes: &[u8],
) -> (StatusCode, serde_json::Value) {
    let mut builder = axum::http::Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header(header::AUTHORIZATION, a);
    }
    let req = builder
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(body_bytes.to_vec()))
        .unwrap();
    let res = app.clone().oneshot(req).await.expect("in-memory request");
    let status = res.status();
    let bytes = BodyExt::collect(res.into_body()).await.unwrap().to_bytes();
    let body = if bytes.is_empty() {
        serde_json::Value::Null
    } else {
        serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null)
    };
    (status, body)
}

fn json_body(v: &serde_json::Value) -> Vec<u8> {
    serde_json::to_vec(v).unwrap()
}

fn bearer(token: &str) -> String {
    format!("Bearer {token}")
}

fn code_of(body: &serde_json::Value) -> String {
    body["error"]["code"].as_str().unwrap_or("?").to_string()
}

// ---- the pins ---------------------------------------------------------------

/// ACL grant/revoke audit rows (the plan's check): the grant and the
/// revoke each row an audit entry with the acting admin as principal;
/// validation both ways (unknown principal 422; write-without-read 422).
#[tokio::test]
async fn access_grant_and_revoke_rows_and_validation() {
    let f = fixture().await;
    let app = f.app.clone();

    // Write-without-read is refused, never silently upgraded.
    let (status, body) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "alice", "read": false, "write": true})),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "{body}");

    // An unknown principal is refused (grants name global principals).
    let (status, body) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "ghost", "read": true, "write": false})),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "{body}");

    // The grant: write implies read (the stored flags echo read=true).
    let (status, body) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "alice", "read": true, "write": true})),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(body["read"], serde_json::json!(true));
    assert_eq!(body["write"], serde_json::json!(true));

    // The revoke.
    let (status, _) = send(
        &app,
        "DELETE",
        "/v1/repos/m13b-a/access/alice",
        Some(&bearer(&f.admin)),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // Both audited actions stand (the audit list is unfiltered; the test
    // picks the actions out of the page).
    let (status, body) = send(
        &app,
        "GET",
        "/v1/audit?limit=200",
        Some(&bearer(&f.admin)),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let rows = body["audit"].as_array().expect("audit page").clone();
    let grants: Vec<&serde_json::Value> = rows
        .iter()
        .filter(|r| r["action"] == "access_grant")
        .collect();
    assert_eq!(grants.len(), 1, "one access_grant row: {rows:?}");
    assert_eq!(grants[0]["principal"], serde_json::json!("admin"));
    let details: serde_json::Value =
        serde_json::from_str(grants[0]["details_json"].as_str().unwrap()).unwrap();
    assert_eq!(details["repo"], serde_json::json!("m13b-a"));
    assert_eq!(details["principal"], serde_json::json!("alice"));
    assert_eq!(details["write"], serde_json::json!(true));

    let revokes: Vec<&serde_json::Value> = rows
        .iter()
        .filter(|r| r["action"] == "access_revoke")
        .collect();
    assert_eq!(revokes.len(), 1, "one access_revoke row: {rows:?}");
}

/// The uniform 404: a private repo's NON-reader gets the exact
/// repo-not-found answer on EVERY repo-scoped route — indistinguishable
/// from a missing repo. Both directions on one surface: the same
/// principal reads the repo FINE once granted.
#[tokio::test]
async fn private_repo_is_uniformly_404_for_non_readers() {
    let f = fixture().await;
    let app = f.app.clone();
    let blob0 = f.blobs[0].to_hex();

    // Make the repo private with a grant for alice only.
    let (status, _) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "alice", "read": true, "write": false})),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    let agent = bearer(&f.agent);
    let obj_uri = format!("/v1/repos/m13b-a/objects/{blob0}");
    let routes: Vec<(&str, &str)> = vec![
        ("GET", "/v1/repos/m13b-a/refs/main"),
        ("GET", obj_uri.as_str()),
        ("GET", "/v1/repos/m13b-a/log"),
        ("GET", "/v1/repos/m13b-a/stats"),
        ("GET", "/v1/repos/m13b-a/redactions"),
        ("GET", "/v1/repos/m13b-a/access"),
        // tickets mint is the POST below.
    ];
    for (method, uri) in &routes {
        let (status, body) = send(&app, method, uri, Some(&agent), b"").await;
        assert_eq!(
            status,
            StatusCode::NOT_FOUND,
            "{method} {uri} must answer the uniform 404, got {status}: {body}"
        );
        assert_eq!(code_of(&body), "repo_not_found", "{uri}: {body}");
    }

    // The POST-shaped routes too (present/get/tickets/redact/access-set).
    for (method, uri, body_bytes) in [
        (
            "POST",
            "/v1/repos/m13b-a/objects/present",
            json_body(&serde_json::json!({"ids": [blob0]})),
        ),
        (
            "POST",
            "/v1/repos/m13b-a/objects/get",
            json_body(&serde_json::json!({"ids": [blob0]})),
        ),
        (
            "POST",
            "/v1/repos/m13b-a/tickets",
            json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
        ),
        (
            "PUT",
            "/v1/repos/m13b-a/access",
            json_body(&serde_json::json!({"principal": "alice", "read": true, "write": true})),
        ),
    ] {
        let (status, body) = send(&app, method, uri, Some(&agent), &body_bytes).await;
        assert_eq!(status, StatusCode::NOT_FOUND, "{method} {uri}: {body}");
        assert_eq!(code_of(&body), "repo_not_found", "{uri}: {body}");
    }

    // A non-reader WRITER-role principal on the write surface: the same
    // uniform 404 (never a 403 — the write surface is as invisible as
    // the read surface).
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/land",
        Some(&agent),
        &json_body(&serde_json::json!({"head": blob0, "key": "k"})),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "{body}");

    // The reader gets through everywhere they touch (200s or the
    // route's own not-found-for-resource answers — never repo-level 404).
    // The fixture has no token row for alice (grants need no token); mint
    // one for her to prove the read path.
    let (alice, alice_hash) = naatre_server::auth::mint_token();
    let s = Arc::clone(&f.state);
    s.write(move |store| {
        let conn = store.conn();
        let tx = conn.transaction().unwrap();
        naatre_server::db::token_insert(&tx, "alice", &alice_hash, None, None).unwrap();
        tx.commit().unwrap();
        Ok(())
    })
    .await
    .unwrap();
    let alice_auth = bearer(&alice);
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13b-a/objects/{blob0}"),
        Some(&alice_auth),
        b"",
    )
    .await;
    assert_eq!(
        status,
        StatusCode::OK,
        "a granted reader reads the private repo"
    );
    // The fixture seeds no main ref: the route's own resource 404 — the
    // CODE is what distinguishes it from invisibility (never
    // repo_not_found for a granted reader).
    let (status, body) = send(
        &app,
        "GET",
        "/v1/repos/m13b-a/refs/main",
        Some(&alice_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_ne!(
        code_of(&body),
        "repo_not_found",
        "a granted reader sees the repo (the 404 is the route's own)"
    );

    // Admins are implicit readers.
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13b-a/objects/{blob0}"),
        Some(&bearer(&f.admin)),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK, "admin reads the private repo");
}

/// Public repos keep the pre-M13 shape: any authenticated principal
/// reads; a role-under-writer write attempt answers the SAME 403 as
/// before (the repo is not a secret — the role is the problem).
#[tokio::test]
async fn public_repo_keeps_the_pre_m13_matrix() {
    let f = fixture().await;
    let app = f.app.clone();
    let blob0 = f.blobs[0].to_hex();
    // No access rows at all: the repo is public.
    let agent = bearer(&f.agent);
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13b-a/objects/{blob0}"),
        Some(&agent),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK, "public repo: any principal reads");
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/land",
        Some(&agent),
        &json_body(&serde_json::json!({"head": blob0, "key": "k"})),
    )
    .await;
    // agent IS a writer — the write proceeds past the gate and fails on
    // the changeset instead (never a repo-level 404). A READER-role
    // principal would 403; pin the gate's 403 shape with alice.
    let _ = (status, body);
    let (alice, alice_hash) = naatre_server::auth::mint_token();
    let s = Arc::clone(&f.state);
    s.write(move |store| {
        let conn = store.conn();
        let tx = conn.transaction().unwrap();
        naatre_server::db::token_insert(&tx, "alice", &alice_hash, None, None).unwrap();
        tx.commit().unwrap();
        Ok(())
    })
    .await
    .unwrap();
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/land",
        Some(&bearer(&alice)),
        &json_body(&serde_json::json!({"head": blob0, "key": "k"})),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN, "{body}");
    assert_eq!(code_of(&body), "forbidden");
}

/// The listing filter (the plan's "filtered from listing"): the private
/// repo appears to the admin and the granted reader, never to a
/// non-reader. The cursor never restarts on a fully-filtered page.
#[tokio::test]
async fn listing_filters_private_repos() {
    let f = fixture().await;
    let app = f.app.clone();

    // A second repo stays public.
    let s = Arc::clone(&f.state);
    s.write(|store| {
        naatre_server::db::repos_create(store.conn(), "m13b-public", BASE_NS as i64).unwrap();
        Ok(())
    })
    .await
    .unwrap();

    let (status, _) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "alice", "read": true, "write": false})),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // Admin sees both.
    let (status, body) = send(
        &app,
        "GET",
        "/v1/repos?limit=50",
        Some(&bearer(&f.admin)),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let names: Vec<&str> = body["repos"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    assert!(
        names.contains(&"m13b-a") && names.contains(&"m13b-public"),
        "{names:?}"
    );

    // alice (granted) sees both.
    let (alice, alice_hash) = naatre_server::auth::mint_token();
    let s = Arc::clone(&f.state);
    s.write(move |store| {
        let conn = store.conn();
        let tx = conn.transaction().unwrap();
        naatre_server::db::token_insert(&tx, "alice", &alice_hash, None, None).unwrap();
        tx.commit().unwrap();
        Ok(())
    })
    .await
    .unwrap();
    let (_, body) = send(
        &app,
        "GET",
        "/v1/repos?limit=50",
        Some(&bearer(&alice)),
        b"",
    )
    .await;
    let names: Vec<&str> = body["repos"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    assert!(names.contains(&"m13b-a"), "{names:?}");

    // agent (no grant) sees only the public one.
    let (_, body) = send(
        &app,
        "GET",
        "/v1/repos?limit=50",
        Some(&bearer(&f.agent)),
        b"",
    )
    .await;
    let names: Vec<&str> = body["repos"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    assert_eq!(
        names,
        vec!["m13b-public"],
        "the private repo is invisible in the listing"
    );
}

/// The mint surface: a private repo's non-reader gets NO ticket (the
/// uniform 404 — invisibility through the cache), the tree validation is
/// the junction probe (a foreign/snapshot tree id is refused), the
/// mint-side lifetime cap is enforced (≤15 min), and a bearer token is
/// the only minting credential (a ticket cannot mint a ticket).
#[tokio::test]
async fn mint_gates_private_repos_trees_and_lifetime() {
    let f = fixture().await;
    let app = f.app.clone();

    // Public repo, happy path: 201 with the documented shape.
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "{body}");
    assert_eq!(body["repo"], serde_json::json!("m13b-a"));
    assert_eq!(
        body["root_tree"],
        serde_json::Value::String(f.root_tree.to_hex())
    );
    assert_eq!(body["principal"], serde_json::json!("agent"));
    let lifetime = body["lifetime_secs"].as_u64().unwrap();
    assert!(
        lifetime <= 900,
        "the mint never exceeds the 15-min cap: {lifetime}"
    );

    // The mint-side cap: 901 s is refused 422.
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex(), "ttl_secs": 901})),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "{body}");

    // The tree validation: a tree this repo never wrote is refused 404.
    let foreign_tree = object_id(Kind::Tree, b"m13b-never-wrote-this");
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": foreign_tree.to_hex()})),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "{body}");

    // A ticket is not a minting credential.
    let (status, minted) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let ticket = minted["ticket"].as_str().unwrap().to_string();
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&format!("NaatreTicket {ticket}")),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN, "{body}");

    // The private-repo refusal: grant alice, then agent mints → uniform
    // 404, no ticket (invisibility through the cache).
    let (status, _) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "alice", "read": true, "write": false})),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let (status, body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "{body}");
    assert_eq!(code_of(&body), "repo_not_found");

    // The granted reader still mints.
    let (alice, alice_hash) = naatre_server::auth::mint_token();
    let s = Arc::clone(&f.state);
    s.write(move |store| {
        let conn = store.conn();
        let tx = conn.transaction().unwrap();
        naatre_server::db::token_insert(&tx, "alice", &alice_hash, None, None).unwrap();
        tx.commit().unwrap();
        Ok(())
    })
    .await
    .unwrap();
    let (status, _) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&alice)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::CREATED,
        "a granted reader mints a ticket"
    );
}

/// The tombstone gate at mint time and the epoch mechanism: the
/// membership walk excludes the tombstoned id (active or purged), the
/// epoch moves on every tombstone event, and the epoch endpoint serves
/// the same number the membership response carried.
#[tokio::test]
async fn membership_excludes_tombstones_and_the_epoch_tracks_marks() {
    let f = fixture().await;
    let app = f.app.clone();

    let (_, ticket_body) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    let ticket = ticket_body["ticket"].as_str().unwrap().to_string();
    let ticket_auth = format!("NaatreTicket {ticket}");

    // The epoch BEFORE (one tombstone already stands from the fixture).
    let (status, epoch0) = send(
        &app,
        "GET",
        "/v1/repos/m13b-a/cache/epoch",
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{epoch0}");
    let epoch0 = epoch0["epoch"].as_i64().unwrap();
    assert_eq!(epoch0, 1, "the fixture's pre-existing tombstone counts");

    // The membership walk: in-scope members only, tombstoned blob3
    // excluded, out-of-tree blob2 absent.
    let (status, mem) = send(
        &app,
        "GET",
        &format!(
            "/v1/repos/m13b-a/cache/membership?tree={}",
            f.root_tree.to_hex()
        ),
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{mem}");
    assert_eq!(
        mem["epoch"],
        serde_json::json!(epoch0),
        "the walk carries the epoch"
    );
    let ids: Vec<String> = mem["blobs"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap().to_string())
        .collect();
    assert!(ids.contains(&f.blobs[0].to_hex()), "{ids:?}");
    assert!(
        ids.contains(&f.blobs[1].to_hex()),
        "the child dir's blob is in scope: {ids:?}"
    );
    assert!(
        !ids.contains(&f.blobs[3].to_hex()),
        "the tombstoned blob never enters the membership set: {ids:?}"
    );
    assert!(
        !ids.contains(&f.blobs[2].to_hex()),
        "the out-of-tree blob is not in this ticket's scope: {ids:?}"
    );

    // A NEW tombstone moves the epoch (the cache's staleness signal).
    let s = Arc::clone(&f.state);
    let blob1 = f.blobs[1];
    let repo_for_mark = "m13b-a".to_string();
    s.write_repo(repo_for_mark, move |cx| {
        let tx = cx.db.transaction_immediate().unwrap();
        naatre_server::db::redaction_mark(&tx, cx.id, &blob1, "blob", BASE_NS as i64 + 1).unwrap();
        tx.commit().unwrap();
        Ok(())
    })
    .await
    .unwrap();
    let (status, epoch1) = send(
        &app,
        "GET",
        "/v1/repos/m13b-a/cache/epoch",
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{epoch1}");
    assert_eq!(
        epoch1["epoch"].as_i64(),
        Some(epoch0 + 1),
        "every tombstone event moves the epoch"
    );

    // And the fresh walk dropped the newly tombstoned id.
    let (status, mem) = send(
        &app,
        "GET",
        &format!(
            "/v1/repos/m13b-a/cache/membership?tree={}",
            f.root_tree.to_hex()
        ),
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let ids: Vec<String> = mem["blobs"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap().to_string())
        .collect();
    assert!(
        !ids.contains(&f.blobs[1].to_hex()),
        "the new tombstone drops out: {ids:?}"
    );
    assert_eq!(mem["epoch"], serde_json::json!(epoch0 + 1));
}

/// The cache-support surface is TICKET-ONLY: bearer callers answer 403;
/// a ticket bound to another repo gets the uniform repo 404; a tree
/// mismatch gets the object 404. And the by-id fetch's ticket arm: the
/// repo binding is enforced (a repo-A ticket on a repo-B route →
/// uniform 404) and a valid ticket fetch serves through the origin's
/// own gates.
#[tokio::test]
async fn ticket_only_surfaces_and_the_binding_checks() {
    let f = fixture().await;
    let app = f.app.clone();

    let (_, minted) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    let ticket = minted["ticket"].as_str().unwrap().to_string();
    let ticket_auth = format!("NaatreTicket {ticket}");

    // Bearer callers are refused on the tier's data plane.
    let (status, body) = send(
        &app,
        "GET",
        "/v1/repos/m13b-a/cache/epoch",
        Some(&bearer(&f.admin)),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN, "{body}");

    // A ticket on a general route: 403 (tickets are credentials for the
    // three ticket surfaces only — refs/log never serve to a ticket).
    let (status, body) = send(&app, "GET", "/v1/repos/m13b-a/log", Some(&ticket_auth), b"").await;
    assert_eq!(status, StatusCode::FORBIDDEN, "{body}");

    // The membership tree mismatch.
    let foreign = object_id(Kind::Tree, b"not-the-root");
    let (status, body) = send(
        &app,
        "GET",
        &format!(
            "/v1/repos/m13b-a/cache/membership?tree={}",
            foreign.to_hex()
        ),
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "{body}");

    // The by-id fetch with the ticket: serves through the origin's gates.
    let blob0 = f.blobs[0].to_hex();
    let (status, _) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13b-a/objects/{blob0}"),
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(
        status,
        StatusCode::OK,
        "the ticket arm serves an in-scope id"
    );

    // The binding: the same ticket against ANOTHER repo's object route →
    // the uniform repo 404. Seed repo B with the same blob id in scope
    // (shared content) so the only thing being tested is the binding.
    let s = Arc::clone(&f.state);
    s.write(|store| {
        naatre_server::db::repos_create(store.conn(), "m13b-b", BASE_NS as i64).unwrap();
        Ok(())
    })
    .await
    .unwrap();
    let (status, body) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13b-b/objects/{blob0}"),
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "{body}");
    assert_eq!(code_of(&body), "repo_not_found");

    // The tombstone gate at the fetch: the tombstoned blob (even though
    // it is in the walk's repo) answers the origin's own 410 — the
    // exact answer the tier passes through on a delegate.
    let blob3 = f.blobs[3].to_hex();
    let (status, body) = send(
        &app,
        "GET",
        &format!("/v1/repos/m13b-a/objects/{blob3}"),
        Some(&ticket_auth),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::GONE, "{body}");
    assert_eq!(code_of(&body), "object_redacted");
}

/// Ticket verification at the origin's middleware: a malformed/garbage
/// ticket and an unknown key-id answer 401 `token_invalid`; an expired
/// ticket answers 401 `token_expired` (the injected clock moves past
/// exp); and the retired-key story — an old key's ticket verifies while
/// unexpired even after rotation.
#[tokio::test]
async fn ticket_verification_at_the_origin() {
    let f = fixture().await;
    let app = f.app.clone();

    let (_, minted) = send(
        &app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex(), "ttl_secs": 600})),
    )
    .await;
    let ticket = minted["ticket"].as_str().unwrap().to_string();
    let exp = minted["expires_ns"].as_u64().unwrap();
    let epoch_probe = "/v1/repos/m13b-a/cache/epoch";

    // Garbage → 401 token_invalid.
    let (status, body) = send(
        &app,
        "GET",
        epoch_probe,
        Some("NaatreTicket garbage.ticket.00000000"),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "{body}");
    assert_eq!(code_of(&body), "token_invalid");

    // Tampered payload (a ticket whose signature does not cover it).
    let (alice_ticket, _) = {
        let (_, t) = send(
            &app,
            "POST",
            "/v1/repos/m13b-a/tickets",
            Some(&bearer(&f.agent)),
            &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
        )
        .await;
        (t["ticket"].as_str().unwrap().to_string(), ())
    };
    let parts: Vec<&str> = alice_ticket.split('.').collect();
    let spliced = format!("{}.{}.{}", parts[0], parts[1], "deadbeef");
    let (status, body) = send(
        &app,
        "GET",
        epoch_probe,
        Some(&format!("NaatreTicket {spliced}")),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "{body}");
    assert_eq!(
        code_of(&body),
        "token_invalid",
        "an unknown key-id is fail-closed"
    );

    // The valid ticket works.
    let (status, _) = send(
        &app,
        "GET",
        epoch_probe,
        Some(&format!("NaatreTicket {ticket}")),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // Move the clock past exp: token_expired.
    f.clock.store(exp + 1, Ordering::Relaxed);
    let (status, body) = send(
        &app,
        "GET",
        epoch_probe,
        Some(&format!("NaatreTicket {ticket}")),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "{body}");
    assert_eq!(code_of(&body), "token_expired");

    // Rotation at the origin: the old key retires but STAYS in the
    // verify list — an unexpired OLD ticket still verifies.
    let (der2, public2) = naatre_server::ticket::generate_keypair().unwrap();
    let old_keys = naatre_server::ticket::load_verify_keys(&f.data_path).unwrap();
    let old_key = &old_keys.keys()[0];
    let old_hex: String = old_key.public.iter().map(|b| format!("{b:02x}")).collect();
    let new_id = naatre_server::ticket::key_id_from_hex_checked(&public2).unwrap();
    let rotated = naatre_server::ticket::VerifyKeys::parse(&format!(
        r#"{{"keys":[{{"id":"{new_id}","public":"{public2}","current":true}},{{"id":"{}","public":"{old_hex}","current":false}}]}}"#,
        old_key.id
    ))
    .unwrap();
    naatre_server::ticket::write_signing_file(&f.data_path, &der2).unwrap();
    naatre_server::ticket::write_verify_file(&f.data_path, &rotated).unwrap();

    // Reset the clock to inside the old ticket's TTL.
    f.clock.store(BASE_NS + 60_000_000_000, Ordering::Relaxed);
    let (status, _) = send(
        &app,
        "GET",
        epoch_probe,
        Some(&format!("NaatreTicket {ticket}")),
        b"",
    )
    .await;
    assert_eq!(
        status,
        StatusCode::OK,
        "a retired key's unexpired ticket verifies after rotation"
    );

    // A ticket minted BEFORE any keys existed (the origin's key files
    // removed): fail-closed 401.
    std::fs::remove_file(f.data_path.join(naatre_server::ticket::VERIFY_FILE)).unwrap();
    let (status, body) = send(
        &app,
        "GET",
        epoch_probe,
        Some(&format!("NaatreTicket {ticket}")),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "{body}");
    assert_eq!(code_of(&body), "token_invalid");
}

/// `cache_disabled`: a mint against an origin whose key files are gone
/// answers the honest 503 (retrying cannot help; the operator
/// configures).
#[tokio::test]
async fn mint_without_keys_answers_cache_disabled() {
    let f = fixture().await;
    std::fs::remove_file(f.data_path.join(naatre_server::ticket::SIGNING_FILE)).unwrap();
    let (status, body) = send(
        &f.app,
        "POST",
        "/v1/repos/m13b-a/tickets",
        Some(&bearer(&f.agent)),
        &json_body(&serde_json::json!({"root_tree": f.root_tree.to_hex()})),
    )
    .await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE, "{body}");
    assert_eq!(code_of(&body), "cache_disabled");
}

// ---- the m13a-fix hardening pins (M13b) ------------------------------------

use std::time::Duration;

/// The M13a-fix lock-order lesson carried to M13b: the ACL-GATED read
/// path (repo_read_gate's catalog-pool probe + the route's read_repo)
/// driven CONCURRENTLY with read_all passes completes without hanging —
/// the m13a deadlock pin's watchdog shape on the split layout, where the
/// opposite-order pool saturation used to hang. Driven through the REAL
/// router (bearer auth, the log route's gate + read_repo closure). The
/// gate takes a catalog READ-pool connection per call and releases it
/// before read_repo runs (no repo lock held while a pool lock is
/// wanted); no M13b read_repo closure touches cx.cat() at all (the one
/// new write-closure cat() — the land gate — rides the WRITE path's
/// dedicated aux connection, by design, never the read pool).
#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
async fn split_gated_reads_and_read_all_complete_without_hanging() {
    let f = fixture().await;
    // Alice's TOKEN pre-adoption (the fixture upserted her principal; a
    // token row in the catalog travels through the adoption).
    let (alice_token, alice_hash) = naatre_server::auth::mint_token();
    let h = alice_hash;
    f.state
        .write(move |store| {
            let conn = store.conn();
            let tx = conn.transaction().unwrap();
            naatre_server::db::token_insert(&tx, "alice", &h, None, None).unwrap();
            tx.commit().unwrap();
            Ok(())
        })
        .await
        .unwrap();
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(
        &data_path,
        &naatre_store::BackendConfig::default(),
    )
    .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let dir = f._dir;
    let cfg = naatre_store::backend::BackendConfig {
        layout: naatre_store::layout::Layout::DbPerRepo,
        ..naatre_store::backend::BackendConfig::default()
    };
    naatre_store::backend::check_data_dir_backend(&data_path, &cfg).unwrap();
    let state = Arc::new(ServerState::open_backend(DataDir::open_any(&data_path), &cfg).unwrap());
    let _ = &dir;

    // The repo goes PRIVATE (a read grant for alice — catalog-side write,
    // the split store slot): the gate now takes the ACL-checked arm —
    // the private-bit probe plus the grant row, one catalog-pool checkout
    // each, released before read_repo.
    let grant_repo_id = state
        .read(|conn| {
            naatre_server::db::repo_id_by_name(conn, "m13b-a")
                .map_err(naatre_server::api_error::ApiError::log_internal)
        })
        .await
        .unwrap()
        .expect("the adopted repo is in the catalog");
    state
        .write(move |store| {
            naatre_server::db::access_grant(
                store.conn(),
                grant_repo_id,
                "m13b-a",
                "alice",
                true,
                false,
                BASE_NS as i64,
                "admin",
            )
            .map_err(naatre_server::api_error::ApiError::log_internal)?;
            Ok(())
        })
        .await
        .unwrap();

    let app = naatre_server::router(Arc::clone(&state));
    let alice_auth = bearer(&alice_token);
    let agent_auth = bearer(&f.agent);

    let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let stop_w = Arc::clone(&stop);
    let watchdog = std::thread::spawn(move || {
        let started = std::time::Instant::now();
        while started.elapsed() < Duration::from_secs(45) {
            if stop_w.load(std::sync::atomic::Ordering::Relaxed) {
                return;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        eprintln!("m13b: ACL-gated reads DEADLOCKED against read_all (watchdog)");
        std::process::exit(101);
    });

    for _round in 0..3 {
        // First wave: gated reads through the REAL router — the gate's
        // catalog probe, then the log route's read_repo closure. One arm
        // for the granted reader (passes the gate, reads the repo), one
        // for the invisible writer (the gate answers the uniform 404
        // BEFORE any repo statement).
        let mut gated = Vec::new();
        for _ in 0..8 {
            let app = app.clone();
            let alice = alice_auth.clone();
            let agent = agent_auth.clone();
            gated.push(tokio::spawn(async move {
                let (status, body) =
                    send(&app, "GET", "/v1/repos/m13b-a/log", Some(&alice), b"").await;
                assert_eq!(
                    status,
                    StatusCode::OK,
                    "the granted reader reads through the gate: {body}"
                );
                let (status, body) =
                    send(&app, "GET", "/v1/repos/m13b-a/log", Some(&agent), b"").await;
                assert_eq!(
                    status,
                    StatusCode::NOT_FOUND,
                    "the invisible writer gets the uniform 404: {body}"
                );
            }));
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
        // Second wave: read_all holds every catalog pool connection and
        // needs the repo ones (the m13a saturation shape).
        let mut alls = Vec::new();
        for _ in 0..4 {
            let s = Arc::clone(&state);
            alls.push(tokio::spawn(async move {
                s.read_all(move |cx| {
                    let mut total = 0i64;
                    for repo_id in naatre_server::db::gc_repos(cx.cat())
                        .map_err(naatre_server::api_error::ApiError::log_internal)?
                    {
                        cx.with_repo(repo_id, |conn| {
                            total += naatre_server::db::repo_changeset_count(conn, repo_id)
                                .map_err(naatre_server::api_error::ApiError::log_internal)?;
                            Ok(())
                        })
                        .map_err(naatre_server::api_error::ApiError::log_internal)?;
                    }
                    Ok(total)
                })
                .await
            }));
        }
        for j in gated {
            j.await.expect("gated read task joins");
        }
        for j in alls {
            j.await
                .expect("read_all joins")
                .expect("read_all completes");
        }
    }
    stop.store(true, std::sync::atomic::Ordering::Relaxed);
    watchdog.join().expect("watchdog joins");
}

/// The M13a-fix audit lesson carried to M13b, split layout: the ACL
/// grant's audit row is IN the grant's catalog transaction in BOTH
/// layouts (the route writes the catalog store slot directly — there is
/// no repo-connection work transaction to host a split after-commit
/// window, so the cat_is_db conditional never arises; an audit failure
/// rolls the grant back and nothing stood, which is exactly the
/// stood-work rule's honest arm). Pin: on a split store, the grant and
/// the revoke each leave their audit row in catalog.db, and a REFUSED
/// grant leaves NO audit row (the atomicity arm).
#[tokio::test]
async fn split_grant_and_revoke_audit_rows_are_catalog_atomic() {
    let f = fixture().await;
    let data_path = f.data_path.clone();
    let report = naatre_server::adopt::adopt_db_per_repo(
        &data_path,
        &naatre_store::BackendConfig::default(),
    )
    .unwrap_or_else(|e| panic!("adopt failed: {e}"));
    assert!(report.fsck_clean);
    let _dir = f._dir;
    let cfg = naatre_store::backend::BackendConfig {
        layout: naatre_store::layout::Layout::DbPerRepo,
        ..naatre_store::backend::BackendConfig::default()
    };
    naatre_store::backend::check_data_dir_backend(&data_path, &cfg).unwrap();
    let state = Arc::new(ServerState::open_backend(DataDir::open_any(&data_path), &cfg).unwrap());
    let app = naatre_server::router(Arc::clone(&state));

    // alice needs a principals row (grants name existing principals).
    let (alice, alice_hash) = naatre_server::auth::mint_token();
    let s = Arc::clone(&state);
    let h = alice_hash;
    s.write(move |store| {
        let conn = store.conn();
        let tx = conn.transaction().unwrap();
        naatre_server::db::principal_upsert(&tx, "alice", "reader", BASE_NS as i64).unwrap();
        naatre_server::db::token_insert(&tx, "alice", &h, None, None).unwrap();
        tx.commit().unwrap();
        Ok(())
    })
    .await
    .unwrap();
    let _ = alice;

    // Grant via the route (admin bearer), then the revoke.
    let (status, body) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "alice", "read": true, "write": false})),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let (status, _) = send(
        &app,
        "DELETE",
        "/v1/repos/m13b-a/access/alice",
        Some(&bearer(&f.admin)),
        b"",
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // The rows stand in the CATALOG's audit_log (split layout).
    let cat = rusqlite::Connection::open(data_path.join("catalog.db")).unwrap();
    let grants: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'access_grant'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    let revokes: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'access_revoke'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(grants, 1, "the grant's audit row is in the catalog");
    assert_eq!(revokes, 1, "the revoke's audit row is in the catalog");

    // The atomicity arm: an UNKNOWN principal's grant is refused 422 and
    // leaves NO audit row (the tx rolled back — nothing stood).
    let (status, _) = send(
        &app,
        "PUT",
        "/v1/repos/m13b-a/access",
        Some(&bearer(&f.admin)),
        &json_body(&serde_json::json!({"principal": "ghost", "read": true, "write": false})),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    let grants_after: i64 = cat
        .query_row(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'access_grant'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(grants_after, grants, "a failed grant rows nothing");
}
