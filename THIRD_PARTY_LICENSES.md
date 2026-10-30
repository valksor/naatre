# Third-party licenses

Approximation notice: this table is **generated from the license fields
declared in each locked dependency's own manifest** (the `license` key of
the crate's `Cargo.toml` at the version pinned in `Cargo.lock`), not with
`cargo-about`, which is not part of the local toolchain. It inventories
every crates.io dependency in the lockfile (transitives included) and its
declared license string. The release/Docker pipeline is expected to
regenerate the authoritative document with `cargo-about` (full license
text inventory) and replace this file — the CI step for that belongs in
the release job once cargo-about is wired there.

naatre itself is GPL-3.0-only (`LICENSE`). `cargo deny check licenses` in
CI enforces that every locked license is GPL-3-compatible (see
`deny.toml`); this table is the human-readable view of the same set.
Versions below are the locked ones at generation time.

| Crate | Version | License (declared) |
|---|---|---|
| `aho-corasick` | 1.1.5 | Unlicense OR MIT |
| `allocator-api2` | 0.2.21 | MIT OR Apache-2.0 |
| `anstream` | 1.0.0 | MIT OR Apache-2.0 |
| `anstyle` | 1.0.14 | MIT OR Apache-2.0 |
| `anstyle-parse` | 1.0.0 | MIT OR Apache-2.0 |
| `anstyle-query` | 1.1.5 | MIT OR Apache-2.0 |
| `anstyle-wincon` | 3.0.11 | MIT OR Apache-2.0 |
| `arc-swap` | 1.9.3 | MIT OR Apache-2.0 |
| `arrayvec` | 0.7.8 | MIT OR Apache-2.0 |
| `atomic-waker` | 1.1.2 | Apache-2.0 OR MIT |
| `autocfg` | 1.5.1 | Apache-2.0 OR MIT |
| `axum` | 0.8.9 | MIT |
| `axum-core` | 0.5.6 | MIT |
| `base64` | 0.22.1 | MIT OR Apache-2.0 |
| `base64` | 0.23.1 | MIT OR Apache-2.0 |
| `bit-set` | 0.11.1 | Apache-2.0 OR MIT |
| `bit-vec` | 0.10.1 | Apache-2.0 OR MIT |
| `bitflags` | 1.3.2 | MIT/Apache-2.0 |
| `bitflags` | 2.13.2 | MIT OR Apache-2.0 |
| `blake3` | 1.8.7 | CC0-1.0 OR Apache-2.0 OR Apache-2.0 WITH LLVM-exception |
| `borsh` | 1.8.1 | MIT OR Apache-2.0 |
| `borsh-derive` | 1.8.1 | Apache-2.0 |
| `bstr` | 1.13.1 | MIT OR Apache-2.0 |
| `bumpalo` | 3.20.3 | MIT OR Apache-2.0 |
| `byteorder` | 1.5.0 | Unlicense OR MIT |
| `bytes` | 1.12.1 | MIT |
| `cc` | 1.10.0 | MIT OR Apache-2.0 |
| `cfg-if` | 1.0.5 | MIT OR Apache-2.0 |
| `cfg_aliases` | 0.2.2 | MIT |
| `chacha20` | 0.10.2 | MIT OR Apache-2.0 |
| `clap` | 4.6.7 | MIT OR Apache-2.0 |
| `clap_builder` | 4.6.7 | MIT OR Apache-2.0 |
| `clap_derive` | 4.6.7 | MIT OR Apache-2.0 |
| `clap_lex` | 1.1.1 | MIT OR Apache-2.0 |
| `clru` | 0.6.3 | MIT |
| `colorchoice` | 1.0.5 | MIT OR Apache-2.0 |
| `constant_time_eq` | 0.4.2 | CC0-1.0 OR MIT-0 OR Apache-2.0 |
| `cookie` | 0.18.2 | MIT OR Apache-2.0 |
| `cookie_store` | 0.22.1 | MIT OR Apache-2.0 |
| `core_detect` | 1.0.0 | MIT/Apache-2.0 |
| `cpufeatures` | 0.3.1 | MIT OR Apache-2.0 |
| `crc32fast` | 1.5.3 | MIT OR Apache-2.0 |
| `crossbeam-channel` | 0.5.17 | MIT OR Apache-2.0 |
| `crossbeam-utils` | 0.8.23 | MIT OR Apache-2.0 |
| `defmt` | 0.3.101 | MIT OR Apache-2.0 |
| `defmt` | 1.2.0 | MIT OR Apache-2.0 |
| `defmt-macros` | 1.2.0 | MIT OR Apache-2.0 |
| `defmt-parser` | 1.1.0 | MIT OR Apache-2.0 |
| `deranged` | 0.5.9 | MIT OR Apache-2.0 |
| `displaydoc` | 0.2.7 | MIT OR Apache-2.0 |
| `document-features` | 0.2.12 | MIT OR Apache-2.0 |
| `dunce` | 1.0.5 | CC0-1.0 OR MIT-0 OR Apache-2.0 |
| `encoding_rs` | 0.8.42 | (Apache-2.0 OR MIT) AND BSD-3-Clause |
| `equivalent` | 1.0.3 | Apache-2.0 OR MIT |
| `errno` | 0.3.14 | MIT OR Apache-2.0 |
| `fallible-iterator` | 0.3.0 | MIT/Apache-2.0 |
| `fallible-streaming-iterator` | 0.1.9 | MIT/Apache-2.0 |
| `faster-hex` | 0.10.1 | MIT |
| `fastrand` | 2.5.0 | Apache-2.0 OR MIT |
| `find-msvc-tools` | 0.1.15 | MIT OR Apache-2.0 |
| `fnv` | 1.0.7 | Apache-2.0 / MIT |
| `foldhash` | 0.2.0 | Zlib |
| `form_urlencoded` | 1.2.2 | MIT OR Apache-2.0 |
| `fs2` | 0.4.3 | MIT/Apache-2.0 |
| `futures-core` | 0.3.34 | MIT OR Apache-2.0 |
| `futures-task` | 0.3.34 | MIT OR Apache-2.0 |
| `futures-util` | 0.3.34 | MIT OR Apache-2.0 |
| `getrandom` | 0.3.4 | MIT OR Apache-2.0 |
| `getrandom` | 0.4.3 | MIT OR Apache-2.0 |
| `gix` | 0.90.0 | MIT OR Apache-2.0 |
| `gix-actor` | 0.44.1 | MIT OR Apache-2.0 |
| `gix-attributes` | 0.37.1 | MIT OR Apache-2.0 |
| `gix-chunk` | 0.10.1 | MIT OR Apache-2.0 |
| `gix-command` | 0.12.1 | MIT OR Apache-2.0 |
| `gix-commitgraph` | 0.42.0 | MIT OR Apache-2.0 |
| `gix-config` | 0.62.2 | MIT OR Apache-2.0 |
| `gix-config-value` | 0.21.1 | MIT OR Apache-2.0 |
| `gix-date` | 0.18.1 | MIT OR Apache-2.0 |
| `gix-diff` | 0.70.0 | MIT OR Apache-2.0 |
| `gix-discover` | 0.57.1 | MIT OR Apache-2.0 |
| `gix-error` | 0.5.2 | MIT OR Apache-2.0 |
| `gix-filter` | 0.36.1 | MIT OR Apache-2.0 |
| `gix-fs` | 0.24.1 | MIT OR Apache-2.0 |
| `gix-glob` | 0.29.1 | MIT OR Apache-2.0 |
| `gix-hash` | 0.28.1 | MIT OR Apache-2.0 |
| `gix-hashtable` | 0.18.1 | MIT OR Apache-2.0 |
| `gix-lock` | 26.0.1 | MIT OR Apache-2.0 |
| `gix-macros` | 0.2.0 | MIT OR Apache-2.0 |
| `gix-note` | 0.3.1 | MIT OR Apache-2.0 |
| `gix-object` | 0.66.1 | MIT OR Apache-2.0 |
| `gix-odb` | 0.87.0 | MIT OR Apache-2.0 |
| `gix-pack` | 0.77.0 | MIT OR Apache-2.0 |
| `gix-packetline` | 0.24.1 | MIT OR Apache-2.0 |
| `gix-parallel` | 0.50.1 | MIT OR Apache-2.0 |
| `gix-path` | 0.14.1 | MIT OR Apache-2.0 |
| `gix-protocol` | 0.68.0 | MIT OR Apache-2.0 |
| `gix-quote` | 0.10.1 | MIT OR Apache-2.0 |
| `gix-ref` | 0.69.1 | MIT OR Apache-2.0 |
| `gix-refspec` | 0.47.1 | MIT OR Apache-2.0 |
| `gix-revision` | 0.52.0 | MIT OR Apache-2.0 |
| `gix-revwalk` | 0.38.0 | MIT OR Apache-2.0 |
| `gix-sec` | 0.16.1 | MIT OR Apache-2.0 |
| `gix-shallow` | 0.15.1 | MIT OR Apache-2.0 |
| `gix-tempfile` | 26.0.1 | MIT OR Apache-2.0 |
| `gix-trace` | 0.3.1 | MIT OR Apache-2.0 |
| `gix-transport` | 0.62.0 | MIT OR Apache-2.0 |
| `gix-traverse` | 0.64.0 | MIT OR Apache-2.0 |
| `gix-url` | 0.40.1 | MIT OR Apache-2.0 |
| `gix-utils` | 0.5.2 | MIT OR Apache-2.0 |
| `gix-validate` | 0.13.1 | MIT OR Apache-2.0 |
| `gix-worktree-stream` | 0.39.0 | MIT OR Apache-2.0 |
| `gix-zlib` | 0.4.0 | MIT OR Apache-2.0 |
| `hash32` | 0.3.1 | MIT OR Apache-2.0 |
| `hashbrown` | 0.16.1 | MIT OR Apache-2.0 |
| `hashbrown` | 0.17.1 | MIT OR Apache-2.0 |
| `hashlink` | 0.12.2 | MIT OR Apache-2.0 |
| `heapless` | 0.8.0 | MIT OR Apache-2.0 |
| `heck` | 0.5.0 | MIT OR Apache-2.0 |
| `http` | 1.5.0 | MIT OR Apache-2.0 |
| `http-body` | 1.1.0 | MIT |
| `http-body-util` | 0.1.5 | MIT |
| `httparse` | 1.10.1 | MIT OR Apache-2.0 |
| `httpdate` | 1.0.3 | MIT OR Apache-2.0 |
| `hyper` | 1.12.0 | MIT |
| `hyper-util` | 0.1.21 | MIT |
| `icu_collections` | 2.4.0 | Unicode-3.0 |
| `icu_locale_core` | 2.4.0 | Unicode-3.0 |
| `icu_normalizer` | 2.4.0 | Unicode-3.0 |
| `icu_normalizer_data` | 2.4.0 | Unicode-3.0 |
| `icu_properties` | 2.4.0 | Unicode-3.0 |
| `icu_properties_data` | 2.4.0 | Unicode-3.0 |
| `icu_provider` | 2.4.0 | Unicode-3.0 |
| `idna` | 1.1.0 | MIT OR Apache-2.0 |
| `idna_adapter` | 1.2.2 | Apache-2.0 OR MIT |
| `indexmap` | 2.14.2 | Apache-2.0 OR MIT |
| `is_terminal_polyfill` | 1.70.2 | MIT OR Apache-2.0 |
| `itoa` | 1.0.18 | MIT OR Apache-2.0 |
| `jiff` | 0.2.38 | Unlicense OR MIT |
| `jiff-core` | 0.1.1 | Unlicense OR MIT |
| `jiff-static` | 0.2.38 | Unlicense OR MIT |
| `jiff-tzdb` | 0.1.9 | Unlicense OR MIT |
| `jiff-tzdb-platform` | 0.1.3 | Unlicense OR MIT |
| `jobserver` | 0.1.35 | MIT OR Apache-2.0 |
| `js-sys` | 0.3.106 | MIT OR Apache-2.0 |
| `lazy_static` | 1.5.1 | MIT OR Apache-2.0 |
| `libc` | 0.2.191 | MIT OR Apache-2.0 |
| `libsqlite3-sys` | 0.39.1 | MIT |
| `linux-raw-sys` | 0.12.1 | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT |
| `litemap` | 0.8.3 | Unicode-3.0 |
| `litrs` | 1.0.1 | MIT OR Apache-2.0 |
| `lock_api` | 0.4.14 | MIT OR Apache-2.0 |
| `log` | 0.4.34 | MIT OR Apache-2.0 |
| `matchers` | 0.2.0 | MIT |
| `matchit` | 0.8.4 | MIT AND BSD-3-Clause |
| `memchr` | 2.8.3 | Unlicense OR MIT |
| `memmap2` | 0.9.12 | MIT OR Apache-2.0 |
| `mime` | 0.3.17 | MIT OR Apache-2.0 |
| `mio` | 1.2.4 | MIT |
| `multiversion_no_op` | 1.0.0 | Apache-2.0 OR MIT |
| `nonempty` | 0.12.0 | MIT |
| `nu-ansi-term` | 0.50.3 | MIT |
| `num-conv` | 0.2.2 | MIT OR Apache-2.0 |
| `num-traits` | 0.2.19 | MIT OR Apache-2.0 |
| `once_cell` | 1.21.4 | MIT OR Apache-2.0 |
| `once_cell_polyfill` | 1.70.2 | MIT OR Apache-2.0 |
| `parking_lot` | 0.12.5 | MIT OR Apache-2.0 |
| `parking_lot_core` | 0.9.12 | MIT OR Apache-2.0 |
| `percent-encoding` | 2.3.2 | MIT OR Apache-2.0 |
| `pin-project-lite` | 0.2.17 | Apache-2.0 OR MIT |
| `pkg-config` | 0.3.34 | MIT OR Apache-2.0 |
| `portable-atomic` | 1.15.0 | Apache-2.0 OR MIT |
| `portable-atomic-util` | 0.2.8 | Apache-2.0 OR MIT |
| `potential_utf` | 0.1.6 | Unicode-3.0 |
| `powerfmt` | 0.2.1 | MIT OR Apache-2.0 |
| `proc-macro-crate` | 3.5.0 | MIT OR Apache-2.0 |
| `proc-macro2` | 1.0.107 | MIT OR Apache-2.0 |
| `prodash` | 31.0.1 | MIT |
| `proptest` | 1.12.0 | MIT OR Apache-2.0 |
| `quick-error` | 1.2.3 | MIT/Apache-2.0 |
| `quote` | 1.0.47 | MIT OR Apache-2.0 |
| `r-efi` | 5.3.0 | MIT OR Apache-2.0 OR LGPL-2.1-or-later |
| `r-efi` | 6.0.0 | MIT OR Apache-2.0 OR LGPL-2.1-or-later |
| `rand` | 0.10.3 | MIT OR Apache-2.0 |
| `rand_core` | 0.10.1 | MIT OR Apache-2.0 |
| `rand_xorshift` | 0.5.0 | MIT OR Apache-2.0 |
| `redox_syscall` | 0.5.18 | MIT |
| `regex-automata` | 0.4.18 | MIT OR Apache-2.0 |
| `regex-syntax` | 0.8.11 | MIT OR Apache-2.0 |
| `rsqlite-vfs` | 0.2.0 | MIT |
| `rusqlite` | 0.41.1 | MIT |
| `rustix` | 1.1.6 | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT |
| `rustversion` | 1.0.23 | MIT OR Apache-2.0 |
| `rusty-fork` | 0.3.1 | MIT/Apache-2.0 |
| `ryu` | 1.0.23 | Apache-2.0 OR BSL-1.0 |
| `same-file` | 1.0.6 | Unlicense/MIT |
| `scopeguard` | 1.2.0 | MIT OR Apache-2.0 |
| `serde` | 1.0.229 | MIT OR Apache-2.0 |
| `serde_core` | 1.0.229 | MIT OR Apache-2.0 |
| `serde_derive` | 1.0.229 | MIT OR Apache-2.0 |
| `serde_json` | 1.0.154 | MIT OR Apache-2.0 |
| `serde_path_to_error` | 0.1.20 | MIT OR Apache-2.0 |
| `serde_urlencoded` | 0.7.1 | MIT/Apache-2.0 |
| `sha1dc` | 0.1.5 | MIT OR Apache-2.0 |
| `sharded-slab` | 0.1.7 | MIT |
| `shlex` | 2.0.1 | MIT OR Apache-2.0 |
| `signal-hook-registry` | 1.4.8 | MIT OR Apache-2.0 |
| `simdutf8` | 0.1.5 | MIT OR Apache-2.0 |
| `similar` | 2.7.0 | Apache-2.0 |
| `slab` | 0.4.12 | MIT |
| `smallvec` | 1.16.4 | MIT OR Apache-2.0 |
| `socket2` | 0.6.5 | MIT OR Apache-2.0 |
| `sqlite-wasm-rs` | 0.6.2 | MIT |
| `stable_deref_trait` | 1.2.1 | MIT OR Apache-2.0 |
| `strsim` | 0.11.1 | MIT |
| `syn` | 2.0.119 | MIT OR Apache-2.0 |
| `syn` | 3.0.9 | MIT OR Apache-2.0 |
| `sync_wrapper` | 1.0.2 | Apache-2.0 |
| `synstructure` | 0.14.0 | MIT |
| `tempfile` | 3.27.0 | MIT OR Apache-2.0 |
| `thiserror` | 2.0.21 | MIT OR Apache-2.0 |
| `thiserror-impl` | 2.0.21 | MIT OR Apache-2.0 |
| `thread_local` | 1.1.10 | MIT OR Apache-2.0 |
| `time` | 0.3.55 | MIT OR Apache-2.0 |
| `time-core` | 0.1.9 | MIT OR Apache-2.0 |
| `time-macros` | 0.2.32 | MIT OR Apache-2.0 |
| `tinystr` | 0.8.5 | Unicode-3.0 |
| `tinyvec` | 1.13.3 | Zlib OR Apache-2.0 OR MIT |
| `tokio` | 1.54.1 | MIT |
| `tokio-macros` | 2.7.2 | MIT |
| `toml_datetime` | 1.1.2+spec-1.1.0 | MIT OR Apache-2.0 |
| `toml_edit` | 0.25.17+spec-1.1.0 | MIT OR Apache-2.0 |
| `toml_parser` | 1.1.5+spec-1.1.0 | MIT OR Apache-2.0 |
| `tower` | 0.5.3 | MIT |
| `tower-layer` | 0.3.3 | MIT |
| `tower-service` | 0.3.3 | MIT |
| `tracing` | 0.1.44 | MIT |
| `tracing-attributes` | 0.1.31 | MIT |
| `tracing-core` | 0.1.36 | MIT |
| `tracing-log` | 0.2.0 | MIT |
| `tracing-subscriber` | 0.3.23 | MIT |
| `unarray` | 0.1.4 | MIT OR Apache-2.0 |
| `unicode-bom` | 2.0.3 | Apache-2.0 |
| `unicode-ident` | 1.0.27 | (MIT OR Apache-2.0) AND Unicode-3.0 |
| `unicode-normalization` | 0.1.26 | (MIT OR Apache-2.0) AND Unicode-3.0 |
| `ureq` | 3.5.0 | MIT OR Apache-2.0 |
| `ureq-proto` | 0.6.5 | MIT OR Apache-2.0 |
| `url` | 2.5.8 | MIT OR Apache-2.0 |
| `utf8-zero` | 0.8.1 | MIT OR Apache-2.0 |
| `utf8_iter` | 1.1.0 | Apache-2.0 OR MIT |
| `utf8parse` | 0.2.2 | Apache-2.0 OR MIT |
| `utf_types` | 1.0.0 | Apache-2.0 OR MIT |
| `utoipa` | 5.5.0 | MIT OR Apache-2.0 |
| `utoipa-gen` | 5.5.0 | MIT OR Apache-2.0 |
| `uuid` | 1.29.0 | Apache-2.0 OR MIT |
| `valuable` | 0.1.1 | MIT |
| `vcpkg` | 0.2.15 | MIT/Apache-2.0 |
| `version_check` | 0.9.5 | MIT/Apache-2.0 |
| `wait-timeout` | 0.2.1 | MIT/Apache-2.0 |
| `walkdir` | 2.5.0 | Unlicense/MIT |
| `wasi` | 0.11.1+wasi-snapshot-preview1 | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT |
| `wasip2` | 1.0.4+wasi-0.2.12 | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT |
| `wasm-bindgen` | 0.2.129 | MIT OR Apache-2.0 |
| `wasm-bindgen-macro` | 0.2.129 | MIT OR Apache-2.0 |
| `wasm-bindgen-macro-support` | 0.2.129 | MIT OR Apache-2.0 |
| `wasm-bindgen-shared` | 0.2.129 | MIT OR Apache-2.0 |
| `winapi` | 0.3.9 | MIT/Apache-2.0 |
| `winapi-i686-pc-windows-gnu` | 0.4.0 | MIT/Apache-2.0 |
| `winapi-util` | 0.1.11 | Unlicense OR MIT |
| `winapi-x86_64-pc-windows-gnu` | 0.4.0 | MIT/Apache-2.0 |
| `windows-link` | 0.2.1 | MIT OR Apache-2.0 |
| `windows-sys` | 0.61.2 | MIT OR Apache-2.0 |
| `winnow` | 1.0.4 | MIT |
| `wit-bindgen` | 0.57.1 | Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT |
| `writeable` | 0.6.4 | Unicode-3.0 |
| `yoke` | 0.8.4 | Unicode-3.0 |
| `yoke-derive` | 0.8.4 | Unicode-3.0 |
| `zerofrom` | 0.1.8 | Unicode-3.0 |
| `zerofrom-derive` | 0.1.8 | Unicode-3.0 |
| `zerotrie` | 0.2.5 | Unicode-3.0 |
| `zerovec` | 0.11.9 | Unicode-3.0 |
| `zerovec-derive` | 0.11.7 | Unicode-3.0 |
| `zlib-rs` | 0.6.8 | Zlib |
| `zmij` | 1.0.23 | MIT |
| `zstd` | 0.14.3 | BSD-3-Clause |
| `zstd-safe` | 8.2.0 | BSD-3-Clause |
| `zstd-sys` | 2.1.2+zstd.1.5.7 | BSD-3-Clause |
