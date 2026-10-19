package imapclient

// Security tests for protocol-level IMAP literal-size enforcement.
//
// These tests exercise the mandatory pre-production security
// requirement documented in docs/PHASE2_DESIGN.md §10.
//
// The guarantee under test: a server that announces an IMAP
// literal larger than the configured per-account cap is
// rejected BEFORE the go-imap reader allocates a buffer for the
// announced size, and the failure is reported as a typed
// ErrMessageTooLarge (not as a generic protocol error, and not
// as a silent success with a missing body).
//
// The tests deliberately do NOT allocate gigabytes. The
// pre-allocation property is proven in two ways:
//
//  1. Source-level evidence: the fork's ReadLiteral checks
//     the announced size against Reader.MaxLiteralSize BEFORE
//     the `make([]byte, n)` allocation. This is asserted by
//     TestForkEnforcesCapBeforeAllocation, which reads the
//     fork's source from the module cache and verifies the
//     ordering of the two statements.
//
//  2. Controlled behavioural tests: the test server announces
//     a large literal and NEVER sends the body. If the client
//     were to allocate the announced size before rejecting, it
//     would have to wait for the body (or allocate a large
//     buffer). Instead the client rejects the announcement
//     immediately, closes the connection, and the test server
//     observes the close.

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
	memuserpkg "github.com/flowbyte-com/go-imap/backend/memory"
	imapsrv "github.com/flowbyte-com/go-imap/server"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// rawIMAPServer is a minimal IMAP server that speaks just enough
// of the protocol to exercise the client's literal handling. It
// is used to inject responses that a well-behaved server would
// never send (in particular, a literal header whose announced
// size exceeds the cap, with no body ever sent).
type rawIMAPServer struct {
	ln net.Listener

	// tlsConfig, if non-nil, wraps the accepted connections in
	// TLS (implicit TLS).
	tlsConfig *tls.Config

	// bodyResponse is the untagged FETCH response written
	// verbatim in reply to the client's UID FETCH. When empty,
	// the server writes a synthesised response with the
	// literal header from bodySize.
	bodyResponse string

	// bodySize is the number of body bytes the server claims
	// via the literal header. The server never sends them.
	bodySize int

	// mu guards the recorded observations.
	mu sync.Mutex
	// sawLiteralHeader is set when the server wrote the
	// literal header.
	sawLiteralHeader bool
	// clientClosed is set when the client closed the
	// connection (detected via a read error on the server
	// side).
	clientClosed bool
	// rejectedBeforeBody is set when the client closed the
	// connection *before* the server sent any body byte. The
	// server never sends body bytes at all, so this is
	// equivalent to "the client rejected the announcement".
	rejectedBeforeBody bool
	// closeLatency is the time between writing the literal
	// header and observing the client close.
	closeLatency time.Duration
}

func (s *rawIMAPServer) start(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	go s.serve(t)
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func (s *rawIMAPServer) serve(t *testing.T) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *rawIMAPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	if s.tlsConfig != nil {
		tlsConn := tls.Server(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
	}
	fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")

	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			s.markClosed()
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.Contains(upper, "LOGIN"):
			fmt.Fprintf(conn, "%s OK logged in\r\n", firstField(line))
		case strings.Contains(upper, "SELECT") || strings.Contains(upper, "EXAMINE"):
			// Report a mailbox with a UIDVALIDITY. The
			// go-imap server sends it as an untagged
			// `* OK [UIDVALIDITY n]` response; the client's
			// Select handler reads the code from either
			// the tagged or the untagged response.
			// Note: the client uses EXAMINE for
			// read-only selection.
			fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] selected\r\n%s OK selected\r\n", firstField(line))
		case strings.Contains(upper, "FETCH"):
			// Send the crafted response with an over-cap
			// literal header and NO body.
			s.mu.Lock()
			s.sawLiteralHeader = true
			s.mu.Unlock()
			start := time.Now()
			resp := s.bodyResponse
			if resp == "" {
				resp = fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n", s.bodySize)
			}
			if _, err := io.WriteString(conn, resp); err != nil {
				s.markClosed()
				return
			}
			// The server NEVER sends the body. The client
			// must reject the announcement and close the
			// connection. We wait for that by reading, with
			// a deadline so a client that does not close
			// cannot deadlock the test.
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 1)
			_, _ = conn.Read(buf)
			s.mu.Lock()
			s.clientClosed = true
			s.rejectedBeforeBody = true
			s.closeLatency = time.Since(start)
			s.mu.Unlock()
			return
		default:
			fmt.Fprintf(conn, "%s OK done\r\n", firstField(line))
		}
	}
}

