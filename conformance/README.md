# Conformance fixtures

Fixtures are language-neutral JSON and are normative only for the versioned
profile named in each file. Every implementation must consume these files
directly and publish machine-readable results that bind the fixture digest,
implementation version, platform, and claimed profile.

Generated fixture changes must be reproducible byte-for-byte. Tests read the
checked-in files rather than duplicating their expected values in Go source.

The `core.scalar.c14n-1` vectors are verified by both the Go reference codec
and the dependency-free JavaScript implementation in `independent/scalars.mjs`.
Run the independent check with the CI-pinned Node 24.21.0 toolchain:

```sh
node conformance/independent/scalars.mjs
```

The `core.value-1` vectors cover schema-directed maps, lists, input objects,
one-of activation, enum compatibility, and recursive value limits.
