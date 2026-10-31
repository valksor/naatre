//! M2b suite: one-way git import. Fixtures are real git repositories built
//! by shelling out to the `git` CLI (with `-c user.name/email` and
//! GIT_AUTHOR/COMMITTER env for deterministic identities and dates); every
//! test asserts `git --version` works first and skips with a message when
//! git is absent — it never passes silently.
//!
//! Coverage: clean import (parent-set parity, apply-both-compare-trees,
//! main set), re-import no-op, unsupported-content abort in default mode,
//! `--skip-unsupported` with the cascade, graft A (fast-forward), graft B
//! (native lands → refused, dangling heads), graft C (rewritten source →
//! `git_history_diverged`), import-while-serving (flock), Cc+Cf
//! sanitization (U+202E message, non-UTF-8 author), and audit provenance
//! (ref_move + changeset_create rows carry via:"import").

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::process::Command;
use std::sync::{Arc, Mutex};

use rusqlite::Connection;

use naatre_core::changeset::{Changeset, FileChange, Op};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::db;
use naatre_server::import::{ImportError, ImportOptions, Report};
use naatre_server::state::ServerState;
use naatre_server::tree_derive::MemNode;
use naatre_store::cas::Cas;
use naatre_store::{DataDir, Store};

// ---- git availability ------------------------------------------------------

/// Asserts `git --version` works; `false` makes every test print a skip
/// message instead of pretending to pass.
fn git_works() -> bool {
    match Command::new("git").arg("--version").output() {
        Ok(out) if out.status.success() => true,
        Ok(out) => {
            eprintln!(
                "skipping import tests: git --version failed: {}",
                String::from_utf8_lossy(&out.stderr)
            );
            false
        }
        Err(e) => {
            eprintln!("skipping import tests: git not available: {e}");
            false
        }
    }
}

// ---- fixture harness -------------------------------------------------------

struct Fixture {
    _dir: tempfile::TempDir,
    path: PathBuf,
    commits: usize,
}

impl Fixture {
    fn new(tag: &str) -> Fixture {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(tag);
        let out = Command::new("git")
            .args(["-c", "init.defaultBranch=main", "init"])
            .arg("-q")
            .arg(&path)
            .output()
            .expect("run git init");
        assert!(
            out.status.success(),
            "git init failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        Fixture {
            _dir: dir,
            path,
            commits: 0,
        }
    }

    fn git(&self, args: &[&str]) -> String {
        String::from_utf8_lossy(&self.git_bytes(args))
            .trim()
            .to_string()
    }

