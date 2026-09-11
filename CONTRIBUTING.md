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
go test -race ./...
golangci-lint run
govulncheck ./...
go generate ./...
git diff --exit-code
```

Do not use public issues for vulnerabilities; follow `SECURITY.md`.
