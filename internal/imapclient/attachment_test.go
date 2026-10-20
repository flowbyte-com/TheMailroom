package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	memuserpkg "github.com/flowbyte-com/go-imap/backend/memory"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

const attachmentMessage = "From: a@example.test\r\n" +
	"To: b@example.test\r\n" +
	"Subject: Attachment Test\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
	"body\r\n" +
	"--B\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"a.bin\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\n" +
	"QUJD\r\n" +
	"--B--\r\n"

func injectINBOXMessage(t *testing.T, raw string) func(u *memuserpkg.User) {
	t.Helper()
	return func(u *memuserpkg.User) {
		mb, err := u.GetMailbox("INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if err := mb.CreateMessage(nil, time.Now(), stringReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func findSubject(t *testing.T, be *Backend, subject string) (uint32, uint32) {
	t.Helper()
	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range list.Messages {
		if m.Subject == subject {
			return m.UID, list.UIDValidity
		}
	}
	t.Fatalf("message %q not found", subject)
	return 0, 0
}

func backendWithLimits(t *testing.T, addr string, limits config.LimitsConfig) *Backend {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		t.Fatal(err)
	}
	cfg := config.AccountConfig{
		ID:      "test",
		Email:   "user@example.test",
		Backend: "imap",
		IMAP: config.IMAPConfig{
			Host:     host,
			Port:     p,
			TLS:      true,
			Username: "username",
			Password: "password",
		},
		Limits: limits,
	}
	be, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return be
}

func TestFetchAttachmentIMAP(t *testing.T) {
	addr, cleanup := startCustomServer(t, injectINBOXMessage(t, attachmentMessage))
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	uid, uv := findSubject(t, be, "Attachment Test")
	got, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "2")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if got.UID != uid || got.UIDValidity != uv {
		t.Errorf("UID/UIDVALIDITY = %d/%d, want %d/%d", got.UID, got.UIDValidity, uid, uv)
	}
	if got.Attachment.Index != 0 {
		t.Errorf("attachment index = %d, want 0", got.Attachment.Index)
	}
	if got.Attachment.Part != "2" || got.Attachment.Filename != "a.bin" ||
		got.Attachment.MimeType != "application/octet-stream" {
		t.Errorf("metadata = %+v", got.Attachment)
	}
	if string(got.Content) != "ABC" {
		t.Errorf("content = %q, want ABC", got.Content)
	}

	// Fetching an attachment must not set \Seen.
	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages after fetch: %v", err)
	}
	for _, m := range list.Messages {
		if m.UID != uid {
			continue
		}
		for _, f := range m.Flags {
			if f == "\\Seen" {
				t.Errorf("\\Seen was set after FetchAttachment; flags=%v", m.Flags)
			}
		}
	}

	// The existing read_message path must still see the same part metadata.
	msg, err := be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
	if err != nil {
		t.Fatalf("FetchMessage regression: %v", err)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Part != got.Attachment.Part ||
		msg.Attachments[0].Size != got.Attachment.Size {
		t.Errorf("FetchMessage attachments = %+v, want the same attachment as FetchAttachment", msg.Attachments)
	}

	if _, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "1"); !errors.Is(err, mailbox.ErrAttachmentNotFound) {
		t.Errorf("body part err = %v, want ErrAttachmentNotFound", err)
	}
	if _, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "0"); !errors.Is(err, mailbox.ErrInvalidPart) {
		t.Errorf("invalid part err = %v, want ErrInvalidPart", err)
	}
	if _, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv+1, "2"); !errors.Is(err, mailbox.ErrUIDMismatch) {
		t.Errorf("stale UIDVALIDITY err = %v, want ErrUIDMismatch", err)
	}
	if _, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, 0, "2"); !errors.Is(err, mailbox.ErrUIDMissing) {
		t.Errorf("zero UIDVALIDITY err = %v, want ErrUIDMissing", err)
	}
	if _, err := be.FetchAttachment(context.Background(), "test", "INBOX", 999999, uv, "2"); !errors.Is(err, mailbox.ErrMessageNotFound) {
		t.Errorf("missing UID err = %v, want ErrMessageNotFound", err)
	}
}

