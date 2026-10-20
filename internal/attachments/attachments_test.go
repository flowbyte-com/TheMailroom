package attachments

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustResolver(t *testing.T, paths []string, limits Limits) *Resolver {
	t.Helper()
	r, err := NewResolver(paths, limits)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestResolveMultipleFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "one.txt"), []byte("one"))
	mustWrite(t, filepath.Join(root, "sub", "two.bin"), []byte{0x00, 0x01, 0x02})

	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          3,
	})
	got, err := r.Resolve([]string{"one.txt", "sub/two.bin"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Filename != "one.txt" || string(got[0].Data) != "one" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Filename != "two.bin" || !bytes.Equal(got[1].Data, []byte{0, 1, 2}) {
		t.Errorf("second = %+v", got[1])
	}
}

func TestResolverRejectsInvalidReferences(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "ok.txt"), []byte("ok"))
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          3,
	})

	for _, ref := range []string{
		"",
		"   ",
		"/etc/passwd",
		"../outside.txt",
		"sub/../../outside.txt",
		`..\outside.txt`,
		`C:\Windows\win.ini`,
		"C:/Windows/win.ini",
		"bad\x00name",
		"bad\nname",
	} {
		_, err := r.Resolve([]string{ref})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Resolve(%q) err = %v, want ErrInvalidPath", ref, err)
		}
	}
}

func TestResolverRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	mustWrite(t, outside, []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires privileges on Windows")
		}
		t.Fatal(err)
	}

	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      100,
		MaxTotalAttachmentBytes: 100,
		MaxAttachments:          3,
	})
	got, err := r.Resolve([]string{"link.txt"})
	if err == nil {
		t.Fatalf("expected symlink escape to fail, got %+v", got)
	}
	if !errors.Is(err, ErrInvalidPath) && !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrInvalidPath or ErrNotFound", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("secret")) {
		t.Errorf("error leaked target content/path: %q", err)
	}
}

func TestResolverRejectsAmbiguousRoots(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	mustWrite(t, filepath.Join(rootA, "same.txt"), []byte("a"))
	mustWrite(t, filepath.Join(rootB, "same.txt"), []byte("b"))

	r := mustResolver(t, []string{rootA, rootB}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          3,
	})
	_, err := r.Resolve([]string{"same.txt"})
	if !errors.Is(err, ErrAmbiguous) {
		t.Errorf("err = %v, want ErrAmbiguous", err)
	}
	if strings.Contains(err.Error(), rootA) || strings.Contains(err.Error(), rootB) {
		t.Errorf("ambiguity error leaked a configured root: %q", err)
	}
}

func TestResolverRejectsNonRegularFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          3,
	})
	_, err := r.Resolve([]string{"dir"})
	if !errors.Is(err, ErrNotRegular) {
		t.Errorf("err = %v, want ErrNotRegular", err)
	}
}

func TestResolverPerFileLimit(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "exact"), []byte("1234"))
	mustWrite(t, filepath.Join(root, "over"), []byte("12345"))
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      4,
		MaxTotalAttachmentBytes: 100,
		MaxAttachments:          3,
	})
	got, err := r.Resolve([]string{"exact"})
	if err != nil {
		t.Fatalf("exact size rejected: %v", err)
	}
	if string(got[0].Data) != "1234" {
		t.Errorf("data = %q", got[0].Data)
	}
	_, err = r.Resolve([]string{"over"})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestResolverTotalLimit(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a"), []byte("1234"))
	mustWrite(t, filepath.Join(root, "b"), []byte("5678"))

	exact := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      4,
		MaxTotalAttachmentBytes: 8,
		MaxAttachments:          3,
	})
	got, err := exact.Resolve([]string{"a", "b"})
	if err != nil {
		t.Fatalf("exact total rejected: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d files, want 2", len(got))
	}

	over := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      4,
		MaxTotalAttachmentBytes: 7,
		MaxAttachments:          3,
	})
	_, err = over.Resolve([]string{"a", "b"})
	if !errors.Is(err, ErrTotalTooLarge) {
		t.Errorf("err = %v, want ErrTotalTooLarge", err)
	}
}

func TestResolverAttachmentCountLimit(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a"), []byte("a"))
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          1,
	})
	if _, err := r.Resolve([]string{"a"}); err != nil {
		t.Fatalf("exact attachment count rejected: %v", err)
	}
	_, err := r.Resolve([]string{"a", "a"})
	if !errors.Is(err, ErrTooMany) {
		t.Errorf("err = %v, want ErrTooMany", err)
	}
}

func TestResolverMissingAndNoRoots(t *testing.T) {
	root := t.TempDir()
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          3,
	})
	_, err := r.Resolve([]string{"missing.txt"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("error leaked configured root: %q", err)
	}

	if _, err := NewResolver(nil, Limits{MaxAttachmentBytes: 1, MaxTotalAttachmentBytes: 1, MaxAttachments: 1}); !errors.Is(err, ErrNoRoots) {
		t.Errorf("NewResolver(nil) err = %v, want ErrNoRoots", err)
	}
}

func TestResolverFreezesContent(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	mustWrite(t, file, []byte("before"))
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      20,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          1,
	})
	got, err := r.Resolve([]string{"file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, file, []byte("after"))
	if string(got[0].Data) != "before" {
		t.Errorf("resolved data changed after file mutation: %q", got[0].Data)
	}
}

func TestResolverCloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "file.txt"), []byte("x"))
	r, err := NewResolver([]string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve([]string{"file.txt"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Resolve after Close err = %v, want ErrClosed", err)
	}
}

func TestResolverDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "file.txt"), []byte("x"))
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          1,
	})
	if _, err := r.Resolve([]string{"file.txt"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("resolver changed root entries: before=%d after=%d", len(before), len(after))
	}
}

// growingReader deliberately provides more bytes than any metadata would
// suggest; readBounded must enforce the limit on actual bytes, not on a
// prior size check.
type growingReader struct {
	data []byte
	pos  int
}

func (r *growingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestReadBoundedActualBytes(t *testing.T) {
	got, err := readBounded(&growingReader{data: []byte("1234")}, 4)
	if err != nil {
		t.Fatalf("exact size: %v", err)
	}
	if string(got) != "1234" {
		t.Errorf("data = %q", got)
	}
	if _, err := readBounded(&growingReader{data: []byte("12345")}, 4); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize err = %v, want ErrTooLarge", err)
	}
	if _, err := readBounded(&growingReader{data: []byte("x")}, 0); !errors.Is(err, ErrInvalidLimits) {
		t.Errorf("zero limit err = %v, want ErrInvalidLimits", err)
	}
}

func TestNewResolverRejectsBadRoots(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	mustWrite(t, file, []byte("x"))

	cases := []struct {
		name  string
		paths []string
	}{
		{name: "relative", paths: []string{"relative/path"}},
		{name: "file", paths: []string{file}},
		{name: "missing", paths: []string{filepath.Join(root, "missing")}},
		{name: "duplicate", paths: []string{root, root}},
		{name: "control", paths: []string{root + "\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewResolver(tc.paths, Limits{
				MaxAttachmentBytes:      10,
				MaxTotalAttachmentBytes: 20,
				MaxAttachments:          1,
			}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
