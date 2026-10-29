//! Acceptance and rejection tables for the path grammar.

use naatre_core::path::{self, PathError};

fn owned(v: &[&str]) -> Vec<String> {
    v.iter().map(|s| (*s).to_string()).collect()
}

#[test]
fn accepts_valid_paths() {
    for p in [
        "a",
        "a/b/c",
        "src/main.rs",
        "a/b",
        "a/bc",
        "café/x",
        "深い/dir/file",
    ] {
        assert_eq!(path::validate(p), Ok(()), "valid path rejected: {p:?}");
    }
}

#[test]
fn rejects_invalid_paths() {
    let cases: &[(&str, PathError)] = &[
        ("", PathError::Empty),
        ("/abs", PathError::Absolute),
        ("a/./b", PathError::ComponentInvalid { index: 1 }),
        ("a/../b", PathError::ComponentInvalid { index: 1 }),
        ("..", PathError::ComponentInvalid { index: 0 }),
        ("a//b", PathError::EmptyComponent { index: 1 }),
        ("a\\b", PathError::Backslash),
        (".naatre/config", PathError::Reserved { prefix: ".naatre" }),
        (".git/HEAD", PathError::Reserved { prefix: ".git" }),
        ("a/\u{0}b", PathError::NulByte),
        ("a/b\u{7}", PathError::ControlChar { index: 3 }),
        ("a/\u{200b}b", PathError::ControlChar { index: 2 }),
        ("cafe\u{301}/x", PathError::NotNormalized),
    ];
    for (p, expected) in cases {
        assert_eq!(
            path::validate(p),
            Err(expected.clone()),
            "invalid path accepted: {p:?}"
        );
    }
}

#[test]
fn rejects_overlong_paths() {
    // 17 components x 250 bytes + 16 separators = 4266 bytes: every component
    // is legal, so this reaches the whole-path cap.
    let long = std::iter::repeat_n("x".repeat(250), 17)
        .collect::<Vec<_>>()
        .join("/");
    assert_eq!(long.len(), 4266);
    assert_eq!(path::validate(&long), Err(PathError::TooLong { len: 4266 }));

    let comp = "a".repeat(256);
    assert_eq!(
        path::validate(&comp),
        Err(PathError::ComponentTooLong { index: 0, len: 256 })
    );
}

#[test]
fn case_fold_duplicates() {
    assert!(path::check_case_fold_duplicates(&owned(&["README.md", "readme.md"])).is_err());
    assert!(path::check_case_fold_duplicates(&owned(&["a/cafes", "a/cafés"])).is_ok());
    assert!(path::check_case_fold_duplicates(&owned(&["x/CAFÉ", "x/café"])).is_err());
    // Same folded name in different directories is fine: comparison is
    // per-directory only.
    assert!(path::check_case_fold_duplicates(&owned(&["d1/Readme", "d2/Readme"])).is_ok());

    match path::check_case_fold_duplicates(&owned(&["README.md", "readme.md"])) {
        Err(PathError::CaseFoldDuplicate { a, b }) => {
            assert_eq!(a, "README.md");
            assert_eq!(b, "readme.md");
        }
        other => panic!("expected CaseFoldDuplicate, got {other:?}"),
    }
}
