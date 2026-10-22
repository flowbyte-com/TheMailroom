package imapclient

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// --- list fixture server ---
//
// listFixtureServer is a minimal IMAP server that serves a fixed set of
// messages with caller-chosen internal dates. It exists because the
// listing order is by internal date, which is unrelated to UID order, so
// the fixtures must be able to place internal dates deliberately out of
// UID order.
//
// The server records the commands it receives, so a test can observe the
// FETCH range the client asked for, and it can stop mid-response or
// disconnect to exercise the failure paths.

type listFixtureMessage struct {
	uid          uint32
	internalDate time.Time
	subject      string
	size         uint32
}

type listFixtureServer struct {
	ln net.Listener

	// tlsConfig, when non-nil, wraps the accepted connections in TLS.
	tlsConfig *tls.Config

	// startTLS makes the listener plaintext and advertises STARTTLS,
	// upgrading the connection on demand. The same fixture therefore
	// serves both the implicit-TLS and the STARTTLS client paths.
	startTLS bool

	// messages are served on a UID FETCH.
	messages []listFixtureMessage

	// exists overrides the message count reported by the EXAMINE
	// response. Zero reports len(messages).
	exists int

	// serveLimit limits how many messages are served on a FETCH.
	// Zero serves all of them.
	serveLimit int

	// disconnectAfter closes the connection after that many FETCH
	// responses have been sent, without the tagged completion. Zero
	// never disconnects.
	disconnectAfter int

	// stallAfter stops the response after that many FETCH responses
	// have been sent: the tagged completion is not sent and the
	// connection is not closed, so the server stalls mid-response.
	// Zero never stalls.
	stallAfter int

	// rejectLogin makes the server refuse the LOGIN command, so a
	// reconnect attempt fails at authentication.
	rejectLogin bool

	// searchUIDs is the UID list the server returns for a UID SEARCH.
	// A nil list means the search matched nothing.
	searchUIDs []uint32

	// searchMalformed makes the server answer a UID SEARCH with a
	// field that is not a number, so the client sees a malformed
	// response.
	searchMalformed bool

	// searchTruncate makes the server drop the connection after the
	// untagged SEARCH response, without the tagged completion.
	searchTruncate bool

	// searchStall makes the server stop after the untagged SEARCH
	// response: the tagged completion is never sent and the connection
	// stays open.
	searchStall bool

	// rawSearchResponse, when set, is written verbatim in answer to a
	// UID SEARCH, instead of the normal response. It lets a test
	// produce a response the fixture would never send, such as an
	// oversized atom.
	rawSearchResponse string

	// badCharsetOnce makes the server answer the first UID SEARCH with
	// NO [BADCHARSET], so the client retries the search with the
	// US-ASCII charset.
	badCharsetOnce bool

	mu sync.Mutex
	// commands records every client command line, in order.
	commands []string
	// fetchCommands records the UID FETCH command lines.
	fetchCommands []string
	// searchCommands records the UID SEARCH command lines.
	searchCommands []string
	// searches counts the UID SEARCH commands received.
	searches int
	// connections counts the accepted connections.
	connections int
	// servedMessages counts the FETCH responses written.
	servedMessages int
	// peerClosed counts the connections whose read loop ended because
	// the peer closed the connection rather than because of a read
	// deadline.
	peerClosed int
	// stalled counts the FETCH responses that stalled mid-response.
	stalled int
	// terminated counts the sessions the server ended with BYE.
	terminated int
	// conns holds the live connections, so that a test can end the
	// session on them.
	conns map[*listFixtureConn]struct{}
	// logins counts the LOGIN commands received.
	logins int
	// rejected counts the LOGIN commands the server refused.
	rejected int
}

func (s *listFixtureServer) start(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	if s.tlsConfig == nil {
		s.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{genCert(t)},
			MinVersion:   tls.VersionTLS12,
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	go s.serve()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func (s *listFixtureServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connections++
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

// connectionCount returns the number of accepted connections.
func (s *listFixtureServer) connectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

// setMessages replaces the served messages and the reported mailbox size.
// It lets a test change the fixture's behaviour between two calls on the
// same backend.
func (s *listFixtureServer) setMessages(msgs []listFixtureMessage, exists int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = msgs
	s.exists = exists
}

// snapshot returns the messages to serve and the mailbox size to report.
func (s *listFixtureServer) snapshot() ([]listFixtureMessage, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages, s.exists
}

// servedCount returns the number of FETCH responses written.
func (s *listFixtureServer) servedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.servedMessages
}

// peerClosedCount returns the number of connections the client closed.
func (s *listFixtureServer) peerClosedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peerClosed
}

// stalledCount returns the number of FETCH responses that stalled.
func (s *listFixtureServer) stalledCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stalled
}

