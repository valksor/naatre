# naatre server image: digest-pinned bases, non-root, one volume,
# HEALTHCHECK. Build from the repo root:
#   docker build -f deploy/Dockerfile -t naatre .
#
# Run (loopback only unless a TLS-terminating proxy is in front — see
# README "TLS and reverse proxies"):
#   docker run -p 127.0.0.1:7400:7400 -v naatre-data:/data naatre
#
# The bases are pinned by digest (immutable); retag deliberately and bump
# both pins together. Digests observed 2026-10-30 from
# `docker buildx imagetools inspect rust:1.99-alpine` / `alpine:3.22`.

# --- build: musl static binary (the alpine rust image is a musl host) ------
FROM rust:1.99-alpine@sha256:7e9e2d068a107a2adafd19930011178d14a78326053cd4c3a9f7ad3fd049557d AS build
WORKDIR /src
# aws-lc-sys (the S3 backend's rustls crypto provider) builds C code via
# cmake; the alpine rust image ships none of that toolchain.
RUN apk add --no-cache cmake clang perl make musl-dev
COPY Cargo.toml Cargo.lock ./
COPY crates ./crates
# --locked: the committed lockfile is the authority (CI policy).
RUN cargo build --release --locked --bin naatre

# --- run: busybox wget for the healthcheck, no package installs -----------
FROM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f
COPY --from=build /src/target/release/naatre /usr/local/bin/naatre
COPY THIRD_PARTY_LICENSES.md /usr/share/doc/naatre/THIRD_PARTY_LICENSES.md

# Non-root: the data dir must live on a volume writable by uid 1000.
RUN addgroup -g 1000 naatre \
	&& adduser -D -H -u 1000 -G naatre naatre

# mkdir+chown BEFORE the VOLUME declaration: build steps after VOLUME see an
# anonymous volume shadowing /data and their changes there are discarded —
# and a fresh named volume inherits this dir's ownership at first-mount
# copy-up, so the uid-1000 image user can init it (no manual chown on boot).
RUN mkdir -p /data && chown naatre:naatre /data
VOLUME /data
EXPOSE 7400
USER naatre:naatre

# /healthz is unversioned and anonymous by design (the only anon routes).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
	CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:7400/healthz"]

ENTRYPOINT ["/usr/local/bin/naatre"]
# serve binds 0.0.0.0 in-container (container-internal loopback is not
# reachable from the host); the binary prints its cleartext warning, which
# is accurate: put the TLS-terminating proxy in front of the published port.
CMD ["serve", "--data", "/data", "--listen", "0.0.0.0:7400"]
