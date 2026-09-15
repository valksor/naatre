# Specification evolution and release policy

## Independently versioned components

The specification, schema model, canonicalization rules, fixture suite,
conformance profiles and report protocol, Go runtime, each generator, each SDK,
and each worker use independent semantic versions. A repository tag is only a
delivery label; it does not make component versions equal. Every published
release uses `naatre.release-manifest-1` and pins each component's semantic
version, immutable source revision, and SHA-256 digest.

The release manifest also pins normative external references, generated
artifacts and their generators, third-party notices, decision statuses,
support rows, and machine-readable conformance reports. Client, remote-worker,
and native-runtime support are separate rows with a maintainer and owning issue.
`planned` and `experimental` rows are not compatibility claims. An
`implemented` row requires applicable evidence, and a conformance claim
requires a report path and report digest. Repository location, drafted code, or
an unimplemented issue is never evidence.

## Compatibility and deprecation

Every change is classified `editorial`, `compatible`, `deprecated`, or
`breaking` in its proposal, decision record, and release manifest. Breaking
changes always name affected components and include executable or concrete
migration guidance.

- A `0.x` component may break within its minor line only when the release notes
  and manifest include migration guidance.
- A stable `1.x` component requires a new major version for a breaking change.
- A stable deprecation remains supported for at least two minor releases and
  six months. Removal then requires a decision record, a major release, and
  migration guidance. A security decision may shorten the time window, but it
  must state why and identify the safe replacement.
- Retired wire fields, schema members and kinds, protocol/profile identifiers,
  extension names, canonicalization revisions, and semantic-hash purposes stay
  reserved for the lifetime of their major line. A retired name cannot be
  reused with different meaning.

Compatibility is evaluated against the exact component versions and profiles
in the manifest, not against the repository as a whole. Maintained release
lines are named in release manifests. The normal support window is the current
minor release and its immediate predecessor within a stable major; security
support outside that window requires an explicit release decision.

## Experimental promotion

Experimental capabilities use an explicit experimental profile or namespace
and cannot alter default v1 parsing, validation, canonical bytes, hashes, error
selection, or execution semantics. Promotion requires an accepted decision,
stable identifiers, migration notes, complete fixtures, implementation
evidence, and a stable conformance profile. The stable capability is
negotiated explicitly. Removing the experimental spelling follows its declared
sunset policy; promotion never silently aliases it into v1.

## Supply-chain integrity

Published artifacts must be reproducible from the pinned source and toolchain.
Each artifact has a SHA-256 checksum, an SPDX or CycloneDX SBOM, a provenance
attestation binding the artifact digest to source, builder, invocation, and
materials, and a signature or ecosystem transparency record where supported.
When signing is not supported, the manifest records `not-applicable` and a
specific rationale; lack of signing never waives checksums or provenance.

Dependencies are pinned by lockfiles and updated through ordinary reviewed
changes. Automated updates may propose changes but may not approve them.
Runtime dependencies receive priority security updates; development-only
updates follow the normal compatibility and reproducibility gates. A release
with a known exploitable dependency cannot be published without a documented
security decision and mitigation.

For a compromised artifact or signing identity, the release owner stops
publication, marks the manifest `revoked`, publishes the affected digests and
replacement guidance, revokes or rotates credentials, issues an advisory, and
rebuilds from a reviewed clean source and isolated builder. A replacement gets
a new release identifier and provenance; artifacts are never silently replaced
under an existing version.

## Release checklist

The release owner completes every item and stores the result with the manifest:

1. Resolve the explicit scope decision and ensure every component issue is
   represented exactly once in the roadmap.
2. Record accepted, rejected, superseded, and unresolved decisions; pin
   external references and applicable third-party notices.
3. Classify compatibility and attach migration guidance for every breaking or
   shortened-deprecation change.
4. Pin all component, generated-artifact, source, toolchain, fixture, and
   conformance-report versions and digests.
5. Verify every `implemented` client, adapter, worker, runtime, and SDK row has
   its applicable machine-readable evidence. Keep unevidenced work `planned`.
6. Build twice in clean isolated environments and compare artifact and
   generated-output bytes. Run the profile and platform commands recorded in
   the reports.
7. Generate checksums, SBOMs, provenance attestations, and applicable
   signatures; verify all of them from the staged release directory.
8. Review vulnerability results and embargo status, publish artifacts and the
   immutable manifest atomically, then verify registry downloads by digest.
9. Publish release notes, migration guidance, support windows, and revocation
   instructions. Archive the completed checklist and decision ledger.
