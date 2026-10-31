//! `naatre mcp [--read-only] [--token T]` — the stdio MCP server (plan M7).
//!
//! Speaks newline-delimited JSON-RPC 2.0 over stdin/stdout: one JSON object
//! per line, no embedded newlines. The implemented MCP surface is small and
//! deliberately hand-rolled (no SDK dependency): `initialize`,
//! `notifications/initialized`, `tools/list`, `tools/call`, `ping`. The
//! verbs are synchronous (blocking ureq), so a tokio-based SDK would drag a
//! full async runtime in for nothing; the plan itself lists an MCP SDK as a
//! heavyweight dependency. Diagnostics go to STDERR (`MCP_LOG=1` enables
//! per-request lines) — stdout is the protocol channel and nothing else
//! writes to it: the tool handlers call the verbs' value-returning cores,
//! which never print.
//!
//! The session is bound to ONE checkout: the working copy at startup is the
//! only root (no per-call path, no server URL override — the server comes
//! from the working copy's config per the client precedence). The session
//! holds `.naatre/wc.lock` for its whole lifetime, so concurrent CLI use in
//! the same tree is detected and refused (and vice versa at startup).
//!
//! `--read-only` gates everything but the pure reads (status, log, diff,
//! cat, ls, blame) behind a tool error naming the flag. `--token` overrides
//! the bearer for every tool call (argv is visible in `ps` — prefer the
//! token store; docs recommend a least-privilege scoped token).

use std::path::Path;

use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::client::{Client, HttpError, uri_segment};
use crate::wc::Wc;
use crate::wc::lock::{self, Outcome, WcLock};

/// Protocol versions this server speaks; `initialize` echoes the client's
/// request when one of these, else the latest.
const SUPPORTED_VERSIONS: [&str; 3] = ["2025-06-18", "2025-03-26", "2024-11-05"];
const LATEST_PROTOCOL: &str = "2025-06-18";

/// The `--read-only` allowlist: the pure reads only.
const READ_ONLY_TOOLS: [&str; 6] = ["status", "log", "diff", "cat", "ls", "blame"];

/// Runs the server: binds the working copy, takes the working-copy flock
/// for the session's lifetime, serves until stdin EOF (exit 0).
pub fn run(root: &Path, read_only: bool, token: Option<String>) -> i32 {
    if let Err(e) = Wc::open(root) {
        eprintln!("error: {e}");
        return 1;
    }
    let _guard: Option<WcLock> = match lock::acquire(root) {
        Ok(Outcome::Locked(g)) => Some(g),
        // Unreachable after Wc::open (which verified .naatre/ exists); a
        // vanishing .naatre mid-startup leaves the session lockless rather
        // than locked-out.
        Ok(Outcome::NoNaatreDir) => None,
        Err(message) => {
            eprintln!("error: {message}");
            return 1;
        }
    };
    let net = Net {
        server: None, // no server override: an MCP session is bound to one checkout
        token,
    };
    serve(root, read_only, &net)
}

/// Diagnostics: `MCP_LOG` set to anything but ""/0/false enables per-request
/// stderr lines. Never on stdout — that is the protocol channel.
fn diag_enabled() -> bool {
    match std::env::var("MCP_LOG") {
        Ok(v) => !matches!(v.as_str(), "" | "0" | "false"),
        Err(_) => false,
    }
}

fn diag(message: &str) {
    if diag_enabled() {
        eprintln!("mcp: {message}");
    }
}

/// The serve loop: one JSON-RPC message per stdin line, one response line
/// per request (notifications are never answered). EOF ends the session.
fn serve(root: &Path, read_only: bool, net: &Net) -> i32 {
    use std::io::{BufRead, Write as _};
    let stdin = std::io::stdin();
    let mut reader = stdin.lock();
    let stdout = std::io::stdout();
    let mut out = stdout.lock();
    loop {
        let mut line = String::new();
        match reader.read_line(&mut line) {
            Ok(0) => break, // EOF: graceful shutdown
            Ok(_) => {}
            Err(e) => {
                diag(&format!("stdin read failed: {e}"));
                break;
            }
        }
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let Some(response) = handle(line, root, read_only, net) else {
            continue;
        };
        if writeln!(out, "{response}").is_err() || out.flush().is_err() {
            diag("stdout write failed; stopping");
            break;
        }
    }
    0
}

