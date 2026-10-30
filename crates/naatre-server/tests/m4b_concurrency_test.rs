//! M4b concurrency: eight racing lands over one base, concurrent identical
//! (key, payload), in-flight same-key different-payload, and pagination
//! read DURING lands. Every wait is bounded: each request runs under
//! `tokio::time::timeout`, 503 `writer_busy` retries are bounded by the
//! 30 s deadline (no unbounded retries), and the pager loop carries a
//! deadline — no sleep-polling without one.

use std::collections::HashSet;
use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::http::StatusCode;
use serde_json::json;
use tokio::task::JoinSet;

mod m4b_common;

use m4b_common::{DEADLINE, Fixture, REPO, add, cs_body, fixture, get_on, log_chain_paged};

/// One land attempt under a hard timeout (full router, auth included).
async fn land_once(f: &Fixture, head: &str, key: &str) -> m4b_common::Resp {
    tokio::time::timeout(DEADLINE, f.land(head, key))
        .await
        .expect("land request answered inside the deadline")
}

/// Land with 503 backpressure folded in: `writer_busy` retries (25 ms) are
/// bounded by the shared deadline; the returned response is the SETTLED
/// outcome (200, or the settled-rules 409).
async fn land_with_retry(f: &Fixture, head: &str, key: &str) -> m4b_common::Resp {
    let deadline = Instant::now() + DEADLINE;
    loop {
        let resp = land_once(f, head, key).await;
        match resp.status {
            StatusCode::SERVICE_UNAVAILABLE if Instant::now() < deadline => {
                tokio::time::sleep(Duration::from_millis(25)).await;
            }
            _ => return resp,
        }
    }
}

/// The shared fixture advanced to a seeded base: c1 (base.txt) is main.
async fn seeded_base() -> (Fixture, String) {
    let f = fixture();
    let b1 = f.put_blob(b"base\n").await;
    let (c1, _) = f
        .create_ok(&cs_body(json!([]), json!([add("base.txt", &b1, 5)]), "c1"))
        .await;
    f.seed_main(&c1);
    (f, c1)
}

/// N heads, one per racer: each adds its own distinct file on the base.
async fn racer_heads(f: &Fixture, base: &str, n: usize) -> Vec<(String, String)> {
    let mut heads = Vec::new();
    for i in 0..n {
        let content = format!("racer {i}\n");
        let blob = f.put_blob(content.as_bytes()).await;
        let path = format!("f{i}.txt");
        let (head, _) = f
            .create_ok(&cs_body(
                json!([base.to_string()]),
                json!([add(&path, &blob, content.len())]),
                &format!("racer{i}"),
            ))
            .await;
        heads.push((head, path));
    }
    heads
}

// ---- 1. eight racing lands ----------------------------------------------------

#[tokio::test]
async fn eight_racers_settle_exactly_once_each() {
    let (f, base) = seeded_base().await;
    let heads = racer_heads(&f, &base, 8).await;
    let f = Arc::new(f);

    let mut tasks = JoinSet::new();
    for (i, (head, path)) in heads.iter().enumerate() {
        let f = Arc::clone(&f);
        let head = head.clone();
        let path = path.clone();
        let key = format!("k-racer-{i}");
        tasks.spawn(async move {
            let resp = land_with_retry(&f, &head, &key).await;
            (key, head, path, resp.status, resp.body)
        });
    }
    let mut results = Vec::new();
    while let Some(done) = tasks.join_next().await {
        results.push(done.expect("racer task"));
    }

    // Every racer settles 200, with a distinct landed tip.
    let mut landed_ids = HashSet::new();
    for (key, head, _, status, body) in &results {
        assert_eq!(
            *status,
            StatusCode::OK,
            "racer {key} (head {head}) did not land: {body}"
        );
        let landed = body["landed"].as_str().expect("landed id").to_string();
        assert!(landed_ids.insert(landed), "two racers share a landed id");
    }

    // Every key settles exactly once (done), audited exactly once.
    assert_eq!(
        f.count("SELECT COUNT(*) FROM lands WHERE state = 'done'"),
        8,
        "exactly 8 settled lands"
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'land'"),
        8
    );

    // GET land/{key} agrees with each racer's own 200 response.
    for (key, head, _, _, body) in &results {
        let resp = get_on(&f.t.app, &format!("/v1/repos/{REPO}/land/{key}"), &f.writer).await;
        assert_eq!(resp.status, StatusCode::OK, "status {key}: {}", resp.body);
        assert_eq!(resp.body["state"], "done");
        assert_eq!(resp.body["head"], head.as_str());
        assert_eq!(resp.body["result"]["landed"], body["landed"]);
    }

    // main moved monotonically: re-reading the sequence — the first-parent
    // chain from final main — is a linear run through ALL 8 landed tips
    // down to the base (a lost update would strand a tip off the chain).
    let final_main = f.main_target().expect("main after the race");
    let chain = log_chain_paged(&f.t.app, &f.reader, &final_main, 2).await;
    assert_eq!(chain.len(), 9, "chain: {chain:?}");
    let mut expected: HashSet<String> = landed_ids.clone();
    expected.insert(base.clone());
    assert_eq!(
        chain.iter().cloned().collect::<HashSet<_>>(),
        expected,
        "the final main chain is exactly the base plus the 8 landed tips"
    );

    // Containment walk check: every 200-land's ORIGINAL head is contained
    // in final main — diffing original head vs final main may only add the
    // OTHER racers' files; the racer's own file is identical on both sides.
    for (_, head, own_path, _, body) in results.iter() {
        let landed = body["landed"].as_str().expect("landed id").to_string();
        let resp = get_on(
            &f.t.app,
            &format!("/v1/repos/{REPO}/diff?a={head}&b={final_main}"),
            &f.reader,
        )
        .await;
        assert_eq!(resp.status, StatusCode::OK, "diff: {}", resp.body);
        for change in resp.body["changes"].as_array().unwrap() {
            assert_ne!(
                change["path"],
                own_path.as_str(),
                "racer file {own_path} did not survive into final main"
            );
        }
        let idx: usize = own_path["f".len()..own_path.len() - ".txt".len()]
            .parse()
            .expect("racer index out of the file name");
        assert_eq!(
            m4b_common::cat_on(&f.t.app, &f.reader, &final_main, own_path).await,
            format!("racer {idx}\n").into_bytes(),
            "final main carries each racer's exact content"
        );
        assert!(landed_ids.contains(&landed));
    }
}

