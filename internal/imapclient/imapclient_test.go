package imapclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	memserver "github.com/emersion/go-imap/backend/memory"
	imapsrv "github.com/emersion/go-imap/server"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// testServer starts an in-process IMAP server backed by go-imap's
// memory backend. It listens on a random local TLS port with a
// self-signed certificate that the test client is configured to
// trust via InsecureSkipVerify. Returns the address and a cleanup
// function. The default user has username "username" and password
// "password".
func testServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	cert := generateSelfSignedCert(t)

	be := memserver.New()
	srv := imapsrv.New(be)
	srv.AllowInsecureAuth = false // we are on TLS

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

func testBackend(t *testing.T, addr string) *Backend {
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
		Limits: config.LimitsConfig{
			MaxMessageBytes:    25 * 1024 * 1024,
			MaxAttachments:     50,
			MaxSearchResults:   500,
			MaxBatchOperations: 50,
		},
	}
	// Tests use a self-signed cert; InsecureSkipVerify is the only
	// way to talk to it. This is gated behind the test-only
	// Options.TLSConfig knob and is not reachable from the
	// configuration file.
	b, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(2 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestMailboxes(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	mbs, err := be.Mailboxes(context.Background(), "test")
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	if len(mbs) == 0 {
		t.Fatal("no mailboxes")
	}
	found := false
	for _, m := range mbs {
		if m.Name == "INBOX" {
			found = true
		}
	}
	if !found {
		t.Errorf("INBOX not in mailboxes: %v", mbs)
	}
}

func TestListMessages(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	res, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if res.UIDValidity == 0 {
		t.Error("ListResult.UIDValidity not surfaced")
	}
	msgs := res.Messages
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	if msgs[0].From == "" {
		t.Error("From missing")
	}
	if msgs[0].UID == 0 {
		t.Error("UID missing")
	}
}

func TestFetchMessage(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(list.Messages) == 0 {
		t.Fatal("no messages")
	}
	uid := list.Messages[0].UID
	// Live UIDVALIDITY from the selected mailbox. We use the
	// list's UIDVALIDITY because that value is bound to the
	// same EXAMINE that produced the UIDs in list.Messages.
	uv := list.UIDValidity
	if uv == 0 {
		t.Fatal("live UIDVALIDITY is zero; server bug or wrapper bug")
	}
	m, err := be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if m.UID != uid {
		t.Errorf("UID = %d, want %d", m.UID, uid)
	}
	if m.Text == "" {
		t.Error("Text body empty")
	}
	if m.Headers.Subject == "" {
		t.Error("Subject missing")
	}
	if m.UIDValidity != uv {
		t.Errorf("UIDValidity = %d, want %d", m.UIDValidity, uv)
	}
}

func TestFetchMessageMissingUID(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	_, err := be.FetchMessage(context.Background(), "test", "INBOX", 99999, 1)
	if err != mailbox.ErrMessageNotFound {
		t.Errorf("err = %v, want ErrMessageNotFound", err)
	}
}

func TestFetchMessageUIDMismatch(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// Pass a UIDVALIDITY that does not match the live one.
	_, err := be.FetchMessage(context.Background(), "test", "INBOX", 1, 42)
	if !errors.Is(err, mailbox.ErrUIDMismatch) {
		t.Errorf("err = %v, want ErrUIDMismatch", err)
	}
}

func TestFetchMessageUIDMissing(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	_, err := be.FetchMessage(context.Background(), "test", "INBOX", 1, 0)
	if !errors.Is(err, mailbox.ErrUIDMissing) {
		t.Errorf("err = %v, want ErrUIDMissing", err)
	}
}

func TestSearchMessages(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// Search by subject text from the default seed message.
	got, err := be.SearchMessages(context.Background(), "test", "INBOX", mailbox.SearchQuery{
		Subject: "little message",
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if got.UIDValidity == 0 {
		t.Error("SearchResult.UIDValidity not surfaced")
	}
	if len(got.Messages) == 0 {
		t.Errorf("search returned no matches; got %d", len(got.Messages))
	}
}

func TestReconnectAfterServerClose(t *testing.T) {
	addr, cleanup := testServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// First call: opens a connection.
	if _, err := be.Mailboxes(context.Background(), "test"); err != nil {
		t.Fatalf("first Mailboxes: %v", err)
	}
	// Force-close the underlying client to simulate a server drop.
	be.mu.Lock()
	if be.client != nil {
		_ = be.client.Close()
		be.client = nil
	}
	be.mu.Unlock()
	// Second call: should reconnect transparently.
	if _, err := be.Mailboxes(context.Background(), "test"); err != nil {
		t.Fatalf("reconnect Mailboxes: %v", err)
	}
}

func TestRejectsNoTLS(t *testing.T) {
	// A config that says neither TLS nor StartTLS must be rejected at
	// dial-time, even if the server is reachable.
	addr, cleanup := testServer(t)
	defer cleanup()
	host, port, _ := net.SplitHostPort(addr)
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	cfg := config.AccountConfig{
		ID: "x", Email: "x@y", Backend: "imap",
		IMAP:   config.IMAPConfig{Host: host, Port: p, Username: "username", Password: "password"},
		Limits: config.LimitsConfig{MaxMessageBytes: 1, MaxAttachments: 1, MaxSearchResults: 1, MaxBatchOperations: 1},
	}
	b, err := New(cfg, Options{DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Mailboxes(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error: no TLS configured")
	}
	if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "starttls") {
		t.Errorf("err = %v, expected mention of tls/starttls", err)
	}
}

func TestPasswordCommand(t *testing.T) {
	got, err := resolvePassword(config.IMAPConfig{
		PasswordCommand: []string{"/bin/sh", "-c", "echo s3cret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Errorf("got %q, want s3cret", got)
	}
}

func TestPasswordCommandFailure(t *testing.T) {
	_, err := resolvePassword(config.IMAPConfig{
		PasswordCommand: []string{"/bin/sh", "-c", "exit 1"},
	})
	if err == nil {
		t.Fatal("expected error from failing password command")
	}
	if !strings.Contains(err.Error(), "password_command failed") {
		t.Errorf("err = %v", err)
	}
}

func TestPasswordCommandRejectsEmpty(t *testing.T) {
	_, err := resolvePassword(config.IMAPConfig{
		PasswordCommand: []string{"/bin/sh", "-c", ":"},
	})
	if err == nil {
		t.Fatal("expected error for empty output")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v", err)
	}
}

func TestRedactAddr(t *testing.T) {
	got := redactAddr("imap.example.test:993")
	if got != "imap.example.test:***" {
		t.Errorf("redactAddr = %q, want redacted port", got)
	}
}

func TestFetchMessageHeadersFromEnvelope(t *testing.T) {
	env := &imap.Envelope{
		Subject: "Test",
		From:    []*imap.Address{{MailboxName: "a", HostName: "b.test"}},
		To:      []*imap.Address{{MailboxName: "c", HostName: "d.test"}},
		Date:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	h := headersFromEnvelope(env, []byte("Message-Id: <x@y>\r\nIn-Reply-To: <p@q>\r\nReferences: <r1> <r2>\r\n\r\n"))
	if h.Subject != "Test" {
		t.Errorf("Subject = %q", h.Subject)
	}
	if h.MessageID != "<x@y>" {
		t.Errorf("MessageID = %q", h.MessageID)
	}
	if h.InReplyTo != "<p@q>" {
		t.Errorf("InReplyTo = %q", h.InReplyTo)
	}
	if len(h.References) != 2 || h.References[0] != "<r1>" {
		t.Errorf("References = %v", h.References)
	}
	if h.Date.IsZero() {
		t.Error("Date zero")
	}
}