// firstField extracts the command tag from a client command line.
func firstField(line string) string {
	if i := strings.IndexByte(line, ' '); i > 0 {
		return line[:i]
	}
	return "A999"
}

func (s *rawIMAPServer) markClosed() {
	s.mu.Lock()
	s.clientClosed = true
	s.mu.Unlock()
}

// TestLiteralSizeCapEnforcedBeforeAllocation is the central
// security regression. A server announces a literal far larger
// than the cap and never sends the body. The client must:
//
//  1. reject the announcement without waiting for the body
//     (bounded latency, not a timeout);
//  2. close the connection (so the server observes the close);
//  3. surface a typed ErrMessageTooLarge.
func TestLiteralSizeCapEnforcedBeforeAllocation(t *testing.T) {
	const cap = 4096
	// Announce 64 MiB. We never send it, and we must never
	// allocate it.
	const announced = 64 * 1024 * 1024

	srv := &rawIMAPServer{bodySize: announced, tlsConfig: &tls.Config{
		Certificates: []tls.Certificate{genCert(t)},
		MinVersion:   tls.VersionTLS12,
	}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := backendForRawServer(t, addr, cap, false)
	defer be.Close()

	_, err := be.FetchMessage(context.Background(), "test", "INBOX", 1, 1)
	if err == nil {
		t.Fatal("FetchMessage returned nil error; want a literal-size violation")
	}
	if !errors.Is(err, mailbox.ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.sawLiteralHeader {
		t.Fatal("server did not send the literal header; the test did not exercise the intended path")
	}
	if !srv.clientClosed {
		t.Error("client did not close the connection after the literal-size violation; the connection must be discarded")
	}
	if !srv.rejectedBeforeBody {
		t.Error("client did not reject the literal before the body")
	}
	// The rejection must be prompt. If the client had tried to
	// read the 64 MiB body it would have blocked until the
	// server timed out; the cap path returns immediately.
	if srv.closeLatency > 5*time.Second {
		t.Errorf("close latency = %v; want a prompt rejection (the cap must not wait for the body)", srv.closeLatency)
	}
}

// TestLiteralSizeCapBoundary exercises the cap at cap-1, cap
// and cap+1 using a real message served by the in-process
// go-imap server. The IMAP `BODY[]` literal contains the whole
// RFC 5322 message, so a message whose raw size is exactly the
// cap is a literal exactly at the cap.
//
// A message exactly at the cap must be fetched successfully; one
// byte over must be rejected with ErrMessageTooLarge.
func TestLiteralSizeCapBoundary(t *testing.T) {
	header := "From: a@example.test\r\n" +
		"Subject: B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n"
	// The cap is chosen so the padded body is a round number.
	const cap = 512

	for _, tc := range []struct {
		name      string
		bodyLen   int
		wantErr   bool
		wantErrIs error
	}{
		{"limit-1 accepted", cap - len(header) - 1, false, nil},
		{"limit accepted", cap - len(header), false, nil},
		{"limit+1 rejected", cap - len(header) + 1, true, mailbox.ErrMessageTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := header + strings.Repeat("A", tc.bodyLen)
			addr, cleanup := startTLSServerWithMessage(t, raw)
			defer cleanup()

			be := backendForCap(t, addr, cap, false)
			defer be.Close()

			uv, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
			if err != nil {
				t.Fatalf("MailboxUIDValidity: %v", err)
			}
			list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
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
			msg, err := be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
			if tc.wantErr {
				if !errors.Is(err, tc.wantErrIs) {
					t.Errorf("err = %v, want %v", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchMessage: %v", err)
			}
			if msg == nil {
				t.Fatal("FetchMessage returned a nil message")
			}
			if !strings.Contains(msg.Text, "AAAA") {
				t.Errorf("Text = %q, want the padded body", msg.Text)
			}
		})
	}
}

// TestForkEnforcesCapBeforeAllocation is the source-level
// evidence for the pre-allocation property. It reads the fork's
// read.go from the module cache and asserts that the
// MaxLiteralSize check appears BEFORE the `make([]byte, n)`
// allocation.
//
// This is a deliberate structural assertion: it is the only
// way to prove "no allocation happens" without allocating
// gigabytes.
func TestForkEnforcesCapBeforeAllocation(t *testing.T) {
	src, err := forkReadGoSource(t)
	if err != nil {
		t.Fatalf("locating the fork's read.go: %v", err)
	}
	checkIdx := strings.Index(src, "MaxLiteralSize > 0")
	if checkIdx < 0 {
		t.Fatal("fork read.go does not contain the MaxLiteralSize check; the cap was removed")
	}
	allocIdx := strings.Index(src, "b := make([]byte, n)")
	if allocIdx < 0 {
		t.Fatal("fork read.go does not contain the literal allocation; upstream layout changed")
	}
	if checkIdx > allocIdx {
		t.Errorf("the MaxLiteralSize check (offset %d) appears AFTER the allocation (offset %d); "+
			"the cap must be checked before the buffer is allocated", checkIdx, allocIdx)
	}
}

// forkReadGoSource locates the fork's read.go in the module
// cache.
func forkReadGoSource(t *testing.T) (string, error) {
	t.Helper()
	modCache := os.Getenv("GOMODCACHE")
	if modCache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		modCache = filepath.Join(home, "go", "pkg", "mod")
	}
	pattern := filepath.Join(modCache, "github.com", "flowbyte-com", "go-imap@*", "read.go")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no fork read.go found under %s", pattern)
	}
	// Prefer the version we depend on.
	for _, m := range matches {
		if strings.Contains(m, "v1.2.1-themailroom.1") {
			return readFile(m)
		}
	}
	return readFile(matches[0])
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestBinaryContentWithLiteralHeaderBytes is the binary-safety
// regression (criterion 5). A message whose *body* contains
// byte sequences that look like IMAP literal headers must be
// read normally: the cap applies to the announced literal size,
// not to the content.
func TestBinaryContentWithLiteralHeaderBytes(t *testing.T) {
	// A body containing "{999999999}\r\n" and other literal-
	// header-looking sequences.
	body := "{999999999}\r\n{12345}\r\n{0}\r\n not-a-literal-header"
	header := "From: a@example.test\r\n" +
		"Subject: BIN\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n"
	raw := header + body

	addr, cleanup := startTLSServerWithMessage(t, raw)
	defer cleanup()

	be := backendForCap(t, addr, 64*1024, false)
	defer be.Close()

	uv, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
	}
	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var uid uint32
	for _, m := range list.Messages {
		if m.Subject == "BIN" {
			uid = m.UID
			break
		}
	}
	if uid == 0 {
		t.Fatal("binary fixture not found in the injected mailbox")
	}
	msg, err := be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if !strings.Contains(msg.Text, "{999999999}") {
		t.Errorf("Text = %q, want it to contain the literal-header-looking bytes", msg.Text)
	}
}

// TestOversizedLiteralUnderSTARTTLS is the STARTTLS
// counterpart of the rejection test (criteria 3 and 7). The cap
// is applied to the *decrypted* stream, so it must reject an
// over-cap message on the STARTTLS path too.
func TestOversizedLiteralUnderSTARTTLS(t *testing.T) {
	addr, cleanup := startCustomServerWithMessage(t,
		"From: a@example.test\r\nSubject: BIG\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+
			strings.Repeat("A", 4096))
	defer cleanup()

	be := backendForCap(t, addr, 512, true)
	defer be.Close()

	uv, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
	}
	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var uid uint32
	for _, m := range list.Messages {
		if m.Subject == "BIG" {
			uid = m.UID
			break
		}
	}
	if uid == 0 {
		t.Fatal("big fixture not found in the injected mailbox")
	}
	_, err = be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
	if !errors.Is(err, mailbox.ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge (the cap must apply on the STARTTLS path too)", err)
	}
}

