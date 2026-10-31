//! M9b fix slice: the ROUTE level against `blob = "s3"`. The M9b matrix
//! exercised `S3Store` behind the seam only; above the seam the routes
//! statted a LOCAL fs shard for `objects.csize` after `cas.put`, so every
//! history PUT answered 500 — blob stored in the bucket, row never
//! upserted, object invisible forever. These legs drive the real router
//! over the real S3 backend (raw PUT, batch PUT, and a changeset commit
//! e2e) and pin the honest version of the plan's M9 check ("full store
//! suite green on all combos").
//!
//! Every leg SKIPS with a printed notice when no REAL S3 server can be
//! produced (`NAATRE_TEST_S3_ENDPOINT`, else the pinned minio binary —
//! see `s3_provision`). CI sets `NAATRE_TEST_S3_REQUIRED=1`, which turns
//! that skip into a panic: a required-but-skipped leg is green vacuously.
//!
//! The legs are SYNC tests driving an explicit current-thread runtime (the
//! same shape m10a's S3 suite uses): the S3Store owns an internal runtime,
//! and tokio refuses to drop one inside an async context — the fixture
//! must drop on a plain thread, after `block_on` returns.

use std::path::PathBuf;
use std::sync::Arc;

use axum::Router;
use axum::body::Body;
use axum::http::{StatusCode, header};
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use http_body_util::BodyExt;
use rusqlite::Connection;
use serde_json::{Value, json};
use tower::ServiceExt;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::ServerState;
use naatre_store::{DataDir, backend::BackendConfig};

#[path = "../../naatre-store/tests/s3_provision/mod.rs"]
mod s3_provision;

const BASE_NS: u64 = 1_700_000_000_000_000_000;
const REPO: &str = "m9s3routes";

struct S3Fixture {
    _dir: tempfile::TempDir,
    _target: s3_provision::S3Target,
    app: Router,
    writer: String,
    db_path: PathBuf,
}

/// The `blob = "s3"` server (SQLite metadata + the S3 CAS/ephemeral pair),
/// background tasks off, wrapped in the real router with a seeded repo and
/// a writer token.
fn fixture() -> Option<S3Fixture> {
    static LOG: std::sync::Once = std::sync::Once::new();
    LOG.call_once(|| naatre_server::middleware::logging_setup("error"));
    let target = s3_provision::acquire()?;
    let dir = tempfile::tempdir().unwrap();
    let data = DataDir::create(dir.path()).unwrap();
    std::fs::write(data.db_path(), b"").unwrap();
    let data = DataDir::open(dir.path()).unwrap();
    let cfg: BackendConfig = target.config(); // meta = sqlite, blob = s3
    let mut state = ServerState::open_backend(data, &cfg).expect("s3-backed state opens");
    state.snapshot_gc_enabled = false;
    state.gc_enabled = false;
    state.derive_queue_enabled = false;
    state.derive_backfill_enabled = false;
    let state = Arc::new(state);

    let db_path = dir.path().join("naatre.db");
    let conn = Connection::open(&db_path).unwrap();
    conn.execute(
        "INSERT INTO repos(name, created_ns) VALUES (?1, ?2)",
        rusqlite::params![REPO, BASE_NS as i64],
    )
    .unwrap();
    let repo_id = conn.last_insert_rowid();
    // A real (rootless, empty) root changeset with main pointing at it —
    // the changeset-create parent validation needs known ids.
    let root_id = Id({
        let mut b = [0u8; 32];
        b[0] = 0x8F;
        b
    });
    conn.execute(
        "INSERT INTO changesets(repo_id, id, canonical, author, message, timestamp_ns, generation, principal, root_tree_id) \
         VALUES (?1, ?2, X'00', 'seeder', 'root', ?3, 0, 'seeder', ?2)",
        rusqlite::params![repo_id, root_id.0.as_slice(), BASE_NS as i64],
    )
    .unwrap();
    conn.execute(
        "INSERT INTO trees(id, repo_id, canonical) VALUES (?1, ?2, X'00')",
        rusqlite::params![root_id.0.as_slice(), repo_id],
    )
    .unwrap();
    conn.execute(
        "INSERT INTO refs(repo_id, name, target, version) VALUES (?1, 'main', ?2, 1)",
        rusqlite::params![repo_id, root_id.0.as_slice()],
    )
    .unwrap();
    drop(conn);

    let writer = seed_token(&db_path, "writer-p", "writer");
    let app = naatre_server::router(Arc::clone(&state));
    Some(S3Fixture {
        _dir: dir,
        _target: target,
        app,
        writer,
        db_path,
    })
}

