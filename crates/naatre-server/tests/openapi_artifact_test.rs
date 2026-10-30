//! The checked OpenAPI artifact (`docs/openapi.json`): CI regenerates it
//! through the same [`ApiDoc::document_json`] render the served route uses
//! and diffs against the committed file (plan "API conventions":
//! "CI regenerates and diffs against the committed file").
//!
//! Default mode compares the committed file against a fresh render and
//! fails with the update command on any drift. `NAATRE_UPDATE_OPENAPI=1`
//! rewrites the file instead (the golden-fixture pattern) — CI runs that
//! mode and then `git diff --exit-code docs/openapi.json`.

use std::path::PathBuf;

use naatre_server::api_code::ApiCode;
use naatre_server::openapi::ApiDoc;

/// `docs/openapi.json`, resolved from the crate manifest.
fn artifact_path() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../docs/openapi.json")
}

#[test]
fn artifact_matches_the_rendered_contract() {
    // Pretty-printed so the CI diff stays reviewable.
    let rendered =
        serde_json::to_string_pretty(&ApiDoc::document_json()).expect("the document serializes");
    let path = artifact_path();
    if std::env::var("NAATRE_UPDATE_OPENAPI").is_ok() {
        std::fs::write(&path, rendered).expect("write docs/openapi.json");
        return;
    }
    let committed = std::fs::read_to_string(&path).unwrap_or_else(|e| {
        panic!(
            "cannot read {}: {e}; generate it with \
             `NAATRE_UPDATE_OPENAPI=1 cargo test -p naatre-server --test openapi_artifact`",
            path.display()
        )
    });
    assert!(
        committed.trim_end() == rendered,
        "docs/openapi.json is stale; regenerate with \
         `NAATRE_UPDATE_OPENAPI=1 cargo test -p naatre-server --test openapi_artifact` \
         and commit the diff"
    );
}

#[test]
fn document_publishes_the_closed_error_code_enum() {
    let doc = ApiDoc::document_json();
    let enum_values: Vec<&str> = doc["components"]["schemas"]["ErrorCode"]["enum"]
        .as_array()
        .expect("ErrorCode enum in components")
        .iter()
        .map(|v| v.as_str().expect("enum values are strings"))
        .collect();
    let expected: Vec<&str> = ApiCode::ALL.iter().map(|c| c.as_str()).collect();
    assert_eq!(enum_values, expected, "published enum must be the registry");

    let envelope = &doc["components"]["schemas"]["ErrorEnvelope"]["properties"]["error"];
    for key in ["code", "message", "retryable", "details", "request_id"] {
        assert!(
            envelope["properties"].get(key).is_some(),
            "envelope property {key} missing"
        );
        assert!(
            envelope["required"]
                .as_array()
                .expect("envelope required list")
                .iter()
                .any(|r| r.as_str() == Some(key)),
            "envelope must require {key}"
        );
    }
}

#[test]
fn document_publishes_the_max_limit_and_success_shapes() {
    let doc = ApiDoc::document_json().to_string();
    // Pagination: every paginated list documents the 1–200 `limit` bound.
    let occurrences = doc.matches("1–200").count();
    assert!(
        occurrences >= 3,
        "expected the 1–200 limit bound on repos/audit/log params, found {occurrences}"
    );
    // Success shapes: creates return 201.
    assert!(doc.contains(r#""201""#), "creates must document 201");
}

#[test]
fn every_router_route_appears_in_the_document() {
    // Name-level router↔document parity: every path literal in the Router
    // construction (`src/lib.rs`) must be a path in the published document —
    // the contract cannot silently drop a served route. (The reverse
    // direction — document paths that no longer exist — is covered by the
    // regenerate-and-diff check above.)
    let src = include_str!("../src/lib.rs");
    let routes = router_route_literals(src);
    assert!(
        routes.len() >= 20,
        "router extraction found only {} route literals — the extractor is broken",
        routes.len()
    );
    let doc = ApiDoc::document_json();
    let paths = doc["paths"]
        .as_object()
        .expect("the document has a paths object");
    for route in routes {
        assert!(
            paths.contains_key(&route),
            "router route {route:?} is missing from the OpenAPI document paths"
        );
    }
}

/// Every `"`-delimited literal starting with `/` in `lib.rs`: within that
/// file, exactly the Router construction carries path literals (handler
/// format strings live under `routes/`, not here).
fn router_route_literals(src: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = src;
    while let Some(start) = rest.find('"') {
        rest = &rest[start + 1..];
        let Some(end) = rest.find('"') else { break };
        let lit = &rest[..end];
        rest = &rest[end + 1..];
        if lit.starts_with('/') {
            out.push(lit.to_string());
        }
    }
    out
}
