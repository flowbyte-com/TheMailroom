package imapclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	memserver "github.com/emersion/go-imap/backend/memory"
	memuserpkg "github.com/emersion/go-imap/backend/memory"
	imapsrv "github.com/emersion/go-imap/server"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// customBackend lets tests inject additional messages into the
// default "username" account before the server starts accepting
// connections. The embedded *memserver.Backend already implements
// backend.Backend, so the only thing we need is a constructor
// that runs the setup hook.
type customBackend struct {
	*memserver.Backend
}

func newCustomBackend(setup func(u *memuserpkg.User)) *customBackend {
	be := memserver.New()
	// memserver.New creates a single "username" user; we can
	// access it via Login, but the field is unexported. Instead
	// we login to obtain a *User (which we can cast).
	u, err := be.Login(nil, "username", "password")
	if err != nil {
		panic(err)
	}
	mu, ok := u.(*memuserpkg.User)
	if !ok {
		panic("not a memory user")
	}
	if setup != nil {
		setup(mu)
	}
	return &customBackend{Backend: be}
}

// startCustomServer starts an in-process IMAP server backed by a
// memory backend with the given setup hook applied to the default
// user.
func startCustomServer(t *testing.T, setup func(u *memuserpkg.User)) (string, func()) {
	t.Helper()
	be := newCustomBackend(setup)
	srv := imapsrv.New(be)
	srv.AllowInsecureAuth = false
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{genCert(t)}, MinVersion: tls.VersionTLS12}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

func genCert(t *testing.T) tls.Certificate {
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

// TestUnreadMessageStaysUnreadAfterRead is the headline regression
// for BODY.PEEK[]: a FETCH that does not set \Seen. We add a
// message with no \Seen flag, read it, and verify the flag is
// still absent on the server.
func TestUnreadMessageStaysUnreadAfterRead(t *testing.T) {
	addr, cleanup := startCustomServer(t, func(u *memuserpkg.User) {
		mb, err := u.GetMailbox("INBOX")
		if err != nil {
			t.Fatal(err)
		}
		// Append a new message via the backend's CreateMessage.
		// CreateMessage always adds \Recent; that is harmless
		// for the test because the test only asserts \Seen
		// remains unset.
		body := "From: a@example.test\r\n" +
			"Subject: Unseen\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
			"secret\r\n"
		if err := mb.CreateMessage(nil, time.Now(), stringReader(body)); err != nil {
			t.Fatal(err)
		}
	})
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	uv, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
	}
	// The default welcome message has uid=6; our injected one
	// has uid=7. Fetch it with BODY.PEEK.
	_, err = be.FetchMessage(context.Background(), "test", "INBOX", 7, uv)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	// Re-list and check the flags. \Seen must be absent.
	msgs, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var unseen *mailbox.MessageSummary
	for i, m := range msgs {
		if m.UID == 7 {
			unseen = &msgs[i]
			break
		}
	}
	if unseen == nil {
		t.Fatal("could not find injected message after read")
	}
	for _, f := range unseen.Flags {
		if f == imap.SeenFlag {
			t.Errorf("\\Seen was set on an unread message after read; flags=%v", unseen.Flags)
		}
	}
}

// TestStaleUIDValidityRejectedAfterReconnect verifies that after
// a connection drop, a stale UIDVALIDITY is rejected. The fresh
// connection must re-EXAMINE; the caller's cached value is not
// trusted.
func TestStaleUIDValidityRejectedAfterReconnect(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	uv, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("first MailboxUIDValidity: %v", err)
	}
	// Force a reconnect.
	be.mu.Lock()
	if be.client != nil {
		_ = be.client.Close()
		be.client = nil
	}
	be.mu.Unlock()
	// Re-issue with a wrong UIDVALIDITY. The reconnect should
	// not paper over the mismatch.
	_, err = be.FetchMessage(context.Background(), "test", "INBOX", 1, uv+1)
	if !errors.Is(err, mailbox.ErrUIDMismatch) {
		t.Errorf("err = %v, want ErrUIDMismatch", err)
	}
}

// TestReconnectPreservesUIDValidityCheck verifies that the
// reconnect path actually reconnects and re-EXAMINEs. A previous
// bug would have left the cached value but the connection dead;
// the next call would have failed with a confusing error.
func TestReconnectPreservesUIDValidityCheck(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// Fetch at least one message first so we know the welcome
	// message UID.
	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil || len(list) == 0 {
		t.Fatalf("ListMessages: %v / %d", err, len(list))
	}
	uid := list[0].UID

	uv, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("first MailboxUIDValidity: %v", err)
	}
	// Force a reconnect.
	be.mu.Lock()
	if be.client != nil {
		_ = be.client.Close()
		be.client = nil
	}
	be.mu.Unlock()
	// Re-issue with the live UIDVALIDITY. The reconnect should
	// re-EXAMINE and accept the call.
	_, err = be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
	if err != nil {
		t.Fatalf("FetchMessage after reconnect: %v", err)
	}
}

// TestConnectionClosedOnBackendError ensures that an error from
// the backend (e.g. mailbox not found) tears down the connection
// so the next call reconnects. Without this, a transient error
// could leave the connection in a broken state and the next call
// would either hang or silently succeed with stale data.
func TestConnectionClosedOnBackendError(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// Cause an error.
	_, err := be.MailboxUIDValidity(context.Background(), "test", "NoSuchMailbox")
	if err == nil {
		t.Fatal("expected error for missing mailbox")
	}
	// Connection should be torn down.
	be.mu.Lock()
	closed := be.client == nil
	be.mu.Unlock()
	if !closed {
		t.Error("connection was not closed after error; future operations could be retried")
	}
	// The next call must succeed (a fresh connection is
	// established and the read-only EXAMINE works).
	if _, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX"); err != nil {
		t.Errorf("recovery failed: %v", err)
	}
}

// TestListMessagesReturnsReadOnlyUIDVALIDITY verifies that
// ListMessages populates the selected-validity cache and that the
// subsequent FetchMessage agrees.
func TestListMessagesReturnsReadOnlyUIDVALIDITY(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	msgs, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	uv := be.SelectedUIDValidity()
	if uv == 0 {
		t.Error("SelectedUIDValidity not populated by ListMessages")
	}
	_, err = be.FetchMessage(context.Background(), "test", "INBOX", msgs[0].UID, uv)
	if err != nil {
		t.Errorf("FetchMessage with cached UIDVALIDITY: %v", err)
	}
}

// stringReader is a tiny helper to avoid pulling strings into the
// imports just for one Reader conversion. imap.Literal requires
// Len(); we provide a minimal implementation.
type stringReaderImpl struct {
	s string
	i int
}

func (r *stringReaderImpl) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func (r *stringReaderImpl) Len() int { return len(r.s) - r.i }

func stringReader(s string) *stringReaderImpl { return &stringReaderImpl{s: s} }