/// Direct-DB seed: principal + token row, returning the token.
fn seed_token(db_path: &std::path::Path, principal: &str, role: &str) -> String {
    let (token, hash) = naatre_server::auth::mint_token();
    let conn = Connection::open(db_path).unwrap();
    conn.execute(
        "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3)",
        rusqlite::params![principal, role, BASE_NS as i64],
    )
    .unwrap();
    conn.execute(
        "INSERT INTO tokens(principal, blake3, expires_ns, revoked, scope_path) \
         VALUES (?1, ?2, ?3, 0, NULL)",
        rusqlite::params![principal, hash, Option::<i64>::None],
    )
    .unwrap();
    token
}

impl S3Fixture {
    /// Count of rows matching a bound object id (the metadata side the
    /// routes must keep honest above the seam).
    fn object_rows(&self, id: &Id) -> i64 {
        Connection::open(&self.db_path)
            .unwrap()
            .query_row(
                "SELECT COUNT(*) FROM objects WHERE object_id = ?1",
                [&id.0[..]],
                |r| r.get(0),
            )
            .unwrap()
    }
}

struct Resp {
    status: StatusCode,
    body: Value,
    bytes: Vec<u8>,
}

async fn send_json(
    app: &Router,
    method: &str,
    uri: &str,
    token: &str,
    body: Option<Value>,
) -> Resp {
    let req = axum::http::Request::builder()
        .method(method)
        .uri(uri)
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(body.map(|b| b.to_string()).unwrap_or_default()))
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    let status = res.status();
    let bytes = res.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Resp {
        status,
        body,
        bytes: bytes.to_vec(),
    }
}

async fn put_raw(app: &Router, uri: &str, token: &str, content: &[u8]) -> Resp {
    let req = axum::http::Request::builder()
        .method("PUT")
        .uri(uri)
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .body(Body::from(content.to_vec()))
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    let status = res.status();
    let bytes = res.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Resp {
        status,
        body,
        bytes: bytes.to_vec(),
    }
}

async fn get_raw(app: &Router, uri: &str, token: &str) -> Resp {
    let req = axum::http::Request::builder()
        .method("GET")
        .uri(uri)
        .header(header::AUTHORIZATION, format!("Bearer {token}"))
        .body(Body::empty())
        .unwrap();
    let res = app.clone().oneshot(req).await.unwrap();
    let status = res.status();
    let bytes = res.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Resp {
        status,
        body,
        bytes: bytes.to_vec(),
    }
}

// ---- 1. raw PUT ----------------------------------------------------------------