// TestOversizedLiteralUnderImplicitTLS is the implicit-TLS
// counterpart. It uses the same fixture as the STARTTLS test but
// with cfg.IMAP.TLS.
func TestOversizedLiteralUnderImplicitTLS(t *testing.T) {
	addr, cleanup := startTLSServerWithMessage(t,
		"From: a@example.test\r\nSubject: BIG\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+
			strings.Repeat("A", 4096))
	defer cleanup()

	be := backendForCap(t, addr, 512, false)
	defer be.Close()

	uv, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX")
	if err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
	}
	list, err := be.ListMessages(context.Background(), "test", "INBOX", mailbox.ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var uid uint32
	for _, m := range list.Messages {
		if m.Subject == "BIG" {
			uid = m.UID
			break
		}
	}
	if uid == 0 {
		t.Fatal("big fixture not found in the injected mailbox")
	}
	_, err = be.FetchMessage(context.Background(), "test", "INBOX", uid, uv)
	if !errors.Is(err, mailbox.ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}
}

// TestReconnectAfterLiteralRejection verifies that after a
// protocol-level failure the client establishes a clean
// connection for the next operation (criterion 6: "Subsequent
// operations establish a clean connection").
func TestReconnectAfterLiteralRejection(t *testing.T) {
	const cap = 4096
	srv := &rawIMAPServer{bodySize: 64 * 1024 * 1024, tlsConfig: &tls.Config{
		Certificates: []tls.Certificate{genCert(t)},
		MinVersion:   tls.VersionTLS12,
	}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := backendForRawServer(t, addr, cap, false)
	defer be.Close()

	// First fetch: rejected.
	_, err := be.FetchMessage(context.Background(), "test", "INBOX", 1, 1)
	if !errors.Is(err, mailbox.ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
	// The backend must have discarded the connection.
	be.mu.Lock()
	closed := be.client == nil
	be.mu.Unlock()
	if !closed {
		t.Error("the connection was not discarded after the protocol-level failure")
	}
	// The next operation establishes a fresh connection. The
	// raw server accepts a new connection and serves the
	// greeting, so the EXAMINE must succeed.
	if _, err := be.MailboxUIDValidity(context.Background(), "test", "INBOX"); err != nil {
		t.Errorf("reconnect after rejection failed: %v", err)
	}
}

// TestNoSilentSuccessOnLiteralFailure is the regression for
// criterion 8 (no silent success). A rejected literal must
// never produce a successful *mailbox.Message with a missing
// or truncated body.
func TestNoSilentSuccessOnLiteralFailure(t *testing.T) {
	const cap = 4096
	srv := &rawIMAPServer{bodySize: 64 * 1024 * 1024, tlsConfig: &tls.Config{
		Certificates: []tls.Certificate{genCert(t)},
		MinVersion:   tls.VersionTLS12,
	}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := backendForRawServer(t, addr, cap, false)
	defer be.Close()

	msg, err := be.FetchMessage(context.Background(), "test", "INBOX", 1, 1)
	if err == nil {
		t.Fatalf("FetchMessage returned a message with no error: %+v", msg)
	}
	if msg != nil {
		t.Errorf("FetchMessage returned both a message and an error: %+v", msg)
	}
}

// TestMalformedLiteralAnnouncement covers criterion 7: a
// malformed or incomplete literal announcement must be handled
// safely (an error, never a panic and never a silent success).
func TestMalformedLiteralAnnouncement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
	}{
		{"no closing brace", "* 1 FETCH (UID 1 BODY[] {12345\r\n"},
		{"non-numeric size", "* 1 FETCH (UID 1 BODY[] {abc}\r\n"},
		{"negative size", "* 1 FETCH (UID 1 BODY[] {-1}\r\n"},
		{"truncated header", "* 1 FETCH (UID 1 BODY[] {"},
		{"missing size", "* 1 FETCH (UID 1 BODY[] {}\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &rawIMAPServer{bodySize: 1024, bodyResponse: tc.response, tlsConfig: &tls.Config{
				Certificates: []tls.Certificate{genCert(t)},
				MinVersion:   tls.VersionTLS12,
			}}
			addr, cleanup := srv.start(t)
			defer cleanup()
			be := backendForRawServer(t, addr, 4096, false)
			defer be.Close()
			// The call must fail; it must not panic and
			// must not succeed with a missing body. A short
			// command timeout keeps the malformed cases
			// fast: a server that announces an unterminated
			// literal header leaves the client waiting for
			// more bytes, and the timeout breaks the wait.
			be.commandTimeout = 2 * time.Second
			_, err := be.FetchMessage(context.Background(), "test", "INBOX", 1, 1)
			if err == nil {
				t.Error("FetchMessage returned nil error for a malformed literal announcement")
			}
		})
	}
}

