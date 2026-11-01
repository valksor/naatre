//! M11 `--sanitize`: the import escape mode. The same fixture under both
//! flags — the default keeps its pinned U+FFFD replacement (that pin lives
//! in import_test and is untouched), `--sanitize` stores the LOSSLESS
//! escape (`\u{hex}` + doubled backslashes) with a per-object `escaped`
//! report entry naming each escaped field and code point, the codec
//! round-trips to the original bytes, and the escaped form re-imports
//! (clean ASCII: nothing to sanitize). The documented residual rides along:
//! the `\r\n` → `\n` normalization happens BEFORE sanitization in both
//! modes, so a CRLF message cannot round-trip by format design.

use std::collections::HashMap;
use std::path::PathBuf;
use std::process::Command;

use rusqlite::Connection;

use naatre_core::ids::Id;
use naatre_server::db;
use naatre_server::import::{self, ImportOptions};
use naatre_store::BackendConfig;

fn git_works() -> bool {
    match Command::new("git").arg("--version").output() {
        Ok(out) if out.status.success() => true,
        Ok(out) => {
            eprintln!(
                "skipping m11 sanitize tests: git --version failed: {}",
                String::from_utf8_lossy(&out.stderr)
            );
            false
        }
        Err(e) => {
            eprintln!("skipping m11 sanitize tests: git not available: {e}");
            false
        }
    }
}

/// A git fixture whose commits are built by PLUMBING, so author and
/// message bytes are exactly what the test writes (the import_test
/// technique: modern git re-encodes GIT_AUTHOR_NAME, so env vars cannot
/// carry raw control characters).
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
        assert!(out.status.success());
        Fixture {
            _dir: dir,
            path,
            commits: 0,
        }
    }

    fn git(&self, args: &[&str]) -> String {
        let out = Command::new("git")
            .arg("-C")
            .arg(&self.path)
            .args(args)
            .output()
            .expect("run git");
        assert!(
            out.status.success(),
            "git {args:?}: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

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
        assert!(out.status.success());
        String::from_utf8_lossy(&out.stdout).trim().to_string()
    }

    fn write(&self, rel: &str, content: &[u8]) {
        let full = self.path.join(rel);
        std::fs::create_dir_all(full.parent().unwrap()).unwrap();
        std::fs::write(full, content).unwrap();
    }

    /// One plumbing commit: `author_name_bytes` are raw (they may carry
    /// control characters); `message_bytes` likewise. Returns the oid.
    fn plumbing_commit(&mut self, author_name: &[u8], message: &[u8]) -> String {
        self.write(&format!("f{}.txt", self.commits), b"content\n");
        self.git(&["add", "-A"]);
        // write-tree works on the unborn HEAD too (the first commit).
        let tree = self.git(&["write-tree"]);
        let parent = if self.commits == 0 {
            None
        } else {
            Some(self.git(&["rev-parse", "HEAD"]))
        };
        let stamp = 1_767_225_600 + self.commits * 60;
        let mut payload = Vec::new();
        payload.extend_from_slice(format!("tree {tree}\n").as_bytes());
        if let Some(p) = &parent {
            payload.extend_from_slice(format!("parent {p}\n").as_bytes());
        }
        payload.extend_from_slice(b"author ");
        payload.extend_from_slice(author_name);
        payload.extend_from_slice(
            format!(
                " <fix@example.com> {stamp} +0000\n\
                 committer Fix Tur e <fix@example.com> {stamp} +0000\n\n"
            )
            .as_bytes(),
        );
        payload.extend_from_slice(message);
        let oid = self.git_stdin(&["hash-object", "-t", "commit", "-w", "--stdin"], &payload);
        self.git(&["update-ref", "refs/heads/main", &oid]);
        self.commits += 1;
        oid
    }
}

/// A fresh SQLite+fs data dir (the init shape) + an import runner.
struct Env {
    _dir: tempfile::TempDir,
    data: PathBuf,
}

impl Env {
    fn new() -> Env {
        let dir = tempfile::tempdir().unwrap();
        let data = dir.path().join("data");
        std::fs::create_dir_all(&data).unwrap();
        std::fs::write(data.join("naatre.db"), b"").unwrap();
        Env { _dir: dir, data }
    }

