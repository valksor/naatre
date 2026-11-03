//! M7 MCP suite: drives the REAL `naatre mcp` binary (spawned as a child
//! process on a prepared working copy, talking newline-delimited JSON-RPC
//! 2.0 over stdio) against the same in-process server harness the client
//! suite uses. Four suites: the e2e session (clone → edit → commit → land,
//! then a sync after another client lands), the --read-only gate, the
//! working-copy flock two-writer rule (both directions), and the
//! tool-schema match against docs/openapi.json.

use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::mpsc::Receiver;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use naatre_cli::verbs::{self, Net};
use naatre_core::ids::{Id, Kind, object_id};
use naatre_server::state::ServerState;
use naatre_store::DataDir;
use serde_json::{Value, json};

// ---- server harness (the client_test.rs shape) -----------------------------

struct TestServer {
    _dir: tempfile::TempDir,
    base: String,
    token: String,
    db_path: PathBuf,
    shutdown: Option<tokio::sync::oneshot::Sender<()>>,
    handle: Option<std::thread::JoinHandle<()>>,
}

impl Drop for TestServer {
    fn drop(&mut self) {
        if let Some(tx) = self.shutdown.take() {
            let _ = tx.send(());
        }
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

fn start_server() -> TestServer {
    let dir = tempfile::tempdir().expect("tempdir");
    let data = DataDir::create(dir.path()).expect("create data dir");
    std::fs::write(data.db_path(), b"").expect("touch naatre.db");

    let (port_tx, port_rx) = std::sync::mpsc::channel();
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel::<()>();
    let data_path = dir.path().to_path_buf();
    let handle = std::thread::spawn(move || {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("test runtime");
        rt.block_on(async move {
            let data = DataDir::open(&data_path).expect("open data dir");
            let mut state = ServerState::open(data).expect("server state");
            // The production serve shape: the M6b derived-data kinds
            // registered, so blame/path-history answer (an unregistered
            // kind answers 503 derivation_pending forever).
            for deriver in naatre_server::derived::production_kinds() {
                state.register_deriver(deriver).expect("register deriver");
            }
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("bind ephemeral port");
            let port = listener.local_addr().unwrap().port();
            port_tx.send(port).expect("send port");
            naatre_server::serve::serve(
                naatre_server::serve::ServeConfig {
                    listener,
                    state: Arc::new(state),
                },
                async move {
                    let _ = shutdown_rx.await;
                },
            )
            .await
            .expect("serve");
        });
    });
    let port = port_rx
        .recv_timeout(Duration::from_secs(10))
        .expect("server started");

    let (token, hash) = naatre_server::auth::mint_token();
    {
        let conn = rusqlite::Connection::open(dir.path().join("naatre.db")).expect("open db");
        conn.busy_timeout(Duration::from_secs(5)).unwrap();
        conn.execute(
            "INSERT INTO principals(name, role, created_ns) VALUES ('admin', 'admin', 0)",
            [],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO tokens(principal, blake3, expires_ns, revoked) \
             VALUES ('admin', ?1, NULL, 0)",
            [&hash],
        )
        .unwrap();
    }

    let db_path = dir.path().join("naatre.db");
    TestServer {
        _dir: dir,
        base: format!("http://127.0.0.1:{port}"),
        token,
        db_path,
        shutdown: Some(shutdown_tx),
        handle: Some(handle),
    }
}

fn agent() -> ureq::Agent {
    ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .build(),
    )
}

fn api(server: &TestServer, method: &str, path: &str, body: Option<Value>) -> (u16, Value) {
    let url = format!("{}{path}", server.base);
    let auth = format!("Bearer {}", server.token);
    let a = agent();
    let res = match (method, &body) {
        ("GET", _) => a
            .get(&url)
            .header("Authorization", auth)
            .call()
            .expect("request"),
        ("POST", Some(v)) => a
            .post(&url)
            .header("Authorization", auth)
            .send_json(v)
            .expect("request"),
        _ => panic!("api helper: unsupported {method}"),
    };
    let status = res.status().as_u16();
    let value: Value = res.into_body().read_json().expect("json body");
    (status, value)
}

/// Bounded setup retry for the single-writer 503 (`writer_busy` envelopes
/// only; fixed 20 ms backoff) — see client_test.rs for the full rationale.
fn api_setup_retry(
    server: &TestServer,
    method: &str,
    path: &str,
    body: Option<Value>,
) -> (u16, Value) {
    let mut waited = 0u64;
    loop {
        let (status, body) = api(server, method, path, body.clone());
        let code = body["error"]["code"].as_str().unwrap_or("");
        if !(status == 503 && code == "writer_busy") || waited >= 2000 {
            return (status, body);
        }
        std::thread::sleep(Duration::from_millis(20));
        waited += 20;
    }
}

fn blob_id(data: &[u8]) -> String {
    object_id(Kind::Blob, data).to_hex()
}

fn put_blob(server: &TestServer, repo: &str, data: &[u8]) {
    let url = format!("{}/v1/repos/{repo}/objects/{}", server.base, blob_id(data));
    let mut waited = 0u64;
    let status = loop {
        let res = agent()
            .put(&url)
            .header("Authorization", format!("Bearer {}", server.token))
            .send(data)
            .expect("put blob");
        let status = res.status().as_u16();
        let body: Value = res.into_body().read_json().unwrap_or(Value::Null);
        let code = body["error"]["code"].as_str().unwrap_or("");
        if !(status == 503 && code == "writer_busy") || waited >= 2000 {
            break status;
        }
        std::thread::sleep(Duration::from_millis(20));
        waited += 20;
    };
    assert!((200..300).contains(&status), "blob PUT failed: {status}");
}

fn change(op: &str, path: &str, data: &[u8]) -> Value {
    match op {
        "del" => json!({ "path": path, "op": "del" }),
        _ => json!({
            "path": path,
            "op": op,
            "blob": blob_id(data),
            "size": data.len(),
            "exec": false,
        }),
    }
}

fn changeset(
    server: &TestServer,
    repo: &str,
    parents: &[String],
    mut changes: Vec<Value>,
    message: &str,
) -> Value {
    changes.sort_by(|a, b| a["path"].as_str().cmp(&b["path"].as_str()));
    let body = json!({
        "parents": parents,
        "author": "test <test@naatre.local>",
        "message": message,
        "changes": changes,
    });
    let (status, created) = api_setup_retry(
        server,
        "POST",
        &format!("/v1/repos/{repo}/changesets"),
        Some(body),
    );
    assert_eq!(status, 201, "changeset create failed: {created}");
    created
}

fn create_repo(server: &TestServer, name: &str) {
    let (status, body) =
        api_setup_retry(server, "POST", "/v1/repos", Some(json!({ "name": name })));
    assert!(
        status == 201 || (status == 409 && body["error"]["code"] == "repo_exists"),
        "repo create failed: {status} {body}"
    );
}

fn seed_main(server: &TestServer, repo: &str, target_hex: &str) {
    let repo_id: i64 = rusqlite::Connection::open(&server.db_path)
        .and_then(|conn| {
            conn.busy_timeout(Duration::from_secs(5))?;
            conn.query_row("SELECT id FROM repos WHERE name = ?1", [repo], |r| r.get(0))
        })
        .expect("repo id");
    let target = Id::from_hex(target_hex).expect("target hex");
    let conn = rusqlite::Connection::open(&server.db_path).expect("open db");
    conn.busy_timeout(Duration::from_secs(5)).unwrap();
    conn.execute(
        "INSERT INTO refs(repo_id, name, target, version, last_principal) \
         VALUES (?1, 'main', ?2, 1, 'seed') \
         ON CONFLICT(repo_id, name) DO UPDATE SET target = excluded.target, \
         version = version + 1",
        rusqlite::params![repo_id, target.0.as_slice()],
    )
    .expect("seed main ref");
}

fn net(server: &TestServer) -> Net {
    Net {
        server: Some(server.base.clone()),
        token: Some(server.token.clone()),
        ca_file: None,
        insecure_http: false,
    }
}

fn write_file(root: &Path, rel: &str, bytes: &[u8]) {
    let path = root.join(rel);
    std::fs::create_dir_all(path.parent().expect("parent")).unwrap();
    std::fs::write(path, bytes).unwrap();
}

/// Runs the real binary with the server env; strips any ambient NAATRE_*
/// values so tests are hermetic.
fn run_binary(server: &TestServer, cwd: &Path, args: &[&str]) -> std::process::Output {
    Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args(args)
        .current_dir(cwd)
        .env("NAATRE_URL", &server.base)
        .env("NAATRE_TOKEN", &server.token)
        .env_remove("NAATRE_AUTHOR")
        .output()
        .expect("spawn naatre")
}

fn is_64_hex(s: &str) -> bool {
    s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit())
}