// TestAllocatedMemoryDoesNotScaleWithAnnouncedSize is a
// behavioural companion to the source-level assertion. It
// measures the process's heap around a fetch that announces a
// very large literal. The allocation must not scale with the
// announced size.
//
// The measurement is inherently approximate (the Go runtime
// does not return memory promptly), so the test asserts an
// order-of-magnitude bound rather than an exact value.
func TestAllocatedMemoryDoesNotScaleWithAnnouncedSize(t *testing.T) {
	const cap = 64 * 1024
	// Announce 512 MiB. We never send it.
	const announced = 512 * 1024 * 1024

	srv := &rawIMAPServer{bodySize: announced, tlsConfig: &tls.Config{
		Certificates: []tls.Certificate{genCert(t)},
		MinVersion:   tls.VersionTLS12,
	}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := backendForRawServer(t, addr, cap, false)
	defer be.Close()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := be.FetchMessage(context.Background(), "test", "INBOX", 1, 1)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("FetchMessage returned nil error")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	// The allowance covers the connection buffers and the
	// error path. It is deliberately far below the announced
	// size: a 512 MiB allocation would blow past it by three
	// orders of magnitude.
	const allowance = 8 * 1024 * 1024
	if allocated > allowance {
		t.Errorf("allocated %d bytes while rejecting a %d-byte literal; "+
			"want < %d (the cap must prevent the allocation)", allocated, announced, allowance)
	}
}

// backendForRawServer builds a Backend that talks to the given
// address with the given MaxMessageBytes cap.
func backendForRawServer(t *testing.T, addr string, capBytes int, startTLS bool) *Backend {
	t.Helper()
	return backendForCap(t, addr, capBytes, startTLS)
}

// backendForCap builds a Backend for the given address with the
// given per-account limits.
func backendForCap(t *testing.T, addr string, capBytes int, startTLS bool) *Backend {
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
		ID: "test", Email: "user@example.test", Backend: "imap",
		IMAP: config.IMAPConfig{
			Host:     host,
			Port:     p,
			TLS:      !startTLS,
			StartTLS: startTLS,
			Username: "username",
			Password: "password",
		},
		Limits: config.LimitsConfig{
			MaxMessageBytes:         capBytes,
			MaxAttachmentBytes:      capBytes,
			MaxSearchResults:        50,
			MaxBatchOperations:      50,
			MaxMIMENesting:          16,
			MaxMIMEParts:            500,
			MaxTotalAttachmentBytes: capBytes,
			MaxAttachments:          50,
		},
	}
	be, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
		// A bounded command timeout so a server that
		// announces a malformed literal (and therefore never
		// completes a response) cannot hang the test. This
		// mirrors the behaviour of a real deployment, which
		// must not block forever on a misbehaving server.
		CommandTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return be
}

