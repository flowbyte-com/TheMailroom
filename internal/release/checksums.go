package release

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SHA256SumsFile is the name of the checksum file written next to the
// release artifacts. It is the conventional name, so that a user who
// already knows how to verify a download does not have to learn a new
// convention.
const SHA256SumsFile = "SHA256SUMS"

// SHA256Sum is one entry of a SHA256SUMS file.
type SHA256Sum struct {
	// Hash is the lowercase hexadecimal SHA-256 digest.
	Hash string
	// Filename is the name of the file the digest covers, relative to the
	// directory that holds the checksum file.
	Filename string
}

// ParseSHA256Sums parses a SHA256SUMS file.
//
// The format is the one produced by sha256sum(1) and shasum(1): a
// hexadecimal digest, two spaces, and the file name. A digest terminated
// by a single "*" marks a binary-mode file; that marker is accepted and
// ignored, because it does not affect the digest.
//
// A line that is blank or begins with "#" is a comment and is skipped.
// Every other line must be well formed: a checksum file that is read
// leniently is a checksum file that can be silently wrong.
func ParseSHA256Sums(data []byte) ([]SHA256Sum, error) {
	var out []SHA256Sum
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		// The digest is the first field. It is followed by exactly two
		// characters that say how the digest was computed: two spaces
		// for a text-mode digest and a space followed by "*" for a
		// binary-mode digest. Anything else is malformed.
		digest, rest, found := strings.Cut(text, " ")
		if !found {
			return nil, fmt.Errorf("release: malformed %s line %d: %q", SHA256SumsFile, line, text)
		}
		if len(rest) < 2 || (rest[0] != ' ' && rest[0] != '*') {
			return nil, fmt.Errorf("release: malformed %s line %d: %q", SHA256SumsFile, line, text)
		}
		digest = strings.TrimSuffix(digest, "*")
		if len(digest) != sha256.Size*2 {
			return nil, fmt.Errorf("release: %s line %d: digest is not %d hex characters: %q",
				SHA256SumsFile, line, sha256.Size*2, digest)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, fmt.Errorf("release: %s line %d: digest is not hexadecimal: %q",
				SHA256SumsFile, line, digest)
		}
		name := strings.TrimPrefix(rest[1:], "*")
		if name == "" {
			return nil, fmt.Errorf("release: %s line %d: file name is empty", SHA256SumsFile, line)
		}
		if strings.ContainsAny(name, "/\\") {
			return nil, fmt.Errorf("release: %s line %d: file name must not contain a path separator: %q",
				SHA256SumsFile, line, name)
		}
		out = append(out, SHA256Sum{
			Hash:     strings.ToLower(digest),
			Filename: name,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("release: read %s: %w", SHA256SumsFile, err)
	}
	return out, nil
}

// WriteSHA256Sums renders the entries in the sha256sum format.
//
// The entries are sorted by file name, so the file is byte-for-byte
// reproducible for a given set of artifacts.
func WriteSHA256Sums(entries []SHA256Sum) []byte {
	sorted := make([]SHA256Sum, len(entries))
	copy(sorted, entries)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Filename < sorted[j-1].Filename; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	var b strings.Builder
	for _, e := range sorted {
		fmt.Fprintf(&b, "%s  %s\n", e.Hash, e.Filename)
	}
	return []byte(b.String())
}

// FileSHA256 returns the SHA-256 digest of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifySHA256Sums checks every entry against the file it names in dir.
//
// A missing file, a file whose content differs and a checksum file that
// names nothing are all errors: a checksum file that verifies "some of
// the artifacts" is not a verification.
func VerifySHA256Sums(dir string, entries []SHA256Sum) error {
	if len(entries) == 0 {
		return fmt.Errorf("release: %s is empty", SHA256SumsFile)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if seen[e.Filename] {
			return fmt.Errorf("release: %s names %q twice", SHA256SumsFile, e.Filename)
		}
		seen[e.Filename] = true

		path := filepath.Join(dir, e.Filename)
		got, err := FileSHA256(path)
		if err != nil {
			return fmt.Errorf("release: %s: %w", e.Filename, err)
		}
		if got != e.Hash {
			return fmt.Errorf("release: %s: checksum mismatch: got %s, want %s", e.Filename, got, e.Hash)
		}
	}
	return nil
}