#[test]
fn s3_raw_put_records_row_and_roundtrips() {
    let Some(f) = fixture() else {
        return; // s3_provision already printed the skip notice
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    rt.block_on(async {
        let content = b"s3 raw put through the route\n";
        let id = object_id(Kind::Blob, content);
        let r = put_raw(
            &f.app,
            &format!("/v1/repos/{REPO}/objects/{}", id.to_hex()),
            &f.writer,
            content,
        )
        .await;
        assert_eq!(r.status, StatusCode::CREATED, "{}", r.body);

        // The row EXISTS with a real csize — the pre-fix bug answered 500
        // here (blob in the bucket, row never upserted, invisible forever).
        let (n, csize): (i64, i64) = Connection::open(&f.db_path)
            .unwrap()
            .query_row(
                "SELECT COUNT(*), COALESCE(MAX(csize), 0) FROM objects WHERE object_id = ?1",
                [&id.0[..]],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .unwrap();
        assert_eq!(n, 1, "the objects row must exist after an S3 raw PUT");
        assert!(csize > 0, "csize comes from the backend, not a local stat");

        // Idempotent re-PUT: 200, not another row.
        let r2 = put_raw(
            &f.app,
            &format!("/v1/repos/{REPO}/objects/{}", id.to_hex()),
            &f.writer,
            content,
        )
        .await;
        assert_eq!(r2.status, StatusCode::OK, "{}", r2.body);

        // The bytes read back hash-verified from the bucket. Membership
        // gates raw fetches (it arrives with changesets — see the routes'
        // module docs), so the leg grants it directly; the commit e2e leg
        // exercises the changeset-granted path.
        Connection::open(&f.db_path)
            .unwrap()
            .execute(
                "INSERT INTO repo_blobs(repo_id, object_id) VALUES (1, ?1)",
                [&id.0[..]],
            )
            .unwrap();
        let g = get_raw(
            &f.app,
            &format!("/v1/repos/{REPO}/objects/{}", id.to_hex()),
            &f.writer,
        )
        .await;
        assert_eq!(g.status, StatusCode::OK, "{}", g.body);
        assert_eq!(g.bytes, content);
    });
    // `f` (and the S3Store it holds) drops HERE, outside any runtime.
}

// ---- 2. batch PUT ----------------------------------------------------------------

#[test]
fn s3_batch_put_records_rows_and_reads_back() {
    let Some(f) = fixture() else {
        return;
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    rt.block_on(async {
        let contents: Vec<Vec<u8>> = (0..3)
            .map(|i| format!("s3 batch entry {i}\n").into_bytes())
            .collect();
        let entries: Vec<Value> = contents
            .iter()
            .map(|c| {
                let id = object_id(Kind::Blob, c);
                json!({
                    "id": id.to_hex(),
                    "data_b64": STANDARD.encode(c),
                })
            })
            .collect();
        let r = send_json(
            &f.app,
            "POST",
            &format!("/v1/repos/{REPO}/objects"),
            &f.writer,
            Some(json!({ "objects": entries })),
        )
        .await;
        assert_eq!(r.status, StatusCode::OK, "{}", r.body);
        assert_eq!(r.body["rejected"].as_array().map(Vec::len), Some(0));

        // Every entry's row exists (the batch form of the same
        // above-the-seam bug would have left the bucket with blobs and the
        // DB with nothing).
        let stored: Vec<&str> = r.body["stored"]
            .as_array()
            .unwrap()
            .iter()
            .map(|v| v.as_str().unwrap())
            .collect();
        assert_eq!(stored.len(), contents.len());
        for hex in &stored {
            let id = Id::from_hex(hex).unwrap();
            assert_eq!(f.object_rows(&id), 1, "row for {hex}");
        }

        // Batch fetch returns every payload byte-identical. Membership
        // gates fetches (it arrives with changesets), so the leg grants it
        // directly; the commit e2e leg exercises the granted path.
        let ids: Vec<String> = contents
            .iter()
            .map(|c| object_id(Kind::Blob, c).to_hex())
            .collect();
        {
            let conn = Connection::open(&f.db_path).unwrap();
            for hex in &ids {
                let id = Id::from_hex(hex).unwrap();
                conn.execute(
                    "INSERT INTO repo_blobs(repo_id, object_id) VALUES (1, ?1)",
                    [&id.0[..]],
                )
                .unwrap();
            }
        }
        let g = send_json(
            &f.app,
            "POST",
            &format!("/v1/repos/{REPO}/objects/get"),
            &f.writer,
            Some(json!({ "ids": ids })),
        )
        .await;
        assert_eq!(g.status, StatusCode::OK, "{}", g.body);
        for (i, c) in contents.iter().enumerate() {
            let entry = &g.body["objects"][i];
            let got = STANDARD
                .decode(entry["data_b64"].as_str().unwrap())
                .unwrap();
            assert_eq!(&got, c);
        }
    });
}

// ---- 3. commit e2e ----------------------------------------------------------------

#[test]
fn s3_commit_e2e_stores_history() {
    let Some(f) = fixture() else {
        return;
    };
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    rt.block_on(async {
        // Blob PUT through the S3-backed CAS…
        let content = b"committed over s3\n";
        let blob = object_id(Kind::Blob, content);
        let p = put_raw(
            &f.app,
            &format!("/v1/repos/{REPO}/objects/{}", blob.to_hex()),
            &f.writer,
            content,
        )
        .await;
        assert_eq!(p.status, StatusCode::CREATED, "{}", p.body);

        // …the changeset that references it…
        let main = get_raw(&f.app, &format!("/v1/repos/{REPO}/refs/main"), &f.writer).await;
        let parent = serde_json::from_slice::<Value>(&main.bytes).unwrap()["target"]
            .as_str()
            .unwrap()
            .to_string();
        let cs = send_json(
            &f.app,
            "POST",
            &format!("/v1/repos/{REPO}/changesets"),
            &f.writer,
            Some(json!({
                "parents": [parent],
                "author": "S3 <s3@naatre.dev>",
                "message": "s3 route e2e",
                "changes": [
                    { "path": "over-s3.txt", "op": "add", "blob": blob.to_hex(), "size": content.len(), "exec": false }
                ],
            })),
        )
        .await;
        assert_eq!(cs.status, StatusCode::CREATED, "{}", cs.body);
        let cs_id = cs.body["id"].as_str().unwrap().to_string();

        // …and the membership + read-back the changeset grants: the blob
        // is served from the bucket, hash-verified, byte-identical.
        let g = get_raw(
            &f.app,
            &format!("/v1/repos/{REPO}/objects/{}", blob.to_hex()),
            &f.writer,
        )
        .await;
        assert_eq!(g.status, StatusCode::OK, "{}", g.body);
        assert_eq!(g.bytes, content);

        // The file route resolves the new changeset's tree to the same
        // blob.
        let file = send_json(
            &f.app,
            "GET",
            &format!("/v1/repos/{REPO}/file?at={cs_id}&path=over-s3.txt"),
            &f.writer,
            None,
        )
        .await;
        assert_eq!(file.status, StatusCode::OK, "{}", file.body);
        assert_eq!(
            file.body["blob_id"].as_str(),
            Some(blob.to_hex().as_str())
        );
    });
}
