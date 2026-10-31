//! Tree derivation: path-copy over in-memory nodes.
//!
//! A changeset's `root_tree_id` is derived eagerly at create: load the
//! parent's tree into [`MemNode`], apply the file changes, canonical-encode
//! every new directory, and hand the rows to [`crate::db::trees_write`].
//! Duality violations (add under a file, mod of a directory, del of an
//! absent path) surface here as typed [`ApplyError`]s and map to 422
//! `duality` at the route.
//!
//! All SQL lives in [`crate::db`]; this module is the algorithm plus pure
//! canonical encoding.

use std::collections::BTreeMap;

use rusqlite::Connection;

use naatre_core::changeset::{FileChange, Op};
use naatre_core::error::CoreError;
use naatre_core::ids::Id;
use naatre_core::tree::{EntryKind, Tree, TreeEntry, tree_id};

use crate::db::{self, TreeEntryRow, TreeWrite};

/// Everything tree derivation can fail with.
#[derive(Debug, thiserror::Error)]
pub enum DeriveError {
    /// A store failure while loading or (via the caller) writing rows.
    #[error(transparent)]
    Sql(#[from] rusqlite::Error),
    /// Canonical encoding failed — in practice only the 16 MiB cap.
    #[error(transparent)]
    Core(#[from] CoreError),
    /// A tree id is CLAIMED (a changeset's root, or another tree's entry)
    /// but its `trees` row is missing — store corruption, never a
    /// legitimate empty tree. Surfaces as an internal error at the routes;
    /// it is never silently loaded as empty.
    #[error("corrupt tree store: no trees row for claimed id {0}")]
    MissingTreeRow(String),
}

/// An in-memory directory node.
///
/// Entries are keyed by name in a `BTreeMap<String, _>`: `String`'s `Ord`
/// compares the underlying UTF-8 bytes, which is exactly the canonical
/// format's "strictly ascending name byte order" — so iterating the map
/// yields entries already in canonical order (pinned by the
/// `multibyte_names_iterate_in_byte_order` test below).
#[derive(Debug, Default, PartialEq, Eq)]
pub struct MemNode {
    pub entries: BTreeMap<String, MemEntry>,
}

/// One entry of a [`MemNode`].
#[derive(Debug, PartialEq, Eq)]
pub enum MemEntry {
    File { blob: Id, size: u64, exec: bool },
    Dir(Box<MemNode>),
}

/// A duality violation while applying changes against the parent tree.
///
/// Every variant maps to 422 `duality`: the path collides with a file or
/// directory of the parent tree in a way a file-level delta cannot express.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ApplyError {
    /// An intermediate component of the path is an existing file, so the
    /// change needs a directory where the parent tree has a file.
    FileInPath { path: String },
    /// `mod` target is not an existing file (absent, or a directory).
    ModNotFile { path: String },
    /// `del` target is not an existing file (absent, or a directory).
    DelNotFile { path: String },
    /// The path has more than [`naatre_core::path::MAX_PATH_COMPONENTS`]
    /// components. Unreachable for validated input (the grammar rejects
    /// deeper paths at the route and on import) — the check here is tree
    /// derivation's own enforcement of the same cap.
    TooDeep { path: String },
}

impl MemNode {
    /// An empty tree (the root node of a parentless changeset).
    pub fn empty() -> MemNode {
        MemNode::default()
    }

    /// Recursively loads a stored tree into memory. `None` root = the
    /// legitimate empty tree (a parentless changeset); a CLAIMED id whose
    /// rows are missing is corruption — [`DeriveError::MissingTreeRow`].
    pub fn load(conn: &Connection, root_tree_id: Option<Id>) -> Result<MemNode, DeriveError> {
        let Some(root_tree_id) = root_tree_id else {
            return Ok(MemNode::empty());
        };
        load_node(conn, &root_tree_id)
    }

