# Cutting a release

This document is the procedure for publishing a TheMailroom release. It
describes what the tooling does, why the guards exist, and what a
maintainer has to do.

The tooling lives in [`internal/release`](../internal/release). The
discoverable entry points are the `make` targets.

## What the release produces

A release contains one binary per supported platform, a `SHA256SUMS`
file, the licence and the changelog:

```
themailroom-linux-amd64
themailroom-linux-arm64
themailroom-darwin-amd64
themailroom-darwin-arm64
themailroom-windows-amd64.exe
SHA256SUMS
LICENSE
CHANGELOG.md
```

GitHub adds the source code archives for the tag automatically, which is
what satisfies the AGPL's requirement that the corresponding source be
offered with the binary.

## The build is reproducible

The same commit, built with the same Go toolchain, produces byte-for-byte
identical artifacts on any machine. That is what makes a checksum file
meaningful and what lets a user verify a download independently of the
publisher.

The Go toolchain is part of the condition, and it is not a formality: the
toolchain records its own version in every binary it produces, and the
compiler differs between Go releases. The release workflow therefore pins
the Go version to the one the module requires, so the module file stays
the single source of truth for the toolchain a release is built with. The
regression tests in `internal/release` check that the pin matches
`go.mod`, and `make verify` checks that every artifact of a release was
built with the same toolchain.

Everything else is pinned by the build itself:

| Source of variation | How it is pinned |
|---|---|
| The build machine's filesystem paths | `-trimpath` |
| The toolchain's own VCS stamping | `-buildvcs=false`; the commit is injected explicitly |
| The absolute output path, in the build id | `-buildid=` |
| The build date | the commit timestamp, not the wall clock. `SOURCE_DATE_EPOCH` overrides it |
| The C library of the build machine | `CGO_ENABLED=0` |
| The dependency set | `GOFLAGS=-mod=readonly` |
| The instruction set | `GOAMD64=v1`, and the Go default for the other architectures |
| A silently downloaded Go version | `GOTOOLCHAIN=local`, and the workflow's pinned version |
| The compiler's behaviour flags | `GOEXPERIMENT`, `GOFIPS140`, `GO_LDFLAGS` and the per-architecture variables are removed from the environment rather than inherited |
| The order of the checksum file | the entries are sorted by file name |

`make release` followed by `make verify` checks that two properties hold
that the definition alone cannot promise: that the metadata actually
reached the binary, and that the artifacts are what they claim to be.

## The guards on publishing

The release workflow is [`.github/workflows/release.yml`](../.github/workflows/release.yml).
It is deliberately hard to publish from the wrong place.

**It can only be started by pushing a version tag.** The workflow has
exactly one trigger: a `push` of a tag matching `v*`. There is no
pull-request trigger, no branch-push trigger and no `workflow_dispatch`
trigger. An ordinary branch or a pull request cannot start the workflow
at all, so there is no code path for one to reach.

**The tag must be a release of a commit on `main`.** A tag is a ref
anyone with push access can create, so the tag alone does not establish
where the commit lives. The workflow fetches `main` and checks that the
tagged commit is an ancestor of it.

**The tag must match the `VERSION` file.** The version in the source tree
and the version in the tag must agree, or the release would ship a binary
that reports a version the repository does not claim.

**The publish job publishes only verified artifacts.** The `publish` job
depends on the `build` job, which builds and verifies the artifacts. The
publish job downloads those artifacts and re-verifies them; it does not
build. A release therefore cannot ship binaries that were not verified.

**Only the publish job may write.** The workflow's default permission is
read-only, the build job runs with read-only permission, and only the
publish job is granted `contents: write`. The build job runs arbitrary
build steps, so it must not be able to push.

These guards are pinned by the regression tests in
`internal/release/workflow_test.go`, so weakening one of them fails the
test suite rather than quietly widening the publish path.

## Procedure

1. **Prepare the release on `main`.** Update `CHANGELOG.md` with the
   section for the new version and adjust its date if the release will
   not be published on that date. Confirm that `VERSION` holds the
   version you intend to release.

2. **Merge the preparation.** The release commit must be on `main`,
   because the workflow requires the tagged commit to be an ancestor of
   `main`.

3. **Tag the release commit.**

   ```sh
   git checkout main
   git pull --ff-only
   git tag -a "v$(cat VERSION)" -m "TheMailroom $(cat VERSION)"
   git push origin "v$(cat VERSION)"
   ```

   Push the tag to `main`'s remote. Do not tag a commit that is not on
   `main`; the workflow will refuse it.

4. **Wait for the workflow.** The `release` workflow builds the
   artifacts, verifies them, runs the clean-install smoke test and then
   publishes the release. Watch it here:
   <https://github.com/flowbyte-com/TheMailroom/actions>.

   If the workflow fails, fix the cause and re-run the failed job from
   the GitHub UI. If the release itself was already created, delete it
   first: the workflow refuses to publish over an existing release, so
   that a published release is never silently replaced.

5. **Verify the published release.** Download the artifacts from the
   release page and verify them independently:

   ```sh
   sha256sum --check SHA256SUMS
   ```

   The digests must match the `SHA256SUMS` file published with the
   release.

## What the release workflow does not do

- It does not publish from a branch or a pull request.
- It does not create a tag. A tag is created by a person, deliberately.
- It does not modify the changelog, the `VERSION` file or any other
  source file.
- It does not sign the artifacts. The checksum file is the integrity
  check, and the source archive is the provenance check.

### A known property

GitHub Actions runs the workflow file **from the tagged commit**, because
that is the tree the workflow checks out. Tagging a commit that predates
these guards would therefore run the workflow as it was at that commit.

In practice the exposure is small: the tagged commit must be an ancestor
of `main`, and `main` is protected, so the workflow file at that commit
is itself a reviewed artifact. A maintainer who tags an old commit is
acting deliberately, not accidentally.

Closing the property completely would require the workflow to live
outside the tagged tree — a reusable workflow in another repository,
pinned by commit SHA — which is a larger change than this release
packaging introduces. It is recorded here rather than left implicit.
