package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSHA256Sums covers the format sha256sum(1) and shasum(1)
// produce.
func TestParseSHA256Sums(t *testing.T) {
	const digest = "601b3f7ccc519815fc321fd1ab6ba8cb424f6c26acbe66417ed66c10c306bb1f"
	data := digest + "  themailroom-linux-amd64\n" +
		digest + "  themailroom-linux-arm64\n"

	got, err := ParseSHA256Sums([]byte(data))
	if err != nil {
		t.Fatalf("ParseSHA256Sums: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(got))
	}
	if got[0].Hash != digest {
		t.Errorf("entries[0].Hash = %q, want %q", got[0].Hash, digest)
	}
	if got[0].Filename != "themailroom-linux-amd64" {
		t.Errorf("entries[0].Filename = %q, want %q", got[0].Filename, "themailroom-linux-amd64")
	}
}

// TestParseSHA256SumsAcceptsBinaryMarker covers the "*" marker that
// sha256sum(1) writes for a binary-mode digest.
func TestParseSHA256SumsAcceptsBinaryMarker(t *testing.T) {
	const digest = "601b3f7ccc519815fc321fd1ab6ba8cb424f6c26acbe66417ed66c10c306bb1f"
	got, err := ParseSHA256Sums([]byte(digest + " *themailroom-linux-amd64\n"))
	if err != nil {
		t.Fatalf("ParseSHA256Sums: %v", err)
	}
	if len(got) != 1 || got[0].Filename != "themailroom-linux-amd64" {
		t.Fatalf("entries = %+v, want one entry named themailroom-linux-amd64", got)
	}
}

// TestParseSHA256SumsSkipsComments covers a checksum file with blank
// lines and comments.
func TestParseSHA256SumsSkipsComments(t *testing.T) {
	const digest = "601b3f7ccc519815fc321fd1ab6ba8cb424f6c26acbe66417ed66c10c306bb1f"
	data := "# a comment\n\n" + digest + "  themailroom-linux-amd64\n\n"
	got, err := ParseSHA256Sums([]byte(data))
	if err != nil {
		t.Fatalf("ParseSHA256Sums: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(got))
	}
}

// TestParseSHA256SumsRejectsMalformedInput covers the inputs a lenient
// parser would accept and silently get wrong.
func TestParseSHA256SumsRejectsMalformedInput(t *testing.T) {
	const digest = "601b3f7ccc519815fc321fd1ab6ba8cb424f6c26acbe66417ed66c10c306bb1f"
	cases := map[string]string{
		"no separator":    digest + " themailroom-linux-amd64\n",
		"short digest":    "601b3f  themailroom-linux-amd64\n",
		"not hexadecimal": "zz1b3f7ccc519815fc321fd1ab6ba8cb424f6c26acbe66417ed66c10c306bb1f  themailroom\n",
		"empty name":      digest + "  \n",
		"path in name":    digest + "  ../themailroom-linux-amd64\n",
		"absolute path":   digest + "  /tmp/themailroom\n",
	}
	for name, data := range cases {
		if _, err := ParseSHA256Sums([]byte(data)); err == nil {
			t.Errorf("ParseSHA256Sums() with %s = nil, want an error", name)
		}
	}
}

// TestWriteSHA256SumsRoundTrip checks that writing and parsing a
// checksum file preserves the entries.
func TestWriteSHA256SumsRoundTrip(t *testing.T) {
	entries := []SHA256Sum{
		{Hash: "601b3f7ccc519815fc321fd1ab6ba8cb424f6c26acbe66417ed66c10c306bb1f", Filename: "themailroom-linux-amd64"},
		{Hash: "02afd0bbcb0152d6dcaf6a4fa9acdc4d7a7bdb791d8de6188d2e9b1731183311", Filename: "themailroom-linux-arm64"},
	}
	got, err := ParseSHA256Sums(WriteSHA256Sums(entries))
	if err != nil {
		t.Fatalf("ParseSHA256Sums: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("len(entries) = %d, want %d", len(got), len(entries))
	}
	for i := range entries {
		if got[i] != entries[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, got[i], entries[i])
		}
	}
}

// TestWriteSHA256SumsIsSortedAndStable pins that the checksum file is
// reproducible: the same artifacts always produce the same bytes.
func TestWriteSHA256SumsIsSortedAndStable(t *testing.T) {
	entries := []SHA256Sum{
		{Hash: "b", Filename: "themailroom-linux-arm64"},
		{Hash: "a", Filename: "themailroom-linux-amd64"},
		{Hash: "c", Filename: "themailroom-darwin-arm64"},
	}
	first := string(WriteSHA256Sums(entries))
	second := string(WriteSHA256Sums(entries))
	if first != second {
		t.Error("WriteSHA256Sums() is not stable across calls")
	}
	lines := strings.Split(strings.TrimSpace(first), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %v, want 3", lines)
	}
	if !strings.HasSuffix(lines[0], "themailroom-darwin-arm64") {
		t.Errorf("lines[0] = %q, want the entries sorted by file name", lines[0])
	}
}

// TestVerifySHA256Sums checks the happy path and the failure it exists
// to catch.
func TestVerifySHA256Sums(t *testing.T) {
	dir := t.TempDir()
	const content = "artifact contents"
	if err := os.WriteFile(filepath.Join(dir, "themailroom-linux-amd64"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := FileSHA256(filepath.Join(dir, "themailroom-linux-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	entries := []SHA256Sum{{Hash: hash, Filename: "themailroom-linux-amd64"}}

	if err := VerifySHA256Sums(dir, entries); err != nil {
		t.Fatalf("VerifySHA256Sums: %v", err)
	}

	// A changed artifact must be reported.
	if err := os.WriteFile(filepath.Join(dir, "themailroom-linux-amd64"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifySHA256Sums(dir, entries); err == nil {
		t.Error("VerifySHA256Sums() with a tampered artifact = nil, want an error")
	}
}

// TestVerifySHA256SumsRejectsMissingFile reports a checksum entry whose
// file is absent, rather than passing it.
func TestVerifySHA256SumsRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	entries := []SHA256Sum{{
		Hash:     "601b3f7ccc519815fc321fd1ab6ba8cb424f6c26acbe66417ed66c10c306bb1f",
		Filename: "themailroom-linux-amd64",
	}}
	if err := VerifySHA256Sums(dir, entries); err == nil {
		t.Error("VerifySHA256Sums() with a missing artifact = nil, want an error")
	}
}

// TestVerifySHA256SumsRejectsEmptyFile reports an empty checksum file,
// which verifies nothing.
func TestVerifySHA256SumsRejectsEmptyFile(t *testing.T) {
	if err := VerifySHA256Sums(t.TempDir(), nil); err == nil {
		t.Error("VerifySHA256Sums() with no entries = nil, want an error")
	}
}

// TestVerifySHA256SumsRejectsDuplicateNames reports a checksum file that
// names the same file twice, which would hide a missing artifact.
func TestVerifySHA256SumsRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	const content = "artifact contents"
	if err := os.WriteFile(filepath.Join(dir, "themailroom-linux-amd64"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := FileSHA256(filepath.Join(dir, "themailroom-linux-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	entries := []SHA256Sum{
		{Hash: hash, Filename: "themailroom-linux-amd64"},
		{Hash: hash, Filename: "themailroom-linux-amd64"},
	}
	if err := VerifySHA256Sums(dir, entries); err == nil {
		t.Error("VerifySHA256Sums() with a duplicate name = nil, want an error")
	}
}

// TestFileSHA256 pins the digest against the known value for a fixed
// input, so that a change in the digest computation is caught.
func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Errorf("FileSHA256() = %q, want %q", got, want)
	}
}