    /// Applies a changeset's file changes in order. Changes must be sorted
    /// (the route rejects unsorted requests before deriving).
    pub fn apply(&mut self, changes: &[FileChange]) -> Result<(), ApplyError> {
        for c in changes {
            // The depth cap (grammar rule, enforced here too): every walk
            // below — insert, remove, and the drop of the built chain —
            // recurses per component.
            if c.path.split('/').count() > naatre_core::path::MAX_PATH_COMPONENTS {
                return Err(ApplyError::TooDeep {
                    path: c.path.clone(),
                });
            }
            match c.op {
                Op::Add | Op::Mod => {
                    let (blob, size, exec) = match (c.blob, c.size, c.exec) {
                        (Some(blob), Some(size), Some(exec)) => (blob, size, exec),
                        // The route's shape validation rejects this earlier.
                        _ => {
                            return Err(ApplyError::ModNotFile {
                                path: c.path.clone(),
                            });
                        }
                    };
                    if c.op == Op::Mod
                        && !matches!(self.lookup(&c.path), Some(MemEntry::File { .. }))
                    {
                        return Err(ApplyError::ModNotFile {
                            path: c.path.clone(),
                        });
                    }
                    self.insert_file(&c.path, blob, size, exec)?;
                }
                Op::Del => {
                    if !matches!(self.lookup(&c.path), Some(MemEntry::File { .. })) {
                        return Err(ApplyError::DelNotFile {
                            path: c.path.clone(),
                        });
                    }
                    self.remove_file(&c.path);
                }
            }
        }
        Ok(())
    }

    /// Resolves a full path to its entry, `None` when any component is
    /// missing (an intermediate file also misses — it has no children).
    fn lookup(&self, path: &str) -> Option<&MemEntry> {
        let (dirs, leaf) = split_leaf(path);
        let mut node = self;
        for name in &dirs {
            match node.entries.get(*name)? {
                MemEntry::Dir(child) => node = child,
                MemEntry::File { .. } => return None,
            }
        }
        node.entries.get(leaf)
    }

    /// Inserts a file, creating intermediate dirs; errors when an
    /// intermediate component is an existing file.
    fn insert_file(
        &mut self,
        path: &str,
        blob: Id,
        size: u64,
        exec: bool,
    ) -> Result<(), ApplyError> {
        let split = split_leaf(path);
        insert_in(self, &split, path, blob, size, exec)
    }

    /// Removes a file (existence checked by the caller) and prunes every
    /// now-empty ancestor directory (both happen inside [`remove_in`]).
    fn remove_file(&mut self, path: &str) {
        let split = split_leaf(path);
        remove_in(self, &split);
    }

