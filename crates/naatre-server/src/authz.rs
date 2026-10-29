//! Authorization primitives: the segment-aligned path-scope matcher.
//!
//! A token's `scope_path` restricts it to a subtree of a repo. `None` means
//! the whole repo; `Some(p)` covers `p` itself and everything under `p/` —
//! always segment-aligned, so `a/b` never matches `a/bc`.
//!
//! M1b has no route that carries a path, so no route enforces the matcher
//! yet; enforcement lands with changesets/land (M2a/M4). The matcher is
//! pure and unit-tested here now.

/// True when `scope` allows access to `path`.
///
/// - `None` allows every path (unscoped token).
/// - `Some(p)` allows `path == p` or `path` under `p/` — a child path must
///   begin with `p` followed by a full `/` separator.
pub fn scope_allows(scope: Option<&str>, path: &str) -> bool {
    match scope {
        None => true,
        Some(prefix) => {
            path == prefix
                || path
                    .strip_prefix(prefix)
                    .is_some_and(|rest| rest.starts_with('/'))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::scope_allows;

    #[test]
    fn none_allows_everything() {
        assert!(scope_allows(None, ""));
        assert!(scope_allows(None, "a"));
        assert!(scope_allows(None, "a/b/c"));
        assert!(scope_allows(None, "zzz/deep/path.txt"));
    }

    #[test]
    fn exact_match_allows() {
        assert!(scope_allows(Some("a"), "a"));
        assert!(scope_allows(Some("a/b"), "a/b"));
        assert!(scope_allows(Some("a/b/c.txt"), "a/b/c.txt"));
    }

    #[test]
    fn descendants_allow() {
        assert!(scope_allows(Some("a"), "a/b"));
        assert!(scope_allows(Some("a/b"), "a/b/c"));
        assert!(scope_allows(Some("a/b"), "a/b/c/d.txt"));
    }

    #[test]
    fn segment_boundary_is_respected() {
        // The pinned pair: a/b never matches a/bc.
        assert!(!scope_allows(Some("a/b"), "a/bc"));
        assert!(!scope_allows(Some("a"), "ab"));
        // Pure prefix gating: "a/b/cd/..x" IS under a/b segment-wise (traversal
        // rejection is the path grammar's job, not the matcher's).
        assert!(scope_allows(Some("a/b"), "a/b/cd/..x"));
        // Partial-segment prefixes in the other direction: a path can never
        // be under a longer scope.
        assert!(!scope_allows(Some("a/bc"), "a/b"));
    }

    #[test]
    fn unrelated_and_parent_paths_reject() {
        assert!(!scope_allows(Some("a"), "b"));
        assert!(!scope_allows(Some("a/b"), "a"));
        assert!(!scope_allows(Some("x/y"), "a/b/c"));
    }

    #[test]
    fn empty_scope_and_path_edges() {
        // An empty scope is not a valid stored value (token create rejects
        // it); the matcher treats it as matching only the empty path.
        assert!(scope_allows(Some(""), ""));
        assert!(!scope_allows(Some(""), "a"));
        // A stored scope never carries a trailing slash (token create
        // rejects it), so a trailing-slash scope matches nothing here.
        assert!(!scope_allows(Some("a/b/"), "a/b"));
    }
}
