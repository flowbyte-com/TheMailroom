package imapclient

// Integration tests for IMAP search correctness.
//
// These tests run against the in-process go-imap server with a
// purpose-built fixture set. Every fixture has a controlled
// Subject, From, To/Cc, Date: header, internal date and flag
// set, so the tests can assert the exact AND semantics,
// flag semantics, date granularity, limit and empty-query
// behaviour that the public API promises.
//
// The semantics under test are defined in
// internal/searchspec, which both backends share.

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	memuserpkg "github.com/flowbyte-com/go-imap/backend/memory"
	imapsrv "github.com/flowbyte-com/go-imap/server"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// searchFixture describes one message in the test mailbox.
type searchFixture struct {
	subject string
	from    string
	to      []string
	cc      []string
	// dateHeader is the value of the RFC 5322 Date: header.
	dateHeader string
	// internalDate is the server-side internal date. It
	// deliberately differs from dateHeader for some
	// fixtures so the tests can tell the two apart.
	internalDate time.Time
	flags        []string
	body         string
}

// raw renders the fixture as an RFC 5322 message.
func (f searchFixture) raw() string {
	var b strings.Builder
	b.WriteString("From: " + f.from + "\r\n")
	if len(f.to) > 0 {
		b.WriteString("To: " + strings.Join(f.to, ", ") + "\r\n")
	}
	if len(f.cc) > 0 {
		b.WriteString("Cc: " + strings.Join(f.cc, ", ") + "\r\n")
	}
	b.WriteString("Subject: " + f.subject + "\r\n")
	if f.dateHeader != "" {
		b.WriteString("Date: " + f.dateHeader + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(f.body + "\r\n")
	return b.String()
}

// searchFixtures is the controlled mailbox used by the search
// tests.
//
//	fixture          subject        from            to/cc                     date header              flags
//	---------------  -------------  --------------  ------------------------  -----------------------  ------------
//	alpha            Alpha report   ann@example     bob@example               Mon, 12 Aug 2024 09:00:00 +0000  (none)
//	beta             Beta report    bob@example     carol@example             Mon, 12 Aug 2024 10:00:00 +0000  \Seen
//	gamma            Gamma report   ann@example     dave@example, eve@example  Tue, 13 Aug 2024 11:00:00 +0000  \Seen \Flagged
//	delta            Delta report    carol@example   ann@example               Wed, 14 Aug 2024 12:00:00 +0000  \Flagged
//	epsilon          Epsilon report  ann@example     bob@example               Thu, 15 Aug 2024 13:00:00 +0000  (none)
func searchFixtures() []searchFixture {
	d := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return t
	}
	return []searchFixture{
		{
			subject:      "Alpha report",
			from:         "ann@example.test",
			to:           []string{"bob@example.test"},
			dateHeader:   "Mon, 12 Aug 2024 09:00:00 +0000",
			internalDate: d("2024-08-12T09:00:00Z"),
			body:         "alpha body",
		},
		{
			subject:      "Beta report",
			from:         "bob@example.test",
			to:           []string{"carol@example.test"},
			dateHeader:   "Mon, 12 Aug 2024 10:00:00 +0000",
			internalDate: d("2024-08-12T10:00:00Z"),
			flags:        []string{"\\Seen"},
			body:         "beta body",
		},
		{
			subject:      "Gamma report",
			from:         "ann@example.test",
			to:           []string{"dave@example.test"},
			cc:           []string{"eve@example.test"},
			dateHeader:   "Tue, 13 Aug 2024 11:00:00 +0000",
			internalDate: d("2024-08-13T11:00:00Z"),
			flags:        []string{"\\Seen", "\\Flagged"},
			body:         "gamma body",
		},
		{
			subject:      "Delta report",
			from:         "carol@example.test",
			to:           []string{"ann@example.test"},
			dateHeader:   "Wed, 14 Aug 2024 12:00:00 +0000",
			internalDate: d("2024-08-14T12:00:00Z"),
			flags:        []string{"\\Flagged"},
			body:         "delta body",
		},
		{
			subject:      "Epsilon report",
			from:         "ann@example.test",
			to:           []string{"bob@example.test"},
			dateHeader:   "Thu, 15 Aug 2024 13:00:00 +0000",
			internalDate: d("2024-08-15T13:00:00Z"),
			body:         "epsilon body",
		},
	}
}

// startSearchServer starts an in-process IMAP server whose INBOX
// contains the search fixtures.
func startSearchServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	return startSearchServerWithFixtures(t, searchFixtures())
}