    /// Raw stdout (binary-safe: `cat-file blob` of binary content).
    fn git_bytes(&self, args: &[&str]) -> Vec<u8> {
        let stamp = 1_767_225_600 + self.commits * 60;
        let out = Command::new("git")
            .args([
                "-c",
                "user.name=Fix Tur e",
                "-c",
                "user.email=fix@example.com",
            ])
            .arg("-C")
            .arg(&self.path)
            .args(args)
            .env("GIT_AUTHOR_DATE", format!("{stamp} +0000"))
            .env("GIT_COMMITTER_DATE", format!("{stamp} +0000"))
            .output()
            .expect("run git");
        assert!(
            out.status.success(),
            "git {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        out.stdout
    }

    /// Runs git with `input` on stdin (plumbing that builds objects).
    fn git_stdin(&self, args: &[&str], input: &[u8]) -> String {
        use std::io::Write;
        let mut child = Command::new("git")
            .arg("-C")
            .arg(&self.path)
            .args(args)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .spawn()
            .expect("spawn git");
        child.stdin.as_mut().unwrap().write_all(input).unwrap();
        let out = child.wait_with_output().unwrap();
        assert!(
            out.status.success(),
            "git {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    fn write(&self, rel: &str, content: &[u8]) {
        let full = self.path.join(rel);
        std::fs::create_dir_all(full.parent().unwrap()).unwrap();
        std::fs::write(full, content).unwrap();
    }

    /// Stages everything and commits with a verbatim message (no cleanup:
    /// CRLF and control characters survive into the commit object).
    fn commit(&mut self, msg: &str) -> String {
        self.git(&["add", "-A"]);
        self.git(&["commit", "-q", "--cleanup=verbatim", "-m", msg]);
        self.commits += 1;
        self.git(&["rev-parse", "HEAD"])
    }

    /// The clean fixture: root commit (incl. a deep path), an exec flip, a
    /// binary blob, a side-branch edit merged 2-parent, a delete, and a
    /// CRLF-message commit — nothing unsupported.
    fn clean(tag: &str) -> Fixture {
        let mut f = Fixture::new(tag);
        f.write("hello.txt", b"hello\n");
        f.write("script.sh", b"#!/bin/sh\necho hi\n");
        f.write("a/b/c/d/e.txt", b"deep\n");
        f.commit("root");

        f.write("script.sh", b"#!/bin/sh\necho hello\n");
        let script = f.path.join("script.sh");
        let mut perms = std::fs::metadata(&script).unwrap().permissions();
        use std::os::unix::fs::PermissionsExt;
        perms.set_mode(0o755);
        std::fs::set_permissions(&script, perms).unwrap();
        f.commit("flip exec");

        f.write("blob.bin", &[0x00u8, 0x01, 0xFF, 0x00, 0x42, 0x00, 0x7F]);
        f.commit("binary blob");

        f.git(&["checkout", "-q", "-b", "side"]);
        f.write("hello.txt", b"side\n");
        f.commit("side edit");

        f.git(&["checkout", "-q", "main"]);
        std::fs::remove_file(f.path.join("a/b/c/d/e.txt")).unwrap();
        f.write("p/q/r/s/t.txt", b"deep too\n");
        f.commit("delete deep, add deeper");

        f.git(&["merge", "--no-ff", "-q", "-m", "merge side", "side"]);
        f.commits += 1;

        f.write("crlf.txt", b"crlf\n");
        f.commit("crlf line1\r\nline2\r\n");
        f
    }

    /// The full fixture: clean history plus a symlink commit, a gitlink
    /// commit, and a U+202E-message commit on top.
    fn full(tag: &str) -> Fixture {
        let mut f = Fixture::clean(tag);
        std::os::unix::fs::symlink("hello.txt", f.path.join("link")).unwrap();
        f.commit("symlink");

        let fake_sub = "0123456789abcdef0123456789abcdef01234567";
        f.git(&[
            "update-index",
            "--add",
            "--cacheinfo",
            &format!("160000,{fake_sub},sub"),
        ]);
        // Commit the index directly: `add -A` would stage a deletion (the
        // gitlink has no worktree file), so this step skips the staging
        // helper.
        f.git(&["commit", "-q", "--cleanup=verbatim", "-m", "gitlink"]);
        f.commits += 1;

        f.write("after.txt", b"after unsupported\n");
        f.commit("evil \u{202E}message");
        f
    }
}

// ---- naatre harness --------------------------------------------------------

struct Env {
    _dir: tempfile::TempDir,
    data: PathBuf,
}

impl Env {
    /// An initialized data dir, as `naatre init` leaves it (naatre.db on
    /// disk; migrations run on first open).
    fn new() -> Env {
        let dir = tempfile::tempdir().unwrap();
        let data = dir.path().join("data");
        let dd = DataDir::create(&data).unwrap();
        std::fs::write(dd.db_path(), b"").unwrap();
        Store::open(DataDir::open(&data).unwrap()).unwrap();
        Env { _dir: dir, data }
    }

    fn conn(&self) -> naatre_store::Db {
        naatre_store::Db::Sqlite(Connection::open(self.data.join("naatre.db")).unwrap())
    }

    fn cas(&self) -> Cas {
        Cas::new(&DataDir::open(&self.data).unwrap()).unwrap()
    }

    fn repo_id(&self, name: &str) -> i64 {
        db::repo_id_by_name(&self.conn(), name).unwrap().unwrap()
    }

    fn count(&self, sql: &str) -> i64 {
        self.conn()
            .query_row_map(
                &naatre_store::Sql::plain_owned(sql.to_string()),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap()
    }

    fn import(&self, f: &Fixture, into: &str, skip: bool) -> Report {
        let opts = ImportOptions {
            git_path: f.path.clone(),
            into: Some(into.into()),
            skip_unsupported: skip,
        };
        naatre_server::import::run(&self.data, &opts, &Default::default()).unwrap()
    }

    fn main_target(&self, name: &str) -> Option<Id> {
        db::ref_get(&self.conn(), self.repo_id(name), "main")
            .unwrap()
            .and_then(|row| row.target)
            .map(|t| Id(t.try_into().unwrap()))
    }

    /// git oid (40-hex, lowercase) → naatre changeset id.
    fn git_map(&self, repo_id: i64) -> BTreeMap<String, Id> {
        self.conn()
            .query_all_map(
                &naatre_store::Sql::plain(
                    "SELECT lower(hex(git_oid)), cs_id FROM git_map WHERE repo_id = ?1",
                ),
                &naatre_store::pv![repo_id],
                |r| Ok((r.get::<String>(0)?, r.get::<Vec<u8>>(1)?)),
            )
            .unwrap()
            .into_iter()
            .map(|(oid, cs)| (oid, Id(cs.try_into().unwrap())))
            .collect()
    }
}

fn json_of(report: &Report) -> serde_json::Value {
    serde_json::from_str(&report.to_json_line()).expect("report JSON line parses")
}

// ---- 1. clean import -------------------------------------------------------

#[test]
fn clean_import_full_parity() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let f = Fixture::clean("clean");
    let env = Env::new();
    let report = env.import(&f, "imp", false);

    assert_eq!(report.status, "ok");
    assert_eq!(report.imported, 7);
    assert!(report.skipped.is_empty() && report.cascaded.is_empty() && report.sanitized.is_empty());
    assert_eq!(report.main, "set");
    assert!(report.reason.is_none());

    // The report's JSON line carries exactly the pinned keys.
    let json = json_of(&report);
    assert_eq!(json["imported"].as_u64(), Some(7));
    assert_eq!(json["main"], "set");
    assert_eq!(json["skipped"], serde_json::json!([]));
    assert_eq!(json["cascaded"], serde_json::json!([]));
    assert_eq!(json["sanitized"], serde_json::json!([]));
    assert!(json["reason"].is_null());
    assert_eq!(json.as_object().unwrap().len(), 6, "report keys: {json:?}");

    let repo_id = env.repo_id("imp");
    let map = env.git_map(repo_id);
    assert_eq!(map.len(), 7, "every commit is mapped");

    // Parent-set parity, both orders: each naatre changeset's parents equal
    // the mapped parents of the git commit's parents.
    for line in f.git(&["rev-list", "--parents", "HEAD"]).lines() {
        let mut parts = line.split(' ');
        let oid = parts.next().unwrap();
        let git_parents: Vec<&str> = parts.collect();
        let cs = map[oid];
        let graph = db::changeset_graph(&env.conn(), repo_id, &cs)
            .unwrap()
            .expect("imported changeset exists");
        let mapped: Vec<Id> = git_parents.iter().map(|p| map[*p]).collect();
        assert_eq!(graph.parents, mapped, "parent parity for {oid}");
    }

    // The head commit maps to a changeset whose metadata mirrors git's.
    let head_oid = f.git(&["rev-parse", "HEAD"]);
    let head_cs = map[&head_oid];
    let row = db::changeset_get(&env.conn(), repo_id, &head_cs)
        .unwrap()
        .expect("head changeset");
    assert_eq!(row.author, "Fix Tur e <fix@example.com>");
    let git_ts: i64 = f
        .git(&["log", "-1", "--format=%at", &head_oid])
        .parse()
        .unwrap();
    assert_eq!(row.timestamp_ns, git_ts * 1_000_000_000);

    // The CRLF-message commit imported with the one documented mutation:
    // \r\n → \n, nothing else.
    let crlf_line = f.git(&["log", "--format=%H", "--grep=crlf"]);
    let crlf_cs = map[crlf_line.trim()];
    let crlf_row = db::changeset_get(&env.conn(), repo_id, &crlf_cs)
        .unwrap()
        .unwrap();
    assert_eq!(crlf_row.message, "crlf line1\nline2\n");
    assert!(!crlf_row.message.contains('\r'));

    // main set to the head changeset.
    assert_eq!(env.main_target("imp"), Some(head_cs));

    // Apply-both-compare-trees: the naatre root tree of the head and
    // `git ls-tree -r HEAD` agree on paths, exec bits, and content bytes.
    let naatre_files = compare_trees(&env, &f, row.root_tree_id.as_deref().expect("root tree"));
    let final_paths: Vec<&str> = naatre_files.keys().map(String::as_str).collect();
    assert!(final_paths.contains(&"p/q/r/s/t.txt"));
    assert!(
        !final_paths.iter().any(|p| p.starts_with("a/b/c/")),
        "deleted subtree gone"
    );

    // Audit trail: repo_create, per-commit changeset_create, ref_move, import.
    let actions: Vec<String> = {
        env.conn()
            .query_all_map(
                &naatre_store::Sql::plain("SELECT action FROM audit_log ORDER BY id"),
                &naatre_store::pv![],
                |r| r.get(0),
            )
            .unwrap()
    };
    assert_eq!(actions.iter().filter(|a| *a == "repo_create").count(), 1);
    assert_eq!(
        actions.iter().filter(|a| *a == "changeset_create").count(),
        7
    );
    assert!(actions.contains(&"ref_move".to_string()));
    assert!(actions.contains(&"import".to_string()));
}

/// Recursively flattens the naatre tree under `root`: path → (exec, blob).
fn naatre_tree_files(conn: &naatre_store::Db, root: &Id) -> BTreeMap<String, (bool, Id)> {
    let mut out = BTreeMap::new();
    walk_tree(conn, root, "", &mut out);
    out
}

fn walk_tree(
    conn: &naatre_store::Db,
    tree: &Id,
    prefix: &str,
    out: &mut BTreeMap<String, (bool, Id)>,
) {
    for e in db::tree_entries_load(conn, tree).unwrap() {
        let path = format!("{prefix}{}", e.name);
        match e.kind {
            0 => {
                out.insert(path, (e.exec, Id(e.object_id.try_into().unwrap())));
            }
            1 => walk_tree(
                conn,
                &Id(e.object_id.try_into().unwrap()),
                &format!("{path}/"),
                out,
            ),
            other => panic!("unexpected tree kind {other}"),
        }
    }
}

/// The apply-both-compare-trees oracle: walk the naatre root tree,
/// materialize both sides (`git ls-tree -r HEAD` + `git cat-file`), and
/// require identical path sets, exec bits, and content bytes.
fn compare_trees(env: &Env, f: &Fixture, root_tree: &[u8]) -> BTreeMap<String, (bool, Id)> {
    let root = Id(root_tree.try_into().unwrap());
    let naatre_files = naatre_tree_files(&env.conn(), &root);
    let cas = env.cas();

    let mut git_files: BTreeMap<String, (String, String)> = BTreeMap::new(); // path → (mode, oid)
    for line in f.git(&["ls-tree", "-r", "HEAD"]).lines() {
        let (meta, path) = line.split_once('\t').expect("ls-tree line");
        let mut parts = meta.split_whitespace();
        let mode = parts.next().unwrap();
        let kind = parts.next().unwrap();
        let oid = parts.next().unwrap();
        assert_eq!(kind, "blob", "only blobs expected in a clean fixture");
        git_files.insert(path.to_string(), (mode.to_string(), oid.to_string()));
    }

    let naatre_paths: Vec<&str> = naatre_files.keys().map(String::as_str).collect();
    let git_paths: Vec<&str> = git_files.keys().map(String::as_str).collect();
    assert_eq!(naatre_paths, git_paths, "same path set");

    for (path, (exec, blob)) in &naatre_files {
        let (mode, git_oid) = &git_files[path];
        assert_eq!(
            exec,
            &(*mode == "100755"),
            "exec bit for {path}: naatre {exec} vs git {mode}"
        );
        let content = cas.get(blob).unwrap();
        let expected = f.git_bytes(&["cat-file", "blob", git_oid]);
        assert_eq!(&content, &expected, "content for {path}");
    }
    naatre_files
}

// ---- 2. re-import no-op ----------------------------------------------------

#[test]
fn reimport_is_a_noop() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let f = Fixture::clean("noop");
    let env = Env::new();
    let first = env.import(&f, "imp", false);
    assert_eq!(first.imported, 7);
    let repo_id = env.repo_id("imp");
    let main_before = env.main_target("imp");

    let again = env.import(&f, "imp", false);
    assert_eq!(again.status, "ok");
    assert_eq!(again.imported, 0);
    assert_eq!(again.main, "unchanged");
    assert_eq!(env.count("SELECT COUNT(*) FROM changesets"), 7);
    assert_eq!(
        env.count(&format!(
            "SELECT COUNT(*) FROM git_map WHERE repo_id = {repo_id}"
        )),
        7
    );
    assert_eq!(env.main_target("imp"), main_before);
}

// ---- 3. unsupported, default mode aborts -----------------------------------

#[test]
fn unsupported_aborts_with_report() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let f = Fixture::full("full");
    let symlink_oid = f.git(&["rev-parse", "HEAD~2"]);
    let gitlink_oid = f.git(&["rev-parse", "HEAD~1"]);
    let env = Env::new();