func (s *listFixtureServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, strings.TrimSpace(line))
	upper := strings.ToUpper(line)
	if strings.Contains(upper, "FETCH") {
		s.fetchCommands = append(s.fetchCommands, strings.TrimSpace(line))
	}
	if strings.Contains(upper, "SEARCH") {
		s.searchCommands = append(s.searchCommands, strings.TrimSpace(line))
	}
}

func (s *listFixtureServer) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.fetchCommands)
}

func (s *listFixtureServer) lastFetch() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.fetchCommands) == 0 {
		return ""
	}
	return s.fetchCommands[len(s.fetchCommands)-1]
}

func (s *listFixtureServer) commandCount(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.commands {
		if strings.Contains(strings.ToUpper(c), prefix) {
			n++
		}
	}
	return n
}

// listFixtureConn is one accepted connection.
//
// The fixture writes the responses from the connection's goroutine, and
// a test may end the session from another one, so every write is
// serialised through the connection's mutex.
//
// A write always flushes the buffered writer first, so a write can
// never overtake a response that is still buffered. That matters for
// the BYE that ends a session: it must arrive after the responses
// that preceded it.
type listFixtureConn struct {
	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	w    *imap.Writer
}

// newListFixtureConn wraps an accepted connection.
func newListFixtureConn(conn net.Conn) *listFixtureConn {
	c := &listFixtureConn{conn: conn}
	c.reset(conn)
	return c
}

// reset installs a (possibly upgraded) connection and its readers and
// writers.
func (c *listFixtureConn) reset(conn net.Conn) {
	c.conn = conn
	c.br = bufio.NewReader(conn)
	c.bw = bufio.NewWriter(conn)
	c.w = imap.NewWriter(c.bw)
}

// upgrade installs an upgraded connection. It is called from the
// connection's own goroutine, but it takes the mutex because a test
// may be writing the BYE at the same time.
func (c *listFixtureConn) upgrade(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reset(conn)
}

// writeLine writes a line to the connection. The buffered writer is
// flushed first, so the line cannot overtake a buffered response.
func (c *listFixtureConn) writeLine(format string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.bw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(c.conn, format, args...)
	return err
}

// writeResponse writes through the buffered writer and flushes it, so
// the response is on the wire before the command that follows it.
func (c *listFixtureConn) writeResponse(write func(*imap.Writer) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := write(c.w); err != nil {
		return err
	}
	return c.bw.Flush()
}

// endSession ends the session with a BYE.
//
// The buffered writer is flushed first, so the BYE cannot overtake a
// response that is still buffered. The test calls this only after the
// command it wants to interrupt has completed on the client, so the
// BYE cannot race that command's completion.
func (c *listFixtureConn) endSession() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.bw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(c.conn, "* BYE the server is closing the connection\r\n")
	return err
}

// endSessions ends the session on every live connection.
//
// A test calls it after the operation it wants to interrupt has
// returned, so the BYE is only ever sent on an idle connection and
// cannot race the completion of the command that preceded it.
func (s *listFixtureServer) endSessions() {
	s.mu.Lock()
	conns := make([]*listFixtureConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if err := c.endSession(); err == nil {
			s.mu.Lock()
			s.terminated++
			s.mu.Unlock()
		}
	}
}

