// Package release defines the reproducible release build for TheMailroom.
//
// It is the single source of truth for the release target matrix, the
// artifact names, the build metadata injected into the binary, and the
// verification of the produced artifacts. The command in
// internal/release/cmd/release drives it, and the Makefile is the
// discoverable entry point.
//
// Nothing in this package publishes anything. It builds, verifies and
// exercises a release candidate; the decision to publish belongs to the
// release workflow, which is gated on a version tag.
package release

import (
	"fmt"
	"strings"
)

// Target is one release target: an operating system and an architecture.
type Target struct {
	// GOOS is the Go operating system name.
	GOOS string
	// GOARCH is the Go architecture name.
	GOARCH string
}

// String returns the conventional "goos/goarch" form.
func (t Target) String() string { return t.GOOS + "/" + t.GOARCH }

// Targets returns the release target matrix.
//
// The matrix is the set of platforms supported for the V1 release. It is
// deliberately the same set the CI cross-platform build job compiles on
// every pull request: a target that CI does not build cannot be
// released, because the release workflow runs this same build.
//
// The order is stable, so the artifact list, the checksum file and the CI
// log always read the same way.
func Targets() []Target {
	return []Target{
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "windows", GOARCH: "amd64"},
	}
}

// ArtifactName returns the release artifact file name for a target.
//
// The name carries the platform so that an operator can tell the
// artifacts apart without consulting a manifest, and so that a checksum
// file is meaningful on its own.
func ArtifactName(t Target) string {
	name := "themailroom-" + t.GOOS + "-" + t.GOARCH
	if t.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Metadata is the build metadata injected into the binary.
//
// The three values are reported by "themailroom -version" and are logged
// when the server starts, so an operator can tell exactly which binary
// is running and which commit it was built from. The MCP initialize
// response carries the version alone.
type Metadata struct {
	// Version is the release version, without a leading "v".
	Version string
	// Commit is the full git commit SHA the binary was built from.
	Commit string
	// BuildDate is the RFC 3339 timestamp of the build.
	BuildDate string
}

// Validate reports whether the metadata is complete.
func (m Metadata) Validate() error {
	if m.Version == "" {
		return fmt.Errorf("release: version is empty")
	}
	if m.Commit == "" {
		return fmt.Errorf("release: commit is empty")
	}
	if m.BuildDate == "" {
		return fmt.Errorf("release: build date is empty")
	}
	return nil
}

// versionVarPath is the import path of the package that holds the
// build-time variables. The linker resolves the -X flags against this
// path, so a change to the module path or to the variable names must be
// reflected here.
const versionVarPath = "github.com/flowbyte-com/themailroom/internal/version"

// LDFlags returns the -ldflags value that injects the metadata into the
// binary.
//
//   - -s and -w drop the symbol table and the DWARF data. The binary is
//     smaller and carries no local symbol names.
//   - -buildid= removes the build id, which otherwise encodes the
//     absolute output path and would differ between build machines.
//   - The three -X flags set the build-time variables.
//
// The linker silently ignores an -X that names a symbol which does not
// exist, so a typo in a variable name would produce a binary that
// quietly reports the development defaults. VerifyCommitEmbedded reads
// the commit back out of the built artifact, and the artifact for the
// host platform is run and asked for its version, which is what catches
// such a typo.
func LDFlags(m Metadata) string {
	parts := []string{
		"-s",
		"-w",
		"-buildid=",
		"-X " + versionVarPath + ".Version=" + m.Version,
		"-X " + versionVarPath + ".Commit=" + m.Commit,
		"-X " + versionVarPath + ".BuildDate=" + m.BuildDate,
	}
	return strings.Join(parts, " ")
}

// BuildFlags returns the go build flags that make the build reproducible.
//
//   - -trimpath removes the build machine's filesystem paths from the
//     binary, so two builds of the same commit from different directories
//     are identical.
//   - -buildvcs=false stops the toolchain from stamping its own VCS
//     information. The commit is injected explicitly instead, and letting
//     the toolchain stamp it as well would put two sources of truth in the
//     binary.
//
// The environment that completes the definition (CGO_ENABLED=0,
// GOFLAGS=-mod=readonly, GOAMD64=v1 and GOTOOLCHAIN=local) is set by the
// build command, not here.
func BuildFlags() []string {
	return []string{"-trimpath", "-buildvcs=false"}
}
