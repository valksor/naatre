# naatre object formats (v1/v2)

Byte-exact specification of naatre's content-addressed objects. The reference
implementation is `crates/naatre-core`; the golden fixtures under
`crates/naatre-core/tests/golden/` pin this document byte for byte. Regenerate
them with `NAATRE_UPDATE_GOLDEN=1 cargo test -p naatre-core --test golden` and
commit the diff together with any format change.

All integers are little-endian. A `str` is a u64 byte length followed by that
many UTF-8 bytes.

## 1. Object ids

Every object id is

```
id = BLAKE3( "naatre\0" + "v1" + "\0" + kind_label + "\0" + len_le64 + "\0" + payload )
```

- `kind_label` ∈ `blob` | `tree` | `changeset` (ASCII, no terminator).
- `len_le64` is the payload byte length as an unsigned 64-bit little-endian
  integer.
- BLAKE3 is unkeyed, default output length (32 bytes).
- Ids are rendered and parsed as exactly 64 lowercase hex characters
  (`[0-9a-f]{64}`); uppercase, short, or non-hex strings are not ids.

Example: `object_id(blob, "")` =
`9ecab71b0beadc307272926dd80527b866f73767e3e0f7cbd37ea26ffe17b250`
(the input being `"naatre\0v1\0blob\0"` followed by nine zero bytes). This
value is pinned by a unit test; any change to the separator, kind labels, or
length framing breaks it.

A changeset carrying first-class conflict representations (§8, format v2)
hashes under the v2 domain instead — `"naatre\0" + "v2" + "\0" + …`. Only
changesets ever use it: a v1 id is never recomputed under v2, and the two
domains never collide (the version bytes are part of the hashed input).
Blobs, trees, and conflict-free changesets always hash under v1.

## 2. Path grammar

A legal path is relative, `/`-separated, NFC-normalized Unicode. Checks run in
a fixed order and the first failure wins:

1. not empty;
2. not absolute (no leading `/`);
3. no backslash `\`;
4. no NUL byte;
5. no character of Unicode general category Cc (control) or Cf (format)
   anywhere;
6. the path equals its own NFC normalization (`path == nfc(path)`);
7. no component is `.` or `..`;
8. no component longer than 255 UTF-8 bytes;
9. no empty component (no `//`);
10. at most 256 components (tree structures recurse per component, so the
    grammar bounds the depth the same way it bounds the byte length);
11. total length at most 4096 UTF-8 bytes;
12. the first component is not exactly `.naatre` (client metadata namespace)
    or `.git`.

## 3. Author and message character rule

Changeset `author` and `message` reject every character of Unicode general
category Cc or Cf, **except** `'\n'` and `'\t'`, which are allowed. (This
covers bidi and other invisible-character spoofing: U+202E, zero-width
spaces, tag characters, and similar.) Paths have no exception: rule 5 above
rejects all Cc and Cf characters, including `\n` and `\t`.

## 4. Changeset encoding

Canonical bytes, in order:

| Field | Encoding |
|---|---|
| magic | the 17 bytes `naatre-changeset\x01` |
| `repo_id` | u64le |
| parents | u8 count (0, 1, or 2), then that many raw 32-byte parent ids in order |
| `author` | `str` |
| `message` | `str` |
| `timestamp_ns` | u64le |
| entries | u32le count, then entries sorted by path **bytes** strictly ascending |

Each entry:

| Field | Encoding |
|---|---|
| path | `str` (raw bytes of the NFC path) |
| op | u8: Add = 0, Mod = 1, Del = 2 |
| Add/Mod only | 32-byte blob id, u64le size, u8 exec (0 or 1) |

`Del` entries carry no blob, size, or exec bytes. A chmod-only change is a
`Mod` with the same blob id and only `exec` flipped.

A changeset with an empty conflict list (§8) encodes to exactly the v1 bytes
above — the v1 format is frozen, and every pre-M12 decoder parses every v1
object ever written. A changeset carrying conflict representations begins
with the magic `naatre-changeset\x02` instead, appends the §8 conflict
section after the entry list, and hashes under the v2 domain (§1).

### Worked example

`changeset-basic`: repo 1, no parents, author `Ada <ada@example.com>`, message
`first`, timestamp 1000000001 ns, one `Add` of `src/main.rs` (blob
`object_id(blob, "fn main() {}\n")`, size 13, not executable). 141 bytes:

```
00000000  6e 61 61 74 72 65 2d 63  68 61 6e 67 65 73 65 74  magic "naatre-changeset"
00000010  01                                                 magic version byte
00000011  01 00 00 00 00 00 00 00                            repo_id = 1
00000019  00                                                 parent count = 0
0000001a  15 00 00 00 00 00 00 00                            author length = 21
00000022  41 64 61 20 3c 61 64 61  40 65 78 61 6d 70 6c 65  "Ada <example"
00000032  2e 63 6f 6d 3e                                     "com>"
00000037  05 00 00 00 00 00 00 00                            message length = 5
0000003f  66 69 72 73 74                                     "first"
00000044  01 ca 9a 3b 00 00 00 00                            timestamp_ns = 1000000001
0000004c  01 00 00 00                                        entry count = 1
00000050  0b 00 00 00 00 00 00 00                            path length = 11
00000058  73 72 63 2f 6d 61 69 6e  2e 72 73                  "src/main.rs"
00000063  00                                                 op = Add
00000064  23 0e 16 7a 0f 30 24 88  1f a8 13 ed 9d e4 c2 7e   blob id
00000074  00 a9 78 4c a4 e5 61 29  f5 60 ca 03 a8 89 9c a7   blob id (cont.)
00000084  0d 00 00 00 00 00 00 00                            size = 13
0000008c  00                                                 exec = 0
```

Changeset id =
`acfe2618d7f73ad5bb84ca051a18c584242df195f765dec8ad5841b73721a927`
(`object_id(changeset, <the 141 bytes above>)`).

## 5. Tree encoding

Canonical bytes, in order:

| Field | Encoding |
|---|---|
| magic | the 12 bytes `naatre-tree\x01` |
| entries | u32le count, then entries sorted by name **bytes** strictly ascending |

Each entry:

| Field | Encoding |
|---|---|
| name | `str`; a single legal path component (no `/`) |
| kind | u8: File = 0, Dir = 1 |
| object | 32-byte id: blob id for File, child tree id for Dir |
| size | u64le: File content byte length; Dir recursive total of contained files |
| exec | u8; File only, 0 or 1 — a Dir entry always writes 0 |

An empty tree is legal (count 0).

### Worked example

`tree-basic`: two entries — File `main.rs` (the blob from the changeset
example above, size 13, not executable) and Dir `src` (the id of the empty
tree, size 0). 126 bytes:

```
00000000  6e 61 61 74 72 65 2d 74  72 65 65 01               magic "naatre-tree\x01"
0000000c  02 00 00 00                                        entry count = 2
00000010  07 00 00 00 00 00 00 00                            name length = 7
00000018  6d 61 69 6e 2e 72 73                               "main.rs"
0000001f  00                                                 kind = File
00000020  23 0e 16 7a 0f 30 24 88  1f a8 13 ed 9d e4 c2 7e   blob id
00000030  00 a9 78 4c a4 e5 61 29  f5 60 ca 03 a8 89 9c a7   blob id (cont.)
00000040  0d 00 00 00 00 00 00 00                            size = 13
00000048  00                                                 exec = 0
00000049  03 00 00 00 00 00 00 00                            name length = 3
00000051  73 72 63                                           "src"
00000054  01                                                 kind = Dir
00000055  7a a9 c3 fe 92 82 11 e8  31 f8 77 79 12 e8 93 9a   child tree id
00000065  19 d4 ce 2f e6 ef 10 d6  46 ed bb 61 43 50 b4 8c   child tree id (cont.)
00000075  00 00 00 00 00 00 00 00                            size = 0
0000007d  00                                                 exec = 0 (always for Dir)
```

The empty tree referenced by the Dir entry has canonical bytes
`6e 61 61 74 72 65 2d 74 72 65 65 01 00 00 00 00` (magic + count 0) and id
`7aa9c3fe928211e831f8777912e8939a19d4ce2fe6ef10d646edbb614350b48c`.

## 6. Size caps

- The canonical encoding of one object (changeset or tree) is at most
  **16 MiB** (16 × 1024 × 1024 bytes); encoding anything larger fails.
- The decoder rejects a length field that exceeds the 16 MiB cap, and any
  length or entry-count claim that cannot fit in the remaining input.
- Decoders reject trailing bytes: the input must end exactly when the object
  does.