// startSearchServerWithFixtures starts an in-process IMAP server
// whose INBOX contains the supplied fixtures.
func startSearchServerWithFixtures(t *testing.T, fixtures []searchFixture) (addr string, cleanup func()) {
	t.Helper()
	be := newCustomBackend(func(u *memuserpkg.User) {
		mb, err := u.GetMailbox("INBOX")
		if err != nil {
			t.Fatalf("GetMailbox: %v", err)
		}
		for _, f := range fixtures {
			if err := mb.CreateMessage(f.flags, f.internalDate, stringReader(f.raw())); err != nil {
				t.Fatalf("CreateMessage: %v", err)
			}
		}
	})
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

// seedSubject is the subject of the message the go-imap memory
// backend seeds into a fresh INBOX. The search tests account for
// it explicitly.
const seedSubject = "A little message, just for you"

// searchSubjects runs a search and returns the subjects of the
// matching messages, sorted, excluding the memory backend's seed
// message.
func searchSubjects(t *testing.T, be *Backend, q mailbox.SearchQuery) []string {
	t.Helper()
	res, err := be.SearchMessages(context.Background(), "test", "INBOX", q)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	out := make([]string, 0, len(res.Messages))
	for _, m := range res.Messages {
		if m.Subject == seedSubject {
			continue
		}
		out = append(out, m.Subject)
	}
	sort.Strings(out)
	return out
}

// equalSubjects compares two subject lists.
func equalSubjects(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// --- the tests ---

// TestSearchSubjectFilter verifies that the Subject filter uses
// the IMAP Subject search key and matches on a substring.
func TestSearchSubjectFilter(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{Subject: "report"})
	want := []string{"Alpha report", "Beta report", "Delta report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Subject=report matched %v, want %v", got, want)
	}

	// A distinctive substring narrows the result.
	got = searchSubjects(t, be, mailbox.SearchQuery{Subject: "Gamma"})
	want = []string{"Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Subject=Gamma matched %v, want %v", got, want)
	}
}

// TestSearchFromFilter verifies that the From filter uses the
// IMAP From search key.
func TestSearchFromFilter(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{From: "ann@example.test"})
	want := []string{"Alpha report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("From=ann matched %v, want %v", got, want)
	}
}

// TestSearchToFilterMatchesToAndCc verifies that the To filter
// matches a recipient in To *or* in Cc. The Gamma fixture has
// its match in Cc only.
func TestSearchToFilterMatchesToAndCc(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{To: "bob@example.test"})
	want := []string{"Alpha report", "Epsilon report"}
	if !equalSubjects(got, want) {
		t.Errorf("To=bob matched %v, want %v", got, want)
	}

	// eve is only in Gamma's Cc.
	got = searchSubjects(t, be, mailbox.SearchQuery{To: "eve@example.test"})
	want = []string{"Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("To=eve (Cc only) matched %v, want %v", got, want)
	}
}

// TestSearchCriteriaAreANDed is the core regression for the
// pre-existing OR semantics. A message that matches the Subject
// filter but not the From filter (and vice versa) must NOT be
// returned.
func TestSearchCriteriaAreANDed(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// Subject=Gamma matches only Gamma (from ann).
	// From=carol matches Beta and Delta.
	// The conjunction must be empty.
	got := searchSubjects(t, be, mailbox.SearchQuery{
		Subject: "Gamma",
		From:    "carol@example.test",
	})
	if len(got) != 0 {
		t.Errorf("Subject=Gamma AND From=carol matched %v, want no results (AND semantics)", got)
	}

	// A conjunction that does hold: Gamma (from ann) AND
	// To=eve (Cc).
	got = searchSubjects(t, be, mailbox.SearchQuery{
		Subject: "Gamma",
		To:      "eve@example.test",
	})
	want := []string{"Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Subject=Gamma AND To=eve matched %v, want %v", got, want)
	}

	// Three-way conjunction that holds.
	got = searchSubjects(t, be, mailbox.SearchQuery{
		Subject: "report",
		From:    "ann@example.test",
		To:      "bob@example.test",
	})
	want = []string{"Alpha report", "Epsilon report"}
	if !equalSubjects(got, want) {
		t.Errorf("Subject=report AND From=ann AND To=bob matched %v, want %v", got, want)
	}
}

// TestSearchFlaggedFilter verifies the Flagged filter maps to
// the IMAP FLAGGED search key.
func TestSearchFlaggedFilter(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{Flagged: true})
	want := []string{"Delta report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Flagged matched %v, want %v", got, want)
	}
}

// TestSearchUnseenFilter verifies the Unseen filter maps to the
// IMAP UNSEEN search key (a message without \Seen).
func TestSearchUnseenFilter(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{Unseen: true})
	want := []string{"Alpha report", "Delta report", "Epsilon report"}
	if !equalSubjects(got, want) {
		t.Errorf("Unseen matched %v, want %v", got, want)
	}
}

// TestSearchSeenAndUnseenCombine verifies that the flag criteria
// AND with the header criteria.
func TestSearchSeenAndUnseenCombine(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// Delta is \Flagged and not \Seen; Gamma is \Flagged and
	// \Seen. Flagged AND Unseen must match only Delta.
	got := searchSubjects(t, be, mailbox.SearchQuery{Flagged: true, Unseen: true})
	want := []string{"Delta report"}
	if !equalSubjects(got, want) {
		t.Errorf("Flagged AND Unseen matched %v, want %v", got, want)
	}
}

