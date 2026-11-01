//! First-class conflict representations (M12, format v2): the logical
//! conflict a changeset can carry inside its payload, its validation, the
//! rebasing carry rule, and the deterministic marker materialization.
//!
//! A representation is content-addressed: sides and base are blob ids, the
//! competing versions' content lives in the CAS. The representation is
//! enough to materialize markers deterministically — [`marker_bytes`] is a
//! pure function of the representation plus the referenced contents, so the
//! recording server and any later client derive the exact same bytes.

use crate::error::CoreError;
use crate::ids::Id;
use crate::path;

/// One logical conflict at one path, carried inside a changeset payload
/// (jj-style: the conflict is data, not marker text in a blob).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ConflictRep {
    /// Repo-relative path the conflict is at; grammar-valid and NFC.
    pub path: String,
    /// The common ancestor version, where one exists (absent for add/add).
    pub base: Option<Id>,
    /// The competing versions: at least 2 distinct blob ids in strictly
    /// ascending byte order. A deletion side has no blob id and cannot be
    /// represented — such conflicts are not recordable (the land refuses
    /// them instead of silently resolving).
    pub sides: Vec<Id>,
}

/// Validates a conflict list: strictly ascending paths without duplicates,
/// grammar-valid paths, per rep at least two distinct strictly ascending
/// sides with the base distinct from every side, and the same entry caps
/// the file-change list carries (rep list and per-rep side count at
/// [`MAX_ENTRIES`] — parity with what the frame format implies).
pub fn validate(reps: &[ConflictRep]) -> Result<(), CoreError> {
    if reps.len() > crate::changeset::MAX_ENTRIES {
        return Err(CoreError::EncodeTooManyEntries);
    }
    if reps
        .windows(2)
        .any(|w| w[0].path.as_bytes() >= w[1].path.as_bytes())
    {
        return Err(CoreError::EncodeUnsortedConflicts);
    }
    for rep in reps {
        path::validate(&rep.path)?;
        if rep.sides.len() > crate::changeset::MAX_ENTRIES {
            return Err(CoreError::EncodeTooManyEntries);
        }
        if rep.sides.len() < 2
            || rep.sides.windows(2).any(|w| w[0].0 >= w[1].0)
            || rep.base.is_some_and(|b| rep.sides.contains(&b))
        {
            return Err(CoreError::EncodeConflictSides);
        }
    }
    Ok(())
}

/// Rebasing a conflicted changeset carries its representations: paths this
/// rebase did not re-conflict keep their reps verbatim, and a re-conflicted
/// path takes the new sides (the conflict composes — the previous
/// materialization is itself a side of the new conflict). Deterministic:
/// the output is sorted by path, whatever order the inputs arrive in.
pub fn carry(existing: &[ConflictRep], reconflicted: &[ConflictRep]) -> Vec<ConflictRep> {
    let mut by_path: std::collections::BTreeMap<String, ConflictRep> = existing
        .iter()
        .map(|rep| (rep.path.clone(), rep.clone()))
        .collect();
    for rep in reconflicted {
        by_path.insert(rep.path.clone(), rep.clone());
    }
    by_path.into_values().collect()
}

/// Materializes the conflict markers for `rep`: the deterministic byte
/// rendering every reader derives from the representation alone. `fetch`
/// resolves a side's (or the base's) blob content; a fetch failure carries
/// the fetcher's own error.
///
/// Layout: one `<<<<<<<` opener per first side, then each further side
/// behind a `=======` divider carrying that side's blob id, the base (when
/// present) between side 1 and side 2 behind a `|||||||` divider, and a
/// `>>>>>>>` closer naming the path. Every content block ends in a newline
/// so the dividers start at line beginnings.
pub fn marker_bytes(
    rep: &ConflictRep,
    mut fetch: impl FnMut(&Id) -> Result<Vec<u8>, String>,
) -> Result<Vec<u8>, String> {
    let n = rep.sides.len();
    let mut out = Vec::new();
    for (i, side) in rep.sides.iter().enumerate() {
        if i == 0 {
            out.extend_from_slice(
                format!("<<<<<<< naatre side 1/{n}: {}\n", side.to_hex()).as_bytes(),
            );
        } else {
            if i == 1
                && let Some(base) = rep.base
            {
                out.extend_from_slice(
                    format!("||||||| naatre base: {}\n", base.to_hex()).as_bytes(),
                );
                push_block(&mut out, fetch(&base)?);
            }
            out.extend_from_slice(
                format!("======= naatre side {}/{n}: {}\n", i + 1, side.to_hex()).as_bytes(),
            );
        }
        push_block(&mut out, fetch(side)?);
    }
    out.extend_from_slice(format!(">>>>>>> naatre conflict: {}\n", rep.path).as_bytes());
    Ok(out)
}

