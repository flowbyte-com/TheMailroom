package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestParseVersionOutput pins the parsing of the version output.
//
// The values are compared exactly elsewhere, so the parser must not
// truncate or extend them. In particular the development default
// "0.1.0-dev" must not satisfy a check for "0.1.0".
func TestParseVersionOutput(t *testing.T) {
	out := "themailroom 1.0.0 (commit 904384a32183ffaad34a1d95432344eeaa920234, built 2026-11-02T12:22:50Z)\n"
	version, commit, buildDate, err := parseVersionOutput(out)
	if err != nil {
		t.Fatalf("parseVersionOutput: %v", err)
	}
	if version != "1.0.0" {
		t.Errorf("version = %q, want %q", version, "1.0.0")
	}
	if commit != "904384a32183ffaad34a1d95432344eeaa920234" {
		t.Errorf("commit = %q, want %q", commit, "904384a32183ffaad34a1d95432344eeaa920234")
	}
	if buildDate != "2026-11-02T12:22:50Z" {
		t.Errorf("build date = %q, want %q", buildDate, "2026-11-02T12:22:50Z")
	}
}

// TestParseVersionOutputRejectsMalformedOutput covers the outputs that
// are not a version line at all.
func TestParseVersionOutputRejectsMalformedOutput(t *testing.T) {
	for _, out := range []string{
		"",
		"themailroom\n",
		"themailroom 1.0.0\n",
		"themailroom 1.0.0 (built 2026-11-02T12:22:50Z)\n",
		"themailroom 1.0.0 (commit abc)\n",
	} {
		if _, _, _, err := parseVersionOutput(out); err == nil {
			t.Errorf("parseVersionOutput(%q) = nil, want an error", out)
		}
	}
}

// TestVerifyRejectsArtifactsFromAnotherCommit covers the check that
// stops a release from shipping artifacts built from different commits.
//
// The commit is embedded in the binary, so an artifact from another
// commit is detectable even when it was never run.
func TestVerifyRejectsArtifactsFromAnotherCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build in short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}

	root := repoRoot(t)
	meta, err := ResolveMetadata(root)
	if err != nil {
		t.Fatalf("ResolveMetadata: %v", err)
	}
	target := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	dist := t.TempDir()

	err = Build(BuildOptions{
		Root:     root,
		Dist:     dist,
		Metadata: meta,
		Targets:  []Target{target},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	opts := VerifyOptions{
		Dist:       dist,
		Metadata:   meta,
		Targets:    []Target{target},
		HostGOOS:   runtime.GOOS,
		HostGOARCH: runtime.GOARCH,
	}
	if err := Verify(opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// The same artifact set, but the verification expects a different
	// commit.
	wrong := meta
	wrong.Commit = "0000000000000000000000000000000000000000"
	wrongOpts := opts
	wrongOpts.Metadata = wrong
	if err := Verify(wrongOpts); err == nil {
		t.Error("Verify() with the wrong commit = nil, want an error")
	}
}

// TestVerifyRejectsANonExecutableArtifact covers the check that catches
// an artifact that lost its executable bit.
//
// The publish job downloads the artifacts through a zip round trip, so
// this is a real failure mode rather than a theoretical one.
func TestVerifyRejectsANonExecutableArtifact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build in short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}

	root := repoRoot(t)
	meta, err := ResolveMetadata(root)
	if err != nil {
		t.Fatalf("ResolveMetadata: %v", err)
	}
	target := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	dist := t.TempDir()

	err = Build(BuildOptions{
		Root:     root,
		Dist:     dist,
		Metadata: meta,
		Targets:  []Target{target},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	artifact := filepath.Join(dist, ArtifactName(target))
	if err := os.Chmod(artifact, 0o644); err != nil {
		t.Fatal(err)
	}

	err = Verify(VerifyOptions{
		Dist:       dist,
		Metadata:   meta,
		Targets:    []Target{target},
		HostGOOS:   runtime.GOOS,
		HostGOARCH: runtime.GOARCH,
	})
	if err == nil {
		t.Fatal("Verify() with a non-executable artifact = nil, want an error")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("Verify() = %v, want it to report that the artifact is not executable", err)
	}
}

// TestVerifyToolchainConsistency covers the check that every artifact of
// a release was built with the same Go toolchain.
//
// The Go version is embedded in the binary, so two builds of the same
// commit with different Go versions are not identical.
func TestVerifyToolchainConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build in short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}

	root := repoRoot(t)
	meta, err := ResolveMetadata(root)
	if err != nil {
		t.Fatalf("ResolveMetadata: %v", err)
	}
	target := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	dist := t.TempDir()

	err = Build(BuildOptions{
		Root:     root,
		Dist:     dist,
		Metadata: meta,
		Targets:  []Target{target},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	artifact := filepath.Join(dist, ArtifactName(target))
	version, err := ToolchainOf(artifact)
	if err != nil {
		t.Fatalf("ToolchainOf: %v", err)
	}
	if version != GoToolchain() {
		t.Errorf("the artifact reports the toolchain %q, want %q", version, GoToolchain())
	}

	if err := VerifyToolchainConsistency([]string{artifact}); err != nil {
		t.Errorf("VerifyToolchainConsistency() = %v, want nil", err)
	}
}

// TestRequiredToolchainMatchesTheModule pins that the Go version the
// repository requires is read from the module file.
func TestRequiredToolchainMatchesTheModule(t *testing.T) {
	root := repoRoot(t)
	got, err := RequiredToolchain(root)
	if err != nil {
		t.Fatalf("RequiredToolchain: %v", err)
	}
	if !strings.HasPrefix(got, "1.") {
		t.Errorf("RequiredToolchain() = %q, want a 1.x version", got)
	}
}
