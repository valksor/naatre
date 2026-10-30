//! M4b: the M2a derivation oracle re-anchored on REBASED changesets. After
//! a land that rewrote a two-node head chain, every rebased node must
//! satisfy: replaying its delta from its parent's root reproduces its
//! recorded root_tree_id, the materialization of that root (via /trees)
//! equals the replayed tree entry by entry, and the whole rebased chain
//! replays from the fork base to the landed root.

use axum::http::StatusCode;
use serde_json::{Value, json};

use naatre_core::changeset::{FileChange, Op};
use naatre_core::ids::Id;
use naatre_server::tree_derive::MemNode;

mod m4b_common;

use m4b_common::{
    Fixture, REPO, add, cs_body, fixture, get_on, log_chain_paged, materialize_tree, mod_,
};

/// The `changes` array of a changeset response as FileChanges.
async fn changes_of(f: &Fixture, cs: &str) -> Vec<FileChange> {
    let resp = get_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{cs}"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "changeset {cs}: {}", resp.body);
    decode_changes(&resp.body)
}

fn decode_changes(body: &Value) -> Vec<FileChange> {
    body["changes"]
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
        .collect()
}

async fn root_of(f: &Fixture, cs: &str) -> String {
    let resp = get_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/changesets/{cs}/root"),
        &f.reader,
    )
    .await;
    assert_eq!(resp.status, StatusCode::OK, "root {cs}: {}", resp.body);
    resp.body["root_tree_id"].as_str().unwrap().to_string()
}

/// Flattens a MemNode to the same shape as [`materialize_tree`].
fn node_files(node: &MemNode) -> Vec<(String, String, u64, bool)> {
    let mut out: Vec<(String, String, u64, bool)> = naatre_server::merge::flatten_files(node)
        .into_iter()
        .map(|(path, meta)| (path, meta.blob.to_hex(), meta.size, meta.exec))
        .collect();
    out.sort_by(|a, b| a.0.cmp(&b.0));
    out
}

#[tokio::test]
async fn rebased_nodes_satisfy_the_root_tree_oracle() {
    let f = fixture();

    // Fork base c1: one.txt (6 lines) + other.txt.
    let one_base = b"l1\nl2\nl3\nl4\nl5\nl6\n".to_vec();
    let other_base = b"o1\no2\n".to_vec();
    let b1 = f.put_blob(&one_base).await;
    let b2 = f.put_blob(&other_base).await;
    let (c1, _) = f
        .create_ok(&cs_body(
            json!([]),
            json!([
                add("one.txt", &b1, one_base.len()),
                add("other.txt", &b2, other_base.len()),
            ]),
            "c1",
        ))
        .await;
    f.seed_main(&c1);

    // Head chain: c2 edits line 1 of one.txt and adds new.txt; c3 edits
    // line 2 (a two-node chain, both rebased by the land).
    let one_a = b"HEAD1\nl2\nl3\nl4\nl5\nl6\n".to_vec();
    let fresh = b"fresh\n".to_vec();
    let ba = f.put_blob(&one_a).await;
    let bf = f.put_blob(&fresh).await;
    let (c2, _) = f
        .create_ok(&cs_body(
            json!([c1.clone()]),
            json!([
                add("new.txt", &bf, fresh.len()),
                mod_("one.txt", &ba, one_a.len()),
            ]),
            "c2",
        ))
        .await;
    let one_b = b"HEAD1\nHEAD2\nl3\nl4\nl5\nl6\n".to_vec();
    let bb = f.put_blob(&one_b).await;
    let (c3, _) = f
        .create_ok(&cs_body(
            json!([c2.clone()]),
            json!([mod_("one.txt", &bb, one_b.len())]),
            "c3",
        ))
        .await;

    // main side (adjacent region of one.txt + other.txt): lands first.
    let one_m = b"l1\nl2\nl3\nl4\nl5\nMAIN6\n".to_vec();
    let other_m = b"O1\no2\n".to_vec();
    let bm = f.put_blob(&one_m).await;
    let bo = f.put_blob(&other_m).await;
    let (c2p, _) = f
        .create_ok(&cs_body(
            json!([c1.clone()]),
            json!([
                mod_("one.txt", &bm, one_m.len()),
                mod_("other.txt", &bo, other_m.len()),
            ]),
            "c2p",
        ))
        .await;
    f.move_main(&c2p);

    let resp = f.land(&c3, "k-oracle").await;
    assert_eq!(resp.status, StatusCode::OK, "body: {}", resp.body);
    let landed = resp.body["landed"].as_str().unwrap().to_string();
    assert_eq!(resp.body["rebased"], 2);
    assert_ne!(landed, c3, "the land rewrote the head chain");

    // The rebased chain from the new main: landed → n2 → c2p → c1.
    let chain = log_chain_paged(&f.t.app, &f.reader, &landed, 2).await;
    assert_eq!(chain.len(), 4, "chain: {chain:?}");
    assert_eq!(chain[2], c2p, "rebased onto main's tip");
    assert_eq!(chain[3], c1, "rooted at the fork base");
    let (n3, n2) = (chain[0].clone(), chain[1].clone());

    // Per rebased node: replay its delta from its parent's root and compare
    // against both the recorded root_tree_id and the /trees materialization
    // of that root.
    let conn = m4b_common::conn_at(&f.t.db_path);
    for (node, parent) in [(&n2, &c2p), (&n3, &n2)] {
        let (node_changes, parent_root, recorded_root) =
            tokio::join!(changes_of(&f, node), root_of(&f, parent), root_of(&f, node),);
        let mut replay = MemNode::load(&conn, Some(Id::from_hex(&parent_root).unwrap())).unwrap();
        replay.apply(&node_changes).unwrap();
        let (replayed_root, _, _) = replay.collect().unwrap();
        assert_eq!(
            replayed_root.to_hex(),
            recorded_root,
            "rebased node {node}: parent-root replay must equal the recorded root_tree_id"
        );
        let recorded_files = materialize_tree(&f.t.app, &f.reader, &recorded_root).await;
        let stored = MemNode::load(&conn, Some(Id::from_hex(&recorded_root).unwrap())).unwrap();
        assert_eq!(
            node_files(&stored),
            recorded_files,
            "rebased node {node}: materialized root equals the replayed tree"
        );
    }

    // Whole-chain replay from the fork base: c1 + c2p + both rebased nodes'
    // deltas, in order, reproduce the landed root.
    let mut whole = MemNode::empty();
    for cs in [&c1, &c2p, &n2, &n3] {
        let changes = changes_of(&f, cs).await;
        whole.apply(&changes).unwrap();
    }
    let (whole_root, _, _) = whole.collect().unwrap();
    assert_eq!(whole_root.to_hex(), root_of(&f, &n3).await);

    // The exact union content at the landed tip.
    let union_one = b"HEAD1\nHEAD2\nl3\nl4\nl5\nMAIN6\n".to_vec();
    assert_eq!(
        m4b_common::cat_on(&f.t.app, &f.reader, &landed, "one.txt").await,
        union_one
    );
    assert_eq!(
        m4b_common::cat_on(&f.t.app, &f.reader, &landed, "other.txt").await,
        other_m
    );
    assert_eq!(
        m4b_common::cat_on(&f.t.app, &f.reader, &landed, "new.txt").await,
        fresh
    );
}