func (s *listFixtureServer) handleConn(conn net.Conn) {
	defer conn.Close()
	if !s.startTLS && s.tlsConfig != nil {
		tlsConn := tls.Server(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
	}
	caps := "IMAP4rev1"
	if s.startTLS {
		caps = "IMAP4rev1 STARTTLS"
	}
	fc := newListFixtureConn(conn)
	s.mu.Lock()
	if s.conns == nil {
		s.conns = make(map[*listFixtureConn]struct{})
	}
	s.conns[fc] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, fc)
		s.mu.Unlock()
	}()

	if err := fc.writeLine("* OK [CAPABILITY %s] ready\r\n", caps); err != nil {
		return
	}

	for {
		line, err := fc.br.ReadString('\n')
		if err != nil {
			s.recordPeerClose(err)
			return
		}
		s.record(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.Contains(upper, "STARTTLS"):
			if err := fc.writeLine("%s OK Begin TLS negotiation now\r\n", firstField(line)); err != nil {
				return
			}
			if fc.br.Buffered() != 0 {
				// The client sent data before the handshake began.
				// That is a protocol violation and the fixture
				// refuses to guess where the TLS record starts.
				return
			}
			tlsConn := tls.Server(fc.conn, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			fc.upgrade(tlsConn)
		case strings.Contains(upper, "LOGIN"):
			if s.rejectLoginNow() {
				fc.writeLine("%s NO login refused\r\n", firstField(line))
				return
			}
			if err := fc.writeLine("%s OK logged in\r\n", firstField(line)); err != nil {
				return
			}
		case strings.Contains(upper, "EXAMINE") || strings.Contains(upper, "SELECT"):
			msgs, exists := s.snapshot()
			if exists == 0 {
				exists = len(msgs)
			}
			if err := fc.writeLine("* %d EXISTS\r\n* OK [UIDVALIDITY 1] selected\r\n%s OK selected\r\n",
				exists, firstField(line)); err != nil {
				return
			}
		case strings.Contains(upper, "SEARCH"):
			if s.serveSearch(fc, firstField(line)) {
				// The server was configured to drop the connection
				// mid-response.
				return
			}
		case strings.Contains(upper, "FETCH"):
			if s.serveFetch(fc, firstField(line)) {
				// The server was configured to drop the connection
				// mid-response.
				return
			}
		default:
			if err := fc.writeLine("%s OK done\r\n", firstField(line)); err != nil {
				return
			}
		}
	}
}

// recordPeerClose notes that the client closed the connection. A read
// that fails with a deadline exceeded is not a close: the fixture sets a
// read deadline so a stalled server cannot block forever.
func (s *listFixtureServer) recordPeerClose(err error) {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return
	}
	s.mu.Lock()
	s.peerClosed++
	s.mu.Unlock()
}

// rejectLoginNow reports whether the server must refuse the LOGIN, and
// records the attempt.
func (s *listFixtureServer) rejectLoginNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logins++
	if !s.rejectLogin {
		return false
	}
	s.rejected++
	return true
}

// setRejectLogin makes the server refuse or accept the LOGIN command.
func (s *listFixtureServer) setRejectLogin(reject bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectLogin = reject
}

// terminatedCount returns the number of sessions the server ended.
func (s *listFixtureServer) terminatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminated
}

// rejectedLoginCount returns the number of refused LOGIN commands.
func (s *listFixtureServer) rejectedLoginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected
}

// loginCount returns the number of LOGIN commands received.
func (s *listFixtureServer) loginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// serveSearch writes the configured UID SEARCH response.
//
// The response is the untagged `* SEARCH <uid> ...` line followed by the
// tagged completion. The malformed and truncated variants stop before
// the completion: the malformed one leaves the field invalid, the
// truncated one drops the connection, so the client sees a broken
// response.
//
// It reports whether the connection was dropped mid-response.
func (s *listFixtureServer) serveSearch(fc *listFixtureConn, tag string) bool {
	s.mu.Lock()
	uids := s.searchUIDs
	malformed := s.searchMalformed
	truncate := s.searchTruncate
	stall := s.searchStall
	badCharset := s.badCharsetOnce && s.searches == 0
	s.searches++
	s.mu.Unlock()

	if badCharset {
		// Refuse the charset, the way a server that does not support
		// UTF-8 does. The client is expected to retry the search with
		// US-ASCII.
		fc.writeLine("%s NO [BADCHARSET (\"US-ASCII\")] charset not supported\r\n", tag)
		return false
	}

	if s.rawSearchResponse != "" {
		fc.writeLine("%s", s.rawSearchResponse)
		return false
	}

	fields := []interface{}{imap.RawString("SEARCH")}
	for _, u := range uids {
		fields = append(fields, u)
	}
	if malformed {
		fields = append(fields, imap.RawString("not-a-number"))
	}
	resp := imap.NewUntaggedResp(fields)
	if err := fc.writeResponse(resp.WriteTo); err != nil {
		return true
	}
	if truncate {
		// Drop the connection without the tagged completion, so the
		// client sees a truncated response.
		return true
	}
	if stall {
		// Stop mid-response: the tagged completion is never sent and
		// the connection stays open.
		s.mu.Lock()
		s.stalled++
		s.mu.Unlock()
		s.waitForPeerClose(fc)
		return true
	}
	fc.writeLine("%s OK SEARCH completed\r\n", tag)
	return false
}