/// Waits for the background derive queue: blame answers 503
/// `derivation_pending` until the derived index covers the chain (the M6
/// read-during-backlog contract). Bounded poll with progress, then act —
/// the same shape client_test.rs uses.
fn wait_blame_ready(server: &TestServer, repo: &str, at: &str, path: &str) {
    let mut derived = false;
    for _ in 0..600 {
        let (status, _) = api(
            server,
            "GET",
            &format!("/v1/repos/{repo}/blame?at={at}&path={path}"),
            None,
        );
        if status == 200 {
            derived = true;
            break;
        }
        assert_eq!(status, 503, "pending until derived");
        std::thread::sleep(Duration::from_millis(50));
    }
    assert!(derived, "queue never drained the blame index");
}

// ---- MCP session driver -----------------------------------------------------

/// A live `naatre mcp` child process. Sequential request/response: the
/// server is single-threaded, so one response line follows each request
/// line. Every stdout line is recorded for the purity assertion.
struct McpSession {
    child: Option<std::process::Child>,
    stdin: Option<std::process::ChildStdin>,
    rx: Receiver<String>,
    lines: Arc<Mutex<Vec<String>>>,
    stderr: Arc<Mutex<String>>,
    next_id: u64,
}

impl Drop for McpSession {
    fn drop(&mut self) {
        self.stdin.take(); // EOF
        if let Some(mut child) = self.child.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

fn spawn_mcp(cwd: &Path, server: &TestServer, extra_args: &[&str]) -> McpSession {
    let mut child = Command::new(env!("CARGO_BIN_EXE_naatre"))
        .args(["mcp"])
        .args(extra_args)
        .current_dir(cwd)
        .env("NAATRE_URL", &server.base)
        .env("NAATRE_TOKEN", &server.token)
        .env("MCP_LOG", "1")
        .env_remove("NAATRE_AUTHOR")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn naatre mcp");
    let stdin = child.stdin.take().expect("stdin");
    let stdout = child.stdout.take().expect("stdout");
    let stderr_pipe = child.stderr.take().expect("stderr");

    let lines: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
    let (tx, rx) = std::sync::mpsc::channel();
    let lines_for_reader = lines.clone();
    let _reader = std::thread::spawn(move || {
        use std::io::BufRead as _;
        let reader = std::io::BufReader::new(stdout);
        for line in reader.lines() {
            match line {
                Ok(l) => {
                    lines_for_reader.lock().unwrap().push(l.clone());
                    if tx.send(l).is_err() {
                        break; // test stopped listening
                    }
                }
                Err(_) => break,
            }
        }
    });
    let stderr: Arc<Mutex<String>> = Arc::new(Mutex::new(String::new()));
    let stderr_for_reader = stderr.clone();
    std::thread::spawn(move || {
        use std::io::Read as _;
        let mut buf = String::new();
        let mut inner = stderr_pipe;
        let _ = inner.read_to_string(&mut buf);
        *stderr_for_reader.lock().unwrap() = buf;
    });

    McpSession {
        child: Some(child),
        stdin: Some(stdin),
        rx,
        lines,
        stderr,
        next_id: 0,
    }
}

impl McpSession {
    fn request(&mut self, method: &str, params: Value) -> Value {
        self.next_id += 1;
        let id = self.next_id;
        let line =
            json!({ "jsonrpc": "2.0", "id": id, "method": method, "params": params }).to_string();
        writeln!(self.stdin.as_mut().expect("stdin"), "{line}").expect("write request");
        let response = self
            .rx
            .recv_timeout(Duration::from_secs(60))
            .expect("response within 60s");
        let v: Value = serde_json::from_str(&response).expect("response is JSON");
        assert_eq!(v["jsonrpc"], "2.0", "JSON-RPC version pinned: {v}");
        assert_eq!(v["id"], id, "response ids match requests: {v}");
        v
    }

    fn notify(&mut self, method: &str) {
        let line = json!({ "jsonrpc": "2.0", "method": method }).to_string();
        writeln!(self.stdin.as_mut().expect("stdin"), "{line}").expect("write notification");
    }

    /// A tools/call that must succeed; returns the parsed payload text.
    fn call_ok(&mut self, name: &str, arguments: Value) -> Value {
        let v = self.request(
            "tools/call",
            json!({ "name": name, "arguments": arguments }),
        );
        assert!(
            v["error"].is_null(),
            "{name} must not be a protocol error: {v}"
        );
        assert!(
            v["result"]["isError"] != true,
            "{name} must not be a tool error: {v}"
        );
        let text = v["result"]["content"][0]["text"].as_str().unwrap_or("");
        serde_json::from_str(text).unwrap_or_else(|e| panic!("{name} payload is JSON: {e}: {text}"))
    }

    /// A tools/call that must fail as a tool error; returns the text.
    fn call_err(&mut self, name: &str, arguments: Value) -> String {
        let v = self.request(
            "tools/call",
            json!({ "name": name, "arguments": arguments }),
        );
        assert!(
            v["error"].is_null(),
            "{name} surfaced as protocol error: {v}"
        );
        assert_eq!(
            v["result"]["isError"], true,
            "{name} must be a tool error: {v}"
        );
        v["result"]["content"][0]["text"]
            .as_str()
            .unwrap_or_default()
            .to_string()
    }

    /// EOFs stdin, waits for the exit, and returns (code, stdout lines,
    /// stderr text).
    fn shutdown(mut self) -> (i32, Vec<String>, String) {
        self.stdin.take(); // EOF: graceful exit
        let mut child = self.child.take().expect("child");
        let code = child
            .wait()
            .expect("wait for mcp exit")
            .code()
            .unwrap_or(-1);
        let lines = self.lines.lock().unwrap().clone();
        let stderr = self.stderr.lock().unwrap().clone();
        (code, lines, stderr)
    }
}

/// Asserts every stdout line the session produced is a JSON-RPC 2.0
/// response (object, version pinned, id + result/error) — diagnostics or
/// verb chatter on stdout would break the protocol channel.
fn assert_stdout_is_pure_jsonrpc(lines: &[String]) {
    assert!(!lines.is_empty(), "session produced no stdout");
    for line in lines {
        let v: Value = serde_json::from_str(line)
            .unwrap_or_else(|e| panic!("stdout line is not JSON: {e}: {line}"));
        assert!(v.is_object(), "stdout line is not a JSON object: {line}");
        assert_eq!(
            v["jsonrpc"], "2.0",
            "stdout line is not JSON-RPC 2.0: {line}"
        );
        assert!(
            (v["result"].is_object() && v["error"].is_null())
                || (v["error"].is_object() && v["result"].is_null()),
            "stdout line is neither a result nor an error response: {line}"
        );
        assert!(v["id"].is_u64(), "response carries a request id: {line}");
    }
}

/// Clones a working copy with the CLI (the prepared checkout the MCP
/// session binds to) and asserts it materialized.
fn prepare_wc(server: &TestServer, repo: &str, dest: &Path) {
    let code = verbs::clone::run(
        &format!("{}/{repo}", server.base),
        &[],
        Some(dest.to_str().unwrap()),
        &net(server),
    );
    assert_eq!(code, 0, "preparing the working copy failed");
    assert!(dest.join(".naatre").is_dir());
}

// ---- tests -----------------------------------------------------------------

const ALL_TOOLS: [&str; 14] = [
    "clone", "sync", "sparse", "status", "log", "diff", "cat", "ls", "commit", "land", "resolve",
    "snapshot", "restore", "blame",
];

/// M12b: the full agent flow through tools alone — land
/// record_conflict:true settles the conflicted node (the settlement's
/// markers materialize into the session's copy), sync reports current,
/// edit + resolve clear the path; a traversal path argument is refused
/// before any read.
#[test]
fn mcp_conflicted_flow_record_land_sync_resolve_through_tools() {
    let server = start_server();
    create_repo(&server, "mcpconflict");
    let v1 = b"base-1\nbase-2\n";
    put_blob(&server, "mcpconflict", v1);
    let c1 = changeset(
        &server,
        "mcpconflict",
        &[],
        vec![change("add", "f.txt", v1)],
        "c1",
    );
    seed_main(&server, "mcpconflict", c1["id"].as_str().unwrap());

    let wc = server._dir.path().join("wc");
    prepare_wc(&server, "mcpconflict", &wc);
    let mut mcp = spawn_mcp(&wc, &server, &[]);
    let init = mcp.request(
        "initialize",
        json!({
            "protocolVersion": "2025-03-26",
            "capabilities": {},
            "clientInfo": { "name": "naatre-mcp-test", "version": "0" },
        }),
    );
    assert_eq!(init["result"]["protocolVersion"], "2025-03-26");
    mcp.notify("notifications/initialized");
    let cloned = mcp.call_ok(
        "clone",
        json!({ "url": format!("{}/mcpconflict", server.base), "into":
              server._dir.path().join("wc2").to_str().unwrap() }),
    );
    assert_eq!(cloned["repo"], "mcpconflict");

    // AGENT A (plain CLI, second directory): the competing edit on main.
    let wc_a = server._dir.path().join("wc_a");
    prepare_wc(&server, "mcpconflict", &wc_a);
    write_file(&wc_a, "f.txt", b"alpha-1\nbase-2\n");
    assert_eq!(
        verbs::commit::run(&wc_a, "alpha edit", None, &net(&server)),
        0
    );
    assert_eq!(verbs::land::run(&wc_a, None, false, &net(&server)), 0);

    // AGENT B (the session, tools alone): the conflicting edit, then the
    // opt-in land settles the conflicted node ONCE.
    write_file(&wc, "f.txt", b"beta-1\nbase-2\n");
    let committed = mcp.call_ok("commit", json!({ "message": "beta edit" }));
    assert_eq!(committed["committed"], true);
    let landed = mcp.call_ok("land", json!({ "key": "kb", "record_conflict": true }));
    assert!(is_64_hex(landed["landed"].as_str().unwrap()));
    assert_eq!(
        landed["conflicts"],
        json!(["f.txt"]),
        "the settlement names the conflicted paths: {landed}"
    );
    // The lander materialized the settlement: the session's disk holds
    // the marker bytes and status marks the path (senior-qa C1).
    let disk = std::fs::read_to_string(wc.join("f.txt")).unwrap();
    assert!(
        disk.starts_with("<<<<<<< naatre side 1/2: "),
        "the lander's disk holds the markers:\n{disk}"
    );
    let st = mcp.call_ok("status", json!({}));
    assert_eq!(st["conflicted"], json!(["f.txt"]), "{st}");
    // sync: the session is at the settlement already — current, marks
    // intact.
    let synced = mcp.call_ok("sync", json!({}));
    assert_eq!(synced["up_to_date"], true, "{synced}");

    // Edit to the resolved content, then resolve — through the tool.
    write_file(&wc, "f.txt", b"resolved-1\nbase-2\n");
    let resolved = mcp.call_ok("resolve", json!({ "path": "f.txt" }));
    assert_eq!(resolved["resolved"], "f.txt");
    assert!(is_64_hex(resolved["id"].as_str().unwrap()));
    let st = mcp.call_ok("status", json!({}));
    assert_eq!(st["conflicted"], json!([]), "the mark cleared: {st}");

    // The jail: traversal and absolute path arguments are refused before
    // any read (the same grammar the other path tools sit behind).
    let traversal = mcp.call_err("resolve", json!({ "path": "../escape.txt" }));
    assert!(traversal.contains("invalid path"), "{traversal}");
    let absolute = mcp.call_err("resolve", json!({ "path": "/etc/passwd" }));
    assert!(absolute.contains("invalid path"), "{absolute}");

    let _ = mcp.shutdown();
}

/// Test (a) — the plan's e2e: an MCP session performs clone → edit →
/// commit → land, then syncs after ANOTHER client lands, ending clean.
/// Also asserts the full tool list with schema sanity, the HTTP-analog
/// read shapes (log/diff/cat/ls/blame), snapshot/restore, stdout purity,
/// and the MCP_LOG stderr diagnostics.
#[test]
fn mcp_e2e_clone_edit_commit_land_and_sync_after_foreign_land() {
    let server = start_server();
    create_repo(&server, "mcp");
    let v1 = b"fn main() {}\n";
    put_blob(&server, "mcp", v1);
    let c1 = changeset(
        &server,
        "mcp",
        &[],
        vec![change("add", "src/main.rs", v1)],
        "c1",
    );
    seed_main(&server, "mcp", c1["id"].as_str().unwrap());

    let wc = server._dir.path().join("wc");
    prepare_wc(&server, "mcp", &wc);

    let mut mcp = spawn_mcp(&wc, &server, &[]);

    // Handshake: initialize echoes the supported protocol, then the
    // initialized notification is answered with silence.
    let init = mcp.request(
        "initialize",
        json!({
            "protocolVersion": "2025-03-26",
            "capabilities": {},
            "clientInfo": { "name": "naatre-mcp-test", "version": "0" },
        }),
    );
    assert_eq!(init["result"]["protocolVersion"], "2025-03-26");
    assert_eq!(init["result"]["serverInfo"]["name"], "naatre");
    assert_eq!(
        init["result"]["capabilities"]["tools"]["listChanged"],
        false
    );
    mcp.notify("notifications/initialized");

    // tools/list: the complete plan set of 14 (M12b adds resolve), each
    // with schema sanity.
    let tools = mcp.request("tools/list", json!({}));
    let listed: Vec<&str> = tools["result"]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    assert_eq!(listed.len(), 14, "exactly the 14 plan tools: {listed:?}");
    for expected in ALL_TOOLS {
        assert!(listed.contains(&expected), "tool {expected} listed");
    }
    for t in tools["result"]["tools"].as_array().unwrap() {
        assert!(!t["description"].as_str().unwrap().is_empty());
        assert_eq!(t["inputSchema"]["type"], "object");
        assert!(t["inputSchema"]["properties"].is_object());
    }

    // clone via MCP: a fresh working copy at wc2.
    let wc2 = server._dir.path().join("wc2");
    let cloned = mcp.call_ok(
        "clone",
        json!({ "url": format!("{}/mcp", server.base), "into": wc2.to_str().unwrap() }),
    );
    assert_eq!(cloned["repo"], "mcp");
    assert_eq!(cloned["files"], 1);
    assert!(is_64_hex(cloned["head"].as_str().unwrap()));
    assert_eq!(
        std::fs::read(wc2.join("src/main.rs")).unwrap(),
        v1,
        "MCP clone materialized the working copy"
    );

    // Edit the SESSION's working copy on disk; MCP status sees it.
    let v2 = b"fn main() { mcp }\n";
    write_file(&wc, "src/main.rs", v2);
    let st = mcp.call_ok("status", json!({}));
    assert_eq!(st["status"], "dirty");
    assert_eq!(st["modified"], json!(["src/main.rs"]));

    // Snapshot the dirty state, then commit + land via MCP.
    let snap = mcp.call_ok("snapshot", json!({ "ttl": "2h" }));
    assert!(is_64_hex(snap["id"].as_str().unwrap()));
    assert!(snap["expires_ns"].as_u64().is_some());

    let committed = mcp.call_ok("commit", json!({ "message": "mcp edit" }));
    assert_eq!(committed["committed"], true);
    assert!(is_64_hex(committed["id"].as_str().unwrap()));
    let landed = mcp.call_ok("land", json!({}));
    assert!(is_64_hex(landed["landed"].as_str().unwrap()));
    assert_eq!(landed["main"], landed["landed"], "main moved onto the land");

    // A SECOND client (plain CLI, another directory) clones, edits, commits,
    // lands. The MCP session must sync its working copy up to that.
    let v3 = b"fn main() { two }\n";
    let wc3 = server._dir.path().join("wc3");
    prepare_wc(&server, "mcp", &wc3);
    write_file(&wc3, "src/main.rs", v3);
    assert_eq!(
        verbs::commit::run(&wc3, "second client", None, &net(&server)),
        0
    );
    assert_eq!(verbs::land::run(&wc3, None, false, &net(&server)), 0);

    let synced = mcp.call_ok("sync", json!({}));
    assert_eq!(synced["up_to_date"], false);
    assert_eq!(
        synced["applied"],
        json!([{ "path": "src/main.rs", "kind": "mod" }]),
        "the MCP session applied the second client's land"
    );
    let st_clean = mcp.call_ok("status", json!({}));
    assert_eq!(st_clean["status"], "clean");

    // The blame index derives in the background; wait for it to cover the
    // chain (main's head) before the blame tool call.
    let (_, refv) = api(&server, "GET", "/v1/repos/mcp/refs/main", None);
    wait_blame_ready(
        &server,
        "mcp",
        refv["target"].as_str().unwrap(),
        "src/main.rs",
    );

    // The HTTP-analog reads return the OpenAPI shapes.
    let log = mcp.call_ok("log", json!({}));
    assert!(log["entries"].is_array());
    assert_eq!(log["has_more"], false);
    assert!(log["next_cursor"].is_null());
    assert!(
        log["entries"].as_array().unwrap().len() >= 3,
        "seed + MCP edit + second client in the chain"
    );

    let cat = mcp.call_ok("cat", json!({ "path": "src/main.rs" }));
    assert_eq!(cat["content"], String::from_utf8_lossy(v3).into_owned());
    assert_eq!(cat["binary"], false);
    assert_eq!(cat["size"], v3.len() as u64);
    assert_eq!(cat["exec"], false);
    assert!(is_64_hex(cat["blob_id"].as_str().unwrap()));

    let blame = mcp.call_ok("blame", json!({ "path": "src/main.rs" }));
    let lines = blame["lines"].as_array().unwrap();
    assert_eq!(lines.len(), 1);
    assert_eq!(lines[0]["text"], String::from_utf8_lossy(v3).trim_end());
    assert!(lines[0]["cs"].is_string() && lines[0]["author"].is_string());

    let diff = mcp.call_ok("diff", json!({ "a": c1["id"], "b": "main" }));
    let changes = diff["changes"].as_array().unwrap();
    assert!(
        changes
            .iter()
            .any(|c| c["path"] == "src/main.rs" && c["kind"] == "mod"),
        "server diff shows the file: {changes:?}"
    );

    let ls_root = mcp.call_ok("ls", json!({}));
    assert!(
        ls_root["entries"]
            .as_array()
            .unwrap()
            .iter()
            .any(|e| e["name"] == "src"),
        "root listing shows src/: {:?}",
        ls_root
    );
    let ls_src = mcp.call_ok("ls", json!({ "path": "src" }));
    assert!(
        ls_src["entries"]
            .as_array()
            .unwrap()
            .iter()
            .any(|e| e["name"] == "main.rs")
    );

    // restore brings the snapshot's (pre-commit) content back over the
    // synced state: the files move, the HEAD stays — the index moves to
    // the restored state, so status is CLEAN again (the divergence is
    // against the head, exactly the M5 restore pin).
    let restored = mcp.call_ok("restore", json!({ "snap_id": snap["id"], "force": true }));
    assert_eq!(restored["restored"], snap["id"]);
    assert_eq!(restored["files"], 1);
    assert_eq!(
        restored["head"], synced["head"],
        "restore leaves the head unchanged"
    );
    let st_after_restore = mcp.call_ok("status", json!({}));
    assert_eq!(st_after_restore["status"], "clean");
    assert_eq!(
        std::fs::read(wc.join("src/main.rs")).unwrap(),
        v2,
        "restore materialized the snapshot's content"
    );

    let (code, stdout_lines, stderr_text) = mcp.shutdown();
    assert_eq!(code, 0, "graceful EOF exit");
    assert_stdout_is_pure_jsonrpc(&stdout_lines);
    // Diagnostics are on stderr (MCP_LOG=1) and never leak to stdout.
    assert!(
        stderr_text.contains("mcp: <- initialize"),
        "MCP_LOG diagnostics on stderr: {stderr_text:?}"
    );
    assert!(!stdout_lines.iter().any(|l| l.contains("mcp:")));
}

/// Test (b) — --read-only: the gated tools answer a tool error NAMING THE
/// FLAG; the pure reads work. The token comes from --token (env cleared)
/// to pin that auth path.
#[test]
fn mcp_read_only_refuses_mutations_and_allows_reads() {
    let server = start_server();
    create_repo(&server, "ro");
    let v1 = b"read only\n";
    put_blob(&server, "ro", v1);
    let c1 = changeset(
        &server,
        "ro",
        &[],
        vec![change("add", "file.txt", v1)],
        "c1",
    );
    seed_main(&server, "ro", c1["id"].as_str().unwrap());

    let wc = server._dir.path().join("wc");
    prepare_wc(&server, "ro", &wc);

    // --token supplies the bearer; NAATRE_TOKEN is stripped so the flag
    // path is what authenticates.
    let mut mcp = spawn_mcp(&wc, &server, &["--read-only", "--token", &server.token]);
    // The blame tool below needs the derived index over the seeded chain.
    wait_blame_ready(&server, "ro", c1["id"].as_str().unwrap(), "file.txt");

    // The tool LIST still shows all 14 (the gate fires at call time).
    let tools = mcp.request("tools/list", json!({}));
    assert_eq!(tools["result"]["tools"].as_array().unwrap().len(), 14);

    // Every non-read tool refuses, naming the flag (resolve included —
    // it commits a resolution changeset).
    for gated in [
        "clone", "sync", "sparse", "commit", "land", "resolve", "snapshot", "restore",
    ] {
        let text = mcp.call_err(gated, json!({}));
        assert!(
            text.contains("--read-only"),
            "{gated} refusal names the flag: {text}"
        );
    }
    let sparse_text = mcp.call_err("sparse", json!({ "action": "list" }));
    assert!(sparse_text.contains("--read-only"));

    // The pure reads work (status makes zero server calls; log exercises
    // the --token client).
    let st = mcp.call_ok("status", json!({}));
    assert_eq!(st["status"], "clean");
    let log = mcp.call_ok("log", json!({}));
    assert_eq!(log["entries"].as_array().unwrap().len(), 1);
    let cat = mcp.call_ok("cat", json!({ "path": "file.txt" }));
    assert_eq!(cat["content"], String::from_utf8_lossy(v1).into_owned());
    let blame = mcp.call_ok("blame", json!({ "path": "file.txt" }));
    assert_eq!(blame["lines"].as_array().unwrap().len(), 1);
    let ls = mcp.call_ok("ls", json!({}));
    assert!(ls["entries"].is_array());
    let diff = mcp.call_ok("diff", json!({}));
    assert!(diff["changes"].is_array());

    let (code, stdout_lines, _) = mcp.shutdown();
    assert_eq!(code, 0);
    assert_stdout_is_pure_jsonrpc(&stdout_lines);
}

/// Test (c) — the working-copy flock, both directions: while the MCP
/// session holds `.naatre/wc.lock`, a CLI verb in the same tree refuses
/// naming the lock; while a CLI-side handle holds it, `naatre mcp` refuses
/// to start, also naming the lock.
#[test]
fn mcp_working_copy_flock_two_writers() {
    let server = start_server();
    create_repo(&server, "flock");
    put_blob(&server, "flock", b"a\n");
    let c1 = changeset(
        &server,
        "flock",
        &[],
        vec![change("add", "a.txt", b"a\n")],
        "c1",
    );
    seed_main(&server, "flock", c1["id"].as_str().unwrap());
    let wc = server._dir.path().join("wc");
    prepare_wc(&server, "flock", &wc);

    // Direction 1: the MCP session holds the lock for its lifetime → the
    // CLI verb refuses, naming the lock.
    let mut mcp = spawn_mcp(&wc, &server, &[]);
    let _ = mcp.request("initialize", json!({}));
    let out = run_binary(&server, &wc, &["status", "--json"]);
    assert_ne!(
        out.status.code(),
        Some(0),
        "CLI status must refuse: {out:?}"
    );
    let stderr = String::from_utf8_lossy(&out.stderr).into_owned();
    assert!(
        stderr.contains("wc.lock"),
        "refusal names the lock: {stderr:?}"
    );
    assert!(
        stderr.contains(".naatre/wc.lock"),
        "refusal names the lock file path: {stderr:?}"
    );
    mcp.shutdown();

    // Direction 2: a held lock (here: a test-process handle, exactly what a
    // CLI verb holds mid-flight) → `naatre mcp` refuses to START.
    let _guard = naatre_cli::wc::lock::acquire(&wc);
    let _guard = match _guard.expect("test takes the lock") {
        naatre_cli::wc::lock::Outcome::Locked(g) => Some(g),
        naatre_cli::wc::lock::Outcome::NoNaatreDir => panic!(".naatre exists"),
    };
    let out = run_binary(&server, &wc, &["mcp"]);
    assert_ne!(out.status.code(), Some(0), "mcp must refuse to start");
    let stderr = String::from_utf8_lossy(&out.stderr).into_owned();
    assert!(
        stderr.contains("wc.lock"),
        "startup refusal names the lock: {stderr:?}"
    );
    drop(_guard);
}

/// Test (d) — tool-schema vs OpenAPI: for the tools with HTTP analogs
/// (log, diff, cat, blame — plus ls's tree shape), the input parameter
/// names/shapes match docs/openapi.json (parsed here, not transcribed).
#[test]
fn mcp_tool_schemas_match_openapi() {
    // A fabricated working copy is enough: tools/list makes no server
    // calls.
    let dir = tempfile::tempdir().unwrap();
    let wc = dir.path().join("wc");
    std::fs::create_dir_all(wc.join(".naatre")).unwrap();
    std::fs::write(
        wc.join(".naatre/config.toml"),
        "server_url = \"http://127.0.0.1:9\"\ntoken = \"t\"\n",
    )
    .unwrap();
    let head = "0".repeat(64);
    std::fs::write(
        wc.join(".naatre/state.json"),
        json!({ "head": head, "base": null, "sparse": [], "server_repo": "mcp" }).to_string(),
    )
    .unwrap();

    let server = TestServer {
        _dir: dir,
        base: String::new(),
        token: String::new(),
        db_path: PathBuf::new(),
        shutdown: None,
        handle: None,
    };
    let mut mcp = spawn_mcp(&wc, &server, &[]);
    let tools = mcp.request("tools/list", json!({}));
    mcp.shutdown();
    let schema_of = |name: &str| -> Value {
        tools["result"]["tools"]
            .as_array()
            .unwrap()
            .iter()
            .find(|t| t["name"] == name)
            .unwrap_or_else(|| panic!("tool {name} listed"))["inputSchema"]
            .clone()
    };

    let openapi: Value = serde_json::from_str(
        &std::fs::read_to_string(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../docs/openapi.json"
        ))
        .expect("docs/openapi.json"),
    )
    .unwrap();
    let params_of = |route: &str| -> Vec<String> {
        openapi["paths"][route]["get"]["parameters"]
            .as_array()
            .unwrap()
            .iter()
            .map(|p| p["name"].as_str().unwrap().to_string())
            .filter(|n| n != "r") // the path template's repo segment
            .collect()
    };
    let props_of = |schema: &Value| -> Vec<String> {
        let mut names: Vec<String> = schema["properties"]
            .as_object()
            .unwrap()
            .keys()
            .cloned()
            .collect();
        names.sort();
        names
    };

    // log: from / limit / cursor / path — exactly the OpenAPI query params.
    let mut log_params = params_of("/v1/repos/{r}/log");
    log_params.sort();
    assert_eq!(props_of(&schema_of("log")), log_params);
    assert_eq!(schema_of("log")["properties"]["limit"]["type"], "integer");

    // diff: a / b.
    let mut diff_params = params_of("/v1/repos/{r}/diff");
    diff_params.sort();
    assert_eq!(props_of(&schema_of("diff")), diff_params);

    // cat: at / path, path required.
    let mut cat_params = params_of("/v1/repos/{r}/file");
    cat_params.sort();
    assert_eq!(props_of(&schema_of("cat")), cat_params);
    assert_eq!(schema_of("cat")["required"], json!(["path"]));

    // blame: at / path, path required.
    let mut blame_params = params_of("/v1/repos/{r}/blame");
    blame_params.sort();
    assert_eq!(props_of(&schema_of("blame")), blame_params);
    assert_eq!(schema_of("blame")["required"], json!(["path"]));

    // ls walks the trees route's shape server-side; its input mirrors the
    // cat/file pair (path + at).
    let mut ls_props = props_of(&schema_of("ls"));
    ls_props.sort();
    assert_eq!(ls_props, vec!["at".to_string(), "path".to_string()]);
}