- At most 2 parents per changeset and 100 000 entries per changeset or tree
  (enforced by validation).

## 7. Versioning

The `v1` in the id domain separator is part of the format contract. Format
v2 (M12) is a strictly additive extension for exactly one payload kind:
changesets carrying first-class conflict representations (§8) hash under
the `naatre\0v2\0` domain and encode with the `naatre-changeset\x02` magic.
`v1` ids keep their meaning forever, are never recomputed under v2, and
every conflict-free changeset still encodes to its exact v1 bytes.

**Schema guard.** The guard is store-level and fires AT OPEN: format v2
stamps every store this binary creates or touches at `schema_version` 12
(migration 012 — an EMPTY version-bump marker; conflict representations
need no schema change, because v2 rows live in the existing tables and the
`lands.state` column is a free-form string). A pre-M12 binary (max 11)
refuses the whole store at open — its migrator's `current > max` check —
before reading any row, exactly as it already refuses a store stamped by a
still-newer binary. **The lockout is one-way and store-wide:** after this
binary has opened a data dir, an older binary can never open it again,
even if the store holds only v1 data; rollback after the first upgrade
means restoring a pre-upgrade backup. The per-frame v2 magic is
defense-in-depth for bytes that get past that guard (a row copied
store-to-store without its version): a v1-era decoder handed such a frame
errors at the magic check — the final byte differs, and the decoder's
trailing-byte and length strictness make any partial reading impossible —
never a misparse. Ids cannot be confused either: the version bytes are
hash input, so a v2 id never equals a v1 id of the same bytes.

**Mixed-fleet skew.** A v2-ignorant binary and a v2-aware binary cannot
share a data dir: the old binary refuses the store at open (the guard
above), and `naatre restore` refuses a v12 backup under a v11 binary by
the same comparison (`staged_version > MAX_SUPPORTED`). The client-side
case: a v2-ignorant client against a v2-aware server sees a conflicted
node as opaque/version-skewed — it refuses rather than guessing at
content it cannot interpret, and points at `GET /version` (which reports
the protocol version clients warn on skew about). It never receives v2
content reinterpreted under v1. The client-side conflicted-node refusals
(sync, `status`) land with the M12 client slice.

## 8. Conflict representations (format v2)

A changeset can carry logical conflicts inside its payload (jj-style: the
conflict is data, not marker text in a blob). The conflict section is
appended after the v1 entry list, under the `\x02` magic:

| Field | Encoding |
|---|---|
| conflicts | u32le count, then conflict reps sorted by path **bytes** strictly ascending |

Each conflict rep:

| Field | Encoding |
|---|---|
| path | `str` (a legal path, the conflict's location) |
| base tag | u8: 0 = no common base (add/add), 1 = base present |
| base | 32-byte blob id, present only when the tag is 1 |
| sides | u32le count (>= 2), then that many 32-byte blob ids in ascending byte order |

A side is a competing version's blob id — content-addressed, the bytes live
in the CAS. The base is the common ancestor version where one exists. A
deletion side has no blob id and cannot be represented: a conflict with
fewer than two content sides is not recordable (the server refuses it
instead of silently resolving). Validation rejects a base that equals any
side (that side did not change — not a conflict).

The node's `root_tree_id` is then the markers-materialized tree: each
conflicted path holds the deterministic rendering of its rep:

```
<<<<<<< naatre side 1/<N>: <side-1 id>
<side-1 content>
||||||| naatre base: <base id>          (only when a base exists)
<base content>
======= naatre side 2/<N>: <side-2 id>
<side-2 content>
…                                       (one divider per further side)
>>>>>>> naatre conflict: <path>
```

Every content block ends in a newline (one is appended to non-empty content
that lacks it), so dividers always start at line beginnings. The rendering
is a pure function of the rep plus the referenced contents: the recording
server and any later client derive byte-identical markers.

Rebasing a conflicted changeset carries its representations: paths the
rebase did not re-conflict keep their reps verbatim; a re-conflicted path
takes the new sides (the conflict composes — the previous materialization
is itself a side of the new conflict). A changeset with reps may record an
empty delta (the parent already held the same markers). The v2 magic
appears exactly when the conflict list is non-empty — never under a v1
changeset.

The golden fixtures `changeset-conflict-v2` and
`changeset-conflict-v2-three-sides` pin this section byte for byte,
including their v2-domain ids; the v1 fixtures are unchanged.