// searchCount returns the number of UID SEARCH commands received.
func (s *listFixtureServer) searchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.searchCommands)
}

// lastSearch returns the last UID SEARCH command line.
func (s *listFixtureServer) lastSearch() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.searchCommands) == 0 {
		return ""
	}
	return s.searchCommands[len(s.searchCommands)-1]
}

// searchCommand returns the n-th UID SEARCH command line.
func (s *listFixtureServer) searchCommand(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 || n >= len(s.searchCommands) {
		return ""
	}
	return s.searchCommands[n]
}

// setSearchUIDs replaces the UID list the server answers a UID SEARCH
// with.
func (s *listFixtureServer) setSearchUIDs(uids []uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchUIDs = uids
}

// serveFetch writes the configured messages as untagged FETCH responses
// and then the tagged completion. It reports whether the connection was
// dropped mid-response.
func (s *listFixtureServer) serveFetch(fc *listFixtureConn, tag string) bool {
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		imap.FetchRFC822Size,
		imap.FetchFlags,
		imap.FetchUid,
	}
	limit := s.serveLimit
	messages, _ := s.snapshot()
	if limit == 0 {
		limit = len(messages)
	}
	for i, m := range messages {
		if i >= limit {
			break
		}
		if s.disconnectAfter > 0 && i >= s.disconnectAfter {
			// Drop the connection without the tagged completion, so the
			// client sees a truncated response.
			return true
		}
		if s.stallAfter > 0 && i >= s.stallAfter {
			// Stall mid-response: the tagged completion is never sent
			// and the connection stays open. The only way out is for
			// the client to abort the transfer.
			s.mu.Lock()
			s.stalled++
			s.mu.Unlock()
			s.waitForPeerClose(fc)
			return true
		}
		msg := imap.NewMessage(uint32(i+1), items)
		msg.Uid = m.uid
		msg.InternalDate = m.internalDate
		msg.Size = m.size
		msg.Flags = []string{}
		msg.Envelope = &imap.Envelope{
			Date:    m.internalDate,
			Subject: m.subject,
			From:    []*imap.Address{{MailboxName: "sender", HostName: "example.test"}},
			To:      []*imap.Address{{MailboxName: "recipient", HostName: "example.test"}},
		}
		resp := imap.NewUntaggedResp([]interface{}{msg.SeqNum, imap.RawString("FETCH"), msg.Format()})
		if err := fc.writeResponse(resp.WriteTo); err != nil {
			return true
		}
		// The counter is incremented as the response goes out, so a
		// test that observes it while the server is still writing sees
		// the responses that were actually delivered.
		s.countServed(1)
	}
	if err := fc.writeLine("%s OK FETCH completed\r\n", tag); err != nil {
		return true
	}
	return false
}

// countServed records the number of FETCH responses written.
func (s *listFixtureServer) countServed(n int) {
	s.mu.Lock()
	s.servedMessages += n
	s.mu.Unlock()
}

// waitForPeerClose blocks until the client closes the connection, or
// until the read deadline expires. The deadline is the safety net for a
// client that never aborts: the fixture must not block forever.
func (s *listFixtureServer) waitForPeerClose(fc *listFixtureConn) {
	_ = fc.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var buf [1]byte
	for {
		_, err := fc.br.Read(buf[:])
		if err != nil {
			s.recordPeerClose(err)
			return
		}
	}
}

// listFixtureBackend builds a Backend that talks to the fixture server
// with the given scan budget.
func listFixtureBackend(t *testing.T, addr string, budget int) *Backend {
	t.Helper()
	// A bounded command timeout, so that a fixture which does not
	// answer fails the test instead of hanging it.
	return listFixtureBackendMode(t, addr, budget, false, 30*time.Second)
}