func TestFetchAttachmentIMAPFailsClosedOnMalformedSibling(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Malformed Sibling\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nb\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"good.bin\"\r\n\r\nGOOD\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"bad.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n!!!!\r\n" +
		"--B--\r\n"
	addr, cleanup := startCustomServer(t, injectINBOXMessage(t, raw))
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	uid, uv := findSubject(t, be, "Malformed Sibling")
	got, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "2")
	if !errors.Is(err, mailbox.ErrMalformed) {
		t.Fatalf("FetchAttachment err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("FetchAttachment returned content despite malformed sibling: %+v", got)
	}
}

func TestFetchAttachmentIMAPPreservesIndex(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Attachment Index\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\nA\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"b.bin\"\r\n\r\nB\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"c.bin\"\r\n\r\nC\r\n" +
		"--B--\r\n"
	addr, cleanup := startCustomServer(t, injectINBOXMessage(t, raw))
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	uid, uv := findSubject(t, be, "Attachment Index")
	msg, err := be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}

	cases := []struct {
		part      string
		filename  string
		wantIndex int
	}{
		{"3", "b.bin", 1},
		{"4", "c.bin", 2},
	}
	for _, tc := range cases {
		got, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, tc.part)
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

func TestFetchAttachmentIMAPOpaqueRFC822(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Opaque IMAP\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: message/rfc822\r\n" +
		"Content-Disposition: attachment; filename=\"inner.eml\"\r\n\r\n" +
		"From: inner@example.test\r\nSubject: Inner\r\n\r\ninner secret\r\n" +
		"--B--\r\n"
	addr, cleanup := startCustomServer(t, injectINBOXMessage(t, raw))
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()
	uid, uv := findSubject(t, be, "Opaque IMAP")
	got, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "2")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if !strings.Contains(string(got.Content), "Subject: Inner") {
		t.Errorf("opaque content = %q", got.Content)
	}
	if _, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "2.1"); !errors.Is(err, mailbox.ErrAttachmentNotFound) {
		t.Errorf("hidden inner part err = %v, want ErrAttachmentNotFound", err)
	}
}

func TestFetchAttachmentIMAPLimits(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Attachment Limits\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nb\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nQUJD\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"b.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nREVGRw==\r\n" +
		"--B--\r\n"
	addr, cleanup := startCustomServer(t, injectINBOXMessage(t, raw))
	defer cleanup()

	base := config.LimitsConfig{
		MaxMessageBytes:         1 << 20,
		MaxAttachmentBytes:      1 << 20,
		MaxTotalAttachmentBytes: 1 << 20,
		MaxAttachments:          50,
		MaxSearchResults:        50,
		MaxBatchOperations:      50,
		MaxMIMENesting:          16,
		MaxMIMEParts:            500,
	}
	uid, uv := func() (uint32, uint32) {
		be := backendWithLimits(t, addr, base)
		defer be.Close()
		return findSubject(t, be, "Attachment Limits")
	}()

	t.Run("per attachment", func(t *testing.T) {
		limits := base
		limits.MaxAttachmentBytes = 2
		be := backendWithLimits(t, addr, limits)
		defer be.Close()
		_, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "2")
		if !errors.Is(err, mailbox.ErrAttachmentTooLarge) {
			t.Errorf("err = %v, want ErrAttachmentTooLarge", err)
		}
	})
	t.Run("total after target captured", func(t *testing.T) {
		limits := base
		limits.MaxTotalAttachmentBytes = 4
		be := backendWithLimits(t, addr, limits)
		defer be.Close()
		_, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "2")
		if !errors.Is(err, mailbox.ErrTotalAttachmentTooLarge) {
			t.Errorf("err = %v, want ErrTotalAttachmentTooLarge", err)
		}
	})
	t.Run("raw message cap", func(t *testing.T) {
		limits := base
		limits.MaxMessageBytes = len(raw) - 1
		be := backendWithLimits(t, addr, limits)
		defer be.Close()
		_, err := be.FetchAttachment(context.Background(), "test", "INBOX", uid, uv, "2")
		if !errors.Is(err, mailbox.ErrMessageTooLarge) {
			t.Errorf("err = %v, want ErrMessageTooLarge", err)
		}
	})
}
