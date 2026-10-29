//! Thin HTTP client for the server verbs: `repo create/stats` and
//! `token list/create/revoke`. Reads `NAATRE_URL` (default
//! `http://127.0.0.1:7400`) and `NAATRE_TOKEN`; on failure prints the
//! server's envelope `code: message` and exits non-zero.
//!
//! The agent is built with `http_status_as_error(false)` so non-2xx
//! responses keep their JSON envelope (ureq's default drops the body of
//! error statuses).

use std::env;

use serde_json::{Value, json};

use crate::{RepoCmd, TokenCmd};

const DEFAULT_URL: &str = "http://127.0.0.1:7400";

struct Client {
    agent: ureq::Agent,
    base: String,
    token: Option<String>,
}

impl Client {
    fn from_env() -> Client {
        let agent = ureq::Agent::new_with_config(
            ureq::config::Config::builder()
                .http_status_as_error(false)
                .build(),
        );
        Client {
            agent,
            base: env::var("NAATRE_URL")
                .unwrap_or_else(|_| DEFAULT_URL.to_string())
                .trim_end_matches('/')
                .to_string(),
            token: env::var("NAATRE_TOKEN").ok(),
        }
    }

    /// One JSON request; `Ok` is any response the server answered with
    /// (status included), `Err` is a transport failure.
    fn request(
        &self,
        method: &str,
        path: &str,
        body: Option<&Value>,
    ) -> Result<(u16, Value), String> {
        let url = format!("{}{}", self.base, path);
        let auth = self.token.clone();
        let res = match (method, body) {
            ("GET", _) => with_bearer(self.agent.get(&url), &auth).call(),
            ("DELETE", _) => with_bearer(self.agent.delete(&url), &auth).call(),
            ("POST", Some(v)) => with_bearer(self.agent.post(&url), &auth).send_json(v),
            ("POST", None) => {
                return Err("internal: POST without a body is not a client verb".to_string());
            }
            (other, _) => {
                return Err(format!("internal: unknown client method {other:?}"));
            }
        };
        let res = res.map_err(|e| format!("request to {url} failed: {e}"))?;
        let status = res.status().as_u16();
        let value: Value = res
            .into_body()
            .read_json()
            .map_err(|e| format!("server sent non-JSON response: {e}"))?;
        Ok((status, value))
    }
}

/// Adds the bearer header to a typed request builder (either typestate).
fn with_bearer<B>(rb: ureq::RequestBuilder<B>, token: &Option<String>) -> ureq::RequestBuilder<B> {
    match token {
        Some(t) => rb.header("Authorization", format!("Bearer {t}")),
        None => rb,
    }
}

/// Extracts the envelope `error.code` / `error.message` for failure output.
fn envelope_code_message(value: &Value) -> Option<(String, String)> {
    let code = value["error"]["code"].as_str()?.to_string();
    let message = value["error"]["message"].as_str()?.to_string();
    Some((code, message))
}

fn fail(value: &Value) -> i32 {
    match envelope_code_message(value) {
        Some((code, message)) => eprintln!("error: {code}: {message}"),
        None => eprintln!("error: unexpected response: {value}"),
    }
    1
}

fn transport_fail(message: &str) -> i32 {
    eprintln!("error: {message}");
    1
}

/// `naatre repo create <name>` / `naatre repo stats <name>`.
pub fn repo(cmd: RepoCmd) -> i32 {
    let client = Client::from_env();
    match cmd {
        RepoCmd::Create { name } => {
            let body = json!({ "name": name });
            match client.request("POST", "/v1/repos", Some(&body)) {
                Ok((201, value)) => {
                    println!("created repo {} (id {})", value["name"], value["id"]);
                    0
                }
                Ok((_, value)) => fail(&value),
                Err(e) => transport_fail(&e),
            }
        }
        RepoCmd::Stats { name } => {
            match client.request("GET", &format!("/v1/repos/{name}/stats"), None) {
                Ok((200, value)) => {
                    println!("changesets: {}", value["changesets"]);
                    println!("objects: {}", value["objects"]);
                    println!("bytes: {}", value["bytes"]);
                    println!("cbytes: {}", value["cbytes"]);
                    0
                }
                Ok((_, value)) => fail(&value),
                Err(e) => transport_fail(&e),
            }
        }
    }
}

/// `naatre token list` / `create` / `revoke` (HTTP, admin).
pub fn token(cmd: TokenCmd) -> i32 {
    let client = Client::from_env();
    match cmd {
        TokenCmd::List => match client.request("GET", "/v1/tokens", None) {
            Ok((200, value)) => {
                println!("{:<6} {:<16} {:<8} expires", "id", "principal", "role");
                for t in value["tokens"].as_array().map_or(&[][..], Vec::as_slice) {
                    let expires = t["expires_ns"]
                        .as_u64()
                        .map_or("never".to_string(), |ns| ns.to_string());
                    println!(
                        "{:<6} {:<16} {:<8} {}",
                        t["id"],
                        t["principal"].as_str().unwrap_or("?"),
                        t["role"].as_str().unwrap_or("?"),
                        expires,
                    );
                }
                0
            }
            Ok((_, value)) => fail(&value),
            Err(e) => transport_fail(&e),
        },
        TokenCmd::Create {
            principal,
            role,
            ttl_days,
            scope,
        } => {
            let mut body = json!({ "principal": principal, "role": role });
            if let Some(days) = ttl_days {
                body["ttl_days"] = json!(days);
            }
            if let Some(scope) = scope {
                body["scope_path"] = json!(scope);
            }
            match client.request("POST", "/v1/tokens", Some(&body)) {
                Ok((201, value)) => {
                    // Printed exactly once; the server stores only the hash.
                    println!("token: {}", value["token"].as_str().unwrap_or("?"));
                    0
                }
                Ok((_, value)) => fail(&value),
                Err(e) => transport_fail(&e),
            }
        }
        TokenCmd::Revoke { id } => {
            match client.request("DELETE", &format!("/v1/tokens/{id}"), None) {
                Ok((200, _)) => {
                    println!("revoked {id}");
                    0
                }
                Ok((_, value)) => fail(&value),
                Err(e) => transport_fail(&e),
            }
        }
    }
}