// listFixtureBackendMode builds a Backend that talks to the fixture
// server with the given scan budget, over implicit TLS or over STARTTLS.
//
// commandTimeout bounds an IMAP command. Zero means the go-imap default
// (no timeout), which makes a test that expects a prompt failure fail by
// hanging instead: the tests that need a bound set one.
func listFixtureBackendMode(t *testing.T, addr string, budget int, startTLS bool, commandTimeout time.Duration) *Backend {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	cfg := config.AccountConfig{
		ID: "test", Email: "user@example.test", Backend: "imap",
		IMAP: config.IMAPConfig{
			Host:     host,
			Port:     port,
			Username: "username",
			Password: "password",
			TLS:      !startTLS,
			StartTLS: startTLS,
		},
		Limits: config.LimitsConfig{
			MaxMessageBytes:         1 << 20,
			MaxAttachmentBytes:      1 << 20,
			MaxSearchResults:        50,
			MaxBatchOperations:      50,
			MaxListScanMessages:     budget,
			MaxMIMENesting:          16,
			MaxMIMEParts:            100,
			MaxTotalAttachmentBytes: 1 << 20,
			MaxAttachments:          10,
			MaxRecipients:           10,
		},
	}
	be, err := New(cfg, Options{
		DialTimeout:    5 * time.Second,
		TLSConfig:      &tls.Config{InsecureSkipVerify: true},
		CommandTimeout: commandTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return be
}

// --- helpers ---

// fixtureMessage builds a fixture message.
func fixtureMessage(uid uint32, internalDate time.Time, subject string) listFixtureMessage {
	return listFixtureMessage{
		uid:          uid,
		internalDate: internalDate,
		subject:      subject,
		size:         100,
	}
}

// baseTime is a fixed reference point for the fixture dates.
var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// --- tests ---

// TestListMessagesOrdersByInternalDateNotUID is the central ordering
// invariant. The fixture places internal dates deliberately out of UID
// order, so an implementation that ordered by UID would fail it.
func TestListMessagesOrdersByInternalDateNotUID(t *testing.T) {
	// UIDs ascend, internal dates do not.
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime.Add(-5*time.Hour), "uid1-oldest"),
		fixtureMessage(2, baseTime.Add(-1*time.Hour), "uid2-newest"),
		fixtureMessage(3, baseTime.Add(-4*time.Hour), "uid3-second-oldest"),
		fixtureMessage(4, baseTime.Add(-2*time.Hour), "uid4-second-newest"),
		fixtureMessage(5, baseTime.Add(-3*time.Hour), "uid5-middle"),
	}
	srv := &listFixtureServer{messages: msgs}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	want := []string{"uid2-newest", "uid4-second-newest", "uid5-middle", "uid3-second-oldest", "uid1-oldest"}
	got := subjects(res.Messages)
	if len(got) != len(want) {
		t.Fatalf("got %d messages (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// TestListMessagesTieBreaksByUIDDescending checks that messages with
// equal internal dates are ordered by descending UID.
func TestListMessagesTieBreaksByUIDDescending(t *testing.T) {
	same := baseTime
	msgs := []listFixtureMessage{
		fixtureMessage(7, same, "uid7"),
		fixtureMessage(3, same, "uid3"),
		fixtureMessage(9, same, "uid9"),
		fixtureMessage(5, same, "uid5"),
	}
	srv := &listFixtureServer{messages: msgs}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	got := subjects(res.Messages)
	want := []string{"uid9", "uid7", "uid5", "uid3"}
	for i := range want {
		if i >= len(got) {
			t.Fatalf("got %d messages (%v), want %d", len(got), got, len(want))
		}
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// TestListMessagesOffsetAndLimitBoundaries checks the pagination
// boundaries: the offset is applied after filtering and ordering, and the
// limit after the offset.
func TestListMessagesOffsetAndLimitBoundaries(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime.Add(-1*time.Hour), "m1"),
		fixtureMessage(2, baseTime.Add(-2*time.Hour), "m2"),
		fixtureMessage(3, baseTime.Add(-3*time.Hour), "m3"),
		fixtureMessage(4, baseTime.Add(-4*time.Hour), "m4"),
		fixtureMessage(5, baseTime.Add(-5*time.Hour), "m5"),
	}
	srv := &listFixtureServer{messages: msgs}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	cases := []struct {
		name   string
		offset int
		limit  int
		want   []string
	}{
		{name: "offset 0 limit 2", offset: 0, limit: 2, want: []string{"m1", "m2"}},
		{name: "offset 1 limit 2", offset: 1, limit: 2, want: []string{"m2", "m3"}},
		{name: "offset 3 limit 2", offset: 3, limit: 2, want: []string{"m4", "m5"}},
		{name: "offset at end", offset: 5, limit: 2, want: nil},
		{name: "offset beyond end", offset: 99, limit: 2, want: nil},
		{name: "limit beyond end", offset: 0, limit: 99, want: []string{"m1", "m2", "m3", "m4", "m5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
				Offset: tc.offset,
				Limit:  tc.limit,
			})
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			got := subjects(res.Messages)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("message %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestListMessagesDateFiltersApplyBeforePagination checks that the date
// filters are applied before the offset and the limit.
func TestListMessagesDateFiltersApplyBeforePagination(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime.Add(-1*time.Hour), "m1"),
		fixtureMessage(2, baseTime.Add(-2*time.Hour), "m2"),
		fixtureMessage(3, baseTime.Add(-3*time.Hour), "m3"),
		fixtureMessage(4, baseTime.Add(-4*time.Hour), "m4"),
	}
	srv := &listFixtureServer{messages: msgs}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	since := baseTime.Add(-3 * time.Hour)
	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
		Since: &since,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// m3 is exactly at the boundary and is kept: Since is inclusive.
	if got, want := subjects(res.Messages), []string{"m1", "m2", "m3"}; !equalStrings(got, want) {
		t.Errorf("since filter = %v, want %v", got, want)
	}

	before := baseTime.Add(-3 * time.Hour)
	res, err = be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
		Before: &before,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// Before is exclusive, so m3 is dropped.
	if got, want := subjects(res.Messages), []string{"m4"}; !equalStrings(got, want) {
		t.Errorf("before filter = %v, want %v", got, want)
	}

	// The filter is applied before the offset: with a filter that keeps
	// two messages, an offset of one leaves the second one.
	res, err = be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
		Since:  &since,
		Offset: 1,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if got, want := subjects(res.Messages), []string{"m2", "m3"}; !equalStrings(got, want) {
		t.Errorf("filtered then offset = %v, want %v", got, want)
	}
}

// TestListMessagesEmptyMailbox checks the empty case.
func TestListMessagesEmptyMailbox(t *testing.T) {
	srv := &listFixtureServer{}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(res.Messages))
	}
}

// TestListMessagesUsesExamineNotSelect checks the read-only invariant.
func TestListMessagesUsesExamineNotSelect(t *testing.T) {
	srv := &listFixtureServer{messages: []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
	}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10}); err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if n := srv.commandCount("EXAMINE"); n != 1 {
		t.Errorf("client issued %d EXAMINE commands, want 1", n)
	}
	if n := srv.commandCount("SELECT"); n != 0 {
		t.Errorf("client issued %d SELECT commands, want 0 (the mailbox must be opened read-only)", n)
	}
}

