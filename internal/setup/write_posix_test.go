//go:build !windows

package setup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteCreatesAnOwnerOnlyFile covers the mode guarantee, including a
// process umask of zero, which would otherwise widen the mode of a file
// created without an explicit setting.
func TestWriteCreatesAnOwnerOnlyFile(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	// The test directory itself is created under the current umask, so it
	// is world-writable while the umask is zero. Use a private subdirectory
	// so the destination directory is the safe one the check requires.
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if _, err := writeConfig(path, []byte("content"), false); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
}