    let report = env.import(&f, "imp", false);
    assert_eq!(report.status, "aborted");
    assert_eq!(report.imported, 0);
    assert_eq!(report.main, "unchanged");

    let reasons: Vec<(String, String)> = report
        .skipped
        .iter()
        .map(|s| (s.oid.clone(), s.reason.clone()))
        .collect();
    assert!(
        reasons
            .iter()
            .any(|(oid, r)| *oid == symlink_oid && r.contains("symlink")),
        "symlink offender in report: {reasons:?}"
    );
    assert!(
        reasons
            .iter()
            .any(|(oid, r)| *oid == gitlink_oid && r.contains("gitlink")),
        "gitlink offender in report: {reasons:?}"
    );

    // Nothing was written: zero changesets, zero blobs, no refs, no git_map.
    assert_eq!(env.count("SELECT COUNT(*) FROM changesets"), 0);
    assert_eq!(env.count("SELECT COUNT(*) FROM git_map"), 0);
    assert_eq!(env.count("SELECT COUNT(*) FROM repo_blobs"), 0);
    assert_eq!(env.count("SELECT COUNT(*) FROM refs"), 0);
    assert!(env.main_target("imp").is_none());
}

// ---- 4. skip mode with cascade ----------------------------------------------

#[test]
fn skip_mode_cascades_and_sets_main() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let f = Fixture::full("full");
    let symlink_oid = f.git(&["rev-parse", "HEAD~2"]);
    let gitlink_oid = f.git(&["rev-parse", "HEAD~1"]);
    let evil_oid = f.git(&["rev-parse", "HEAD"]);
    let env = Env::new();

    let report = env.import(&f, "imp", true);
    assert_eq!(report.status, "ok");
    assert_eq!(report.imported, 7);

    let skipped_oids: Vec<&str> = report.skipped.iter().map(|s| s.oid.as_str()).collect();
    assert_eq!(
        skipped_oids,
        vec![symlink_oid.as_str(), gitlink_oid.as_str()]
    );
    assert!(report.skipped[0].reason.contains("symlink"));
    assert!(report.skipped[1].reason.contains("gitlink"));
    assert_eq!(report.cascaded, vec![evil_oid.clone()]);
    // The cascade-skipped commit was never sanitized (it never imported).
    assert!(report.sanitized.is_empty());

    // Every naatre changeset's parents resolve — the parent walk passes.
    let repo_id = env.repo_id("imp");
    let conn = env.conn();
    let all: Vec<Vec<u8>> = conn
        .query_all_map(
            &naatre_store::Sql::plain("SELECT id FROM changesets WHERE repo_id = ?1"),
            &naatre_store::pv![repo_id],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(all.len(), 7);
    for cs in all {
        let id = Id(cs.try_into().unwrap());
        let graph = db::changeset_graph(&conn, repo_id, &id)
            .unwrap()
            .expect("changeset exists");
        for parent in graph.parents {
            assert!(
                db::changeset_get(&conn, repo_id, &parent)
                    .unwrap()
                    .is_some(),
                "parent {} of {} resolves",
                parent.to_hex(),
                id.to_hex()
            );
        }
    }

    // main moved to the head of the longest importable chain: the CRLF
    // commit (last importable on the first-parent chain).
    let crlf_line = f.git(&["log", "--format=%H", "--grep=crlf"]);
    let map = env.git_map(repo_id);
    assert_eq!(env.main_target("imp"), Some(map[crlf_line.trim()]));
    assert_eq!(report.main, "set");
}