/// The tools/list result: each descriptor as {name, description,
/// inputSchema}.
fn tools_list_result() -> Value {
    let tools: Vec<Value> = tool_defs()
        .into_iter()
        .map(|t| {
            json!({
                "name": t.name,
                "description": t.description,
                "inputSchema": t.schema,
            })
        })
        .collect();
    json!({ "tools": tools })
}

/// Handles one line; `None` = no response (a notification, or a broken
/// transport). Every `Some` is a single-line JSON-RPC message.
fn handle(line: &str, root: &Path, read_only: bool, net: &Net) -> Option<String> {
    let value: Value = match serde_json::from_str(line) {
        Ok(v) => v,
        Err(e) => {
            return Some(error_response(
                Value::Null,
                -32700,
                &format!("parse error: {e}"),
            ));
        }
    };
    if !value.is_object() {
        return Some(error_response(
            Value::Null,
            -32600,
            "request is not a JSON object",
        ));
    }
    let id = value.get("id").cloned();
    let method = value["method"].as_str().unwrap_or_default().to_string();
    diag(&format!("<- {method}"));
    // A message without an "id" member is a notification: handled (or
    // ignored) silently, never answered.
    let Some(id) = id else {
        return None; // notifications/initialized and friends need no reply
    };
    let params = value.get("params").cloned().unwrap_or(Value::Null);
    let result = match method.as_str() {
        "initialize" => Ok(initialize_result(&params)),
        "ping" => Ok(json!({})),
        "tools/list" => Ok(tools_list_result()),
        "tools/call" => call_tool(&params, root, read_only, net),
        m if m.starts_with("notifications/") => return None,
        other => Err(json!({
            "code": -32601,
            "message": format!("method not found: {other}"),
        })),
    };
    Some(match result {
        Ok(v) => json!({ "jsonrpc": "2.0", "id": id, "result": v }).to_string(),
        Err(e) => json!({ "jsonrpc": "2.0", "id": id, "error": e }).to_string(),
    })
}

fn initialize_result(params: &Value) -> Value {
    let requested = params["protocolVersion"].as_str();
    let protocol = requested
        .filter(|v| SUPPORTED_VERSIONS.contains(v))
        .unwrap_or(LATEST_PROTOCOL);
    json!({
        "protocolVersion": protocol,
        "capabilities": { "tools": { "listChanged": false } },
        "serverInfo": {
            "name": "naatre",
            "version": env!("CARGO_PKG_VERSION"),
        },
    })
}

fn error_response(id: Value, code: i64, message: &str) -> String {
    json!({
        "jsonrpc": "2.0",
        "id": id,
        "error": { "code": code, "message": message },
    })
    .to_string()
}

/// One tool descriptor: name, description, JSON Schema input.
struct Tool {
    name: &'static str,
    description: &'static str,
    schema: Value,
}

