# Contributing

Use Go 1.27, keep package dependencies in the direction documented in the
README, and accompany behavior changes with tests and conformance fixtures.
Normative changes need stable clause identifiers, a decision record, and a
migration note when they change the v1 contract.

Before opening a change, run:

```sh
go fmt ./...
go vet ./...
go test ./...
go test -race -count=3 ./...
golangci-lint run
govulncheck ./...
go generate ./...
git diff --exit-code
```

The full Go fuzz, leak, fault-injection, and benchmark profile is documented in
[`docs/go-quality-harness.md`](docs/go-quality-harness.md).

The documented dependency direction is enforced by a test, so a new package
must be added to the layer table it declares.

## Generated and untracked files

A generated artifact is committed only when `go generate ./...` reproduces it
byte for byte from a clean checkout; CI fails on any resulting diff. An
artifact that cannot meet that bar is generated during the build instead of
being committed.

Conformance fixtures under `conformance/` are authored, not generated. They are
the language-neutral contract, so a behavior change edits the fixture in the
same commit and every implementation is expected to follow it.

Local agent and editor state, runtime logs, and disposable build output are not
tracked. Add new tool directories to `.gitignore` rather than committing them.

Do not use public issues for vulnerabilities; follow `SECURITY.md`.
