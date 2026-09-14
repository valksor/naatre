# Generator plugin host

The `generatorplugin` package and `cmd/naatre-generator-plugin-host`
command implement the `sdk.generator-plugin-host-1` integration profile. Issue
#76 owns this host, its JavaScript fixture, and its executable conformance
evidence. The language-neutral generator model and all normative generation
behavior remain owned by issue #35 and `spec/v1/generation.md`; the host
manifest is installation metadata and does not define another schema or
operation model.

## Lifecycle and discovery

The profile and `naatre.generator-plugin-manifest-1` manifest are versioned
experimental interfaces. An incompatible manifest or host-envelope change
requires a new version. Plugin algorithm versions remain independent and are
included in generated output. Discovery accepts only exact manifest files or
the immediate `*.naatre-generator.json` children of directories explicitly
provided by the caller. It never searches the current directory, `PATH`, user
configuration, or a remote registry. Duplicate IDs, symlinked discovery
locations, escaping entrypoints, unknown manifest members, and unsupported
model versions fail with stable `GENERATOR_*` codes.

An installed plugin is trusted executable code. Model, configuration, plugin
stdout, stderr, diagnostics, artifact paths, and artifact contents are
untrusted. The host starts each plugin in a fresh temporary working directory,
passes only `LANG=C`, `LC_ALL=C`, `TZ=UTC`, and `SOURCE_DATE_EPOCH=0`, sends a
bounded request on stdin, and accepts one bounded response on stdout. The
plugin never receives the final output root. Host-controlled validation then
passes artifacts to `generator.WriteArtifacts`, which rejects absolute,
traversing, duplicate-normalized, and symlink-parent paths and writes each file
through a same-directory temporary file and rename. Plugin stderr and
implementation errors are discarded; public errors contain only stable codes,
safe pointers, and fixed messages.

The standard-library host does not claim kernel filesystem confinement,
network namespaces, container or virtual-machine isolation, CPU quotas, or
process memory quotas. Deployments that do not trust installed plugin code must
add an operating-system sandbox outside this profile. Context cancellation
actively terminates the child process, and byte/count bounds cover models,
configuration, stdout, stderr, individual artifacts, total artifacts, and the
artifact count.

## Supported boundary

The executed profile supports the public Go package on Go 1.27 and explicit
Node runtimes with major version 24 or 26 on Linux amd64 and macOS arm64. CI
evidence is pinned to Node 24.21.0; the local conformance run records its exact
runtime and platform in the runner result. The third-party fixture is a
dependency-free JavaScript generator which consumes
`naatre.generator-model-1` and emits a JavaScript/TypeScript wire-model
artifact.

The profile does not support ambient or remote discovery, native plugin
loading, remote plugin transports, browser runtimes, Windows host execution,
package publication, third-party certification, framework integration, cache
adapters, workers, operating-system adapters, or HTTP, SSE, and WebSocket
transport generation. A wire-model artifact makes none of those capability
claims. The exhaustive machine-readable list is
`conformance/v1/generator-plugin-host.json` under
`unsupportedOptionalCapabilities`.

## Reproducible verification

From the repository root, with the exact Go and Node revisions recorded in the
evidence file:

```sh
go test ./generatorplugin ./cmd/naatre-generator-plugin-host -count=1
go test ./internal/conformance -run TestGeneratorPluginHostConformance -count=1
printf '%s\n' '{"protocol":"naatre.conformance.runner-1","id":"plugin-host","command":"run","path":{"source":{"kind":"sdk","language":"javascript-typescript"},"destination":{"kind":"native-runtime","language":"go"}},"profiles":["sdk.generator-plugin-host-1"]}' | go run ./cmd/naatre-conformance --require-pass
```

The conformance profile executes positive, hostile-source, output-boundary,
private-failure, cancellation, and resource-limit cases. It runs the non-Go
fixture twice into different output roots and requires byte-identical output,
then asks Node to parse the generated module so hostile schema text cannot
become source.
