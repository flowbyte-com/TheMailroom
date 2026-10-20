//go:build unix

package attachments

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResolverRejectsFIFO(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustResolver(t, []string{root}, Limits{
		MaxAttachmentBytes:      10,
		MaxTotalAttachmentBytes: 20,
		MaxAttachments:          1,
	})
	if _, err := r.Resolve([]string{"pipe"}); !errors.Is(err, ErrNotRegular) {
		t.Errorf("err = %v, want ErrNotRegular", err)
	}
}
