# Release records

Every published Naatre release has an immutable directory named for its release
identifier. The directory contains `manifest.json`, checksum files, SBOMs,
provenance attestations, signatures or transparency records where supported,
and the machine-readable conformance reports cited by the manifest.

`release-manifest.schema.json` is the authoritative
`naatre.release-manifest-1` format. `example.manifest.json` is a non-published
draft demonstrating complete component inventory and separate client,
remote-worker, and native-runtime support rows. It deliberately makes no
implementation or conformance claim and is not a release.

Publication is atomic: validate the manifest, verify every referenced digest,
complete the checklist in
[`docs/governance/releases.md`](../docs/governance/releases.md), sign applicable
artifacts, and publish the immutable directory. A correction or rebuild gets a
new release identifier. A compromised release is retained as a `revoked`
record that names affected digests and replacement guidance.

The issue 69 aggregator links `release.stable-1` only after the complete matrix
passes. See [`conformance/RELEASE_GATE.md`](../conformance/RELEASE_GATE.md).
Planned and experimental rows remain unevidenced and cannot be promoted merely
because their source is present.