// TestListMessagesSurfacesUIDValidity checks that the UIDVALIDITY comes
// from the same EXAMINE that produced the UIDs, and that it is stable
// across calls.
func TestListMessagesSurfacesUIDValidity(t *testing.T) {
	srv := &listFixtureServer{messages: []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
	}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	first, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if first.UIDValidity == 0 {
		t.Fatal("UIDValidity not surfaced")
	}
	second, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if second.UIDValidity != first.UIDValidity {
		t.Errorf("UIDValidity changed between calls: %d then %d", first.UIDValidity, second.UIDValidity)
	}
}

// TestListMessagesObservesFetchRange records the FETCH range the client
// asked for. Listing is ordered by internal date, which is unrelated to
// UID order, so the range is the whole mailbox; the bound on the work is
// the scan budget, not a narrowed range.
func TestListMessagesObservesFetchRange(t *testing.T) {
	srv := &listFixtureServer{messages: []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
		fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
	}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10}); err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if srv.fetchCount() != 1 {
		t.Fatalf("server saw %d fetches, want 1", srv.fetchCount())
	}
	if got := srv.lastFetch(); !strings.Contains(strings.ToUpper(got), "UID FETCH") ||
		!strings.Contains(got, "1:*") {
		t.Errorf("fetch command = %q, want a UID FETCH of 1:*", got)
	}
}

