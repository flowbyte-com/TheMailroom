package release

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseVersion covers the versions the release workflow accepts.
//
// A stable version and a release candidate must both be accepted, and a
// malformed version must be refused. The refusal matters as much as the
// acceptance: a version the tooling cannot reason about must never
// reach a release.
func TestParseVersion(t *testing.T) {
	cases := []struct {
		version string
		want    Version
	}{
		{version: "1.0.0", want: Version{Major: 1, Minor: 0, Patch: 0}},
		{version: "0.1.0", want: Version{Major: 0, Minor: 1, Patch: 0}},
		{version: "1.2.3", want: Version{Major: 1, Minor: 2, Patch: 3}},
		{version: "10.20.30", want: Version{Major: 10, Minor: 20, Patch: 30}},
		{
			version: "1.0.0-rc.1",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "rc.1"},
		},
		{
			version: "1.0.0-rc1",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "rc1"},
		},
		{
			version: "1.0.0-alpha",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "alpha"},
		},
		{
			version: "1.0.0-alpha.1",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "alpha.1"},
		},
		{
			version: "1.0.0-0.3.7",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "0.3.7"},
		},
		{
			version: "1.0.0-x-y-z.--",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "x-y-z.--"},
		},
		{
			version: "1.0.0+build.1",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Build: "build.1"},
		},
		{
			version: "1.0.0-rc.1+build.1",
			want:    Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "rc.1", Build: "build.1"},
		},
	}
	for _, tc := range cases {
		got, err := ParseVersion(tc.version)
		if err != nil {
			t.Errorf("ParseVersion(%q) = %v, want no error", tc.version, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", tc.version, got, tc.want)
		}
	}
}

// TestParseVersionRejectsMalformedVersions covers the versions that must
// be refused.
//
// The rules are the ones the Semantic Versioning specification states:
// the numeric identifiers must not carry leading zeros, and a prerelease
// identifier that is entirely numeric must not carry one either.
func TestParseVersionRejectsMalformedVersions(t *testing.T) {
	cases := []string{
		"",
		"v1.0.0",
		"1.0",
		"1",
		"1.0.0.0",
		"1.0.x",
		"01.0.0",
		"1.00.0",
		"1.0.00",
		"1.0.0-",
		"1.0.0-rc..1",
		"1.0.0-rc.1.",
		"1.0.0-01",
		"1.0.0-rc.01",
		"1.0.0+",
		"1.0.0+build..1",
		"1.0.0+build!",
		"1.0.0 rc",
		"1.0.0-rc.1+",
	}
	for _, version := range cases {
		if _, err := ParseVersion(version); err == nil {
			t.Errorf("ParseVersion(%q) = nil, want an error", version)
		}
	}
}

// TestVersionIsPrerelease pins the distinction the release workflow
// relies on: a release candidate must be published as a prerelease, and
// a stable version must not be.
func TestVersionIsPrerelease(t *testing.T) {
	cases := map[string]bool{
		"1.0.0":            false,
		"1.0.0+build.1":    false,
		"1.0.0-rc.1":       true,
		"1.0.0-rc1":        true,
		"1.0.0-alpha":      true,
		"1.0.0-rc.1+build": true,
	}
	for version, want := range cases {
		got, err := ParseVersion(version)
		if err != nil {
			t.Errorf("ParseVersion(%q) = %v, want no error", version, err)
			continue
		}
		if got.IsPrerelease() != want {
			t.Errorf("ParseVersion(%q).IsPrerelease() = %v, want %v", version, got.IsPrerelease(), want)
		}
	}
}

// TestVersionStringRoundTrip pins that a parsed version renders back to
// the text it was parsed from.
func TestVersionStringRoundTrip(t *testing.T) {
	for _, version := range []string{"1.0.0", "1.0.0-rc.1", "1.0.0+build.1", "1.0.0-rc.1+build.1"} {
		got, err := ParseVersion(version)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", version, err)
		}
		if got.String() != version {
			t.Errorf("ParseVersion(%q).String() = %q, want %q", version, got.String(), version)
		}
	}
}

// TestReadVersionAcceptsAReleaseCandidate covers the version file the
// release workflow reads.
func TestReadVersionAcceptsAReleaseCandidate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, VersionFile), []byte("1.0.0-rc.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadVersion(dir)
	if err != nil {
		t.Fatalf("ReadVersion: %v", err)
	}
	if got != "1.0.0-rc.1" {
		t.Errorf("ReadVersion() = %q, want %q", got, "1.0.0-rc.1")
	}

	info, err := ReadVersionInfo(dir)
	if err != nil {
		t.Fatalf("ReadVersionInfo: %v", err)
	}
	if !info.IsPrerelease() {
		t.Error("ReadVersionInfo().IsPrerelease() = false, want true")
	}
}

// TestReadVersionRejectsMalformedVersions covers the version files that
// must be refused.
//
// A malformed VERSION file has to fail at the point it is read, not
// somewhere later in the release.
func TestReadVersionRejectsMalformedVersions(t *testing.T) {
	for _, content := range []string{"", "v1.0.0", "1.0", "1.0.0-", "01.0.0", "1.0.0-rc..1"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, VersionFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadVersion(dir); err == nil {
			t.Errorf("ReadVersion() with %q = nil, want an error", content)
		}
	}
}
