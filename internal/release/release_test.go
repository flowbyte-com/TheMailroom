package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTargetsAreStable pins the release target matrix.
//
// The matrix is a contract: the artifacts a release produces, the
// checksum file and the release documentation all name the same set. A
// change here is a change to what the project supports, and it has to be
// a deliberate one.
func TestTargetsAreStable(t *testing.T) {
	got := Targets()
	want := []Target{
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "windows", GOARCH: "amd64"},
	}
	if len(got) != len(want) {
		t.Fatalf("len(Targets()) = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Targets()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestTargetsAreUniqueAndSorted guards the invariants the checksum file
// and the documentation rely on: no target appears twice, and the order
// is stable.
func TestTargetsAreUniqueAndSorted(t *testing.T) {
	seen := make(map[Target]bool)
	previous := ""
	for _, target := range Targets() {
		if seen[target] {
			t.Errorf("target %v appears twice", target)
		}
		seen[target] = true
		key := target.String()
		if previous != "" && key < previous {
			t.Errorf("targets are not sorted: %q comes after %q", key, previous)
		}
		previous = key
	}
}

// TestArtifactName pins the artifact file names.
//
// The name carries the platform so that an operator can tell the
// artifacts apart without a manifest, and so that a checksum file is
// meaningful on its own.
func TestArtifactName(t *testing.T) {
	cases := []struct {
		target Target
		want   string
	}{
		{Target{GOOS: "linux", GOARCH: "amd64"}, "themailroom-linux-amd64"},
		{Target{GOOS: "linux", GOARCH: "arm64"}, "themailroom-linux-arm64"},
		{Target{GOOS: "darwin", GOARCH: "amd64"}, "themailroom-darwin-amd64"},
		{Target{GOOS: "darwin", GOARCH: "arm64"}, "themailroom-darwin-arm64"},
		{Target{GOOS: "windows", GOARCH: "amd64"}, "themailroom-windows-amd64.exe"},
	}
	for _, tc := range cases {
		if got := ArtifactName(tc.target); got != tc.want {
			t.Errorf("ArtifactName(%v) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// TestArtifactNamesAreUnique guards the checksum file: two targets that
// produced the same file name would make one artifact silently
// overwrite the other.
func TestArtifactNamesAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, target := range Targets() {
		name := ArtifactName(target)
		if seen[name] {
			t.Errorf("artifact name %q is produced by more than one target", name)
		}
		seen[name] = true
	}
}

// TestLDFlags pins the linker flags.
//
// The linker ignores an -X that names a symbol which does not exist, so
// a typo in a variable path would produce a binary that quietly reports
// the development defaults. The paths are pinned here so that a rename
// of a version variable is a test failure rather than a silent
// regression.
func TestLDFlags(t *testing.T) {
	meta := Metadata{
		Version:   "1.2.3",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildDate: "2026-11-02T12:00:00Z",
	}
	got := LDFlags(meta)
	for _, want := range []string{
		"-X github.com/flowbyte-com/themailroom/internal/version.Version=1.2.3",
		"-X github.com/flowbyte-com/themailroom/internal/version.Commit=0123456789abcdef0123456789abcdef01234567",
		"-X github.com/flowbyte-com/themailroom/internal/version.BuildDate=2026-11-02T12:00:00Z",
		"-buildid=",
		"-s",
		"-w",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LDFlags() = %q, want it to contain %q", got, want)
		}
	}
}

// TestBuildFlags pins the flags that make the build reproducible.
func TestBuildFlags(t *testing.T) {
	got := strings.Join(BuildFlags(), " ")
	for _, want := range []string{"-trimpath", "-buildvcs=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("BuildFlags() = %q, want it to contain %q", got, want)
		}
	}
}

// TestBuildEnvironment pins the environment that completes the
// reproducible build definition.
//
// CGO_ENABLED=0 removes the C toolchain from the build and makes the
// binary static. GOFLAGS=-mod=readonly keeps the build from rewriting
// go.mod. GOAMD64=v1 pins the instruction set. GOTOOLCHAIN=local stops
// the toolchain from silently downloading another Go version, which would
// make the build irreproducible.
func TestBuildEnvironment(t *testing.T) {
	env := buildEnvironment([]string{"PATH=/usr/bin", "CGO_ENABLED=1", "GOOS=plan9"}, Target{GOOS: "linux", GOARCH: "amd64"})
	got := make(map[string]string, len(env))
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		got[key] = value
	}
	for key, want := range map[string]string{
		"CGO_ENABLED": "0",
		"GOFLAGS":     "-mod=readonly",
		"GOTOOLCHAIN": "local",
		"GOOS":        "linux",
		"GOARCH":      "amd64",
		"GOAMD64":     "v1",
		"PATH":        "/usr/bin",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// TestMetadataValidate rejects incomplete metadata, which would produce
// a binary that reports an unknown version.
func TestMetadataValidate(t *testing.T) {
	complete := Metadata{Version: "1.0.0", Commit: "abc", BuildDate: "2026-11-02T12:00:00Z"}
	if err := complete.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for name, meta := range map[string]Metadata{
		"no version":    {Commit: "abc", BuildDate: "2026-11-02T12:00:00Z"},
		"no commit":     {Version: "1.0.0", BuildDate: "2026-11-02T12:00:00Z"},
		"no build date": {Version: "1.0.0", Commit: "abc"},
	} {
		if err := meta.Validate(); err == nil {
			t.Errorf("Validate() with %s = nil, want an error", name)
		}
	}
}

// TestReadVersion pins the version file conventions.
//
// The version must not carry a leading "v": the tag carries the "v" and
// the version does not, so that the two are never confused for one
// another.
func TestReadVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, VersionFile), []byte("1.2.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadVersion(dir)
	if err != nil {
		t.Fatalf("ReadVersion: %v", err)
	}
	if got != "1.2.3" {
		t.Errorf("ReadVersion() = %q, want %q", got, "1.2.3")
	}
}

// TestReadVersionRejectsBadVersions covers the inputs that would produce
// a confusing release.
func TestReadVersionRejectsBadVersions(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"leading v":   "v1.2.3",
		"only spaces": "   \n",
	}
	for name, content := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, VersionFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadVersion(dir); err == nil {
			t.Errorf("ReadVersion() with %s = nil, want an error", name)
		}
	}
}

// TestReadVersionMissingFile reports a missing version file rather than
// silently defaulting.
func TestReadVersionMissingFile(t *testing.T) {
	if _, err := ReadVersion(t.TempDir()); err == nil {
		t.Fatal("ReadVersion() with no version file = nil, want an error")
	}
}

// TestBuildInjectsMetadata is the end-to-end check that the linker flags
// actually land in the binary.
//
// It is the only check that catches a typo in a version variable name:
// the linker ignores an -X that names a symbol which does not exist, so
// a broken path would otherwise produce a binary that quietly reports
// the development defaults.
func TestBuildInjectsMetadata(t *testing.T) {
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
	dist := t.TempDir()

	target := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
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
	if err := VerifyCommitEmbedded(artifact, meta); err != nil {
		t.Fatalf("VerifyCommitEmbedded: %v", err)
	}

	// The binary must report the metadata it was built with.
	out, err := exec.Command(artifact, "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s -version: %v\n%s", artifact, err, out)
	}
	text := string(out)
	for _, want := range []string{meta.Version, meta.Commit, meta.BuildDate} {
		if !strings.Contains(text, want) {
			t.Errorf("%s -version = %q, want it to contain %q", artifact, text, want)
		}
	}
}

// TestBuildIsReproducible checks that two builds of the same commit
// produce the same artifact.
//
// The build is reproducible by construction: the build date is the
// commit timestamp, the paths are trimmed and the build id is empty. A
// change that breaks that would make a release impossible to verify.
func TestBuildIsReproducible(t *testing.T) {
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

	first := t.TempDir()
	second := t.TempDir()
	for _, dist := range []string{first, second} {
		err := Build(BuildOptions{
			Root:     root,
			Dist:     dist,
			Metadata: meta,
			Targets:  []Target{target},
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
	}

	firstHash, err := FileSHA256(filepath.Join(first, ArtifactName(target)))
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := FileSHA256(filepath.Join(second, ArtifactName(target)))
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash {
		t.Errorf("two builds of the same commit differ: %s != %s", firstHash, secondHash)
	}
}

// repoRoot returns the repository root, which is the parent of the
// directory holding this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/release -> internal -> repository root
	return filepath.Dir(filepath.Dir(wd))
}
