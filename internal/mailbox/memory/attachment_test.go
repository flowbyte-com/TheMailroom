package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

func TestFetchAttachment(t *testing.T) {
	b := newBackend(t)
	got, err := b.FetchAttachment(context.Background(), "any", "Sent", 1, 1, "2")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if got.UID != 1 || got.UIDValidity != 1 {
		t.Errorf("UID/UIDVALIDITY = %d/%d", got.UID, got.UIDValidity)
	}
	if got.Attachment.Part != "2" || got.Attachment.Filename != "hello.txt" ||
		got.Attachment.MimeType != "application/octet-stream" {
		t.Errorf("attachment metadata = %+v", got.Attachment)
	}
	if string(got.Content) != "Hello from an attachment!\n" {
		t.Errorf("content = %q", got.Content)
	}
	if got.Attachment.Size != int64(len(got.Content)) {
		t.Errorf("metadata size = %d, content len = %d", got.Attachment.Size, len(got.Content))
	}
}

func TestFetchAttachmentErrors(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()
	if _, err := b.FetchAttachment(ctx, "any", "Sent", 1, 0, "2"); err != mailbox.ErrUIDMissing {
		t.Errorf("zero UIDVALIDITY err = %v, want ErrUIDMissing", err)
	}
	if _, err := b.FetchAttachment(ctx, "any", "Sent", 1, 99, "2"); err != mailbox.ErrUIDMismatch {
		t.Errorf("stale UIDVALIDITY err = %v, want ErrUIDMismatch", err)
	}
	if _, err := b.FetchAttachment(ctx, "any", "Sent", 999, 1, "2"); err != mailbox.ErrMessageNotFound {
		t.Errorf("missing UID err = %v, want ErrMessageNotFound", err)
	}
	if _, err := b.FetchAttachment(ctx, "any", "Sent", 1, 1, "1"); err != mailbox.ErrAttachmentNotFound {
		t.Errorf("body part err = %v, want ErrAttachmentNotFound", err)
	}
	if _, err := b.FetchAttachment(ctx, "any", "Sent", 1, 1, "9"); err != mailbox.ErrAttachmentNotFound {
		t.Errorf("missing part err = %v, want ErrAttachmentNotFound", err)
	}
	if _, err := b.FetchAttachment(ctx, "any", "Sent", 1, 1, "0"); err != mailbox.ErrInvalidPart {
		t.Errorf("invalid part err = %v, want ErrInvalidPart", err)
	}
	if _, err := b.FetchAttachment(ctx, "any", "Nope", 1, 1, "2"); err != mailbox.ErrMailboxNotFound {
		t.Errorf("missing mailbox err = %v, want ErrMailboxNotFound", err)
	}
}

func TestRawSnapshotPolicy(t *testing.T) {
	b := newBackend(t)
	sawAttachment := false
	sawPlain := false
	for _, msgs := range b.loaded {
		for _, rec := range msgs {
			if len(rec.msg.Attachments) > 0 {
				sawAttachment = true
				if rec.raw == nil {
					t.Errorf("message %q has attachments but no raw snapshot", rec.msg.Headers.Subject)
				}
				continue
			}
			sawPlain = true
			if rec.raw != nil {
				t.Errorf("message %q has no attachments but retains a raw snapshot", rec.msg.Headers.Subject)
			}
		}
	}
	if !sawAttachment || !sawPlain {
		t.Fatalf("fixture coverage = attachment:%v plain:%v, want both", sawAttachment, sawPlain)
	}
}

func TestFetchAttachmentSurvivesSourceDeletion(t *testing.T) {
	root := t.TempDir()
	raw := "From: a@example.test\r\n" +
		"Subject: Snapshot\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\n" +
		"payload\r\n" +
		"--B--\r\n"
	path := filepath.Join(root, "msg.eml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := b.FetchAttachment(context.Background(), "any", "INBOX", 1, 1, "2")
	if err != nil {
		t.Fatalf("FetchAttachment after source deletion: %v", err)
	}
	if string(got.Content) != "payload" {
		t.Errorf("content = %q, want payload", got.Content)
	}
}

func TestFetchAttachmentOpaqueMessageRFC822(t *testing.T) {
	root := t.TempDir()
	raw := "From: a@example.test\r\n" +
		"Subject: Opaque\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: message/rfc822\r\n" +
		"Content-Disposition: attachment; filename=\"inner.eml\"\r\n\r\n" +
		"From: inner@example.test\r\nSubject: Inner\r\n\r\ninner secret\r\n" +
		"--B--\r\n"
	if err := os.WriteFile(filepath.Join(root, "msg.eml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.FetchAttachment(context.Background(), "any", "INBOX", 1, 1, "2")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if !strings.Contains(string(got.Content), "Subject: Inner") {
		t.Errorf("opaque content = %q", got.Content)
	}
	if _, err := b.FetchAttachment(context.Background(), "any", "INBOX", 1, 1, "2.1"); err != mailbox.ErrAttachmentNotFound {
		t.Errorf("hidden inner part err = %v, want ErrAttachmentNotFound", err)
	}
}
