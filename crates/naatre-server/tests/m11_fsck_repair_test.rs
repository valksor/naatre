//! M11 `naatre fsck --repair`: the one write fsck can do, scoped to EXACTLY
//! the dirt class the pre-M11 import left behind — blobs this store
//! REFERENCES (repo_blobs membership / file_changes) whose CAS bytes are
//! present but whose `objects` record row is missing. Pins: the pre-fix
//! shape reports the dirt; the repair backfills and names the ids; an
//! UNRELATED dirt class survives untouched; a second repair run is a
//! no-op; exactly one audit row names the count. SQLite + fs (the legacy
//! stores this repairs are SQLite+fs imports).

use std::path::PathBuf;
use std::sync::Arc;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::db;
use naatre_server::fsck;
use naatre_server::state::ServerState;
use naatre_store::{BackendConfig, DataDir, Sql, Store};

const BASE_NS: i64 = 1_700_000_000_000_000_000;

/// A pre-fix-shaped store: one repo, one blob with CAS bytes,
/// `repo_blobs` membership AND a `file_changes` reference — but NO
/// `objects` row (what import wrote before M11's record step). Plus one
/// UNRELATED dangling row (a changeset parent that does not exist) to pin
/// the repair's scope.
struct PreFix {
    _root: tempfile::TempDir,
    data: PathBuf,
    blob: Id,
}

fn seed_prefix() -> PreFix {
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    std::fs::create_dir_all(&data).unwrap();
    std::fs::write(data.join("naatre.db"), b"").unwrap();
    let cfg = BackendConfig::default();
    let store = Store::open_backend(DataDir::open_any(&data), &cfg).unwrap();
    let conn = store.conn();

    let repo_id = db::repos_create(conn, "legacy", BASE_NS).unwrap();
    let content = b"legacy import bytes\n";
    let blob = object_id(Kind::Blob, content);
    let cas = naatre_store::blob::history_cas(&DataDir::open_any(&data), &cfg).unwrap();
    cas.put(content).unwrap();
    // The reference rows — and deliberately NOT the objects row.
    conn.execute(
        &Sql::plain("INSERT INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)"),
        &naatre_store::pv![repo_id, blob.0.as_slice()],
    )
    .unwrap();
    conn.execute(
        &Sql::plain(
            "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
             VALUES (?1, ?2, 'f.txt', 0, ?3, ?4, 0)",
        ),
        &naatre_store::pv![repo_id, [0xEE; 32], blob.0.as_slice(), content.len() as i64],
    )
    .unwrap();
    // The unrelated dirt: a parent reference nothing satisfies.
    conn.execute(
        &Sql::plain(
            "INSERT INTO changeset_parents(repo_id, cs, parent, ord) VALUES (?1, ?2, ?3, 0)",
        ),
        &naatre_store::pv![repo_id, [0xDD; 32], [0xCC; 32]],
    )
    .unwrap();
    PreFix {
        _root: root,
        data,
        blob,
    }
}

fn state_of(data: &std::path::Path) -> Arc<ServerState> {
    Arc::new(ServerState::open_backend(DataDir::open_any(data), &BackendConfig::default()).unwrap())
}

fn danglings_of(report: &fsck::FsckReport, name: &str) -> usize {
    report
        .danglings
        .iter()
        .find(|(n, _)| n == name)
        .map(|(_, c)| *c)
        .unwrap_or(0)
}

