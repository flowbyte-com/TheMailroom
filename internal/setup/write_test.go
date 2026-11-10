package setup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMain lets the filesystem tests exercise the write path on any host.
//
// The real platform policy is tested directly, and the refusal path is
// tested by injecting it, so overriding it here does not hide the Windows
// behaviour.
func TestMain(m *testing.M) {
	writePolicy = func(string) error { return nil }
	os.Exit(m.Run())
}

// TestWriteRefusesAnExistingFile covers the no-overwrite default.
func TestWriteRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConfig(path, []byte("replacement"), false); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Error("the existing file was modified")
	}
}

// TestWriteRefusesASymlink covers the symlink overwrite attack: the target
// must not be touched, even with force.
func TestWriteRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}

	for _, force := range []bool{false, true} {
		if _, err := writeConfig(link, []byte("replacement"), force); !errors.Is(err, ErrSymlink) {
			t.Fatalf("force=%v: err = %v, want ErrSymlink", force, err)
		}
	}
	data, _ := os.ReadFile(target)
	if string(data) != "target" {
		t.Error("the symlink target was modified")
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
}

// TestWriteForceReplacesARegularFile covers the explicit override.
func TestWriteForceReplacesARegularFile(t *testing.T) {
	// Forced replacement is confined to a directory group and other users
	// cannot write to.
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	written, err := writeConfig(path, []byte("replacement"), true)
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	if written != path {
		t.Errorf("written = %q, want %q", written, path)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "replacement" {
		t.Errorf("content = %q, want replacement", data)
	}
}

// TestWriteRefusesADirectory covers a destination that is not a regular
// file.
func TestWriteRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adir")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConfig(path, []byte("x"), true); err == nil {
		t.Fatal("writeConfig accepted a directory destination")
	}
}

// TestWriteRefusesAMissingParent covers an unusable destination directory.
func TestWriteRefusesAMissingParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "config.yaml")
	if _, err := writeConfig(path, []byte("x"), false); err == nil {
		t.Fatal("writeConfig accepted a missing parent directory")
	}
}

// TestWriteCleansUpOnPublishFailure covers temporary-file cleanup when the
// publish step fails after the file has been written.
func TestWriteCleansUpOnPublishFailure(t *testing.T) {
	dir := t.TempDir()
	original := publish
	publish = func(string, string, []byte) error { return errors.New("publish failed") }
	t.Cleanup(func() { publish = original })

	path := filepath.Join(dir, "config.yaml")
	if _, err := writeConfig(path, []byte("content"), false); err == nil {
		t.Fatal("writeConfig succeeded with a failing publish")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("temporary files were not cleaned up: %v", names)
	}
}

// TestWriteLeavesNoTemporaryFiles covers the success path.
func TestWriteLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if _, err := writeConfig(path, []byte("content"), false); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %v, want only the configuration", names)
	}
}

// TestPublishExclusiveRefusesAnExistingFile covers the publish primitive.
func TestPublishExclusiveRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishExclusive(tmp, dst, []byte("x")); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "existing" {
		t.Error("the destination was overwritten")
	}
}

// TestWriteExclusiveRefusesAnExistingFile covers the fallback primitive.
func TestWriteExclusiveRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(dst, []byte("x")); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
}

// TestWriteRefusesAnEmptyPath covers the argument check.
func TestWriteRefusesAnEmptyPath(t *testing.T) {
	if _, err := writeConfig("   ", []byte("x"), false); err == nil {
		t.Fatal("writeConfig accepted an empty path")
	}
}

// TestWriteReportsTheAbsolutePath covers path resolution.
func TestWriteReportsTheAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	rel, err := filepath.Rel(mustGetwd(t), filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Skipf("cannot make a relative path: %v", err)
	}
	written, err := writeConfig(rel, []byte("x"), false)
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	if !filepath.IsAbs(written) {
		t.Errorf("written = %q, want an absolute path", written)
	}
	if !strings.HasSuffix(written, "config.yaml") {
		t.Errorf("written = %q", written)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// TestWriteRefusesAWorldWritableDirectory covers a destination directory
// any local user can write to and replace files in.
func TestWriteRefusesAWorldWritableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions do not apply on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Skipf("cannot make the directory world-writable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	path := filepath.Join(dir, "config.yaml")
	if _, err := writeConfig(path, []byte("x"), false); err == nil {
		t.Fatal("writeConfig accepted a world-writable destination directory")
	} else if !strings.Contains(err.Error(), "writable by any user") {
		t.Errorf("err = %v, want a clear explanation", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("a file was created in an unsafe directory")
	}
}

// TestForceRefusedInAGroupWritableDirectory covers the forced-replacement
// constraint: another user who can write to the directory could change the
// destination entry between the inspection and the rename.
func TestForceRefusedInAGroupWritableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions do not apply on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Skipf("cannot change the directory mode: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConfig(path, []byte("replacement"), true); err == nil {
		t.Fatal("forced replacement was allowed in a group-writable directory")
	} else if !strings.Contains(err.Error(), "group and other users") {
		t.Errorf("err = %v, want a clear explanation", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Error("the existing file was modified")
	}
}

// TestWriteRefusesWhenThePlatformCannotGuaranteeConfidentiality covers the
// policy gate inside the write path: nothing is inspected or created when
// the platform cannot assure the file's confidentiality.
func TestWriteRefusesWhenThePlatformCannotGuaranteeConfidentiality(t *testing.T) {
	original := writePolicy
	writePolicy = func(string) error { return errWindowsACL }
	t.Cleanup(func() { writePolicy = original })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if _, err := writeConfig(path, []byte("x"), false); !errors.Is(err, errWindowsACL) {
		t.Fatalf("err = %v, want errWindowsACL", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("a file was created despite the platform refusal")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refusal left files behind: %v", entries)
	}
}
