// Package attachments provides the confined filesystem resolver used for
// outbound attachments.
//
// Agent input is always a relative path. Each operator-configured root is
// opened once as an *os.Root; every read goes through one of those roots so a
// symlink cannot escape the configured directory. Resolved host paths are
// intentionally not exposed to callers.
package attachments

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

var (
	// ErrNoRoots is returned when no attachment roots are configured.
	ErrNoRoots = errors.New("attachments: no roots configured")

	// ErrInvalidLimits is returned for non-positive resolver limits.
	ErrInvalidLimits = errors.New("attachments: invalid limits")

	// ErrClosed is returned after the resolver has been closed.
	ErrClosed = errors.New("attachments: resolver closed")

	// ErrInvalidPath is returned for an agent-supplied reference that is
	// not a simple relative path or cannot be resolved safely.
	ErrInvalidPath = errors.New("attachments: invalid path")

	// ErrNotFound is returned when no configured root contains the
	// requested path.
	ErrNotFound = errors.New("attachments: attachment not found")

	// ErrAmbiguous is returned when more than one configured root
	// contains the same relative path.
	ErrAmbiguous = errors.New("attachments: ambiguous attachment path")

	// ErrNotRegular is returned for directories, devices, sockets, FIFOs
	// and other non-regular entries.
	ErrNotRegular = errors.New("attachments: not a regular file")

	// ErrTooLarge is returned when one attachment exceeds its limit.
	ErrTooLarge = errors.New("attachments: attachment too large")

	// ErrTotalTooLarge is returned when the sum of attachment bytes
	// exceeds the aggregate limit.
	ErrTotalTooLarge = errors.New("attachments: total attachment size too large")

	// ErrTooMany is returned when the number of requested attachments
	// exceeds the configured limit.
	ErrTooMany = errors.New("attachments: too many attachments")
)

// Limits is the attachment resolver's resource budget. All values must be
// positive. The resolver enforces the limits on actual bytes read, not on
// file metadata alone.
type Limits struct {
	MaxAttachmentBytes      int64
	MaxTotalAttachmentBytes int64
	MaxAttachments          int
}

// File is one frozen, bounded attachment. Filename is a basename only; the
// resolved host path is deliberately not retained or returned.
type File struct {
	Filename string
	Data     []byte
}

// Resolver resolves agent-supplied relative paths against a fixed set of
// operator-configured roots. It is safe for concurrent use.
type Resolver struct {
	mu     sync.RWMutex
	roots  []*os.Root
	limits Limits
	closed bool
}

// NewResolver opens every configured root. Roots must be absolute existing
// directories. The caller owns the returned resolver and must Close it.
func NewResolver(paths []string, limits Limits) (*Resolver, error) {
	if len(paths) == 0 {
		return nil, ErrNoRoots
	}
	if limits.MaxAttachmentBytes <= 0 ||
		limits.MaxTotalAttachmentBytes <= 0 ||
		limits.MaxAttachments <= 0 {
		return nil, ErrInvalidLimits
	}

	r := &Resolver{limits: limits}
	seen := make(map[string]struct{}, len(paths))
	for i, p := range paths {
		if p == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, '\x00') || HasControl(p) {
			r.Close()
			return nil, fmt.Errorf("attachment_send_paths[%d] must be an absolute control-free directory path", i)
		}
		canonical, err := filepath.EvalSymlinks(p)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("attachment_send_paths[%d]: cannot resolve root", i)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("attachment_send_paths[%d]: cannot stat root", i)
		}
		if !info.IsDir() {
			r.Close()
			return nil, fmt.Errorf("attachment_send_paths[%d] is not a directory", i)
		}
		if _, duplicate := seen[canonical]; duplicate {
			r.Close()
			return nil, fmt.Errorf("attachment_send_paths[%d] duplicates another root", i)
		}
		root, err := os.OpenRoot(canonical)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("attachment_send_paths[%d]: cannot open root", i)
		}
		seen[canonical] = struct{}{}
		r.roots = append(r.roots, root)
	}
	return r, nil
}