// ---- 5. graft A: fast-forward ----------------------------------------------

#[test]
fn graft_a_fast_forwards_main() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let mut f = Fixture::clean("grafta");
    let env = Env::new();
    assert_eq!(env.import(&f, "imp", false).imported, 7);
    let repo_id = env.repo_id("imp");
    let old_main = env.main_target("imp").unwrap();

    f.write("next.txt", b"one\n");
    f.commit("graft one");
    f.write("next.txt", b"two\n");
    f.commit("graft two");

    let report = env.import(&f, "imp", false);
    assert_eq!(report.imported, 2, "only the two new commits");
    assert_eq!(report.main, "fast-forwarded");
    assert_eq!(env.count("SELECT COUNT(*) FROM changesets"), 9);

    let map = env.git_map(repo_id);
    let head_cs = map[&f.git(&["rev-parse", "HEAD"])];
    assert_eq!(env.main_target("imp"), Some(head_cs));
    assert_ne!(head_cs, old_main);
}

// ---- 6. graft B: native lands refuse the move -------------------------------

#[test]
fn graft_b_native_lands_refuse_main_move() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let mut f = Fixture::clean("graftb");
    let env = Env::new();
    env.import(&f, "imp", false);
    let repo_id = env.repo_id("imp");
    let head_cs = env.main_target("imp").unwrap();

    // A native naatre changeset on top of the imported head, through the db
    // layer (the same txn a land will use), with main moved to it.
    let conn = env.conn();
    let cas = env.cas();
    let parent_graph = db::changeset_graph(&conn, repo_id, &head_cs)
        .unwrap()
        .unwrap();
    let content = b"native landing\n";
    let blob = cas.put(content).unwrap().id;
    let mut node = MemNode::load(
        &conn,
        parent_graph
            .root_tree_id
            .clone()
            .map(|r| Id(r.try_into().unwrap())),
    )
    .unwrap();
    node.apply(&[FileChange {
        path: "native.txt".into(),
        op: Op::Add,
        blob: Some(blob),
        size: Some(content.len() as u64),
        exec: Some(false),
    }])
    .unwrap();
    let (root_tree, _, trees) = node.collect().unwrap();
    let cs = Changeset {
        repo_id: repo_id as u64,
        parents: vec![head_cs],
        author: "Native <native@example.com>".into(),
        message: "native land\n".into(),
        timestamp_ns: 1_767_225_600_000_000_000,
        changes: vec![FileChange {
            path: "native.txt".into(),
            op: Op::Add,
            blob: Some(blob),
            size: Some(content.len() as u64),
            exec: Some(false),
        }],
    };
    let canonical = naatre_core::changeset::encode(&cs).unwrap();
    let cs_id = object_id(Kind::Changeset, &canonical);
    db::changeset_insert(
        &conn,
        db::ChangesetInsert {
            repo_id,
            repo_name: "imp",
            id: &cs_id,
            canonical: &canonical,
            author: &cs.author,
            message: &cs.message,
            timestamp_ns: cs.timestamp_ns as i64,
            generation: parent_graph.generation + 1,
            principal: "admin",
            root_tree_id: &root_tree,
            parents: &cs.parents,
            changes: &cs.changes,
            blobs: &[blob],
            trees: &trees,
            now_ns: cs.timestamp_ns as i64,
        },
    )
    .unwrap();
    let version = db::ref_get(&conn, repo_id, "main")
        .unwrap()
        .unwrap()
        .version;
    let outcome = db::ref_cas(
        &conn,
        db::RefMove {
            repo_id,
            repo_name: "imp",
            name: "main",
            target: &cs_id,
            expected: Some(version),
            principal: "admin",
            now_ns: cs.timestamp_ns as i64,
        },
    )
    .unwrap();
    assert!(matches!(outcome, db::RefCas::Updated { .. }));

    // One more git commit on top of the imported tip.
    f.write("later.txt", b"later\n");
    f.commit("after native");

    let report = env.import(&f, "imp", false);
    assert_eq!(report.status, "ok");
    assert_eq!(
        report.imported, 1,
        "the git commit imports as a dangling head"
    );
    assert_eq!(report.main, "refused");
    assert_eq!(
        report.reason.as_deref(),
        Some(
            "native naatre changesets on main; import refuses to move it \
             (the imported commits are dangling heads, fetchable by id)"
        )
    );
    // main is untouched; the imported head is fetchable by id.
    assert_eq!(env.main_target("imp"), Some(cs_id));
    assert_eq!(env.count("SELECT COUNT(*) FROM changesets"), 9);
}