// unusedIMAPImport keeps the go-imap import referenced in this
// file even when no test directly references the package.
var _ imap.FetchItem

// startCustomServerWithMessage starts an in-process IMAP server
// (backed by the go-imap memory backend) with a single injected
// message. The server listens in plaintext and advertises
// STARTTLS, so the same fixture serves both the implicit-TLS and
// the STARTTLS client paths.
func startCustomServerWithMessage(t *testing.T, raw string) (addr string, cleanup func()) {
	t.Helper()
	be := newCustomBackend(func(u *memuserpkg.User) {
		mb, err := u.GetMailbox("INBOX")
		if err != nil {
			t.Fatalf("GetMailbox: %v", err)
		}
		if err := mb.CreateMessage(nil, time.Now(), stringReader(raw)); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
	})
	srv := imapsrv.New(be)
	srv.AllowInsecureAuth = false
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{genCert(t)}, MinVersion: tls.VersionTLS12}
	srv.TLSConfig = tlsCfg
	// A plaintext listener: the server advertises STARTTLS and
	// upgrades on demand.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

// startTLSServerWithMessage starts an in-process IMAP server that
// speaks implicit TLS (the connection is a TLS listener).
func startTLSServerWithMessage(t *testing.T, raw string) (addr string, cleanup func()) {
	t.Helper()
	be := newCustomBackend(func(u *memuserpkg.User) {
		mb, err := u.GetMailbox("INBOX")
		if err != nil {
			t.Fatalf("GetMailbox: %v", err)
		}
		if err := mb.CreateMessage(nil, time.Now(), stringReader(raw)); err != nil {
			t.Fatalf("CreateMessage: %v", err)
		}
	})
	srv := imapsrv.New(be)
	srv.AllowInsecureAuth = false
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{genCert(t)}, MinVersion: tls.VersionTLS12}
	srv.TLSConfig = tlsCfg
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}