/// One content block: the raw bytes; a non-empty block always ends in a
/// newline so the next divider starts at a line beginning.
fn push_block(out: &mut Vec<u8>, content: Vec<u8>) {
    out.extend_from_slice(&content);
    if !content.is_empty() && !content.ends_with(b"\n") {
        out.push(b'\n');
    }
}

/// True when `bytes` IS a materialized marker file — the exact shape
/// [`marker_bytes`] renders, matched structurally (the client's marker
/// detector behind `status`'s conflict marks, the `conflict_markers_present`
/// commit gate, and `resolve`'s still-conflicted check; one detector, so
/// the writer and the readers can never drift).
///
/// The whole input must parse as one marker structure: the FIRST line is
/// the opener `<<<<<<< naatre side 1/N: <64 lowercase hex>` (N ≥ 2), each
/// further side K = 2..=N appears as the exact divider line
/// `======= naatre side K/N: <64 lowercase hex>` in order (the optional
/// `||||||| naatre base: <64 lowercase hex>` line appears once, inside
/// side 1's block — the renderer writes it after side 1's content,
/// followed by the base's own content), and the LAST line is the closer
/// `>>>>>>> naatre conflict: <non-empty path>`. Everything between is
/// content. Prose that merely quotes the format (docs, another tool's
/// markers) fails the structure — the opener must be the first line and
/// the closer the last, with every divider exact.
///
/// Structural ambiguity, acknowledged: a side's content line that
/// EXACTLY equals a valid divider form for the expected next side
/// (byte-identical, blob id included) is consumed as that divider —
/// the detection is structural, not content-addressed, so such content
/// shifts the parse (a false-negative on the surrounding shape). The
/// same holds for a truncated closer whose path prefix survives: a
/// byte-level cut inside the closer's path text is indistinguishable
/// from a shorter path. Both are inherent to detecting a rendering
/// whose content blocks are arbitrary bytes; the renderer itself never
/// emits them.
pub fn marker_shaped(bytes: &[u8]) -> bool {
    let Ok(text) = std::str::from_utf8(bytes) else {
        return false;
    };
    let mut lines = text.lines();
    // The opener pins the side count and side 1's blob id.
    let Some(first) = lines.next() else {
        return false;
    };
    let Some((n, id)) = parse_opener(first) else {
        return false;
    };
    if !is_blob_hex(id) {
        return false;
    }
    let mut middle: Vec<&str> = lines.collect();
    // The closer is the last line; a marker file is never just the opener.
    let Some(last) = middle.pop() else {
        return false;
    };
    let Some(path) = last.strip_prefix(">>>>>>> naatre conflict: ") else {
        return false;
    };
    if path.is_empty() {
        return false;
    }
    let mut expected_side = 2usize;
    let mut base_line_seen = false;
    for line in middle {
        if let Some((k, n_line, id)) = parse_side_divider(line) {
            if k != expected_side || n_line != n || !is_blob_hex(id) {
                return false;
            }
            expected_side += 1;
        } else if let Some(id) = line.strip_prefix("||||||| naatre base: ") {
            // Exactly one base line, in side 1's block: the renderer
            // writes it after side 1's content, ahead of the base's own
            // content block.
            if base_line_seen || expected_side != 2 || !is_blob_hex(id.as_bytes()) {
                return false;
            }
            base_line_seen = true;
        }
    }
    expected_side == n + 1
}

/// Parses the opener `<<<<<<< naatre side 1/N: <hex>` into `(N, hex)`.
/// N is capped at [`MAX_ENTRIES`] — the entry cap the frame format
/// itself carries — so a hostile opener (`side 1/18446744073709551615`)
/// parses to None here instead of overflowing the side-count arithmetic
/// downstream (`n + 1` would panic the debug profile; the cap removes
/// the overflow path entirely, both profiles).
fn parse_opener(line: &str) -> Option<(usize, &[u8])> {
    let rest = line.strip_prefix("<<<<<<< naatre side 1/")?;
    let (n, id) = rest.split_once(": ")?;
    let n = n.parse::<usize>().ok()?;
    if !(2..=crate::changeset::MAX_ENTRIES).contains(&n) {
        return None;
    }
    Some((n, id.as_bytes()))
}