// ---- 7. graft C: diverged git history ---------------------------------------

#[test]
fn graft_c_diverged_history_aborts() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let mut f = Fixture::clean("graftc");
    let env = Env::new();
    env.import(&f, "imp", false);
    let main_before = env.main_target("imp").unwrap();

    // Rewrite the source: reset to the merge and commit a sibling.
    f.git(&["reset", "--hard", "HEAD~1"]);
    f.write("sibling.txt", b"sibling\n");
    f.commit("sibling history");

    let report = env.import(&f, "imp", false);
    assert_eq!(report.status, "aborted");
    assert!(
        report
            .reason
            .as_deref()
            .unwrap_or("")
            .contains("git_history_diverged"),
        "diverged reason: {:?}",
        report.reason
    );
    assert_eq!(report.imported, 0);
    // Nothing new imported, main unmoved.
    assert_eq!(env.count("SELECT COUNT(*) FROM changesets"), 7);
    assert_eq!(env.count("SELECT COUNT(*) FROM git_map"), 7);
    assert_eq!(env.main_target("imp"), Some(main_before));
}

// ---- 8. import while serving ------------------------------------------------

#[test]
fn import_while_serving_is_refused_by_the_flock() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let f = Fixture::clean("locking");
    let env = Env::new();
    // `naatre serve` holds the data dir flock for its whole life; holding
    // it here is the same contention import must face.
    let _serve_lock = DataDir::open(&env.data).unwrap().lock().unwrap();

    let err: ImportError = naatre_server::import::run(
        &env.data,
        &ImportOptions {
            git_path: f.path.clone(),
            into: Some("imp".into()),
            skip_unsupported: false,
        },
        &Default::default(),
    )
    .unwrap_err();
    let message = err.to_string();
    assert!(
        message.contains("stop naatre serve before importing"),
        "remedy in message: {message}"
    );
    assert!(
        message.contains(&env.data.join("lockfile").display().to_string()),
        "lockfile path in message: {message}"
    );
}

