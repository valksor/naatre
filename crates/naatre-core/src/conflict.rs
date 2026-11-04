//! First-class conflict representations (M12, format v2): the logical
//! conflict a changeset can carry inside its payload, its validation, the
//! rebasing carry rule, and the deterministic marker materialization.
//!
//! A representation is content-addressed: sides and base are blob ids (each
//! with its exec bit), the competing versions' content lives in the CAS. The representation is
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
    /// The base's exec bit (`false` when there is no base).
    pub base_exec: bool,
    /// The competing blob versions: distinct `(blob id, exec)` pairs in
    /// strictly ascending order, at least 2 (at least 1 when `deleted` is
    /// set). The same blob may appear twice with different exec bits.
    pub sides: Vec<Id>,
    /// The exec bit of each blob side, parallel to `sides`.
    pub sides_exec: Vec<bool>,
    /// One side of the conflict deleted the path. That side has no blob id.
    /// It always renders last.
    pub deleted: bool,
}

/// The longest prefix of a blob that always holds the whole first line of a
/// marker file: the opener `<<<<<<< naatre side 1/N: <hex>` with the
/// optional ` +x` exec token is at most 22 + 6 + 2 + 64 + 3 = 97 bytes
/// (N is capped at `MAX_ENTRIES`, 6 digits) plus the newline, so 128 bytes
/// always hold line 1.
pub const MARKER_PREFIX_BYTES: usize = 128;

/// True when any exec bit of the rep (base or a blob side) is set.
pub(crate) fn has_exec(rep: &ConflictRep) -> bool {
    rep.base_exec || rep.sides_exec.iter().any(|&e| e)
}

/// The conflict format a set of reps needs a client to understand: 0 for no
/// reps, 1 for content sides only (M12), 2 when any rep has a deletion
/// side or an exec bit (F2).
pub fn conflict_format(reps: &[ConflictRep]) -> u32 {
    if reps.is_empty() {
        0
    } else if reps.iter().any(|r| r.deleted || has_exec(r)) {
        2
    } else {
        1
    }
}

/// Validates a conflict list: strictly ascending paths without duplicates,
/// grammar-valid paths, per rep at least two distinct strictly ascending
/// `(blob, exec)` sides (one when a deletion side is set) with the base
/// distinct from every side by `(blob, exec)`, and the same entry caps
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
        let keys: Vec<(&[u8; 32], bool)> = rep
            .sides
            .iter()
            .zip(&rep.sides_exec)
            .map(|(id, &e)| (&id.0, e))
            .collect();
        if rep.sides.len() < if rep.deleted { 1 } else { 2 }
            || rep.sides_exec.len() != rep.sides.len()
            || (rep.base.is_none() && rep.base_exec)
            || keys.windows(2).any(|w| w[0] >= w[1])
            || rep
                .base
                .is_some_and(|b| keys.contains(&(&b.0, rep.base_exec)))
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
/// `>>>>>>>` closer naming the path. A side (or the base) whose exec bit is
/// set shows ` +x` after its blob id. A deletion side renders last, as the
/// divider `======= naatre side N/N: deleted` with an empty block (N counts
/// it; the base section sits between side 1 and that divider when side 1 is
/// the only blob side). Every content block ends in a newline so the
/// dividers start at line beginnings.
pub fn marker_bytes(
    rep: &ConflictRep,
    mut fetch: impl FnMut(&Id) -> Result<Vec<u8>, String>,
) -> Result<Vec<u8>, String> {
    let n = rep.sides.len() + usize::from(rep.deleted);
    let x = |exec: bool| if exec { EXEC_TOKEN } else { "" };
    let mut out = Vec::new();
    for (i, side) in rep.sides.iter().enumerate() {
        let sx = x(rep.sides_exec.get(i).copied().unwrap_or(false));
        if i == 0 {
            out.extend_from_slice(
                format!("<<<<<<< naatre side 1/{n}: {}{sx}\n", side.to_hex()).as_bytes(),
            );
        } else {
            if i == 1
                && let Some(base) = rep.base
            {
                out.extend_from_slice(
                    format!(
                        "||||||| naatre base: {}{}\n",
                        base.to_hex(),
                        x(rep.base_exec)
                    )
                    .as_bytes(),
                );
                push_block(&mut out, fetch(&base)?);
            }
            out.extend_from_slice(
                format!("======= naatre side {}/{n}: {}{sx}\n", i + 1, side.to_hex()).as_bytes(),
            );
        }
        push_block(&mut out, fetch(side)?);
    }
    if rep.deleted {
        if rep.sides.len() == 1
            && let Some(base) = rep.base
        {
            out.extend_from_slice(
                format!(
                    "||||||| naatre base: {}{}\n",
                    base.to_hex(),
                    x(rep.base_exec)
                )
                .as_bytes(),
            );
            push_block(&mut out, fetch(&base)?);
        }
        out.extend_from_slice(format!("======= naatre side {n}/{n}: deleted\n").as_bytes());
    }
    out.extend_from_slice(format!(">>>>>>> naatre conflict: {}\n", rep.path).as_bytes());
    Ok(out)
}