    fn import(&self, f: &Fixture, into: &str, sanitize: bool) -> import::Report {
        let opts = ImportOptions {
            git_path: f.path.clone(),
            into: Some(into.into()),
            skip_unsupported: false,
            sanitize,
        };
        naatre_server::import::run_backend(
            &self.data,
            &BackendConfig::default(),
            &opts,
            &Default::default(),
        )
        .unwrap()
    }

    fn conn(&self) -> naatre_store::Db {
        naatre_store::Db::Sqlite(Connection::open(self.data.join("naatre.db")).unwrap())
    }

    fn repo_id(&self, name: &str) -> i64 {
        let conn = self.conn();
        conn.query_row_map(
            &naatre_store::Sql::plain("SELECT id FROM repos WHERE name = ?1"),
            &naatre_store::pv![name],
            |r| r.get(0),
        )
        .unwrap()
    }

    fn git_map(&self, repo_id: i64) -> HashMap<String, Id> {
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

    fn changeset(&self, repo_id: i64, cs: &Id) -> db::ChangesetRow {
        db::changeset_get(&self.conn(), repo_id, cs)
            .unwrap()
            .unwrap()
    }
}

/// The fixture: a Cf-message commit, a Cc-author commit, and a CRLF+lone-CR
/// message commit (the documented residual).
fn dirty_fixture(tag: &str) -> (Fixture, String, String, String) {
    let mut f = Fixture::new(tag);
    let cf_msg = f.plumbing_commit(b"Fix Tur e", "bad \u{202E}override\u{200B}\n".as_bytes());
    let cc_author = f.plumbing_commit("ev\u{7}il".as_bytes(), b"cc author\n");
    let cr_msg = f.plumbing_commit(b"Fix Tur e", b"crlf\r\nline\rend\n");
    (f, cf_msg, cc_author, cr_msg)
}

#[test]
fn default_mode_keeps_today_replacement() {
    if !git_works() {
        return;
    }
    let (f, cf_msg, cc_author, cr_msg) = dirty_fixture("m11sn-default");
    let e = Env::new();
    let report = e.import(&f, "imp", false);
    assert_eq!(report.status, "ok");
    assert_eq!(report.imported, 3);
    assert_eq!(report.escaped, vec![], "the default never escapes");
    assert_eq!(report.sanitized.len(), 3, "{:?}", report.sanitized);

    let repo_id = e.repo_id("imp");
    let map = e.git_map(repo_id);
    let cf = e.changeset(repo_id, &map[&cf_msg]);
    assert_eq!(cf.message, "bad \u{FFFD}override\u{FFFD}\n");
    let cc = e.changeset(repo_id, &map[&cc_author]);
    assert_eq!(cc.author, "ev\u{FFFD}il <fix@example.com>");
    let cr = e.changeset(repo_id, &map[&cr_msg]);
    assert_eq!(
        cr.message, "crlf\nline\u{FFFD}end\n",
        "lone CR replaced, CRLF normalized"
    );
}

#[test]
fn sanitize_mode_escapes_losslessly_with_report() {
    if !git_works() {
        return;
    }
    let (f, cf_msg, cc_author, cr_msg) = dirty_fixture("m11sn-escape");
    let e = Env::new();
    let report = e.import(&f, "imp", true);
    assert_eq!(report.status, "ok");
    assert_eq!(report.imported, 3);
    // The CRLF commit's lone \r is an offender too — every dirty commit
    // is listed, exactly like the default mode lists them.
    assert_eq!(report.sanitized.len(), 3, "{:?}", report.sanitized);

    // The per-object report: one entry per escaped FIELD, naming the
    // changeset id, the git commit, the field, and every code point.
    let repo_id = e.repo_id("imp");
    let map = e.git_map(repo_id);
    assert_eq!(report.escaped.len(), 3, "{:?}", report.escaped);
    let msg_entry = report
        .escaped
        .iter()
        .find(|x| x.field == "message" && x.commit == cf_msg)
        .expect("the Cf message entry");
    assert_eq!(msg_entry.id, map[&cf_msg].to_hex());
    assert_eq!(msg_entry.chars, vec!["U+202E", "U+200B"]);
    let author_entry = report
        .escaped
        .iter()
        .find(|x| x.field == "author")
        .expect("an author entry");
    assert_eq!(author_entry.commit, cc_author);
    assert_eq!(author_entry.chars, vec!["U+0007"]);
    let cr_entry = report
        .escaped
        .iter()
        .find(|x| x.commit == cr_msg)
        .expect("the lone-CR entry");
    assert_eq!(cr_entry.chars, vec!["U+000D"]);

    // The STORED content is exactly the escaped form: the raw offenders
    // are gone, the escapes are plain ASCII text.
    let cf = e.changeset(repo_id, &map[&cf_msg]);
    assert_eq!(cf.message, "bad \\u{202e}override\\u{200b}\n");
    assert!(!cf.message.contains('\u{202E}'));
    let cc = e.changeset(repo_id, &map[&cc_author]);
    assert_eq!(cc.author, "ev\\u{7}il <fix@example.com>");
    assert!(!cc.author.contains('\u{7}'));

    // The codec round-trips: decode(stored) == the exact input strings.
    assert_eq!(
        import::unescape_text_lossless(&cf.message).unwrap(),
        "bad \u{202E}override\u{200B}\n"
    );
    assert_eq!(
        import::unescape_text_lossless(&cc.author).unwrap(),
        "ev\u{7}il <fix@example.com>"
    );
    // …and the codec is stable: encode(decode(encode(x))) == encode(x).
    // (Re-encoding the ALREADY-escaped string is a no-op — clean ASCII.)
    let original = import::unescape_text_lossless(&cc.author).unwrap();
    let (again, chars) = import::escape_text_lossless(&original);
    assert_eq!(again, cc.author);
    assert_eq!(chars, vec!['\u{7}']);
}

#[test]
fn sanitize_codec_roundtrips_and_residual() {
    if !git_works() {
        return;
    }
    // Codec identity over a mixed bag: offenders, backslashes, clean text,
    // allowed whitespace (\n \t pass through untouched).
    for (input, escaped, offenders) in [
        ("clean text", "clean text", vec![]),
        ("a\u{7}b", "a\\u{7}b", vec!['\u{7}']),
        // A backslash WITHOUT offenders passes through byte-identical (a
        // clean string is never rewritten); the doubling only applies to
        // strings that carry escapes.
        ("back\\slash", "back\\slash", vec![]),
        (
            "both \\ and \u{200B}",
            "both \\\\ and \\u{200b}",
            vec!['\u{200B}'],
        ),
        ("nl\ntab\t", "nl\ntab\t", vec![]),
        ("dir \u{202E}rlm", "dir \\u{202e}rlm", vec!['\u{202E}']),
    ] {
        let (out, chars) = import::escape_text_lossless(input);
        assert_eq!(out, escaped, "escape({input:?})");
        assert_eq!(chars, offenders, "escaped chars of {input:?}");
        if chars.is_empty() {
            assert_eq!(out, input, "a clean string is never rewritten");
        } else {
            assert_eq!(
                import::unescape_text_lossless(&out).unwrap(),
                input,
                "round-trip of {input:?}"
            );
        }
    }
    // Malformed decodes are errors, never guesses.
    assert!(import::unescape_text_lossless("a\\b").is_err());
    assert!(import::unescape_text_lossless("a\\u{zz}").is_err());
    assert!(import::unescape_text_lossless("a\\u{110000}").is_err());

    // The documented residual, end to end: \r\n is normalized BEFORE
    // sanitization (the one content mutation both modes share), so a CRLF
    // message cannot round-trip; the lone \r escapes losslessly.
    let (out, chars) = import::escape_text_lossless("crlf\nline\rend");
    assert_eq!(out, "crlf\nline\\u{d}end");
    assert_eq!(chars, vec!['\r']);
    assert_eq!(
        import::unescape_text_lossless(&out).unwrap(),
        "crlf\nline\rend"
    );
}

#[test]
fn escaped_form_reimports_without_sanitization() {
    if !git_works() {
        return;
    }
    // A repo whose message IS the escaped text (plain ASCII): the default
    // import takes it verbatim — nothing to sanitize, nothing escaped.
    let mut f = Fixture::new("m11sn-reimport");
    let escaped_msg = "bad \\u{202e}override\\u{200b}\n";
    let oid = f.plumbing_commit(b"Fix Tur e", escaped_msg.as_bytes());
    let e = Env::new();
    let report = e.import(&f, "imp", false);
    assert_eq!(report.status, "ok");
    assert_eq!(report.imported, 1);
    assert!(report.sanitized.is_empty(), "{:?}", report.sanitized);
    assert!(report.escaped.is_empty());
    let repo_id = e.repo_id("imp");
    let map = e.git_map(repo_id);
    let row = e.changeset(repo_id, &map[&oid]);
    assert_eq!(row.message, escaped_msg);
}