// TestListMessagesScanBudgetRefusesLargeMailbox is the resource-bound
// regression test. A mailbox with more messages than the scan budget must
// be refused with an explicit typed error, and the refusal must happen
// before any FETCH is sent, so no work is done at all.
func TestListMessagesScanBudgetRefusesLargeMailbox(t *testing.T) {
	const budget = 100
	srv := &listFixtureServer{
		exists:   5000,
		messages: []listFixtureMessage{fixtureMessage(1, baseTime, "m1")},
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err == nil {
		t.Fatal("expected an error for a mailbox over the scan budget")
	}
	if !strings.Contains(err.Error(), "list scan budget") {
		t.Errorf("err = %v, want it to mention the scan budget", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a refused mailbox must not return a partial page", len(res.Messages))
	}
	// The refusal happens before the FETCH, so the server did no work.
	if n := srv.fetchCount(); n != 0 {
		t.Errorf("server saw %d fetches, want 0: the over-budget mailbox must be refused before any work", n)
	}
}

// TestListMessagesScanBudgetBoundary checks that a mailbox exactly at the
// budget is still listed, and one message over it is refused.
func TestListMessagesScanBudgetBoundary(t *testing.T) {
	const budget = 5
	msgs := make([]listFixtureMessage, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i+1))
	}

	t.Run("exactly at budget", func(t *testing.T) {
		srv := &listFixtureServer{messages: msgs}
		addr, cleanup := srv.start(t)
		defer cleanup()
		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 3})
		if err != nil {
			t.Fatalf("ListMessages at the budget: %v", err)
		}
		if len(res.Messages) != 3 {
			t.Errorf("got %d messages, want 3", len(res.Messages))
		}
	})

	t.Run("one over budget", func(t *testing.T) {
		srv := &listFixtureServer{messages: msgs, exists: budget + 1}
		addr, cleanup := srv.start(t)
		defer cleanup()
		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 3})
		if err == nil {
			t.Fatal("expected an error one message over the budget")
		}
		if n := srv.fetchCount(); n != 0 {
			t.Errorf("server saw %d fetches, want 0", n)
		}
	})
}

// TestListMessagesLargeMailboxWithinBudget lists a large mailbox and
// checks that the result is exactly the newest messages, and that the
// working set does not grow with the mailbox.
func TestListMessagesLargeMailboxWithinBudget(t *testing.T) {
	const total = 2000
	msgs := make([]listFixtureMessage, total)
	for i := range msgs {
		// The internal dates descend with the UID, so the newest
		// message is the first one.
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Second), fmt.Sprintf("m%d", i+1))
	}
	srv := &listFixtureServer{messages: msgs}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, total)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	want := []string{"m1", "m2", "m3", "m4", "m5"}
	if got := subjects(res.Messages); !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestListMessagesPartialResponseReturnsNoResults checks that a
// truncated response is an error and never a partial page.
func TestListMessagesPartialResponseReturnsNoResults(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
		fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
		fixtureMessage(3, baseTime.Add(-2*time.Hour), "m3"),
	}
	// The server sends one response and then drops the connection
	// without the tagged completion.
	srv := &listFixtureServer{messages: msgs, disconnectAfter: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err == nil {
		t.Fatal("expected an error for a truncated response")
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a truncated response must not produce a partial page", len(res.Messages))
	}
}

// TestSummarySelectorKeepsBestK is the unit-level guarantee that the
// working set does not grow with the number of summaries offered.
func TestSummarySelectorKeepsBestK(t *testing.T) {
	const k = 3
	sel := &summarySelector{limit: k}
	for i := 0; i < 1000; i++ {
		sel.Add(mailbox.MessageSummary{
			UID:          uint32(i + 1),
			InternalDate: baseTime.Add(-time.Duration(i) * time.Minute),
		})
	}
	if sel.Len() != k {
		t.Fatalf("selector retained %d summaries, want %d", sel.Len(), k)
	}
	got := sel.Sorted()
	want := []uint32{1, 2, 3}
	for i, w := range want {
		if got[i].UID != w {
			t.Errorf("kept[%d].UID = %d, want %d (kept %v)", i, got[i].UID, w, uids(got))
		}
	}
}

// TestSummarySelectorEvictsWorst checks that a better summary replaces
// the worst kept one, and that the retained set stays ordered.
func TestSummarySelectorEvictsWorst(t *testing.T) {
	sel := &summarySelector{limit: 2}
	sel.Add(mailbox.MessageSummary{UID: 1, InternalDate: baseTime.Add(-2 * time.Hour)})
	sel.Add(mailbox.MessageSummary{UID: 2, InternalDate: baseTime.Add(-1 * time.Hour)})
	// A worse summary must not be retained.
	sel.Add(mailbox.MessageSummary{UID: 3, InternalDate: baseTime.Add(-3 * time.Hour)})
	if sel.Len() != 2 {
		t.Fatalf("retained %d summaries, want 2", sel.Len())
	}
	// A better summary must replace the worst.
	sel.Add(mailbox.MessageSummary{UID: 4, InternalDate: baseTime})
	if sel.Len() != 2 {
		t.Fatalf("retained %d summaries, want 2", sel.Len())
	}
	got := sel.Sorted()
	if got[0].UID != 4 || got[1].UID != 2 {
		t.Errorf("kept %v, want UIDs 4 and 2", uids(got))
	}
}