/// The token a marker line appends after a blob id when that side's (or the
/// base's) exec bit is set.
const EXEC_TOKEN: &str = " +x";

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
/// `======= naatre side K/N: <64 lowercase hex>` in order (the LAST side
/// N alone may instead read `======= naatre side N/N: deleted`, a deletion
/// side with no blob id; the closer must follow it directly, and the
/// opener never accepts `deleted`). Each 64-hex blob id (opener, dividers,
/// base line) may carry the exec token ` +x` right after it, and nowhere
/// else (the optional
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
    if !is_side_ref(id) {
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
    let mut deleted_seen = false;
    for line in middle {
        // The deletion divider is the last side; nothing sits between it
        // and the closer.
        if deleted_seen {
            return false;
        }
        if let Some((k, n_line, id)) = parse_side_divider(line) {
            if k != expected_side || n_line != n {
                return false;
            }
            if id == b"deleted" {
                if k != n {
                    return false;
                }
                deleted_seen = true;
            } else if !is_side_ref(id) {
                return false;
            }
            expected_side += 1;
        } else if let Some(id) = line.strip_prefix("||||||| naatre base: ") {
            // Exactly one base line, in side 1's block: the renderer
            // writes it after side 1's content, ahead of the base's own
            // content block.
            if base_line_seen || expected_side != 2 || !is_side_ref(id.as_bytes()) {
                return false;
            }
            base_line_seen = true;
        }
    }
    expected_side == n + 1
}

/// Cheap first-line screen for [`marker_shaped`]: true when `prefix` (the
/// first [`MARKER_PREFIX_BYTES`] bytes of a blob, or more) starts with a
/// complete opener line `<<<<<<< naatre side 1/N: <64 lowercase hex>`
/// (optionally followed by ` +x`). It
/// never returns false for a blob [`marker_shaped`] accepts; a hit only
/// means the full blob must be loaded and parsed. A prefix with no newline
/// has no complete first line and returns false.
pub fn marker_opener(prefix: &[u8]) -> bool {
    let Some(end) = prefix.iter().position(|&b| b == b'\n') else {
        return false;
    };
    // `str::lines` (what `marker_shaped` splits with) drops a `\r` before
    // the newline, so the screen does too.
    let line = &prefix[..end];
    let line = line.strip_suffix(b"\r").unwrap_or(line);
    let Ok(line) = std::str::from_utf8(line) else {
        return false;
    };
    parse_opener(line).is_some_and(|(_, id)| is_side_ref(id))
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
    let n = parse_canonical_decimal(n)?;
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
        parse_canonical_decimal(k)?,
        parse_canonical_decimal(n)?,
        id.as_bytes(),
    ))
}

/// A canonical decimal count: ASCII digits only, no sign, no leading zero,
/// at most 6 digits (the width of `MAX_ENTRIES`). `usize::from_str` also
/// takes `+2` and `0002`, which would let a padded opener outgrow the
/// [`MARKER_PREFIX_BYTES`] screen the renderer's own output always fits; the
/// renderer writes plain decimal, so nothing it produces is narrowed.
fn parse_canonical_decimal(s: &str) -> Option<usize> {
    let b = s.as_bytes();
    if b.is_empty() || b.len() > 6 || !b.iter().all(u8::is_ascii_digit) {
        return None;
    }
    if b.len() > 1 && b[0] == b'0' {
        return None;
    }
    s.parse().ok()
}