/// Parses a divider `======= naatre side K/N: <hex>` into `(K, N, hex)`.
fn parse_side_divider(line: &str) -> Option<(usize, usize, &[u8])> {
    let rest = line.strip_prefix("======= naatre side ")?;
    let (kn, id) = rest.split_once(": ")?;
    let (k, n) = kn.split_once('/')?;
    Some((
        k.parse::<usize>().ok()?,
        n.parse::<usize>().ok()?,
        id.as_bytes(),
    ))
}

/// 64 lowercase hex characters.
fn is_blob_hex(id: &[u8]) -> bool {
    id.len() == 64
        && id
            .iter()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn side(tag: u8) -> Id {
        let mut b = [0u8; 32];
        b[0] = tag;
        Id(b)
    }

    fn rep(base: Option<Id>, sides: Vec<Id>) -> ConflictRep {
        ConflictRep {
            path: "a.txt".into(),
            base,
            sides,
        }
    }

    fn fetch_of(id: &Id) -> Result<Vec<u8>, String> {
        Ok(id.to_hex().into_bytes())
    }

    /// Pins the marker rendering byte-exactly: sides in rep order, the base
    /// section between side 1 and side 2, dividers carry blob ids, the
    /// closer names the path.
    #[test]
    fn marker_bytes_two_sides_with_base() {
        let r = rep(Some(side(0)), vec![side(1), side(2)]);
        let bytes = marker_bytes(&r, fetch_of).expect("markers");
        let s = |t: u8| side(t).to_hex();
        let expected = format!(
            "<<<<<<< naatre side 1/2: {}\n{}\n||||||| naatre base: {}\n{}\n\
             ======= naatre side 2/2: {}\n{}\n>>>>>>> naatre conflict: a.txt\n",
            s(1),
            s(1),
            s(0),
            s(0),
            s(2),
            s(2),
        );
        assert_eq!(bytes, expected.as_bytes());
    }

    /// Three sides, no base: every side behind its own divider, no base
    /// section.
    #[test]
    fn marker_bytes_three_sides_without_base() {
        let r = rep(None, vec![side(1), side(2), side(3)]);
        let bytes = marker_bytes(&r, fetch_of).expect("markers");
        let text = String::from_utf8(bytes).expect("ASCII");
        assert!(text.starts_with("<<<<<<< naatre side 1/3: "));
        assert!(text.contains("======= naatre side 2/3: "));
        assert!(text.contains("======= naatre side 3/3: "));
        assert!(!text.contains("base"));
        assert!(text.ends_with(">>>>>>> naatre conflict: a.txt\n"));
    }

    /// A side whose content lacks a trailing newline cannot glue a divider
    /// onto the previous content line: every divider starts at a line
    /// beginning.
    #[test]
    fn marker_bytes_forces_trailing_newline() {
        let r = rep(None, vec![side(1), side(2)]);
        let bytes = marker_bytes(&r, |id| Ok(vec![b'x', id.0[0]])).expect("markers");
        let text = String::from_utf8(bytes).expect("ASCII");
        assert!(text.starts_with("<<<<<<<"));
        for marker in ["=======", ">>>>>>>"] {
            for (i, _) in text.match_indices(marker) {
                assert_eq!(
                    text.as_bytes()[i - 1],
                    b'\n',
                    "{marker} not at a line beginning"
                );
            }
        }
    }

    #[test]
    fn carry_overrides_reconflicted_paths_and_keeps_the_rest() {
        let kept = ConflictRep {
            path: "a.txt".into(),
            base: Some(side(0)),
            sides: vec![side(1), side(2)],
        };
        let replaced = ConflictRep {
            path: "b.txt".into(),
            base: None,
            sides: vec![side(3), side(4)],
        };
        let new_b = ConflictRep {
            path: "b.txt".into(),
            base: None,
            sides: vec![side(5), side(6)],
        };
        let out = carry(&[kept.clone(), replaced], std::slice::from_ref(&new_b));
        assert_eq!(out, vec![kept, new_b]);
    }

    #[test]
    fn validate_rejects_bad_sides_and_sort() {
        let ok = vec![rep(Some(side(0)), vec![side(1), side(2)])];
        assert_eq!(validate(&ok), Ok(()));
        // One side.
        assert_eq!(
            validate(&[rep(None, vec![side(1)])]),
            Err(CoreError::EncodeConflictSides)
        );
        // Unsorted sides.
        assert_eq!(
            validate(&[rep(None, vec![side(2), side(1)])]),
            Err(CoreError::EncodeConflictSides)
        );
        // Base among the sides.
        assert_eq!(
            validate(&[rep(Some(side(1)), vec![side(1), side(2)])]),
            Err(CoreError::EncodeConflictSides)
        );
        // Unsorted rep list.
        let a = rep(None, vec![side(1), side(2)]);
        let mut z = a.clone();
        z.path = "z.txt".into();
        assert_eq!(validate(&[z, a]), Err(CoreError::EncodeUnsortedConflicts));
    }

    /// Every rendering `marker_bytes` produces is shaped — two sides with
    /// a base, three sides without, empty content blocks.
    #[test]
    fn marker_shaped_accepts_every_rendering() {
        let two = rep(Some(side(0)), vec![side(1), side(2)]);
        assert!(marker_shaped(&marker_bytes(&two, fetch_of).unwrap()));
        let three = rep(None, vec![side(1), side(2), side(3)]);
        assert!(marker_shaped(&marker_bytes(&three, fetch_of).unwrap()));
        // Empty side content: dividers stack directly.
        let bytes = marker_bytes(&rep(None, vec![side(1), side(2)]), |_| Ok(Vec::new())).unwrap();
        assert!(marker_shaped(&bytes));
    }

    /// Content that is not a rendered marker file is not shaped: ordinary
    /// prose, prose quoting the marker format mid-file, git-style
    /// markers, truncated renderings, a stray opener.
    #[test]
    fn marker_shaped_rejects_non_markers() {
        assert!(!marker_shaped(b"hello\nworld\n"));
        assert!(!marker_shaped(
            b"see the format:\n<<<<<<< naatre side 1/2: aa\n=======\n"
        ));
        assert!(!marker_shaped(b"<<<<<<< mine\ncontent\n>>>>>>> mine\n"));
        // A rendering with the closer line cut off (whole-line truncation
        // is detectable; a byte-level cut inside the closer's path text
        // is structurally indistinguishable from a shorter path and is
        // not — detection is structural by design).
        let r = rep(Some(side(0)), vec![side(1), side(2)]);
        let full = marker_bytes(&r, fetch_of).unwrap();
        let text = String::from_utf8(full).unwrap();
        let no_closer = text
            .lines()
            .filter(|l| !l.starts_with(">>>>>>> "))
            .collect::<Vec<_>>()
            .join("\n");
        assert!(!marker_shaped(no_closer.as_bytes()));
        // An opener-only file.
        let opener = format!("<<<<<<< naatre side 1/2: {}\n", side(1).to_hex());
        assert!(!marker_shaped(opener.as_bytes()));
        // Divider side count disagreement (opener says 2, two dividers).
        let bogus = format!(
            "<<<<<<< naatre side 1/2: {s1}\n======= naatre side 2/2: {s2}\n\
             ======= naatre side 3/2: {s3}\n>>>>>>> naatre conflict: a.txt\n",
            s1 = side(1).to_hex(),
            s2 = side(2).to_hex(),
            s3 = side(3).to_hex(),
        );
        assert!(!marker_shaped(bogus.as_bytes()));
        // A second base line.
        let twobase = format!(
            "<<<<<<< naatre side 1/2: {s1}\n||||||| naatre base: {s0}\n\
             ||||||| naatre base: {s0}\n======= naatre side 2/2: {s2}\n\
             >>>>>>> naatre conflict: a.txt\n",
            s0 = side(0).to_hex(),
            s1 = side(1).to_hex(),
            s2 = side(2).to_hex(),
        );
        assert!(!marker_shaped(twobase.as_bytes()));
        // Non-UTF-8 bytes.
        assert!(!marker_shaped(&[0xff, 0xfe, 0xfd]));
        // A hostile side count at the usize boundary (security A1): the
        // cap rejects it before any arithmetic on N — this MUST run in a
        // debug build to mean anything (the plain `cargo test` profile
        // is debug; release would hide the pre-fix overflow panic), and
        // post-fix it cannot panic at all.
        let hostile = format!(
            "<<<<<<< naatre side 1/18446744073709551615: {s1}\n\
             ======= naatre side 2/18446744073709551615: {s2}\n\
             >>>>>>> naatre conflict: a.txt\n",
            s1 = side(1).to_hex(),
            s2 = side(2).to_hex(),
        );
        assert!(!marker_shaped(hostile.as_bytes()));
    }
}