// Resolve validates and reads the referenced attachments in order. All
// requested files are read into memory so later file changes cannot affect a
// future submission. On any error no partial result is returned.
func (r *Resolver) Resolve(refs []string) ([]File, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, ErrClosed
	}
	if len(refs) > r.limits.MaxAttachments {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooMany, len(refs), r.limits.MaxAttachments)
	}

	out := make([]File, 0, len(refs))
	var total int64
	for _, ref := range refs {
		clean, err := validateReference(ref)
		if err != nil {
			return nil, err
		}
		f, err := r.open(clean)
		if err != nil {
			return nil, err
		}
		data, readErr := readBounded(f, r.limits.MaxAttachmentBytes)
		closeErr := f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("attachments: close reference: %w", closeErr)
		}
		total += int64(len(data))
		if total > r.limits.MaxTotalAttachmentBytes {
			return nil, fmt.Errorf("%w: %d > %d", ErrTotalTooLarge, total, r.limits.MaxTotalAttachmentBytes)
		}
		out = append(out, File{Filename: path.Base(clean), Data: data})
	}
	return out, nil
}

// Close closes all roots. It is idempotent and safe to call concurrently
// with Resolve.
func (r *Resolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	for _, root := range r.roots {
		if err := root.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	r.roots = nil
	return errors.Join(errs...)
}

// open returns an open file for clean from exactly one root. It checks every
// root so the same relative path in two roots is rejected as ambiguous rather
// than silently selecting the first match.
func (r *Resolver) open(clean string) (*os.File, error) {
	target := filepath.FromSlash(clean)
	var match *os.File
	for _, root := range r.roots {
		// Stat before opening so a FIFO or device cannot make open block.
		// The post-open stat below is still authoritative: the entry could
		// change between these calls.
		info, err := root.Stat(target)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			// Do not propagate os.Root's error: it may contain the
			// configured host path. A symlink escape or permission
			// failure is treated as an invalid reference.
			if match != nil {
				_ = match.Close()
			}
			return nil, fmt.Errorf("%w: %s", ErrInvalidPath, clean)
		}
		if !info.Mode().IsRegular() {
			if match != nil {
				_ = match.Close()
			}
			return nil, fmt.Errorf("%w: %s", ErrNotRegular, clean)
		}

		// O_NONBLOCK is harmless for regular files and prevents a
		// last-moment swap to a FIFO from blocking the resolver on Unix.
		f, err := root.OpenFile(target, os.O_RDONLY|nonBlockOpenFlag, 0)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if match != nil {
				_ = match.Close()
			}
			return nil, fmt.Errorf("%w: %s", ErrInvalidPath, clean)
		}
		openedInfo, err := f.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() {
			_ = f.Close()
			if match != nil {
				_ = match.Close()
			}
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrInvalidPath, clean)
			}
			return nil, fmt.Errorf("%w: %s", ErrNotRegular, clean)
		}
		if match != nil {
			_ = f.Close()
			_ = match.Close()
			return nil, fmt.Errorf("%w: %s", ErrAmbiguous, clean)
		}
		match = f
	}
	if match == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	return match, nil
}

// validateReference rejects anything that is not a simple relative path. The
// returned value uses slash separators and has been cleaned but not resolved
// against the filesystem.
func validateReference(ref string) (string, error) {
	if ref == "" || strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	if strings.ContainsRune(ref, '\x00') || HasControl(ref) {
		return "", fmt.Errorf("%w: control character", ErrInvalidPath)
	}

	// Treat both slash styles as separators for validation so Windows-style
	// traversal cannot slip through on another platform. The public reference
	// format is slash-separated.
	slashRef := strings.ReplaceAll(ref, "\\", "/")
	if strings.HasPrefix(slashRef, "/") || path.IsAbs(slashRef) ||
		filepath.IsAbs(ref) || filepath.VolumeName(ref) != "" {
		return "", fmt.Errorf("%w: absolute path", ErrInvalidPath)
	}
	if len(slashRef) >= 2 && slashRef[1] == ':' {
		return "", fmt.Errorf("%w: drive-qualified path", ErrInvalidPath)
	}
	for _, part := range strings.Split(slashRef, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: traversal", ErrInvalidPath)
		}
	}
	clean := path.Clean(slashRef)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: invalid path", ErrInvalidPath)
	}
	base := path.Base(clean)
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("%w: invalid filename", ErrInvalidPath)
	}
	return clean, nil
}

// readBounded reads at most max+1 bytes so it can distinguish an exact-size
// file from an oversized one without trusting a prior Stat. The extra byte is
// never returned.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 || max == int64(^uint64(0)>>1) {
		return nil, ErrInvalidLimits
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("attachments: read: %w", err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrTooLarge, max)
	}
	return data, nil
}

// HasControl reports whether s contains a control character. The resolver
// rejects control characters in agent-supplied references and configured
// roots.
func HasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