// TestSearchDateBoundaries verifies the day-granularity date
// semantics.
//
// The IMAP search date carries no time, so "since 2024-08-12"
// includes every message dated on 2024-08-12, even one sent at
// 09:00 when the caller asked for 23:00. The query value is
// truncated to its UTC day, while the message Date header keeps
// the calendar day named by its own fields; the memory backend
// applies the same two rules, so the two agree.
func TestSearchDateBoundaries(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	// Since 2024-08-12T00:00:00Z: Alpha (12th 09:00) and
	// everything later.
	since := time.Date(2024, 8, 12, 0, 0, 0, 0, time.UTC)
	got := searchSubjects(t, be, mailbox.SearchQuery{Since: &since})
	want := []string{"Alpha report", "Beta report", "Delta report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Since=2024-08-12 matched %v, want %v", got, want)
	}

	// Since 2024-08-13T00:00:00Z excludes the 12th.
	since = time.Date(2024, 8, 13, 0, 0, 0, 0, time.UTC)
	got = searchSubjects(t, be, mailbox.SearchQuery{Since: &since})
	want = []string{"Delta report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Since=2024-08-13 matched %v, want %v", got, want)
	}

	// Before 2024-08-14T00:00:00Z includes the 12th and
	// 13th but not the 14th.
	before := time.Date(2024, 8, 14, 0, 0, 0, 0, time.UTC)
	got = searchSubjects(t, be, mailbox.SearchQuery{Before: &before})
	want = []string{"Alpha report", "Beta report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Before=2024-08-14 matched %v, want %v", got, want)
	}

	// A time-of-day on the boundary date does not narrow the
	// result: the IMAP date key has no time component, so
	// "since 2024-08-12T23:00:00Z" still includes Alpha,
	// which was sent at 09:00 that day. The memory backend
	// truncates the same way, so the two agree.
	since = time.Date(2024, 8, 12, 23, 0, 0, 0, time.UTC)
	got = searchSubjects(t, be, mailbox.SearchQuery{Since: &since})
	want = []string{"Alpha report", "Beta report", "Delta report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Since=2024-08-12T23:00 matched %v, want %v (the time of day must be discarded)", got, want)
	}
}

// TestSearchEmptyQueryReturnsEverything verifies the documented
// empty-query behaviour: every message in the mailbox, bounded
// by the limit.
func TestSearchEmptyQueryReturnsEverything(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{})
	want := []string{"Alpha report", "Beta report", "Delta report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("empty query matched %v, want %v", got, want)
	}
}

// TestSearchLimit verifies that the limit is honoured.
//
// The subject filter is used so that the memory backend's seed
// message (which the limit would otherwise consume) is not part
// of the result set.
func TestSearchLimit(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{Subject: "report", Limit: 2})
	if len(got) != 2 {
		t.Errorf("Limit=2 matched %d messages, want 2", len(got))
	}

	// A limit larger than the result set returns everything.
	got = searchSubjects(t, be, mailbox.SearchQuery{Subject: "report", Limit: 100})
	if len(got) != 5 {
		t.Errorf("Limit=100 matched %d messages, want 5", len(got))
	}
}

// TestSearchBodyFilter verifies the Body filter maps to the IMAP
// BODY search key.
func TestSearchBodyFilter(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	got := searchSubjects(t, be, mailbox.SearchQuery{Body: "gamma body"})
	want := []string{"Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Body=gamma body matched %v, want %v", got, want)
	}
}

// TestSearchErrorPropagates verifies that a failing IMAP
// SEARCH is surfaced as an error rather than being swallowed
// and reported as an empty result set.
//
// The raw server answers a SEARCH with a tagged NO response,
// which the go-imap client turns into an error. The backend
// must return it, so the caller can tell "no matches" apart
// from "the search failed".
func TestSearchErrorPropagates(t *testing.T) {
	addr, cleanup := startFailingSearchServer(t)
	defer cleanup()
	be := testBackend(t, addr)
	defer be.Close()

	_, err := be.SearchMessages(context.Background(), "test", "INBOX", mailbox.SearchQuery{
		Subject: "anything",
	})
	if err == nil {
		t.Fatal("SearchMessages returned nil error for a failing SEARCH; the error must be propagated")
	}
}

// startFailingSearchServer starts an in-process IMAP server that
// rejects every SEARCH with a tagged NO response.
func startFailingSearchServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			upper := strings.ToUpper(line)
			tag := line
			if i := strings.IndexByte(tag, ' '); i > 0 {
				tag = tag[:i]
			}
			switch {
			case strings.Contains(upper, "LOGIN"):
				io.WriteString(conn, tag+" OK logged in\r\n")
			case strings.Contains(upper, "SELECT") || strings.Contains(upper, "EXAMINE"):
				io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] selected\r\n"+tag+" OK selected\r\n")
			case strings.Contains(upper, "SEARCH"):
				// Refuse the search.
				io.WriteString(conn, tag+" NO search is not available\r\n")
			default:
				io.WriteString(conn, tag+" OK done\r\n")
			}
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}
