package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrExists is returned when the destination already exists and the caller
// did not ask to replace it.
var ErrExists = errors.New("setup: destination already exists")

// ErrSymlink is returned when the destination is a symbolic link.
//
// Writing through it would modify a file the operator did not name, so it
// is refused even when the caller asked to replace an existing file.
var ErrSymlink = errors.New("setup: destination is a symbolic link")

// isWindows reports whether the process is running on Windows. The write
// path uses it to skip checks that only mean something on POSIX.
func isWindows() bool { return runtime.GOOS == "windows" }

// writePolicy is the platform confidentiality gate. It is a variable so the
// filesystem tests can exercise the POSIX write path on any host, and so the
// refusal path can be tested without a Windows machine.
var writePolicy = platformWritePolicy

// writeConfig writes data to path and returns the absolute path written.
//
// The strategy avoids every failure mode that would matter here:
//
//   - The destination is inspected with lstat, so a symbolic link is seen
//     for what it is rather than followed.
//   - An existing file is refused unless force is set.
//   - The data is written to a temporary file in the destination directory,
//     so the final step is a same-directory publish rather than a copy
//     across filesystems.
//   - The temporary file is created with mode 0600 and chmod-ed to 0600, so
//     the result does not depend on the process umask. On Windows mode bits
//     are not an ACL; the caller is told so separately.
//   - The publish does not overwrite. On POSIX a hard link is used, which is
//     atomic and fails if the destination exists. Filesystems without hard
//     links fall back to an exclusive create, which also cannot overwrite.
//   - The temporary file is removed if anything fails.
func writeConfig(path string, data []byte, force bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("setup: no destination path was given")
	}
	// A platform that cannot guarantee the file's confidentiality is
	// refused before anything is inspected or created.
	if err := writePolicy(runtime.GOOS); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("setup: resolve %s: %w", path, err)
	}
	dir := filepath.Dir(abs)

	// Inspect the final component without following a symlink.
	if info, statErr := os.Lstat(abs); statErr == nil {
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("%w: %s; remove it or choose another path", ErrSymlink, abs)
		case !info.Mode().IsRegular():
			return "", fmt.Errorf("setup: %s exists and is not a regular file; choose another path", abs)
		case !force:
			return "", fmt.Errorf("%w: %s; choose another path or pass -force", ErrExists, abs)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("setup: inspect %s: %w", abs, statErr)
	}

	// The parent must be a directory. A symlinked parent is accepted: it is
	// a normal layout and the operator named the final path.
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("setup: the directory %s is not usable: %w", dir, err)
	}
	if !dirInfo.IsDir() {
		return "", fmt.Errorf("setup: %s is not a directory", dir)
	}
	// A world-writable directory without the sticky bit lets any local user
	// replace the file after it is created. A sticky directory such as /tmp
	// does not, so it is allowed.
	if !isWindows() && dirInfo.Mode().Perm()&0o002 != 0 && dirInfo.Mode()&os.ModeSticky == 0 {
		return "", fmt.Errorf(
			"setup: the directory %s is writable by any user and is not sticky; "+
				"choose a directory only you can write to", dir)
	}
	// Replacing an existing file renames over the destination entry. That is
	// only safe when no other user can change the entry between the
	// inspection above and the rename, so forced replacement is confined to
	// a directory that group and other users cannot write to.
	if force && !isWindows() && dirInfo.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf(
			"setup: -force is only allowed in a directory that group and other users cannot write to; "+
				"remove %s yourself or choose another path", abs)
	}

	tmp, err := os.CreateTemp(dir, ".themailroom-setup-*")
	if err != nil {
		return "", fmt.Errorf("setup: create a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		// The temporary name is removed on every path. After a link
		// publish it still exists as a second name for the same file;
		// after a rename it is already gone and the removal is a no-op.
		_ = os.Remove(tmpName)
	}()

	// CreateTemp already uses 0600, but the mode is set explicitly so the
	// guarantee does not depend on the standard library's default.
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("setup: restrict the temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return "", fmt.Errorf("setup: write the configuration: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("setup: flush the configuration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("setup: close the temporary file: %w", err)
	}

	if force {
		// The destination was inspected above. On POSIX rename replaces it
		// atomically; on Windows it is replaced by MoveFileEx.
		if err := os.Rename(tmpName, abs); err != nil {
			return "", fmt.Errorf("setup: replace %s: %w", abs, err)
		}
		return abs, nil
	}

	if err := publish(tmpName, abs, data); err != nil {
		return "", err
	}
	return abs, nil
}

// publish is the publish step, replaceable in tests to exercise the cleanup
// path when publishing fails.
var publish = publishExclusive

// publishExclusive publishes the temporary file at dst without overwriting.
//
// A hard link is the primary mechanism: it is atomic, it never follows a
// symbolic link at dst, and it fails if dst exists. Filesystems that do not
// support hard links fall back to an exclusive create, which also refuses
// to overwrite, though it is not atomic.
func publishExclusive(tmpName, dst string, data []byte) error {
	linkErr := os.Link(tmpName, dst)
	switch {
	case linkErr == nil:
		return nil
	case errors.Is(linkErr, fs.ErrExist):
		return fmt.Errorf("%w: %s; choose another path or pass -force", ErrExists, dst)
	}
	if err := writeExclusive(dst, data); err != nil {
		return err
	}
	return nil
}

// writeExclusive creates dst with an exclusive create and writes data.
func writeExclusive(dst string, data []byte) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s; choose another path or pass -force", ErrExists, dst)
		}
		return fmt.Errorf("setup: create %s: %w", dst, err)
	}
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = os.Remove(dst)
		}
	}()

	// The create mode is filtered by the umask; setting it explicitly keeps
	// the guarantee independent of the process environment.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("setup: restrict %s: %w", dst, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("setup: write %s: %w", dst, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("setup: flush %s: %w", dst, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("setup: close %s: %w", dst, err)
	}
	remove = false
	return nil
}