// ---- 2. concurrent identical (key, payload) ------------------------------------

#[tokio::test]
async fn concurrent_identical_key_and_head_settle_identically() {
    let (f, base) = seeded_base().await;
    let blob = f.put_blob(b"twin\n").await;
    let (head, _) = f
        .create_ok(&cs_body(
            json!([base]),
            json!([add("twin.txt", &blob, 5)]),
            "twin",
        ))
        .await;
    let f = Arc::new(f);

    // FULL router, auth included: reads (auth lookups included) run on the
    // server's read pool and never contend for the write slot, so the
    // loser either JOINS the in-flight land or — arriving after
    // deregistration — replays the settled row through its own transaction;
    // both observe the identical 200 with a single settlement and no 503.
    // Single attempts on purpose: a 503 would fail the status assert, so
    // land_with_retry's folding is deliberately not used.
    let (r1, r2) = tokio::join!(
        land_once(&f, &head, "k-twin"),
        land_once(&f, &head, "k-twin"),
    );
    assert_eq!(r1.status, StatusCode::OK, "body: {}", r1.body);
    assert_eq!(r2.status, StatusCode::OK, "body: {}", r2.body);
    assert_eq!(
        r1.body["landed"], r2.body["landed"],
        "identical (key, head) → identical landed ids"
    );
    assert_eq!(r1.body, r2.body, "the responses are byte-identical replays");

    // One settlement.
    let principal = "writer-p";
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM lands WHERE principal = ?1 AND key = ?2",
            &[&principal, &"k-twin"],
        ),
        1
    );
    assert_eq!(
        f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'land'"),
        1
    );
}

// ---- 3. in-flight same-key, different payload -----------------------------------

#[tokio::test]
async fn same_key_different_head_never_double_lands() {
    let (f, base) = seeded_base().await;
    let ba = f.put_blob(b"a\n").await;
    let bb = f.put_blob(b"b\n").await;
    let (head_a, _) = f
        .create_ok(&cs_body(
            json!([base.clone()]),
            json!([add("a.txt", &ba, 2)]),
            "a",
        ))
        .await;
    let (head_b, _) = f
        .create_ok(&cs_body(json!([base]), json!([add("b.txt", &bb, 2)]), "b"))
        .await;
    let f = Arc::new(f);

    // Two tasks, one key, two heads. The retry wrapper folds any 503 into
    // the settled rules; the observed FINAL outcomes must be one 200 and
    // one 409 — never a double land.
    let (ra, rb) = tokio::join!(
        land_with_retry(&f, &head_a, "k-clash"),
        land_with_retry(&f, head_b.as_str(), "k-clash"),
    );
    let responses = [
        ("a", head_a.clone(), ra.status, ra.body.clone()),
        ("b", head_b.clone(), rb.status, rb.body.clone()),
    ];
    let winners: Vec<_> = responses
        .iter()
        .filter(|(_, _, s, _)| *s == StatusCode::OK)
        .collect();
    let losers: Vec<_> = responses
        .iter()
        .filter(|(_, _, s, _)| *s == StatusCode::CONFLICT)
        .collect();
    assert_eq!(winners.len(), 1, "exactly one 200: {responses:?}");
    assert_eq!(losers.len(), 1, "exactly one 409: {responses:?}");
    assert_eq!(
        losers[0].3["error"]["code"], "idempotency_key_settled",
        "the loser hit the settled rules: {}",
        losers[0].3
    );
    assert_eq!(
        losers[0].3["error"]["details"]["settled_head"],
        winners[0].1.as_str(),
        "the settled head is the winner's head"
    );

    // One settlement, under the winner's head.
    let principal = "writer-p";
    assert_eq!(
        f.count_params(
            "SELECT COUNT(*) FROM lands WHERE principal = ?1 AND key = ?2",
            &[&principal, &"k-clash"],
        ),
        1
    );
    let resp = get_on(
        &f.t.app,
        &format!("/v1/repos/{REPO}/land/k-clash"),
        &f.writer,
    )
    .await;
    assert_eq!(resp.body["state"], "done");
    assert_eq!(resp.body["head"], winners[0].1.as_str());
}