// ---- 9. sanitization --------------------------------------------------------

#[test]
fn sanitize_replaces_cc_cf_with_replacement_char() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let mut f = Fixture::new("sanitize");
    f.write("a.txt", b"a\n");
    f.commit("root");
    f.write("b.txt", b"b\n");
    let evil = f.commit("bad \u{202E}override");

    // Non-UTF-8 author (raw 0xFF byte): modern git re-encodes
    // GIT_AUTHOR_NAME, so build the commit object with plumbing.
    f.write("c.txt", b"c\n");
    f.git(&["add", "-A"]);
    let tree = f.git(&["rev-parse", "HEAD^{tree}"]);
    let parent = f.git(&["rev-parse", "HEAD"]);
    let stamp = 1_767_225_600 + f.commits * 60;
    let mut payload = Vec::new();
    payload.extend_from_slice(format!("tree {tree}\nparent {parent}\nauthor ").as_bytes());
    payload.push(0xFF);
    payload.extend_from_slice(
        format!(
            " <fix@example.com> {stamp} +0000\n\
             committer Fix Tur e <fix@example.com> {stamp} +0000\n\nnon-utf8 author\n"
        )
        .as_bytes(),
    );
    let author_oid = f.git_stdin(&["hash-object", "-t", "commit", "-w", "--stdin"], &payload);
    f.git(&["update-ref", "refs/heads/main", &author_oid]);
    f.commits += 1;

    let env = Env::new();
    let report = env.import(&f, "imp", false);
    assert_eq!(report.status, "ok");
    assert_eq!(report.imported, 3);
    assert_eq!(report.sanitized.len(), 2, "both sanitized commits listed");

    let repo_id = env.repo_id("imp");
    let map = env.git_map(repo_id);
    assert!(report.sanitized.contains(&evil));
    assert!(report.sanitized.contains(&author_oid));

    let evil_row = db::changeset_get(&env.conn(), repo_id, &map[&evil])
        .unwrap()
        .unwrap();
    assert_eq!(evil_row.message, "bad \u{FFFD}override\n");
    assert!(!evil_row.message.contains('\u{202E}'));

    let author_row = db::changeset_get(&env.conn(), repo_id, &map[&author_oid])
        .unwrap()
        .unwrap();
    assert_eq!(author_row.author, "\u{FFFD} <fix@example.com>");
    assert!(
        !author_row.author.as_bytes().contains(&0xFF),
        "raw byte replaced"
    );
}

