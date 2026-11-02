package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ciWorkflow is the path of the continuous integration workflow,
// relative to the repository root.
const ciWorkflow = ".github/workflows/ci.yml"

// TestVersionFileMatchesTheChangelog pins that the version the release
// will carry has a changelog section.
//
// A release without a changelog entry tells a user nothing about what
// changed, and the version file is the value the release workflow
// requires the tag to match.
func TestVersionFileMatchesTheChangelog(t *testing.T) {
	root := repoRoot(t)

	version, err := ReadVersion(root)
	if err != nil {
		t.Fatalf("ReadVersion: %v", err)
	}
	if _, err := ParseVersion(version); err != nil {
		t.Errorf("the version %q is not a semantic version: %v", version, err)
	}

	data, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	heading := "## [" + version + "]"
	if !strings.Contains(string(data), heading) {
		t.Errorf("CHANGELOG.md has no section %q for the version the release will carry", heading)
	}
}

// TestLicenseIsTheAfferoLicence pins that the AGPL travels with the
// release.
//
// The AGPL requires the licence to accompany the binary, and the
// release workflow copies LICENSE into the published assets. The file
// must therefore be the AGPL and not some other licence.
func TestLicenseIsTheAfferoLicence(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "GNU AFFERO GENERAL PUBLIC LICENSE") {
		t.Error("LICENSE is not the GNU Affero General Public License")
	}
	if !strings.Contains(text, "Version 3") {
		t.Error("LICENSE is not version 3 of the Affero General Public License")
	}
}

// TestMakefileExposesTheReleaseTargets pins that the release build is
// discoverable.
//
// The Makefile is what a maintainer or a reviewer runs; the Go command
// behind it is an implementation detail.
func TestMakefileExposesTheReleaseTargets(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	text := string(data)

	for _, target := range []string{"release", "verify", "smoke", "build", "clean"} {
		if !strings.Contains(text, "\n"+target+":") {
			t.Errorf("the Makefile has no %q target", target)
		}
	}
	if !strings.Contains(text, "./internal/release/cmd/release") {
		t.Error("the Makefile does not drive the release command")
	}
}

// TestCIRunsTheReleaseBuild pins that the release definitions are
// exercised on every change.
//
// A release build that is only ever run when a release is cut is a
// release build that breaks silently. The continuous integration
// workflow builds and verifies the artifacts, so a change that breaks
// them is caught before it reaches a tag.
func TestCIRunsTheReleaseBuild(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ciWorkflow))
	if err != nil {
		t.Fatalf("read %s: %v", ciWorkflow, err)
	}
	text := string(data)

	if !strings.Contains(text, "make release") {
		t.Error("the CI workflow does not build the release artifacts")
	}
	if !strings.Contains(text, "make verify") {
		t.Error("the CI workflow does not verify the release artifacts")
	}
	if !strings.Contains(text, "make smoke") {
		t.Error("the CI workflow does not run the clean-install smoke test")
	}
}

// TestReleaseArtifactsAreIgnored pins that build output is not committed.
//
// The artifacts are reproducible and are produced by the release
// workflow, so committing them would only create a second source of
// truth.
func TestReleaseArtifactsAreIgnored(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "/dist/") {
		t.Error(".gitignore does not exclude the distribution directory")
	}
}

// TestInstallationDocumentationExists pins that the release ships the
// documentation a user needs to install and verify the binary.
func TestInstallationDocumentationExists(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"docs/INSTALL.md", "docs/RELEASE.md", "CHANGELOG.md"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s is missing: %v", name, err)
		}
	}

	install, err := os.ReadFile(filepath.Join(root, "docs", "INSTALL.md"))
	if err != nil {
		t.Fatalf("read docs/INSTALL.md: %v", err)
	}
	text := string(install)
	for _, want := range []string{"SHA256SUMS", "sha256sum", "shasum", "AGPL", "Affero"} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/INSTALL.md does not mention %q", want)
		}
	}
}