    /// Canonically encodes every directory of this subtree and returns the
    /// writes in child-first order plus the root's id and recursive size.
    /// Pure: no SQL — rows go through [`db::trees_write`]. Iterative (an
    /// explicit frame stack, children encoded before parents): the
    /// recursion this replaced overflows the stack on deep trees.
    pub fn collect(&self) -> Result<(Id, u64, Vec<TreeWrite>), DeriveError> {
        let mut writes = Vec::new();
        // One frame per directory on the current path: its name in the
        // parent, the not-yet-encoded entries, and the entries/total
        // accumulated so far.
        struct Frame<'a> {
            name_in_parent: String,
            remaining: Vec<(&'a String, &'a MemEntry)>,
            entries: Vec<TreeEntry>,
            total: u64,
        }
        enum Action<'a> {
            File(String, Id, u64, bool),
            Dir(String, &'a MemNode),
            Done,
        }
        let (root_id, root_total) = {
            // Frames drain `remaining` from the back (pop), so each frame's
            // list is stored reversed: entries encode in BTreeMap order.
            let mut stack: Vec<Frame<'_>> = vec![Frame {
                name_in_parent: String::new(),
                remaining: {
                    let mut v: Vec<_> = self.entries.iter().collect();
                    v.reverse();
                    v
                },
                entries: Vec::new(),
                total: 0,
            }];
            loop {
                let action = {
                    let frame = stack
                        .last_mut()
                        .expect("the root frame only leaves the stack when finished");
                    match frame.remaining.pop() {
                        Some((name, MemEntry::File { blob, size, exec })) => {
                            Action::File(name.clone(), *blob, *size, *exec)
                        }
                        Some((name, MemEntry::Dir(child))) => {
                            Action::Dir(name.clone(), child.as_ref())
                        }
                        None => Action::Done,
                    }
                };
                match action {
                    Action::File(name, blob, size, exec) => {
                        let frame = stack.last_mut().expect("frame just checked");
                        frame.total += size;
                        frame.entries.push(TreeEntry {
                            name,
                            kind: EntryKind::File,
                            object: blob,
                            size,
                            exec,
                        });
                    }
                    Action::Dir(name, child) => stack.push(Frame {
                        name_in_parent: name,
                        remaining: {
                            let mut v: Vec<_> = child.entries.iter().collect();
                            v.reverse();
                            v
                        },
                        entries: Vec::new(),
                        total: 0,
                    }),
                    Action::Done => {
                        let frame = stack.pop().expect("frame just checked");
                        let tree = Tree {
                            entries: frame.entries,
                        };
                        let canonical = naatre_core::tree::encode(&tree)?;
                        let id = tree_id(&tree)?;
                        writes.push(TreeWrite {
                            id,
                            canonical,
                            entries: tree
                                .entries
                                .iter()
                                .map(|e| TreeEntryRow {
                                    name: e.name.clone(),
                                    kind: match e.kind {
                                        EntryKind::File => 0,
                                        EntryKind::Dir => 1,
                                    },
                                    object_id: e.object.0.to_vec(),
                                    size: e.size as i64,
                                    exec: e.exec,
                                })
                                .collect(),
                        });
                        match stack.last_mut() {
                            Some(parent) => {
                                parent.total += frame.total;
                                parent.entries.push(TreeEntry {
                                    name: frame.name_in_parent,
                                    kind: EntryKind::Dir,
                                    object: id,
                                    size: frame.total,
                                    exec: false,
                                });
                            }
                            None => break (id, frame.total),
                        }
                    }
                }
            }
        };
        Ok((root_id, root_total, writes))
    }
}

/// Splits a path into its intermediate components and leaf name. The
/// returned references point into `path`.
fn split_leaf(path: &str) -> (Vec<&str>, &str) {
    let components: Vec<&str> = path.split('/').collect();
    let (dirs, leaf) = components.split_at(components.len() - 1);
    (dirs.to_vec(), leaf[0])
}

/// Recursive insert: creates intermediate dirs, errors on a file in the
/// middle of the path (`full` is the requested path, for the error).
fn insert_in(
    node: &mut MemNode,
    path: &(Vec<&str>, &str),
    full: &str,
    blob: Id,
    size: u64,
    exec: bool,
) -> Result<(), ApplyError> {
    let (dirs, leaf) = path;
    let mut node = node;
    for name in dirs {
        let entry = node
            .entries
            .entry((*name).to_string())
            .or_insert_with(|| MemEntry::Dir(Box::default()));
        match entry {
            MemEntry::Dir(child) => node = child,
            MemEntry::File { .. } => {
                return Err(ApplyError::FileInPath {
                    path: full.to_string(),
                });
            }
        }
    }
    node.entries
        .insert(leaf.to_string(), MemEntry::File { blob, size, exec });
    Ok(())
}