// ---- 4. pagination during land ----------------------------------------------------

/// One full pass over /log (limit 2) from the CURRENT main, following
/// cursors to exhaustion.
async fn one_pass(f: &Fixture) -> Vec<String> {
    let mut ids = Vec::new();
    let mut cursor: Option<String> = None;
    loop {
        let uri = match &cursor {
            None => format!("/v1/repos/{REPO}/log?limit=2"),
            Some(c) => format!("/v1/repos/{REPO}/log?limit=2&cursor={c}"),
        };
        let resp = tokio::time::timeout(DEADLINE, get_on(&f.t.app, &uri, &f.reader))
            .await
            .expect("log page inside the deadline");
        assert_eq!(resp.status, StatusCode::OK, "log page: {}", resp.body);
        for e in resp.body["entries"].as_array().unwrap() {
            ids.push(e["id"].as_str().unwrap().to_string());
        }
        match resp.body["next_cursor"].as_str() {
            Some(c) => cursor = Some(c.to_string()),
            None => return ids,
        }
    }
}

#[tokio::test]
async fn pagination_during_land_drops_nothing() {
    let (f, base) = seeded_base().await;
    let heads = racer_heads(&f, &base, 8).await;
    let f = Arc::new(f);

    let racers_f = Arc::clone(&f);
    let racer_jobs: Vec<_> = heads
        .iter()
        .enumerate()
        .map(|(i, (head, _))| (head.clone(), format!("k-page-{i}")))
        .collect();
    let racers = tokio::spawn(async move {
        let mut out = Vec::new();
        for (head, key) in &racer_jobs {
            let resp = land_with_retry(&racers_f, head, key).await;
            out.push(resp.status);
        }
        out
    });

    // Page limit-2 passes while the lands run; stop one full pass after the
    // racers finish (deadline-bounded loop, 2 ms between passes). The pager
    // owns the racers' handle while it runs and hands it back with its
    // pages.
    let pager_f = Arc::clone(&f);
    let pager = tokio::spawn(async move {
        let mut paged = Vec::new();
        let deadline = Instant::now() + DEADLINE;
        loop {
            paged.extend(one_pass(&pager_f).await);
            if racers.is_finished() {
                paged.extend(one_pass(&pager_f).await);
                break;
            }
            assert!(Instant::now() < deadline, "pager exceeded the deadline");
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
        (paged, racers)
    });

    let (paged, racers) = pager.await.expect("pager");
    let statuses = racers.await.expect("racers");
    for (i, status) in statuses.iter().enumerate() {
        assert_eq!(*status, StatusCode::OK, "racer {i} did not land");
    }
    assert_eq!(statuses.len(), 8);

    // The tolerance under concurrent lands (documented plan limit): pages
    // may repeat entries, but there are no skips — every id ever served by
    // a page exists on the final main chain, and the final chain is fully
    // covered by the pages read.
    let final_main = f.main_target().expect("main after the race");
    let chain = log_chain_paged(&f.t.app, &f.reader, &final_main, 2).await;
    assert_eq!(chain.len(), 9, "chain: {chain:?}");
    let chain_set: HashSet<_> = chain.iter().cloned().collect();
    let paged_set: HashSet<_> = paged.into_iter().collect();
    for id in &chain_set {
        assert!(
            paged_set.contains(id),
            "data loss: {id} is on the final chain but never appeared in any page"
        );
    }
    for id in &paged_set {
        assert!(
            chain_set.contains(id),
            "ghost page: {id} was served by /log but is not on the final main chain"
        );
    }
}