// ---- 10. audit provenance ---------------------------------------------------

/// The audit details JSON of every row for one action, in id order.
fn audit_details(env: &Env, action: &str) -> Vec<serde_json::Value> {
    env.conn()
        .query_all_map(
            &naatre_store::Sql::plain(
                "SELECT details_json FROM audit_log WHERE action = ?1 ORDER BY id",
            ),
            &naatre_store::pv![action],
            |r| r.get::<String>(0),
        )
        .unwrap()
        .into_iter()
        .map(|d| serde_json::from_str(&d).expect("details_json parses"))
        .collect()
}

/// Whether an import moves main (set) or fast-forwards it, its ref_move
/// audit row and every changeset_create audit row it writes carry
/// via:"import" — the closure round's ref_cas dedup silently dropped that
/// provenance once; pinned so it never regresses.
#[test]
fn import_audit_rows_carry_via_import() {
    if !git_works() {
        eprintln!("WARNING: git not found — import suite skipped");
        return;
    }
    let mut f = Fixture::clean("via");
    let env = Env::new();

    // Clean import: main is SET.
    let report = env.import(&f, "imp", false);
    assert_eq!(report.status, "ok");
    assert_eq!(report.main, "set");
    let details = audit_details(&env, "changeset_create");
    assert_eq!(details.len(), 7, "one row per imported commit");
    let moves = audit_details(&env, "ref_move");
    assert_eq!(moves.len(), 1, "the main move");
    for v in details.iter().chain(moves.iter()) {
        assert_eq!(v["via"], "import", "provenance lost: {v}");
    }

    // Graft import: main is FAST-FORWARDED; the second move carries the
    // provenance too.
    f.write("next.txt", b"one\n");
    f.commit("graft one");
    f.write("next.txt", b"two\n");
    f.commit("graft two");
    let report = env.import(&f, "imp", false);
    assert_eq!(report.imported, 2);
    assert_eq!(report.main, "fast-forwarded");
    let details = audit_details(&env, "changeset_create");
    assert_eq!(details.len(), 9);
    let moves = audit_details(&env, "ref_move");
    assert_eq!(moves.len(), 2);
    for v in details.iter().chain(moves.iter()) {
        assert_eq!(v["via"], "import", "provenance lost: {v}");
    }
}