/// A blob reference on a marker line: 64 lowercase hex characters, then
/// optionally the exec token ` +x`.
fn is_side_ref(field: &[u8]) -> bool {
    let id = field.strip_suffix(EXEC_TOKEN.as_bytes()).unwrap_or(field);
    is_blob_hex(id)
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
            base_exec: false,
            sides_exec: vec![false; sides.len()],
            sides,
            deleted: false,
        }
    }

    fn del_rep(base: Option<Id>, sides: Vec<Id>) -> ConflictRep {
        ConflictRep {
            deleted: true,
            ..rep(base, sides)
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
            base_exec: false,
            sides: vec![side(1), side(2)],
            sides_exec: vec![false; 2],
            deleted: false,
        };
        let replaced = ConflictRep {
            path: "b.txt".into(),
            base: None,
            base_exec: false,
            sides: vec![side(3), side(4)],
            sides_exec: vec![false; 2],
            deleted: false,
        };
        let new_b = ConflictRep {
            path: "b.txt".into(),
            base: None,
            base_exec: false,
            sides: vec![side(5), side(6)],
            sides_exec: vec![false; 2],
            deleted: false,
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
        // A deletion side needs only one blob side; zero blob sides still
        // refuse, and the strict ordering/base rules still apply.
        assert_eq!(validate(&[del_rep(Some(side(0)), vec![side(1)])]), Ok(()));
        assert_eq!(
            validate(&[del_rep(None, vec![])]),
            Err(CoreError::EncodeConflictSides)
        );
        assert_eq!(
            validate(&[del_rep(Some(side(1)), vec![side(1)])]),
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

    /// The deletion rendering: the deleted side is the last divider, the
    /// base sits between side 1 and it, and the block after it is empty.
    #[test]
    fn marker_bytes_deletion_side_renders_last() {
        let r = del_rep(Some(side(0)), vec![side(1)]);
        let bytes = marker_bytes(&r, fetch_of).expect("markers");
        let s = |t: u8| side(t).to_hex();
        let expected = format!(
            "<<<<<<< naatre side 1/2: {}\n{}\n||||||| naatre base: {}\n{}\n\
             ======= naatre side 2/2: deleted\n>>>>>>> naatre conflict: a.txt\n",
            s(1),
            s(1),
            s(0),
            s(0),
        );
        assert_eq!(bytes, expected.as_bytes());
        // Two blob sides plus the deletion: the deleted divider is 3/3.
        let three = del_rep(None, vec![side(1), side(2)]);
        let text = String::from_utf8(marker_bytes(&three, fetch_of).unwrap()).unwrap();
        assert!(text.starts_with("<<<<<<< naatre side 1/3: "));
        assert!(text.contains("======= naatre side 2/3: "));
        assert!(
            text.ends_with("======= naatre side 3/3: deleted\n>>>>>>> naatre conflict: a.txt\n")
        );
    }

    /// `deleted` is accepted only as the last side's divider, directly
    /// before the closer.
    #[test]
    fn marker_shaped_accepts_deleted_only_as_the_last_side() {
        let s1 = side(1).to_hex();
        let s2 = side(2).to_hex();
        let ok = marker_bytes(&del_rep(Some(side(0)), vec![side(1)]), fetch_of).unwrap();
        assert!(marker_shaped(&ok));
        let ok3 = marker_bytes(&del_rep(None, vec![side(1), side(2)]), fetch_of).unwrap();
        assert!(marker_shaped(&ok3));
        // `deleted` on the opener.
        let on_opener = "<<<<<<< naatre side 1/2: deleted\n======= naatre side 2/2: deleted\n\
                         >>>>>>> naatre conflict: a.txt\n";
        assert!(!marker_shaped(on_opener.as_bytes()));
        // `deleted` on a middle side (K < N).
        let middle = format!(
            "<<<<<<< naatre side 1/3: {s1}\n======= naatre side 2/3: deleted\n\
             ======= naatre side 3/3: {s2}\n>>>>>>> naatre conflict: a.txt\n"
        );
        assert!(!marker_shaped(middle.as_bytes()));
        // A content line between the `deleted` divider and the closer.
        let content = format!(
            "<<<<<<< naatre side 1/2: {s1}\n{s1}\n======= naatre side 2/2: deleted\n\
             stray\n>>>>>>> naatre conflict: a.txt\n"
        );
        assert!(!marker_shaped(content.as_bytes()));
        // A base line after the `deleted` divider.
        let base_after = format!(
            "<<<<<<< naatre side 1/2: {s1}\n======= naatre side 2/2: deleted\n\
             ||||||| naatre base: {s2}\n>>>>>>> naatre conflict: a.txt\n"
        );
        assert!(!marker_shaped(base_after.as_bytes()));
        // A misspelled token.
        let wrong = format!(
            "<<<<<<< naatre side 1/2: {s1}\n======= naatre side 2/2: Deleted\n\
             >>>>>>> naatre conflict: a.txt\n"
        );
        assert!(!marker_shaped(wrong.as_bytes()));
    }

    /// The opener screen: true for an opener line with its newline inside
    /// the prefix; false without a newline, for a line longer than the
    /// prefix, and for prose quoting the opener on a later line.
    #[test]
    fn marker_opener_cases() {
        let r = rep(None, vec![side(1), side(2)]);
        let bytes = marker_bytes(&r, fetch_of).unwrap();
        let cut = bytes.len().min(MARKER_PREFIX_BYTES);
        assert!(marker_opener(&bytes[..cut]));
        // No newline inside the prefix: no complete first line.
        let first_line_end = bytes.iter().position(|&b| b == b'\n').unwrap();
        assert!(!marker_opener(&bytes[..first_line_end]));
        // A first line longer than the prefix.
        let long = vec![b'x'; MARKER_PREFIX_BYTES + 1];
        assert!(!marker_opener(&long));
        // The opener quoted on line 2.
        let mut prose = b"see the format:\n".to_vec();
        prose.extend_from_slice(&bytes);
        assert!(!marker_opener(
            &prose[..prose.len().min(MARKER_PREFIX_BYTES)]
        ));
        // A CRLF first line is a marker opener to `lines()`, so also here.
        let crlf = format!("<<<<<<< naatre side 1/2: {}\r\nx\r\n", side(1).to_hex());
        assert!(marker_opener(crlf.as_bytes()));
        // A short id is not an opener.
        assert!(!marker_opener(b"<<<<<<< naatre side 1/2: aa\n"));
    }

    #[test]
    fn conflict_format_by_content() {
        assert_eq!(conflict_format(&[]), 0);
        assert_eq!(conflict_format(&[rep(None, vec![side(1), side(2)])]), 1);
        assert_eq!(
            conflict_format(&[
                rep(None, vec![side(1), side(2)]),
                del_rep(None, vec![side(1)])
            ]),
            2
        );
    }

    /// The side counts are canonical decimals: a `+` sign and leading zeros
    /// are refused by the full parse AND the prefix screen, which agree. A
    /// padded opener longer than the prefix can no longer be accepted by
    /// `marker_shaped` while `marker_opener` misses it.
    #[test]
    fn marker_grammar_accepts_only_canonical_decimal_counts() {
        let id = side(1).to_hex();
        let id2 = side(2).to_hex();
        let render = |opener_n: &str, divider_n: &str, divider_k: &str| {
            format!(
                "<<<<<<< naatre side 1/{opener_n}: {id}\nx\n\
                 ======= naatre side {divider_k}/{divider_n}: {id2}\ny\n\
                 >>>>>>> naatre conflict: a.txt\n"
            )
        };
        let agree = |text: &str| {
            let bytes = text.as_bytes();
            let cut = bytes.len().min(MARKER_PREFIX_BYTES);
            (marker_shaped(bytes), marker_opener(&bytes[..cut]))
        };
        // The canonical form is shaped (and the screen agrees).
        assert_eq!(agree(&render("2", "2", "2")), (true, true));
        // A `+2` side count on the opener: both refuse. On a divider only
        // the full parse sees it (the screen reads line 1 alone, and a
        // screen hit never contradicts the full parse's refusal).
        assert_eq!(agree(&render("+2", "2", "2")), (false, false));
        assert_eq!(agree(&render("2", "+2", "2")), (false, true));
        assert_eq!(agree(&render("2", "2", "+2")), (false, true));
        // Leading zeros, short and padded past the 128-byte prefix.
        assert_eq!(agree(&render("02", "02", "02")), (false, false));
        let padded = format!("{}2", "0".repeat(120));
        let text = render(&padded, &padded, "2");
        assert!(text.lines().next().unwrap().len() > MARKER_PREFIX_BYTES);
        assert_eq!(agree(&text), (false, false));
        // Seven digits: over the 6-digit width.
        assert_eq!(agree(&render("0000002", "2", "2")), (false, false));
    }

    /// A rep with explicit exec bits (`base_exec`, then one per side).
    fn exec_rep(base: Option<(Id, bool)>, sides: Vec<(Id, bool)>) -> ConflictRep {
        ConflictRep {
            path: "a.txt".into(),
            base: base.map(|(id, _)| id),
            base_exec: base.is_some_and(|(_, e)| e),
            sides: sides.iter().map(|(id, _)| *id).collect(),
            sides_exec: sides.iter().map(|(_, e)| *e).collect(),
            deleted: false,
        }
    }

    /// The exec token: ` +x` after the blob id of an executable side (the
    /// opener, a divider, the base line), nothing for a plain side.
    #[test]
    fn marker_bytes_shows_exec_bits() {
        let r = exec_rep(
            Some((side(0), true)),
            vec![(side(1), true), (side(2), false)],
        );
        let bytes = marker_bytes(&r, fetch_of).expect("markers");
        let s = |t: u8| side(t).to_hex();
        let expected = format!(
            "<<<<<<< naatre side 1/2: {} +x\n{}\n||||||| naatre base: {} +x\n{}\n\
             ======= naatre side 2/2: {}\n{}\n>>>>>>> naatre conflict: a.txt\n",
            s(1),
            s(1),
            s(0),
            s(0),
            s(2),
            s(2),
        );
        assert_eq!(bytes, expected.as_bytes());
        assert!(marker_shaped(&bytes));
        // The same blob twice, differing only in the exec bit.
        let same = exec_rep(None, vec![(side(1), false), (side(1), true)]);
        let text = String::from_utf8(marker_bytes(&same, fetch_of).unwrap()).unwrap();
        assert!(text.contains(&format!("======= naatre side 2/2: {} +x\n", s(1))));
        assert!(marker_shaped(text.as_bytes()));
        // A deletion side next to an executable survivor.
        let mut del = exec_rep(Some((side(0), false)), vec![(side(1), true)]);
        del.deleted = true;
        let bytes = marker_bytes(&del, fetch_of).unwrap();
        assert!(marker_shaped(&bytes));
        assert!(
            String::from_utf8(bytes)
                .unwrap()
                .starts_with(&format!("<<<<<<< naatre side 1/2: {} +x\n", s(1)))
        );
    }

    /// The token is accepted only right after a blob id on the opener, a
    /// divider or the base line: never doubled, never mis-cased, never glued
    /// on, never after `deleted`. The prefix screen
    /// agrees with the full parse on the opener.
    #[test]
    fn marker_shaped_accepts_the_exec_token_only_after_a_blob_id() {
        let (a, b) = (side(1).to_hex(), side(2).to_hex());
        let render = |opener: &str, base: &str, divider: &str| {
            format!(
                "<<<<<<< naatre side 1/2: {a}{opener}\nx\n||||||| naatre base: {b}{base}\nz\n\
                 ======= naatre side 2/2: {b}{divider}\ny\n>>>>>>> naatre conflict: p\n"
            )
        };
        assert!(marker_shaped(render("", "", "").as_bytes()));
        assert!(marker_shaped(render(" +x", " +x", " +x").as_bytes()));
        for bad in [" +x +x", " +X", "+x", " +x ", " x", " +xx", "  +x"] {
            for (o, bs, d) in [(bad, "", ""), ("", bad, ""), ("", "", bad)] {
                let text = render(o, bs, d);
                assert!(!marker_shaped(text.as_bytes()), "accepted {bad:?}: {text}");
                let cut = text.len().min(MARKER_PREFIX_BYTES);
                if !o.is_empty() {
                    assert!(!marker_opener(&text.as_bytes()[..cut]), "screen {bad:?}");
                }
            }
        }
        // The token after `deleted`, and on the closer.
        let del = format!(
            "<<<<<<< naatre side 1/2: {a}\nx\n======= naatre side 2/2: deleted +x\n\
             >>>>>>> naatre conflict: p\n"
        );
        assert!(!marker_shaped(del.as_bytes()));
    }

    /// The longest possible opener (N at the 6-digit cap, exec token set)
    /// fits the 128-byte prefix with its newline.
    #[test]
    fn the_longest_opener_fits_the_prefix_screen() {
        let line = format!(
            "<<<<<<< naatre side 1/{}: {} +x\n",
            crate::changeset::MAX_ENTRIES,
            side(1).to_hex()
        );
        assert_eq!(line.len(), 98);
        assert!(line.len() <= MARKER_PREFIX_BYTES);
        assert!(marker_opener(line.as_bytes()));
    }

    /// Validation by `(blob, exec)`: the same blob may be both sides when
    /// the exec bits differ (ascending by exec), the base may share a blob
    /// with a side when its exec differs, and the parallel exec list must
    /// match.
    #[test]
    fn validate_by_blob_and_exec() {
        let ok = exec_rep(
            Some((side(1), true)),
            vec![(side(1), false), (side(2), true)],
        );
        assert_eq!(validate(&[ok]), Ok(()));
        let twin = exec_rep(None, vec![(side(1), false), (side(1), true)]);
        assert_eq!(validate(&[twin]), Ok(()));
        // The same (blob, exec) twice, and the wrong order of the pair.
        for bad in [
            vec![(side(1), true), (side(1), true)],
            vec![(side(1), true), (side(1), false)],
        ] {
            assert_eq!(
                validate(&[exec_rep(None, bad)]),
                Err(CoreError::EncodeConflictSides)
            );
        }
        // The base equal to a side by (blob, exec).
        assert_eq!(
            validate(&[exec_rep(
                Some((side(1), true)),
                vec![(side(1), true), (side(2), false)]
            )]),
            Err(CoreError::EncodeConflictSides)
        );
        // A deletion side whose survivor differs from the base only by exec.
        let mut del = exec_rep(Some((side(1), false)), vec![(side(1), true)]);
        del.deleted = true;
        assert_eq!(validate(&[del.clone()]), Ok(()));
        del.base_exec = true;
        assert_eq!(validate(&[del]), Err(CoreError::EncodeConflictSides));
        // The exec list must be parallel; a base exec needs a base.
        let mut bad = exec_rep(None, vec![(side(1), false), (side(2), false)]);
        bad.sides_exec.pop();
        assert_eq!(validate(&[bad]), Err(CoreError::EncodeConflictSides));
        let mut bad = exec_rep(None, vec![(side(1), false), (side(2), false)]);
        bad.base_exec = true;
        assert_eq!(validate(&[bad]), Err(CoreError::EncodeConflictSides));
    }

    #[test]
    fn conflict_format_counts_exec_bits() {
        let plain = rep(None, vec![side(1), side(2)]);
        assert_eq!(conflict_format(std::slice::from_ref(&plain)), 1);
        let exec = exec_rep(None, vec![(side(1), false), (side(1), true)]);
        assert_eq!(conflict_format(&[plain.clone(), exec]), 2);
        let base_only = exec_rep(
            Some((side(0), true)),
            vec![(side(1), false), (side(2), false)],
        );
        assert_eq!(conflict_format(&[base_only]), 2);
    }
}