/// The complete plan set: clone, sync, sparse, status, log, diff, cat, ls,
/// commit, land, snapshot, restore, blame. Input schemas mirror the CLI
/// flags; for the HTTP-analog reads (log, diff, cat, ls-tree via `ls`,
/// blame) the parameter names match the OpenAPI query params (test
/// `mcp_tool_schemas_match_openapi` pins this against docs/openapi.json).
fn tool_defs() -> Vec<Tool> {
    let at =
        || json!({ "type": "string", "description": "Changeset id or ref name (default: head)" });
    vec![
        Tool {
            name: "clone",
            description: "Clone a repo into a NEW working copy (fresh directory; this session \
                          stays bound to its own working copy). The url must be this \
                          session's configured server — a different origin is refused unless \
                          the call carries its own explicit token (the session's token is \
                          never sent to another origin). Relative --into paths resolve \
                          inside the MCP server process's working directory.",
            schema: json!({
                "type": "object",
                "properties": {
                    "url": { "type": "string",
                             "description": "http://host[:port]/<repo> or a bare repo name" },
                    "sparse": { "type": "array", "items": { "type": "string" },
                                "description": "Materialize only these path prefixes" },
                    "into": { "type": "string",
                              "description": "Destination directory (default: the repo name; \
                                              must not exist)" },
                    "token": { "type": "string",
                               "description": "Explicit bearer for cloning from an origin \
                                               OTHER than this session's server; required \
                                               there (the session's token never crosses)" },
                },
                "required": ["url"],
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "sync",
            description: "Advance this working copy to the server's main (refuses only when \
                          incoming paths collide with local edits/untracked files).",
            schema: json!({
                "type": "object",
                "properties": {
                    "force": { "type": "boolean",
                               "description": "Discard local changes to incoming-colliding \
                                               files, then proceed" },
                },
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "sparse",
            description: "The sparse prefix list: add materializes a prefix at head, remove \
                         drops it, list prints it (empty list = full checkout).",
            schema: json!({
                "type": "object",
                "properties": {
                    "action": { "type": "string", "enum": ["add", "remove", "list"] },
                    "prefix": { "type": "string",
                                "description": "Path prefix (required for add/remove)" },
                    "force": { "type": "boolean",
                               "description": "add: discard local edits and colliding untracked \
                                               files on the widened paths" },
                },
                "required": ["action"],
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "status",
            description: "Working-copy status: disk vs the stat index. Zero server calls. \
                         Returns {status, modified, deleted, untracked}.",
            schema: json!({
                "type": "object",
                "properties": {},
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "log",
            description: "The first-parent chain, newest first (GET /v1/…/log). Returns \
                         {entries, has_more, next_cursor}.",
            schema: json!({
                "type": "object",
                "properties": {
                    "from": { "type": "string",
                              "description": "Changeset id to walk from (default: the main \
                                              ref target)" },
                    "limit": { "type": "integer", "minimum": 1,
                               "description": "Page size (server caps apply)" },
                    "cursor": { "type": "string",
                                "description": "Resume after the given changeset id (wins \
                                                over from)" },
                    "path": { "type": "string",
                              "description": "Path-history mode: only changesets touching \
                                              this path" },
                },
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "diff",
            description: "With a and b: the server-side diff of two changesets (ids or ref \
                          names). Without: head vs disk (local edits). Returns {changes}.",
            schema: json!({
                "type": "object",
                "properties": {
                    "a": { "type": "string", "description": "First changeset id or ref name" },
                    "b": { "type": "string", "description": "Second changeset id or ref name" },
                },
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "cat",
            description: "Print a file's raw bytes (default: at head). Returns {path, at, \
                         blob_id, size, exec, content, binary} — content is the file text, \
                         empty when binary.",
            schema: json!({
                "type": "object",
                "properties": {
                    "path": { "type": "string", "description": "Repo-relative path" },
                    "at": at(),
                },
                "required": ["path"],
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "ls",
            description: "List one tree node's entries (default: the root at head). Returns \
                         {entries} in the server's tree-entry shape.",
            schema: json!({
                "type": "object",
                "properties": {
                    "path": { "type": "string", "description": "Directory path (default: the \
                                                              root)" },
                    "at": at(),
                },
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "commit",
            description: "Commit all changes under the sparse prefixes (add-all semantics). \
                         Returns {committed, id, root_tree_id, generation} — committed is \
                         false when there was nothing to commit.",
            schema: json!({
                "type": "object",
                "properties": {
                    "message": { "type": "string", "description": "Commit message" },
                    "author": { "type": "string",
                                "description": "Author string (default: NAATRE_AUTHOR, else \
                                                user@host)" },
                },
                "required": ["message"],
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "land",
            description: "Land this working copy's head onto the server's main (server-side \
                         rebase). Returns {key, landed, main, rebased} — the key goes back \
                         into a retry or a re-land after a conflict.",
            schema: json!({
                "type": "object",
                "properties": {
                    "key": { "type": "string",
                             "description": "Idempotency key (default: a random 8-hex \
                                             suffix)" },
                },
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "snapshot",
            description: "Capture the working copy as an ephemeral server-side snapshot. \
                         Returns {id, expires_ns}; head and index stay.",
            schema: json!({
                "type": "object",
                "properties": {
                    "ttl": { "type": "string",
                             "description": "Lifetime, e.g. 90s, 5m, 2h, 7d (min 60s; \
                                             default 7d)" },
                },
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "restore",
            description: "Restore a snapshot's files into this working copy (head unchanged).",
            schema: json!({
                "type": "object",
                "properties": {
                    "snap_id": { "type": "string",
                                 "description": "Snapshot id (64 hex, from the snapshot \
                                                 tool)" },
                    "force": { "type": "boolean",
                               "description": "Discard local edits and colliding untracked \
                                               files on the snapshot's paths" },
                },
                "required": ["snap_id"],
                "additionalProperties": false,
            }),
        },
        Tool {
            name: "blame",
            description: "Blame a file: per-line attribution. Returns {lines:[{cs,author, \
                         text}]} (empty for a binary file — a structured empty result).",
            schema: json!({
                "type": "object",
                "properties": {
                    "path": { "type": "string", "description": "Repo-relative path" },
                    "at": at(),
                },
                "required": ["path"],
                "additionalProperties": false,
            }),
        },
    ]
}

/// A successful tool call: the primary JSON payload plus optional extra
/// text blocks (e.g. blame's degraded warning).
struct ToolOk {
    payload: Value,
    notes: Vec<String>,
}

fn call_tool(params: &Value, root: &Path, read_only: bool, net: &Net) -> Result<Value, Value> {
    let Some(name) = params["name"].as_str() else {
        return Err(invalid_params("tools/call requires params.name"));
    };
    let args = params.get("arguments").cloned().unwrap_or(json!({}));
    if !tool_defs().iter().any(|t| t.name == name) {
        return Err(invalid_params(&format!("unknown tool: {name}")));
    }
    if read_only && !READ_ONLY_TOOLS.contains(&name) {
        // The tool error names the flag (the plan's --read-only contract).
        return Ok(tool_error(format!(
            "tool '{name}' is disabled: this server was started with --read-only \
             (the read-only allowlist is: status, log, diff, cat, ls, blame)"
        )));
    }
    match dispatch(name, &args, root, net) {
        Ok(ok) => {
            let mut content = vec![json!({ "type": "text", "text": ok.payload.to_string() })];
            for note in ok.notes {
                content.push(json!({ "type": "text", "text": note }));
            }
            Ok(json!({ "content": content, "isError": false }))
        }
        Err(ToolFailure::Params(message)) => Err(invalid_params(&message)),
        Err(ToolFailure::Tool(message)) => Ok(tool_error(message)),
    }
}

/// Why a dispatched tool call failed: `Params` is protocol-level (the
/// arguments don't match the tool's schema → JSON-RPC -32602); `Tool` is a
/// verb failure reported as an `isError` tool result.
enum ToolFailure {
    Params(String),
    Tool(String),
}

impl From<String> for ToolFailure {
    fn from(message: String) -> Self {
        ToolFailure::Params(message)
    }
}

/// Maps a verb failure into the tool-level failure (the argument helpers'
/// `Err(String)`s auto-wrap into `Params` via [`ToolFailure::from`]).
fn tool<T>(r: Result<T, VerbError>) -> Result<T, ToolFailure> {
    r.map_err(|e| ToolFailure::Tool(verb_error_text(e)))
}

fn invalid_params(message: &str) -> Value {
    json!({ "code": -32602, "message": message })
}

fn tool_error(text: String) -> Value {
    json!({
        "content": [{ "type": "text", "text": text }],
        "isError": true,
    })
}

/// Renders a verb failure as the tool error text: `code: message` for an
/// envelope (plus its details — a land conflict carries the conflict list
/// and the resolution recipe there), the plain message otherwise.
fn verb_error_text(e: VerbError) -> String {
    match e {
        VerbError::Http(HttpError::Envelope {
            code,
            message,
            details: Some(details),
            ..
        }) => format!("{code}: {message}\ndetails: {details}"),
        e => e.to_string(),
    }
}

// ---- argument helpers (mistyped/missing params are protocol -32602) ------

fn take_str(args: &Value, key: &str, required: bool) -> Result<Option<String>, String> {
    match args.get(key) {
        None | Some(Value::Null) => {
            if required {
                Err(format!("missing required argument: {key}"))
            } else {
                Ok(None)
            }
        }
        Some(v) => match v.as_str() {
            Some(s) => Ok(Some(s.to_string())),
            None => Err(format!("argument {key:?} must be a string")),
        },
    }
}

fn take_bool(args: &Value, key: &str) -> Result<Option<bool>, String> {
    match args.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(v) => match v.as_bool() {
            Some(b) => Ok(Some(b)),
            None => Err(format!("argument {key:?} must be a boolean")),
        },
    }
}

fn take_u64(args: &Value, key: &str) -> Result<Option<u64>, String> {
    match args.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(v) => match v.as_u64() {
            Some(n) => Ok(Some(n)),
            None => Err(format!("argument {key:?} must be a non-negative integer")),
        },
    }
}

fn take_str_list(args: &Value, key: &str) -> Result<Option<Vec<String>>, String> {
    match args.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(v) => match v.as_array() {
            Some(items) => {
                let mut out = Vec::with_capacity(items.len());
                for item in items {
                    match item.as_str() {
                        Some(s) => out.push(s.to_string()),
                        None => {
                            return Err(format!("argument {key:?} must be an array of strings"));
                        }
                    }
                }
                Ok(Some(out))
            }
            None => Err(format!("argument {key:?} must be an array of strings")),
        },
    }
}

/// Dispatches one tool call to the matching verb's value-returning core.
/// Argument-schema problems are `Params` (protocol -32602); verb failures
/// are `Tool` (isError results).
fn dispatch(name: &str, args: &Value, root: &Path, net: &Net) -> Result<ToolOk, ToolFailure> {
    match name {
        "clone" => {
            let url = take_str(args, "url", true)?;
            let sparse = take_str_list(args, "sparse")?.unwrap_or_default();
            let into = take_str(args, "into", false)?;
            let token = take_str(args, "token", false)?;
            let url = url.as_deref().unwrap_or_default();
            // sec-C2: the clone tool is the one tool whose target the
            // CALLER names — the channel a prompt injection reaches for.
            // The session's bearer must never cross to a foreign origin:
            // a url whose origin is not the session's configured server is
            // refused (error names the configured server; no request is
            // sent), unless the call carries its own explicit token — which
            // then REPLACES the session's for that one clone.
            let session_origin = Wc::open(root)
                .and_then(|wc| wc.load_config().map(|c| c.server_url))
                .map_err(ToolFailure::Tool)?
                .trim_end_matches('/')
                .to_string();
            let (url_origin, _) = super::clone::split_url(url, net)
                .map_err(|e| ToolFailure::Tool(verb_error_text(e)))?;
            if url_origin.trim_end_matches('/') != session_origin {
                match token.as_deref() {
                    None => {
                        return Err(ToolFailure::Tool(format!(
                            "refusing to clone from {url_origin}: it is not this session's \
                             configured server ({session_origin}); the session's token is \
                             never sent to another origin — pass an explicit \"token\" \
                             argument to clone from elsewhere"
                        )));
                    }
                    Some(explicit) => {
                        let net = Net {
                            server: None,
                            token: Some(explicit.to_string()),
                        };
                        let payload =
                            tool(super::clone::value(url, &sparse, into.as_deref(), &net))?;
                        return Ok(ToolOk {
                            payload,
                            notes: vec![],
                        });
                    }
                }
            }
            let payload = tool(super::clone::value(url, &sparse, into.as_deref(), net))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "sync" => {
            let force = take_bool(args, "force")?.unwrap_or(false);
            let payload = tool(super::sync::value(root, force, net))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "sparse" => {
            let action = take_str(args, "action", true)?.unwrap_or_default();
            let prefix = take_str(args, "prefix", false)?;
            let force = take_bool(args, "force")?.unwrap_or(false);
            let action = match action.as_str() {
                "add" => {
                    let Some(prefix) = prefix.clone() else {
                        return Err(ToolFailure::Params(
                            "sparse add requires the 'prefix' argument".into(),
                        ));
                    };
                    super::sparse::Action::Add { prefix, force }
                }
                "remove" => {
                    let Some(prefix) = prefix else {
                        return Err(ToolFailure::Params(
                            "sparse remove requires the 'prefix' argument".into(),
                        ));
                    };
                    super::sparse::Action::Remove(prefix)
                }
                "list" => super::sparse::Action::List,
                other => {
                    return Err(ToolFailure::Params(format!(
                        "argument \"action\" must be one of add, remove, list (got {other:?})"
                    )));
                }
            };
            let payload = tool(super::sparse::value(root, action, net))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "status" => {
            let payload = tool(
                super::status::report(root)
                    .map_err(VerbError::Local)
                    .map(|rep| rep.to_json()),
            )?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "log" => {
            let from = take_str(args, "from", false)?;
            let limit = take_u64(args, "limit")?;
            let cursor = take_str(args, "cursor", false)?;
            let path = take_str(args, "path", false)?;
            let payload = tool(log_value(
                root,
                net,
                from.as_deref(),
                limit,
                cursor.as_deref(),
                path.as_deref(),
            ))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "diff" => {
            let a = take_str(args, "a", false)?;
            let b = take_str(args, "b", false)?;
            let at = match (a.as_deref(), b.as_deref()) {
                (Some(a), Some(b)) => Some((a, b)),
                (None, None) => None,
                _ => {
                    return Err(ToolFailure::Params(
                        "diff takes both 'a' and 'b', or neither (then it diffs head vs disk)"
                            .into(),
                    ));
                }
            };
            let payload = tool(super::reads::diff_value(root, at, net))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "cat" => {
            let path = take_str(args, "path", true)?;
            let at = take_str(args, "at", false)?;
            let (blob, size, exec, bytes) = tool(super::reads::cat_bytes(
                root,
                path.as_deref().unwrap_or_default(),
                at.as_deref(),
                net,
            ))?;
            let text = String::from_utf8(bytes).ok();
            let payload = json!({
                "path": path,
                "at": at,
                "blob_id": blob.to_hex(),
                "size": size,
                "exec": exec,
                "binary": text.is_none(),
                "content": text.unwrap_or_default(),
            });
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "ls" => {
            let path = take_str(args, "path", false)?;
            let at = take_str(args, "at", false)?;
            let payload = tool(super::reads::ls_value(
                root,
                path.as_deref(),
                at.as_deref(),
                net,
            ))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "commit" => {
            let message = take_str(args, "message", true)?;
            let author = take_str(args, "author", false)?;
            let payload = tool(super::commit::value(
                root,
                message.as_deref().unwrap_or_default(),
                author.as_deref(),
                net,
            ))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "land" => {
            let key = take_str(args, "key", false)?;
            let outcome = tool(super::land::core(root, key.as_deref(), net, &mut |_| {}))?;
            let payload = json!({
                "key": outcome.key,
                "landed": outcome.landed,
                "main": outcome.main,
                "rebased": outcome.rebased,
            });
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "snapshot" => {
            let ttl = take_str(args, "ttl", false)?;
            let payload = tool(super::snapshot::value(root, ttl.as_deref(), net))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "restore" => {
            let snap_id = take_str(args, "snap_id", true)?;
            let force = take_bool(args, "force")?.unwrap_or(false);
            let payload = tool(super::restore::value(
                root,
                snap_id.as_deref().unwrap_or_default(),
                force,
                net,
            ))?;
            Ok(ToolOk {
                payload,
                notes: vec![],
            })
        }
        "blame" => {
            let path = take_str(args, "path", true)?;
            let at = take_str(args, "at", false)?;
            let result = tool(super::reads::blame_value(
                root,
                path.as_deref().unwrap_or_default(),
                at.as_deref(),
                net,
            ))?;
            let payload = json!({ "lines": result.lines });
            let mut notes = Vec::new();
            if let Some(warning) = result.degraded_warning {
                notes.push(warning);
            }
            Ok(ToolOk { payload, notes })
        }
        other => Err(ToolFailure::Params(format!("unknown tool: {other}"))),
    }
}

/// The MCP `log` tool: GET /v1/…/log through the client lib (no CLI verb;
/// the plan's tool set includes log). Query params match the OpenAPI:
/// from, limit, cursor, path; the response is the LogResponse as-is.
fn log_value(
    root: &Path,
    net: &Net,
    from: Option<&str>,
    limit: Option<u64>,
    cursor: Option<&str>,
    path: Option<&str>,
) -> Result<Value, VerbError> {
    let wc = Wc::open(root).map_err(VerbError::Local)?;
    let state = wc.load_state().map_err(VerbError::Local)?;
    let client: Client = wc.client(net).map_err(VerbError::Local)?;
    let mut query: Vec<(String, String)> = Vec::new();
    if let Some(f) = from {
        query.push(("from".into(), f.to_string()));
    }
    if let Some(l) = limit {
        query.push(("limit".into(), l.to_string()));
    }
    if let Some(c) = cursor {
        query.push(("cursor".into(), c.to_string()));
    }
    if let Some(p) = path {
        query.push(("path".into(), p.to_string()));
    }
    let refs: Vec<(&str, &str)> = query
        .iter()
        .map(|(k, v)| (k.as_str(), v.as_str()))
        .collect();
    client
        .get_json(
            &format!("/v1/repos/{}/log", uri_segment(&state.server_repo)),
            &refs,
        )
        .map_err(VerbError::Http)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tool_set_is_the_complete_plan_thirteen() {
        let defs = tool_defs();
        let names: Vec<&str> = defs.iter().map(|t| t.name).collect();
        assert_eq!(
            names,
            [
                "clone", "sync", "sparse", "status", "log", "diff", "cat", "ls", "commit", "land",
                "snapshot", "restore", "blame"
            ]
        );
        for t in &defs {
            assert!(!t.description.is_empty(), "{} has a description", t.name);
            assert_eq!(t.schema["type"], "object", "{} schema is an object", t.name);
            assert!(
                t.schema["properties"].is_object(),
                "{} schema has properties",
                t.name
            );
        }
    }

    #[test]
    fn read_only_allowlist_is_the_pure_reads() {
        assert_eq!(
            READ_ONLY_TOOLS,
            ["status", "log", "diff", "cat", "ls", "blame"]
        );
        // Every allowlisted name is a real tool.
        let defs = tool_defs();
        for name in READ_ONLY_TOOLS {
            assert!(defs.iter().any(|t| t.name == name), "{name} exists");
        }
    }

    #[test]
    fn initialize_echoes_a_supported_protocol_else_latest() {
        let mut params = json!({ "protocolVersion": "2025-03-26" });
        assert_eq!(initialize_result(&params)["protocolVersion"], "2025-03-26");
        params = json!({ "protocolVersion": "2999-01-01" });
        assert_eq!(
            initialize_result(&params)["protocolVersion"],
            LATEST_PROTOCOL
        );
        params = json!({});
        assert_eq!(
            initialize_result(&params)["protocolVersion"],
            LATEST_PROTOCOL
        );
    }

    #[test]
    fn initialize_result_carries_tools_capability_and_server_info() {
        let result = initialize_result(&json!({}));
        assert_eq!(result["capabilities"]["tools"]["listChanged"], false);
        assert_eq!(result["serverInfo"]["name"], "naatre");
        assert_eq!(result["serverInfo"]["version"], env!("CARGO_PKG_VERSION"));
    }

    #[test]
    fn unknown_method_is_a_protocol_error() {
        let response = handle(
            r#"{"jsonrpc":"2.0","id":7,"method":"resources/list"}"#,
            Path::new("."),
            false,
            &Net::default(),
        )
        .unwrap();
        let v: Value = serde_json::from_str(&response).unwrap();
        assert_eq!(v["id"], 7);
        assert_eq!(v["error"]["code"], -32601);
    }

    #[test]
    fn notification_never_gets_a_response() {
        assert!(
            handle(
                r#"{"jsonrpc":"2.0","method":"notifications/initialized"}"#,
                Path::new("."),
                false,
                &Net::default(),
            )
            .is_none()
        );
    }

    #[test]
    fn unknown_tool_is_invalid_params() {
        let response = handle(
            r#"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}"#,
            Path::new("."),
            false,
            &Net::default(),
        )
        .unwrap();
        let v: Value = serde_json::from_str(&response).unwrap();
        assert_eq!(v["error"]["code"], -32602);
        assert!(v["error"]["message"].as_str().unwrap().contains("nope"));
    }

    #[test]
    fn broken_json_is_a_parse_error_with_null_id() {
        let response = handle("{not json", Path::new("."), false, &Net::default()).unwrap();
        let v: Value = serde_json::from_str(&response).unwrap();
        assert_eq!(v["error"]["code"], -32700);
        assert!(v["id"].is_null());
    }

    #[test]
    fn missing_required_argument_is_invalid_params() {
        let response = handle(
            r#"{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cat"}}"#,
            Path::new("."),
            false,
            &Net::default(),
        )
        .unwrap();
        let v: Value = serde_json::from_str(&response).unwrap();
        assert_eq!(v["error"]["code"], -32602);
        assert!(v["error"]["message"].as_str().unwrap().contains("path"));
    }

    #[test]
    fn mistyped_argument_is_invalid_params() {
        let response = handle(
            r#"{"jsonrpc":"2.0","id":3,"method":"tools/call",
                "params":{"name":"cat","arguments":{"path":42}}}"#,
            Path::new("."),
            false,
            &Net::default(),
        )
        .unwrap();
        let v: Value = serde_json::from_str(&response).unwrap();
        assert_eq!(v["error"]["code"], -32602);
    }

    #[test]
    fn envelope_error_text_carries_the_details() {
        let text = verb_error_text(VerbError::Http(HttpError::Envelope {
            status: 409,
            code: "ref_conflict".into(),
            message: "land conflict".into(),
            details: Some(json!({ "recipe": "re-land with the same key" })),
            retry_after: None,
        }));
        assert!(text.starts_with("ref_conflict: land conflict"));
        assert!(text.contains("re-land with the same key"));
    }

    /// A session working copy bound to one origin, with the token an
    /// injection would be after.
    fn session_wc(server_url: &str) -> (tempfile::TempDir, std::path::PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join(".naatre")).unwrap();
        let wc = Wc::open(dir.path()).unwrap();
        wc.save_config(&crate::wc::Config {
            server_url: server_url.to_string(),
            token: Some("session-secret-token".into()),
        })
        .unwrap();
        let path = dir.path().to_path_buf();
        (dir, path)
    }

    fn clone_call(url: &str, extra: &str) -> String {
        format!(
            r#"{{"jsonrpc":"2.0","id":9,"method":"tools/call",
                "params":{{"name":"clone","arguments":{{{url}{extra}}}}}}}"#
        )
    }

    /// sec-C2 pin: a prompt-injection-shaped url naming a foreign origin is
    /// REFUSED — the tool error names the session's configured server,
    /// never quotes the token, and NO request reaches the foreign origin
    /// (a recording listener stands in for it and must stay silent).
    #[test]
    fn clone_refuses_a_foreign_origin_without_sending_the_session_token() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let foreign = format!("http://127.0.0.1:{}", listener.local_addr().unwrap().port());
        let (_dir, root) = session_wc("http://127.0.0.1:7400");
        let response = handle(
            &clone_call(&format!("\"url\":\"{foreign}/repo\""), ""),
            &root,
            false,
            &Net::default(),
        )
        .unwrap();
        let v: Value = serde_json::from_str(&response).unwrap();
        assert_eq!(v["result"]["isError"], true, "{response}");
        let text = v["result"]["content"][0]["text"].as_str().unwrap();
        assert!(
            text.contains("http://127.0.0.1:7400"),
            "the error names the configured server: {text}"
        );
        assert!(
            !text.contains("session-secret-token"),
            "the error never quotes the token: {text}"
        );
        // The recording origin stays silent: nothing was sent.
        listener
            .set_nonblocking(true)
            .expect("nonblocking recording listener");
        for _ in 0..20 {
            assert!(
                listener.accept().is_err(),
                "a request reached the foreign origin"
            );
            std::thread::sleep(std::time::Duration::from_millis(25));
        }
    }

    /// The explicit-token escape hatch: a foreign-origin clone WITH its own
    /// token argument proceeds, and the recording origin sees exactly that
    /// bearer — never the session's.
    #[test]
    fn clone_foreign_origin_with_explicit_token_sends_only_that_token() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let foreign = format!("http://127.0.0.1:{}", listener.local_addr().unwrap().port());
        let (_dir, root) = session_wc("http://127.0.0.1:7400");
        // The recording origin accepts + reads request heads on its own
        // thread (while handle() blocks on the clone). The client's FIRST
        // request is the bearer-less /version probe; the recorder answers
        // each probe and reports the first request that carries
        // Authorization — the clone call itself.
        let (tx, rx) = std::sync::mpsc::channel::<String>();
        std::thread::spawn(move || {
            use std::io::{Read as _, Write as _};
            listener
                .set_nonblocking(false)
                .expect("blocking recorder listener");
            for _ in 0..10 {
                let (mut stream, _) = match listener.accept() {
                    Ok(s) => s,
                    Err(_) => break,
                };
                stream
                    .set_read_timeout(Some(std::time::Duration::from_secs(10)))
                    .expect("read timeout");
                let mut got = Vec::new();
                let mut buf = [0u8; 4096];
                loop {
                    match stream.read(&mut buf) {
                        Ok(0) => break,
                        Ok(n) => {
                            got.extend_from_slice(&buf[..n]);
                            if got.windows(4).any(|w| w == b"\r\n\r\n") {
                                break;
                            }
                        }
                        Err(_) => break,
                    }
                }
                let head = String::from_utf8_lossy(&got).to_string();
                // ureq emits lowercase header names.
                let authed = head.to_lowercase().contains("authorization:");
                let _ = stream.write_all(
                    b"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                );
                drop(stream);
                if authed {
                    let _ = tx.send(head);
                    return;
                }
            }
            let _ = tx.send(String::new());
        });
        let _response = handle(
            &clone_call(
                &format!("\"url\":\"{foreign}/repo\""),
                ", \"token\":\"explicit-token\"",
            ),
            &root,
            false,
            &Net::default(),
        )
        .unwrap();
        let head = rx
            .recv_timeout(std::time::Duration::from_secs(30))
            .expect("the recorded request head");
        assert!(
            head.to_lowercase()
                .contains("authorization: bearer explicit-token"),
            "the explicit token rides the foreign request: {head}"
        );
        assert!(
            !head.contains("session-secret-token"),
            "the session's token never crosses: {head}"
        );
    }
}
