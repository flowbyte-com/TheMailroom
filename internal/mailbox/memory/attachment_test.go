package memory

import (
	"context"
	"errors"
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
	if got.Attachment.Index != 0 {
		t.Errorf("attachment index = %d, want 0", got.Attachment.Index)
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

func TestFetchAttachmentFailsClosedOnMalformedSibling(t *testing.T) {
	root := t.TempDir()
	raw := "From: a@example.test\r\n" +
		"Subject: Malformed Sibling\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nb\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"good.bin\"\r\n\r\nGOOD\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"bad.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n!!!!\r\n" +
		"--B--\r\n"
	if err := os.WriteFile(filepath.Join(root, "msg.eml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.FetchAttachment(context.Background(), "any", "INBOX", 1, 1, "2")
	if !errors.Is(err, mailbox.ErrMalformed) {
		t.Fatalf("FetchAttachment err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("FetchAttachment returned content despite malformed sibling: %+v", got)
	}
}

func TestFetchAttachmentPreservesIndex(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()
	res, err := b.ListMessages(ctx, "any", "INBOX", mailbox.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var uid uint32
	for _, m := range res.Messages {
		if m.Subject == "Multi-part: alternative + related + mixed" {
			uid = m.UID
			break
		}
	}
	if uid == 0 {
		t.Fatal("multipart fixture message not found")
	}
	msg, err := b.FetchMessage(ctx, "any", "INBOX", uid, res.UIDValidity)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}

	cases := []struct {
		part      string
		filename  string
		wantIndex int
	}{
		{"2", "report.pdf", 1},
		{"3", "data.csv", 2},
	}
	for _, tc := range cases {
		got, err := b.FetchAttachment(ctx, "any", "INBOX", uid, res.UIDValidity, tc.part)
		if err != nil {
			t.Fatalf("FetchAttachment(%s): %v", tc.part, err)
		}
		if got.Attachment.Index != tc.wantIndex {
			t.Errorf("FetchAttachment(%s) index = %d, want %d", tc.part, got.Attachment.Index, tc.wantIndex)
		}
		if got.Attachment.Filename != tc.filename {
			t.Errorf("FetchAttachment(%s) filename = %q, want %q", tc.part, got.Attachment.Filename, tc.filename)
		}
		var listed *mailbox.Attachment
		for i := range msg.Attachments {
			if msg.Attachments[i].Part == tc.part {
				listed = &msg.Attachments[i]
				break
			}
		}
		if listed == nil {
			t.Fatalf("FetchMessage has no part %s", tc.part)
		}
		if listed.Index != got.Attachment.Index {
			t.Errorf("FetchMessage(%s) index = %d, FetchAttachment index = %d", tc.part, listed.Index, got.Attachment.Index)
		}
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
