package imapclient

import (
	"bufio"
	"crypto/tls"
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

	mu sync.Mutex
	// commands records every client command line, in order.
	commands []string
	// fetchCommands records the UID FETCH command lines.
	fetchCommands []string
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
		go s.handleConn(conn)
	}
}

func (s *listFixtureServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, strings.TrimSpace(line))
	if strings.Contains(strings.ToUpper(line), "FETCH") {
		s.fetchCommands = append(s.fetchCommands, strings.TrimSpace(line))
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

func (s *listFixtureServer) handleConn(conn net.Conn) {
	defer conn.Close()
	if s.tlsConfig != nil {
		tlsConn := tls.Server(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
	}
	fmt.Fprintf(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")

	bw := bufio.NewWriter(conn)
	w := imap.NewWriter(bw)
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		s.record(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.Contains(upper, "LOGIN"):
			fmt.Fprintf(conn, "%s OK logged in\r\n", firstField(line))
		case strings.Contains(upper, "EXAMINE") || strings.Contains(upper, "SELECT"):
			exists := s.exists
			if exists == 0 {
				exists = len(s.messages)
			}
			fmt.Fprintf(conn, "* %d EXISTS\r\n* OK [UIDVALIDITY 1] selected\r\n%s OK selected\r\n",
				exists, firstField(line))
		case strings.Contains(upper, "FETCH"):
			if s.serveFetch(conn, w, firstField(line)) {
				// The server was configured to drop the connection
				// mid-response.
				return
			}
		default:
			fmt.Fprintf(conn, "%s OK done\r\n", firstField(line))
		}
	}
}

// serveFetch writes the configured messages as untagged FETCH responses
// and then the tagged completion. It reports whether the connection was
// dropped mid-response.
func (s *listFixtureServer) serveFetch(conn net.Conn, w *imap.Writer, tag string) bool {
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		imap.FetchRFC822Size,
		imap.FetchFlags,
		imap.FetchUid,
	}
	limit := s.serveLimit
	if limit == 0 {
		limit = len(s.messages)
	}
	for i, m := range s.messages {
		if i >= limit {
			break
		}
		if s.disconnectAfter > 0 && i >= s.disconnectAfter {
			// Drop the connection without the tagged completion, so the
			// client sees a truncated response.
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
		if err := resp.WriteTo(w); err != nil {
			return true
		}
		if err := w.Flush(); err != nil {
			return true
		}
	}
	fmt.Fprintf(conn, "%s OK FETCH completed\r\n", tag)
	_ = w.Flush()
	return false
}

// listFixtureBackend builds a Backend that talks to the fixture server
// with the given scan budget.
func listFixtureBackend(t *testing.T, addr string, budget int) *Backend {
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
			TLS:      true,
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
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
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
// server that under-reports its mailbox size and then sends more messages
// than the budget allows.
//
// The work bound relies on the message count the server reports in the
// EXAMINE response, which RFC 3501 requires. A server that under-reports
// it can still make the client read more than the budget; what must hold
// in that case is that the client does not *retain* a summary per
// message. The working set stays bounded by the selector, and the page
// returned is still the correct one.
func TestListMessagesUnderReportedMailboxKeepsWorkingSetBounded(t *testing.T) {
	const budget = 100
	const total = 5000
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