#[tokio::test]
async fn repair_backfills_exactly_the_missing_record_rows() {
    let d = seed_prefix();

    // The pre-fix shape reports the dirt; a plain scan backfills nothing.
    let report = fsck::fsck(&state_of(&d.data)).await.unwrap();
    assert_eq!(danglings_of(&report, "membership_row_object_missing"), 1);
    assert_eq!(danglings_of(&report, "file_change_blob_row_missing"), 1);
    assert!(!report.clean());
    assert!(report.backfilled.is_empty());

    // The repair backfills the row (size from the verified CAS bytes) and
    // names it — and leaves the UNRELATED dirt class untouched.
    let report = fsck::fsck_repair(&state_of(&d.data)).await.unwrap();
    assert_eq!(
        report.backfilled,
        vec![d.blob.to_hex()],
        "{:?}",
        report.backfilled
    );
    assert_eq!(danglings_of(&report, "membership_row_object_missing"), 0);
    assert_eq!(danglings_of(&report, "file_change_blob_row_missing"), 0);
    assert_eq!(
        danglings_of(&report, "changeset_parent_missing"),
        1,
        "the unrelated dirt class is NOT the repair's business"
    );
    assert!(!report.clean(), "the unrelated finding still stands");

    // The backfilled row carries the verified sizes: uncompressed from the
    // read path, stored from the CAS put contract.
    let store = Store::open_backend(DataDir::open_any(&d.data), &BackendConfig::default()).unwrap();
    let (size, csize): (i64, i64) = store
        .conn()
        .query_row_map(
            &Sql::plain("SELECT size, csize FROM objects WHERE object_id = ?1"),
            &naatre_store::pv![d.blob.0.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .unwrap();
    assert_eq!(size, "legacy import bytes\n".len() as i64);
    // csize is the STORED size — the exact shard file's length (zstd
    // framing can exceed the raw size on tiny payloads).
    let hex = d.blob.to_hex();
    let shard = d
        .data
        .join("objects")
        .join(&hex[0..2])
        .join(&hex[2..4])
        .join(&hex[4..]);
    assert_eq!(csize as u64, std::fs::metadata(&shard).unwrap().len());
    let cas =
        naatre_store::blob::history_cas(&DataDir::open_any(&d.data), &BackendConfig::default())
            .unwrap();
    assert_eq!(cas.get(&d.blob).unwrap(), b"legacy import bytes\n");

    // The unrelated row goes (the test's own cleanup): the store is clean.
    store
        .conn()
        .execute(
            &Sql::plain("DELETE FROM changeset_parents WHERE parent = ?1"),
            &naatre_store::pv![[0xCC; 32]],
        )
        .unwrap();
    let report = fsck::fsck(&state_of(&d.data)).await.unwrap();
    assert!(
        report.clean(),
        "fsck exits clean after the repair: {report:?}"
    );

    // A second repair run is a NO-OP: nothing to backfill, no new audit.
    let report = fsck::fsck_repair(&state_of(&d.data)).await.unwrap();
    assert!(report.backfilled.is_empty());
    let audits: i64 = store
        .conn()
        .query_row_map(
            &Sql::plain("SELECT COUNT(*) FROM audit_log WHERE action = 'fsck_repair'"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(audits, 1, "exactly one audit row names the count");
    let details: String = store
        .conn()
        .query_row_map(
            &Sql::plain("SELECT details_json FROM audit_log WHERE action = 'fsck_repair'"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert!(details.contains("\"backfilled_objects\":1"), "{details}");
}

/// Round 4: the backfill's two EXCLUSIONS, pinned together — a tombstoned
/// id (active OR purged, the objects table is global) never gains a row,
/// and an absent-bytes id never gains one (the size comes from the CAS
/// bytes; without them the repair refuses to write). The clean id in the
/// same store backfills normally.
#[tokio::test]
async fn repair_skips_tombstoned_and_absent_bytes() {
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    std::fs::create_dir_all(&data).unwrap();
    std::fs::write(data.join("naatre.db"), b"").unwrap();
    let cfg = BackendConfig::default();
    let store = Store::open_backend(DataDir::open_any(&data), &cfg).unwrap();
    let conn = store.conn();
    let repo_id = db::repos_create(conn, "legacy", BASE_NS).unwrap();
    let cas = naatre_store::blob::history_cas(&DataDir::open_any(&data), &cfg).unwrap();

    let mk_blob = |bytes: &[u8]| object_id(Kind::Blob, bytes);
    let blob_clean = mk_blob(b"repair me\n");
    let blob_tombstoned_active = mk_blob(b"redacted, active tombstone\n");
    let blob_tombstoned_purged = mk_blob(b"redacted, purged tombstone\n");
    let blob_absent = mk_blob(b"never had bytes\n");
    for blob in [
        &blob_clean,
        &blob_tombstoned_active,
        &blob_tombstoned_purged,
    ] {
        cas.put(object_bytes(blob)).unwrap();
    }
    // blob_absent: membership + reference, deliberately NO CAS bytes.

    // The reference rows for all four — and deliberately NO objects rows.
    let reference = |blob: &Id, cs: [u8; 32], path: &str| {
        conn.execute(
            &Sql::plain("INSERT INTO repo_blobs(repo_id, object_id) VALUES (?1, ?2)"),
            &naatre_store::pv![repo_id, blob.0.as_slice()],
        )
        .unwrap();
        conn.execute(
            &Sql::plain(
                "INSERT INTO file_changes(repo_id, cs, path, op, blob_id, size, exec) \
                 VALUES (?1, ?2, ?3, 0, ?4, 0, 0)",
            ),
            &naatre_store::pv![repo_id, cs, path, blob.0.as_slice()],
        )
        .unwrap();
    };
    reference(&blob_clean, [0x11; 32], "clean.txt");
    reference(&blob_tombstoned_active, [0x12; 32], "active.txt");
    reference(&blob_tombstoned_purged, [0x13; 32], "purged.txt");
    reference(&blob_absent, [0x14; 32], "absent.txt");

    // The tombstones: one ACTIVE, one PURGED (the objects table is global,
    // so the exclusion covers a tombstone from ANY repo — both arms here).
    for (blob, purged) in [
        (&blob_tombstoned_active, None),
        (&blob_tombstoned_purged, Some(BASE_NS + 1)),
    ] {
        conn.execute(
            &Sql::plain(
                "INSERT INTO redactions(repo_id, object_id, kind, path, marked_ns, purged_ns) \
                 VALUES (?1, ?2, 'blob', NULL, ?3, ?4)",
            ),
            &naatre_store::pv![repo_id, blob.0.as_slice(), BASE_NS, purged],
        )
        .unwrap();
    }

    // The pre-repair scan reports the dirt; nothing backfilled yet.
    let report = fsck::fsck(&state_of(&data)).await.unwrap();
    assert_eq!(danglings_of(&report, "membership_row_object_missing"), 4);
    assert!(report.backfilled.is_empty());

    // The repair backfills ONLY the clean id: the tombstoned ids (active
    // or purged) and the absent-bytes id gain NO row.
    let report = fsck::fsck_repair(&state_of(&data)).await.unwrap();
    assert_eq!(report.backfilled, vec![blob_clean.to_hex()]);
    for blob in [
        &blob_tombstoned_active,
        &blob_tombstoned_purged,
        &blob_absent,
    ] {
        let rows: i64 = store
            .conn()
            .query_row_map(
                &Sql::plain("SELECT COUNT(*) FROM objects WHERE object_id = ?1"),
                &naatre_store::pv![blob.0.as_slice()],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(rows, 0, "no row for an excluded id");
    }
    // What remains is exactly what the exclusions explain: the tombstoned
    // ids' and the absent id's dangling references.
    assert_eq!(danglings_of(&report, "membership_row_object_missing"), 3);
    assert_eq!(danglings_of(&report, "file_change_blob_row_missing"), 3);
    assert!(!report.clean());

    // The audit row records ONLY a count — no ids, no paths, no content.
    let details: String = store
        .conn()
        .query_row_map(
            &Sql::plain("SELECT details_json FROM audit_log WHERE action = 'fsck_repair'"),
            &[],
            |r| r.get(0),
        )
        .unwrap();
    assert!(details.contains("\"backfilled_objects\":1"), "{details}");
    assert!(
        !details.contains(&blob_clean.to_hex()[..8])
            && !details.contains("repair me")
            && !details.contains("path"),
        "the audit row carries ids/content: {details}"
    );
}

/// The bytes behind an id — for building CAS fixtures in the exclusion pin.
fn object_bytes(blob: &Id) -> &'static [u8] {
    // The id IS content-derived, so the fixture maps ids back to the exact
    // bytes each test blob was built from.
    match blob.to_hex().as_str() {
        h if h == object_id(Kind::Blob, b"repair me\n").to_hex() => b"repair me\n",
        h if h == object_id(Kind::Blob, b"redacted, active tombstone\n").to_hex() => {
            b"redacted, active tombstone\n"
        }
        h if h == object_id(Kind::Blob, b"redacted, purged tombstone\n").to_hex() => {
            b"redacted, purged tombstone\n"
        }
        _ => unreachable!("only fixture ids are put into the CAS"),
    }
}