/// Recursive remove; prunes empty ancestor dirs on the way back up.
/// Returns `true` when the leaf was removed. Deliberately recursive (the
/// bottom-up prune reads naturally here) and bounded: `apply` rejects paths
/// deeper than [`naatre_core::path::MAX_PATH_COMPONENTS`], so the recursion
/// can never outgrow the stack.
fn remove_in(node: &mut MemNode, path: &(Vec<&str>, &str)) -> bool {
    let (dirs, leaf) = path;
    if dirs.is_empty() {
        return node.entries.remove(*leaf).is_some();
    }
    let Some(MemEntry::Dir(child)) = node.entries.get_mut(dirs[0]) else {
        return false;
    };
    let removed = remove_in(child, &(dirs[1..].to_vec(), *leaf));
    if removed && child.entries.is_empty() {
        node.entries.remove(dirs[0]);
    }
    removed
}

/// Loads one stored tree into memory. Iterative, in two passes (the
/// recursion this replaced overflowed the stack on trees deeper than the
/// grammar now admits — data written before the depth cap): discovery
/// loads every directory's rows exactly once (content-addressed ids
/// memoize), then the build assembles bottom-up with an explicit stack —
/// a directory's children are always built before it. Subtrees shared
/// under several parents rebuild per occurrence, as the recursion did.
/// A claimed id without a `trees` row (root or sub-tree) is corruption —
/// an empty entry set would silently truncate history, so it errors.
fn load_node(conn: &Connection, tree_id: &Id) -> Result<MemNode, DeriveError> {
    // Discovery: iterative DFS over dir entries; `loaded` memoizes by id.
    let mut loaded: std::collections::HashMap<Id, Vec<TreeEntryRow>> =
        std::collections::HashMap::new();
    let mut discover = vec![*tree_id];
    while let Some(id) = discover.pop() {
        if loaded.contains_key(&id) {
            continue;
        }
        if db::tree_repo(conn, &id)?.is_none() {
            return Err(DeriveError::MissingTreeRow(id.to_hex()));
        }
        let rows = db::tree_entries_load(conn, &id)?;
        for row in &rows {
            if row.kind == 1 {
                discover.push(Id(row
                    .object_id
                    .clone()
                    .try_into()
                    .expect("object id is 32 bytes")));
            }
        }
        loaded.insert(id, rows);
    }
    // Build: one frame per (occurrence of a) directory on the current
    // path; children complete before their parent's entry is appended.
    struct Frame {
        name_in_parent: String,
        remaining: std::vec::IntoIter<TreeEntryRow>,
        entries: std::collections::BTreeMap<String, MemEntry>,
    }
    enum Action {
        Build(Id, String),
        Row(TreeEntryRow),
        Done,
    }
    let mut stack: Vec<Frame> = vec![Frame {
        name_in_parent: String::new(),
        remaining: loaded
            .get(tree_id)
            .expect("the root is discovered first")
            .clone()
            .into_iter(),
        entries: std::collections::BTreeMap::new(),
    }];
    loop {
        let action = {
            let frame = stack
                .last_mut()
                .expect("the root frame only leaves the stack when finished");
            match frame.remaining.next() {
                Some(row) if row.kind == 1 => {
                    let id = Id(row
                        .object_id
                        .clone()
                        .try_into()
                        .expect("object id is 32 bytes"));
                    Action::Build(id, row.name.clone())
                }
                Some(row) => Action::Row(row),
                None => Action::Done,
            }
        };
        match action {
            Action::Build(id, name) => {
                stack.push(Frame {
                    name_in_parent: name,
                    remaining: loaded
                        .get(&id)
                        .expect("discovery saw every dir")
                        .clone()
                        .into_iter(),
                    entries: std::collections::BTreeMap::new(),
                });
            }
            Action::Row(row) => {
                let frame = stack.last_mut().expect("frame just checked");
                let object = Id(row.object_id.try_into().expect("object id is 32 bytes"));
                let entry = match row.kind {
                    0 => MemEntry::File {
                        blob: object,
                        size: row.size as u64,
                        exec: row.exec,
                    },
                    // Unreachable: dir rows became Action::Build above.
                    1 => unreachable!("dir rows build as their own frame"),
                    other => {
                        // A corrupt derived row is store corruption, not
                        // client input: surface as a SQL-layer failure.
                        // `other` names the stored kind byte.
                        return Err(DeriveError::Sql(rusqlite::Error::InvalidColumnType(
                            1,
                            format!("kind ({other})"),
                            rusqlite::types::Type::Integer,
                        )));
                    }
                };
                frame.entries.insert(row.name, entry);
            }
            Action::Done => {
                let frame = stack.pop().expect("frame just checked");
                let node = MemNode {
                    entries: frame.entries,
                };
                match stack.last_mut() {
                    Some(parent) => {
                        parent
                            .entries
                            .insert(frame.name_in_parent, MemEntry::Dir(Box::new(node)));
                    }
                    None => return Ok(node),
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use naatre_core::changeset::FileChange;

    fn id(n: u8) -> Id {
        let mut b = [0u8; 32];
        b[0] = n;
        Id(b)
    }

    fn add(path: &str, blob: u8, size: u64, exec: bool) -> FileChange {
        FileChange {
            path: path.into(),
            op: Op::Add,
            blob: Some(id(blob)),
            size: Some(size),
            exec: Some(exec),
        }
    }

    fn del(path: &str) -> FileChange {
        FileChange {
            path: path.into(),
            op: Op::Del,
            blob: None,
            size: None,
            exec: None,
        }
    }

    fn mod_(path: &str, blob: u8, size: u64, exec: bool) -> FileChange {
        FileChange {
            path: path.into(),
            op: Op::Mod,
            blob: Some(id(blob)),
            size: Some(size),
            exec: Some(exec),
        }
    }

    /// Applying a chain of changes and canonically encoding at each step
    /// must always produce a tree whose `BTreeMap` iteration equals the
    /// canonical byte order — proven with multi-byte names (É = C3 89,
    /// ÿ = C3 BF, c = 63, z = 7A: byte order is c, z, É, ÿ — NOT Unicode
    /// scalar order and NOT `String`'s char-wise order... which is the same
    /// thing as byte order for UTF-8; the assert pins it).
    #[test]
    fn multibyte_names_iterate_in_byte_order() {
        let mut node = MemNode::empty();
        node.apply(&[
            add("z", 1, 1, false),
            add("É", 2, 2, false),
            add("ÿ", 3, 3, false),
            add("c", 4, 4, false),
        ])
        .unwrap();
        let names: Vec<&str> = node.entries.keys().map(String::as_str).collect();
        assert_eq!(names, vec!["c", "z", "É", "ÿ"]);
        // And the canonical encoding accepts that order (strictly ascending
        // name bytes — a mismatch would fail encode with EncodeUnsorted).
        let (_, _, writes) = node.collect().unwrap();
        assert_eq!(writes.len(), 1);
        assert_eq!(writes[0].entries.len(), 4);
    }

    #[test]
    fn add_mod_del_and_dir_pruning() {
        let mut node = MemNode::empty();
        node.apply(&[add("a/b.txt", 1, 10, false), add("a/c/d.txt", 2, 20, true)])
            .unwrap();

        let (_, _, writes) = node.collect().unwrap();
        // Three nodes, child-first: a/c, a, root. Root size = 30, a = 30,
        // c = 20.
        assert_eq!(writes.len(), 3);
        let root = writes.last().unwrap();
        assert_eq!(root.entries.len(), 1);
        let a_entry = &root.entries[0];
        assert_eq!(a_entry.name, "a");
        assert_eq!(a_entry.kind, 1);
        assert_eq!(a_entry.size, 30);
        let a_node = &writes
            .iter()
            .find(|w| a_entry.object_id.as_slice() == w.id.0)
            .unwrap();
        let c_entry = a_node.entries.iter().find(|e| e.name == "c").unwrap();
        assert_eq!(c_entry.kind, 1);
        assert_eq!(c_entry.size, 20);

        // Mod: size change propagates up.
        node.apply(&[mod_("a/b.txt", 3, 15, false)]).unwrap();
        let (_, total, _) = node.collect().unwrap();
        assert_eq!(total, 35);

        // Exec-only Mod changes the tree (exec is in the canonical bytes).
        let (before, _, _) = node.collect().unwrap();
        node.apply(&[mod_("a/b.txt", 3, 15, true)]).unwrap();
        let (after, _, _) = node.collect().unwrap();
        assert_ne!(before, after);

        // Del: leaf and now-empty `a/c` prune away; `a` keeps b.txt.
        node.apply(&[del("a/c/d.txt")]).unwrap();
        let (_, total, _) = node.collect().unwrap();
        assert_eq!(total, 15);
        assert!(node.lookup("a/c").is_none());
        assert!(node.lookup("a/b.txt").is_some());
    }

    #[test]
    fn duality_errors_are_typed() {
        let mut node = MemNode::empty();
        node.apply(&[add("file.txt", 1, 1, false), add("d/x.txt", 2, 1, false)])
            .unwrap();
        // add under a file
        assert_eq!(
            node.apply(&[add("file.txt/under", 3, 1, false)]),
            Err(ApplyError::FileInPath {
                path: "file.txt/under".into()
            })
        );
        // deep: "a" file blocks "a/b/c"
        node.apply(&[add("a", 4, 1, false)]).unwrap();
        assert_eq!(
            node.apply(&[add("a/b/c", 5, 1, false)]),
            Err(ApplyError::FileInPath {
                path: "a/b/c".into()
            })
        );
        // mod of a directory
        assert_eq!(
            node.apply(&[mod_("d", 6, 1, false)]),
            Err(ApplyError::ModNotFile { path: "d".into() })
        );
        // mod of an absent path
        assert_eq!(
            node.apply(&[mod_("nope", 7, 1, false)]),
            Err(ApplyError::ModNotFile {
                path: "nope".into()
            })
        );
        // mod whose parent chain crosses a file
        assert_eq!(
            node.apply(&[mod_("a/anything", 8, 1, false)]),
            Err(ApplyError::ModNotFile {
                path: "a/anything".into()
            })
        );
        // del of an absent path, and del of a directory
        assert_eq!(
            node.apply(&[del("nope")]),
            Err(ApplyError::DelNotFile {
                path: "nope".into()
            })
        );
        assert_eq!(
            node.apply(&[del("d")]),
            Err(ApplyError::DelNotFile { path: "d".into() })
        );
    }

    /// Round-trip through the store: `trees_write` + `load` reproduce the
    /// in-memory node (this is what the M2a oracle test replays on top of).
    #[test]
    fn persist_and_load_roundtrip() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute_batch(
            "CREATE TABLE trees(id BLOB PRIMARY KEY, repo_id INTEGER NOT NULL, canonical BLOB NOT NULL);
             CREATE TABLE tree_entries(tree_id BLOB NOT NULL, name TEXT NOT NULL, kind INTEGER NOT NULL, \
              object_id BLOB NOT NULL, size INTEGER NOT NULL, exec INTEGER NOT NULL DEFAULT 0, \
              PRIMARY KEY(tree_id, name));
             CREATE TABLE repo_trees(repo_id INTEGER NOT NULL, tree_id BLOB NOT NULL, \
              PRIMARY KEY(repo_id, tree_id));",
        )
        .unwrap();

        let mut node = MemNode::empty();
        node.apply(&[add("a/b.txt", 1, 10, true), add("a/c/d.txt", 2, 20, false)])
            .unwrap();
        let (root, total, writes) = node.collect().unwrap();
        assert_eq!(total, 30);
        db::trees_write(&conn, 7, &writes).unwrap();

        let loaded = MemNode::load(&conn, Some(root)).unwrap();
        assert_eq!(loaded, node);

        // A claimed tree id with no `trees` row is corruption, NOT an empty
        // tree: load errors naming the id (missing rows must never read as
        // a legitimate empty tree).
        let unknown = MemNode::load(&conn, Some(id(200)));
        assert!(
            matches!(&unknown, Err(DeriveError::MissingTreeRow(hex)) if hex == &id(200).to_hex()),
            "unknown tree id must be MissingTreeRow, got {unknown:?}"
        );
        // None root → empty node (the legitimate empty tree).
        assert_eq!(MemNode::load(&conn, None).unwrap(), MemNode::empty());
    }

    /// A stored tree DEEPER than the grammar now admits (data written
    /// before the 256-component cap) still loads and re-encodes without
    /// aborting: `load` and `collect` are iterative, so depth costs heap,
    /// never stack.
    #[test]
    fn deep_legacy_tree_loads_and_collects_without_abort() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute_batch(
            "CREATE TABLE trees(id BLOB PRIMARY KEY, repo_id INTEGER NOT NULL, canonical BLOB NOT NULL);
             CREATE TABLE tree_entries(tree_id BLOB NOT NULL, name TEXT NOT NULL, kind INTEGER NOT NULL, \
              object_id BLOB NOT NULL, size INTEGER NOT NULL, exec INTEGER NOT NULL DEFAULT 0, \
              PRIMARY KEY(tree_id, name));
             CREATE TABLE repo_trees(repo_id INTEGER NOT NULL, tree_id BLOB NOT NULL, \
              PRIMARY KEY(repo_id, tree_id));",
        )
        .unwrap();

        fn level(n: u16) -> Id {
            let mut b = [0u8; 32];
            b[..2].copy_from_slice(&n.to_be_bytes());
            Id(b)
        }
        // A 1500-deep chain: tree_n holds one dir entry "d" pointing at
        // tree_{n-1}; the innermost tree holds one file. (1500 components
        // of one byte each fit the legacy 4096-byte path cap, so this shape
        // was writable by the pre-cap grammar — and it overflowed the
        // recursive load in debug builds.)
        const DEPTH: u16 = 1500;
        conn.execute(
            "INSERT INTO trees(id, repo_id, canonical) VALUES (?1, 7, X'00')",
            rusqlite::params![level(0).0.as_slice()],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO tree_entries(tree_id, name, kind, object_id, size) \
             VALUES (?1, 'f', 0, ?2, 1)",
            rusqlite::params![level(0).0.as_slice(), id(1).0.as_slice()],
        )
        .unwrap();
        for n in 1..=DEPTH {
            conn.execute(
                "INSERT INTO trees(id, repo_id, canonical) VALUES (?1, 7, X'00')",
                rusqlite::params![level(n).0.as_slice()],
            )
            .unwrap();
            conn.execute(
                "INSERT INTO tree_entries(tree_id, name, kind, object_id, size) \
                 VALUES (?1, 'd', 1, ?2, 1)",
                rusqlite::params![level(n).0.as_slice(), level(n - 1).0.as_slice()],
            )
            .unwrap();
        }

        let node = MemNode::load(&conn, Some(level(DEPTH))).unwrap();
        let (_, total, writes) = node.collect().unwrap();
        // The re-encode reproduces the same shape: DEPTH dir nodes + the
        // file's tree, total size 1. (The ids recompute from canonical
        // bytes; the crafted level ids were arbitrary.)
        assert_eq!(writes.len(), DEPTH as usize + 1);
        assert_eq!(total, 1);
    }

    /// apply enforces the grammar's depth cap itself: a 257-component path
    /// is a typed TooDeep error (never a stack walk past the bound).
    #[test]
    fn apply_rejects_paths_past_the_component_cap() {
        let deep = std::iter::repeat_n("d", naatre_core::path::MAX_PATH_COMPONENTS + 1)
            .collect::<Vec<_>>()
            .join("/");
        let mut node = MemNode::empty();
        assert_eq!(
            node.apply(&[add(&deep, 1, 1, false)]),
            Err(ApplyError::TooDeep { path: deep })
        );
    }
}
