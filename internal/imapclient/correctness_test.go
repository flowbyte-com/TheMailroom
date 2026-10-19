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
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
	memserver "github.com/flowbyte-com/go-imap/backend/memory"
	memuserpkg "github.com/flowbyte-com/go-imap/backend/memory"
	imapsrv "github.com/flowbyte-com/go-imap/server"

	"github.com/flowbyte-com/themailroom/internal/config"
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
	listRes, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var unseen *mailbox.MessageSummary
	for i, m := range listRes.Messages {
		if m.UID == 7 {
			unseen = &listRes.Messages[i]
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
	// message UID. The list's UIDVALIDITY is bound to the same
	// EXAMINE that produced the UIDs; we use it directly.
	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil || len(list.Messages) == 0 {
		t.Fatalf("ListMessages: %v / %d", err, len(list.Messages))
	}
	uid := list.Messages[0].UID
	uv := list.UIDValidity

	if _, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX"); err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
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

// TestListMessagesUIDValidityBoundToUIDs is the central
// pairing regression: the UIDVALIDITY in ListResult must come
// from the same EXAMINE that produced the UIDs. Two separate
// EXAMINEs (e.g. list then MailboxUIDValidity) can disagree if
// the server re-issued UIDVALIDITY between them. The contract
// here is enforced by the implementation: ListResult.UIDValidity
// is captured at the same time as the UIDs, and a subsequent
// MailboxUIDValidity can return a different value if the server
// re-selected the mailbox.
func TestListMessagesUIDValidityBoundToUIDs(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	res, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if res.UIDValidity == 0 {
		t.Fatal("ListResult.UIDValidity is zero")
	}
	// A follow-up MailboxUIDValidity must return the same
	// value because no EXAMINE has been issued in between; if
	// it returns a different value, the contract is broken.
	uv2, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
	}
	if uv2 != res.UIDValidity {
		t.Errorf("MailboxUIDValidity = %d, ListResult.UIDValidity = %d; they must agree when no EXAMINE has been issued in between", uv2, res.UIDValidity)
	}
}

// TestFetchMessageBoundaryAtBodySize is the IMAP-side
// counterpart of the mimeparse boundary tests. We construct
// a backend with a configurable per-account MaxMessageBytes
// and a server that serves a message whose body is exactly
// the cap, one byte below, or one byte above. A valid
// message exactly at the cap must succeed; one over must
// return ErrMessageTooLarge.
func TestFetchMessageBoundaryAtBodySize(t *testing.T) {
	// Build the seed body. The header is fixed; the body
	// length is what we vary.
	hdr := "From: a@example.test\r\nSubject: B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n"
	const seedSize = 100

	// We want a body of size N. Construct three
	// configurations: a backend with cap=seedSize-1, a
	// backend with cap=seedSize, a backend with
	// cap=seedSize+1. The same message body of size
	// seedSize is used for all three.
	body := strings.Repeat("A", seedSize)
	raw := hdr + body

	for _, tc := range []struct {
		name      string
		cap       int
		wantErr   bool
		wantErrIs error
	}{
		{"limit-1", seedSize - 1, true, mailbox.ErrMessageTooLarge},
		{"limit", seedSize, false, nil},
		{"limit+1", seedSize + 1, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, cleanup := startCustomServer(t, func(u *memuserpkg.User) {
				mb, err := u.GetMailbox("INBOX")
				if err != nil {
					t.Fatal(err)
				}
				if err := mb.CreateMessage(nil, time.Now(), stringReader(raw)); err != nil {
					t.Fatal(err)
				}
			})
			defer cleanup()

			host, port, _ := net.SplitHostPort(addr)
			var p int
			_, _ = fmt.Sscanf(port, "%d", &p)
			cfg := config.AccountConfig{
				ID: "x", Email: "x@y", Backend: "imap",
				IMAP: config.IMAPConfig{Host: host, Port: p, Username: "username", Password: "password", TLS: true},
				Limits: config.LimitsConfig{
					MaxMessageBytes:         tc.cap + len(hdr),
					MaxAttachmentBytes:      1 << 20,
					MaxSearchResults:        1,
					MaxBatchOperations:      1,
					MaxMIMENesting:          16,
					MaxMIMEParts:            500,
					MaxTotalAttachmentBytes: 1 << 20,
					MaxAttachments:          50,
				},
			}
			be, err := New(cfg, Options{
				DialTimeout: 5 * time.Second,
				TLSConfig:   &tls.Config{InsecureSkipVerify: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer be.Close()

			// Read the UID list to discover the injected
			// message UID. The seed welcome message
			// already exists at uid=6, so our injected
			// message is uid=7. We use ListMessages to
			// find it by subject.
			list, err := be.ListMessages(context.Background(), "x", "INBOX", mailbox.ListOptions{Limit: 50})
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			var uid uint32
			for _, m := range list.Messages {
				if m.Subject == "B" {
					uid = m.UID
					break
				}
			}
			if uid == 0 {
				t.Fatal("injected message not found")
			}
			_, err = be.FetchMessage(context.Background(), "x", "INBOX", uid, list.UIDValidity)
			if tc.wantErr {
				if !errors.Is(err, tc.wantErrIs) {
					t.Errorf("err = %v, want %v", err, tc.wantErrIs)
				}
			} else if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

// TestSearchMessagesUIDValidityBoundToUIDs is the search-side
// counterpart.
func TestSearchMessagesUIDValidityBoundToUIDs(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	res, err := be.SearchMessages(context.Background(), "test", "INBOX", mailbox.SearchQuery{
		Subject: "little",
		Limit:   5,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if res.UIDValidity == 0 {
		t.Fatal("ListResult.UIDValidity is zero")
	}
	uv2, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
	}
	if uv2 != res.UIDValidity {
		t.Errorf("MailboxUIDValidity = %d, ListResult.UIDValidity = %d; they must agree", uv2, res.UIDValidity)
	}
}

// TestFetchMessageBodyCapTriggersError verifies that a body
// whose on-wire size reaches the configured MaxMessageBytes cap
// is treated as a hard failure rather than as a successful
// complete message. The contract is necessary because once
// the read hits the cap we cannot tell whether the underlying
// message was exactly cap bytes long or was truncated; we
// choose the conservative interpretation.
func TestFetchMessageBodyCapTriggersError(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()

	// Build a backend with a 256-byte MaxMessageBytes cap. The
	// default welcome message in the in-process memory backend
	// is well under 256 bytes, so the cap is not hit; we want
	// the cap to be tight enough to bound the test.
	host, port, _ := net.SplitHostPort(addr)
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	cfg := config.AccountConfig{
		ID: "x", Email: "x@y", Backend: "imap",
		IMAP: config.IMAPConfig{Host: host, Port: p, Username: "username", Password: "password", TLS: true},
		Limits: config.LimitsConfig{
			MaxMessageBytes:         10, // 10 bytes: any real message will hit the cap
			MaxAttachmentBytes:      1,
			MaxSearchResults:        1,
			MaxBatchOperations:      1,
			MaxMIMENesting:          1,
			MaxMIMEParts:            1,
			MaxTotalAttachmentBytes: 1,
		},
	}
	be, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	// Find the welcome message UID via ListMessages. The
	// in-process memory backend assigns uid=6 to the welcome
	// message, but reading the live value is more robust.
	list, err := be.ListMessages(context.Background(), "x", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil || len(list.Messages) == 0 {
		t.Fatalf("ListMessages: %v / %d", err, len(list.Messages))
	}
	uid := list.Messages[0].UID

	_, err = be.FetchMessage(context.Background(), "x", "INBOX", uid, list.UIDValidity)
	if !errors.Is(err, mailbox.ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}
}

// TestFetchMessagePropagatesMIMEError is the propagation
// regression: a parse failure during the MIME stage is
// surfaced as the typed error, not silently swallowed. We
// install a body that parses partially and then trips the
// MaxMIMENesting cap, and we verify the IMAP backend returns
// the typed error rather than a truncated message.
//
// We achieve this by setting MaxMIMENesting to 1, which is
// below the depth of any real multipart message. Since the
// in-process memory backend only ships a single-part text/plain
// message, this test uses a custom server that injects a
// multipart message; the body's MIME structure trips the
// cap.
func TestFetchMessagePropagatesMIMEError(t *testing.T) {
	multipart := "From: a@example.test\r\nSubject: M\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\nBIN\r\n" +
		"--B--\r\n"
	addr, cleanup := startCustomServer(t, func(u *memuserpkg.User) {
		mb, err := u.GetMailbox("INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if err := mb.CreateMessage(nil, time.Now(), stringReader(multipart)); err != nil {
			t.Fatal(err)
		}
	})
	defer cleanup()

	host, port, _ := net.SplitHostPort(addr)
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	cfg := config.AccountConfig{
		ID: "x", Email: "x@y", Backend: "imap",
		IMAP: config.IMAPConfig{Host: host, Port: p, Username: "username", Password: "password", TLS: true},
		Limits: config.LimitsConfig{
			MaxMessageBytes:         1 << 20,
			MaxAttachmentBytes:      1 << 20,
			MaxSearchResults:        1,
			MaxBatchOperations:      1,
			MaxMIMENesting:          1, // 1 < depth of multipart/mixed, so parse fails
			MaxMIMEParts:            500,
			MaxTotalAttachmentBytes: 1 << 20,
		},
	}
	be, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	// The injected message has uid=2 (uid=6 was the welcome
	// message in the original memory backend). We read the
	// live list to discover the UID.
	list, err := be.ListMessages(context.Background(), "x", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var uid uint32
	for _, m := range list.Messages {
		if m.Subject == "M" {
			uid = m.UID
			break
		}
	}
	if uid == 0 {
		t.Fatal("injected message not found")
	}
	_, err = be.FetchMessage(context.Background(), "x", "INBOX", uid, list.UIDValidity)
	if !errors.Is(err, mailbox.ErrNestingTooDeep) {
		t.Errorf("err = %v, want ErrNestingTooDeep", err)
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
	if len(msgs.Messages) == 0 {
		t.Fatal("no messages")
	}
	uv := be.SelectedUIDValidity()
	if uv == 0 {
		t.Error("SelectedUIDValidity not populated by ListMessages")
	}
	if uv != msgs.UIDValidity {
		t.Errorf("SelectedUIDValidity = %d, ListResult.UIDValidity = %d; they must agree", uv, msgs.UIDValidity)
	}
	_, err = be.FetchMessage(context.Background(), "test", "INBOX", msgs.Messages[0].UID, uv)
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
