# naatre object formats (v1)

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

The `v1` in the id domain separator is part of the format contract. Future
format changes take a new version (`v2`) with its own domain separator; `v1`
ids keep their meaning forever and are never reused under a different
interpretation. Data written under `v1` stays readable as `v1`.