// ---- M6a: the import enqueue hook -------------------------------------------

/// A minimal derived-data kind for the import-hook pin: output = the count
/// of parents' outputs + 1 (one byte), so the child's output proves the
/// parent's output flowed through the queue's parents-done gate.
struct QueueProbe {
    calls: Arc<Mutex<Vec<Id>>>,
}

impl naatre_server::derive::Deriver for QueueProbe {
    fn kind(&self) -> &'static str {
        "queue-probe"
    }

    fn derive(
        &self,
        _ctx: &naatre_server::derive::DeriveCtx,
        _conn: &naatre_store::Db,
        _repo_id: i64,
        cs: Id,
        parents_done: &[naatre_server::derive::ParentOut],
    ) -> Result<naatre_server::derive::DerivedOut, naatre_server::derive::DeriveFailure> {
        self.calls.lock().unwrap().push(cs);
        Ok(naatre_server::derive::DerivedOut(vec![
            (parents_done.len() + 1) as u8,
        ]))
    }
}

/// Importing with a REGISTERED derive kind enqueues every imported commit
/// (the hook runs inside each commit's transaction); the serving side's
/// queue (`derive_pass` over a ServerState on the same data dir) drains it
/// parents-first. The offline `naatre import` process itself registers no
/// kinds — that no-op path is every pre-existing import test.
#[test]
fn import_enqueues_registered_derive_kinds() {
    if !git_works() {
        return;
    }
    let mut f = Fixture::new("deriveq");
    f.write("a.txt", b"one\n");
    f.commit("root");
    f.write("a.txt", b"two\n");
    f.commit("second");

    let env = Env::new();
    let mut state = ServerState::open(DataDir::open(&env.data).unwrap()).unwrap();
    let calls = Arc::new(Mutex::new(Vec::new()));
    state
        .register_deriver(Arc::new(QueueProbe {
            calls: Arc::clone(&calls),
        }))
        .unwrap();
    let registry = state.derive.clone();
    let state = Arc::new(state);

    let report = naatre_server::import::run(
        &env.data,
        &ImportOptions {
            git_path: f.path.clone(),
            into: Some("imp".into()),
            skip_unsupported: false,
        },
        &registry,
    )
    .unwrap();
    assert_eq!(report.status, "ok", "{}", report.to_json_line());
    assert_eq!(report.imported, 2);

    // Both commits enqueued, pending, under the registered kind's id.
    let kind: i64 = env
        .conn()
        .query_row_map(
            &naatre_store::Sql::plain("SELECT id FROM derive_kinds WHERE name = 'queue-probe'"),
            &naatre_store::pv![],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        env.count(&format!(
            "SELECT COUNT(*) FROM derive_state WHERE kind = {kind} AND state = 'pending'"
        )),
        2
    );

    // The queue drains it: parents first (the child's output counts its
    // parent's), everything done. Rowid order is enqueue order, and the
    // import walks parents before children.
    let rep = tokio::runtime::Runtime::new()
        .unwrap()
        .block_on(async { naatre_server::derive::derive_pass(&state).await.unwrap() });
    assert_eq!(rep.derived, 2);
    let order = calls.lock().unwrap().clone();
    let outs: Vec<Vec<u8>> = env
        .conn()
        .query_all_map(
            &naatre_store::Sql::plain(
                "SELECT output FROM derive_state WHERE kind = ?1 ORDER BY rowid ASC",
            ),
            &naatre_store::pv![kind],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(outs.len(), 2);
    assert_eq!(outs[0], vec![1], "root derives with no parents");
    assert_eq!(outs[1], vec![2], "child derives FROM its parent's output");
    assert_eq!(order.len(), 2);
    assert_eq!(
        env.count(&format!(
            "SELECT COUNT(*) FROM derive_state WHERE kind = {kind} AND state = 'done'"
        )),
        2
    );
}