// TestListMessagesUnderReportedMailboxKeepsWorkingSetBounded covers a
// server that under-reports its mailbox size and then sends more
// messages than it said.
//
// While the delivered messages stay within the scan budget the listing
// succeeds and the page is the correct one; the working set stays
// bounded by the selector, not by the mailbox size. A server that
// delivers more than the budget is refused instead; see
// TestListScanBudgetUnderReportedExistsCannotExceedBudget.
func TestListMessagesUnderReportedMailboxKeepsWorkingSetBounded(t *testing.T) {
	const budget = 100
	const total = 50
	const subjectSize = 4096
	big := strings.Repeat("x", subjectSize)
	msgs := make([]listFixtureMessage, total)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Second), fmt.Sprintf("m%d-%s", i+1, big))
	}
	// The server claims one message but sends many.
	srv := &listFixtureServer{messages: msgs, exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// The page must still be the newest five, despite the server having
	// sent far more messages than it reported.
	want := []string{"m1-" + big, "m2-" + big, "m3-" + big, "m4-" + big, "m5-" + big}
	got := subjects(res.Messages)
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d subject = %.20q, want %.20q", i, got[i], want[i])
		}
	}
}

// TestSummarySelectorMatchesFullSort is the equivalence property of the
// bounded selection: for any input, the bounded selector must produce
// exactly the page a full sort of every summary would produce.
func TestSummarySelectorMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewSource(20261022))
	for round := 0; round < 200; round++ {
		n := rng.Intn(200)
		all := make([]mailbox.MessageSummary, n)
		for i := range all {
			all[i] = mailbox.MessageSummary{
				UID:          uint32(rng.Intn(50) + 1),
				InternalDate: baseTime.Add(-time.Duration(rng.Intn(10)) * time.Hour),
			}
		}
		offset := rng.Intn(10)
		limit := rng.Intn(10) + 1

		keep := offset + limit
		sel := &summarySelector{limit: keep}
		for _, m := range all {
			sel.Add(m)
		}
		got := sel.Sorted()
		if offset > 0 {
			if offset >= len(got) {
				got = nil
			} else {
				got = got[offset:]
			}
		}
		if len(got) > limit {
			got = got[:limit]
		}
		want := referenceList(all, offset, limit)
		if len(got) != len(want) {
			t.Fatalf("round %d: got %d summaries, want %d", round, len(got), len(want))
		}
		for i := range want {
			if got[i].UID != want[i].UID || !got[i].InternalDate.Equal(want[i].InternalDate) {
				t.Fatalf("round %d: summary %d = %v, want %v", round, i, got[i], want[i])
			}
		}
	}
}

// referenceList is the original, unbounded algorithm: collect every
// summary, sort them all, then apply the offset and the limit. The
// bounded selector must agree with it exactly.
func referenceList(all []mailbox.MessageSummary, offset, limit int) []mailbox.MessageSummary {
	out := make([]mailbox.MessageSummary, len(all))
	copy(out, all)
	sortSummaries(out)
	if offset > 0 {
		if offset >= len(out) {
			return nil
		}
		out = out[offset:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// TestListMessagesHugeOffsetIsBounded checks that a very large offset is
// handled without overflowing the working-set arithmetic and without
// growing the retained set.
func TestListMessagesHugeOffsetIsBounded(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
		fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
	}
	srv := &listFixtureServer{messages: msgs}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	for _, offset := range []int{1 << 30, math.MaxInt32, math.MaxInt64} {
		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
			Offset: offset,
			Limit:  5,
		})
		if err != nil {
			t.Fatalf("ListMessages with offset %d: %v", offset, err)
		}
		if len(res.Messages) != 0 {
			t.Errorf("offset %d returned %d messages, want 0", offset, len(res.Messages))
		}
	}
}

// subjects returns the subjects of a list of summaries, for order
// assertions.
func subjects(list []mailbox.MessageSummary) []string {
	out := make([]string, len(list))
	for i, m := range list {
		out[i] = m.Subject
	}
	return out
}

// uids returns the UIDs of a list of summaries, for order assertions.
func uids(list []mailbox.MessageSummary) []uint32 {
	out := make([]uint32, len(list))
	for i, m := range list {
		out[i] = m.UID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
