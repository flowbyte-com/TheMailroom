package memory

// Tests for the memory backend's search contract.
//
// The memory backend must honour the same public contract as the
// IMAP backend (see internal/searchspec). These tests use the
// same fixture shape as the IMAP integration tests in
// internal/imapclient, so the two backends can be compared
// directly (the cross-backend comparison lives in
// internal/imapclient/search_consistency_test.go).

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// memFixture describes one message in the test mailbox.
type memFixture struct {
	subject string
	from    string
	to      []string
	cc      []string
	date    string
	status  string
	body    string
}

// raw renders the fixture as an RFC 5322 message.
func (f memFixture) raw() string {
	var b []byte
	b = append(b, "From: "+f.from+"\r\n"...)
	if len(f.to) > 0 {
		b = append(b, "To: "+joinComma(f.to)+"\r\n"...)
	}
	if len(f.cc) > 0 {
		b = append(b, "Cc: "+joinComma(f.cc)+"\r\n"...)
	}
	b = append(b, "Subject: "+f.subject+"\r\n"...)
	b = append(b, "Date: "+f.date+"\r\n"...)
	if f.status != "" {
		b = append(b, "Status: "+f.status+"\r\n"...)
	}
	b = append(b, "MIME-Version: 1.0\r\n"...)
	b = append(b, "Content-Type: text/plain; charset=utf-8\r\n"...)
	b = append(b, "\r\n"...)
	b = append(b, f.body+"\r\n"...)
	return string(b)
}

func joinComma(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// memFixtures mirrors the fixture set used by the IMAP search
// integration tests.
func memFixtures() []memFixture {
	return []memFixture{
		{subject: "Alpha report", from: "ann@example.test", to: []string{"bob@example.test"},
			date: "Mon, 12 Aug 2024 09:00:00 +0000", body: "alpha body"},
		{subject: "Beta report", from: "bob@example.test", to: []string{"carol@example.test"},
			date: "Mon, 12 Aug 2024 10:00:00 +0000", status: "R", body: "beta body"},
		{subject: "Gamma report", from: "ann@example.test", to: []string{"dave@example.test"}, cc: []string{"eve@example.test"},
			date: "Tue, 13 Aug 2024 11:00:00 +0000", status: "RF", body: "gamma body"},
		{subject: "Delta report", from: "carol@example.test", to: []string{"ann@example.test"},
			date: "Wed, 14 Aug 2024 12:00:00 +0000", status: "F", body: "delta body"},
		{subject: "Epsilon report", from: "ann@example.test", to: []string{"bob@example.test"},
			date: "Thu, 15 Aug 2024 13:00:00 +0000", body: "epsilon body"},
	}
}

// newFixtureBackend builds a memory backend containing the
// fixtures.
func newFixtureBackend(t *testing.T) *Backend {
	t.Helper()
	dir := t.TempDir()
	for i, f := range memFixtures() {
		name := filepath.Join(dir, fixtureName(i))
		if err := os.WriteFile(name, []byte(f.raw()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureName(i int) string {
	return "m" + string(rune('0'+i)) + ".eml"
}

// searchSubjects runs a search and returns the matching
// subjects, sorted.
func searchSubjects(t *testing.T, b *Backend, q mailbox.SearchQuery) []string {
	t.Helper()
	res, err := b.SearchMessages(context.Background(), "any", "INBOX", q)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	out := make([]string, 0, len(res.Messages))
	for _, m := range res.Messages {
		out = append(out, m.Subject)
	}
	sort.Strings(out)
	return out
}

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

// TestMemorySearchSubject verifies the Subject filter.
func TestMemorySearchSubject(t *testing.T) {
	b := newFixtureBackend(t)
	got := searchSubjects(t, b, mailbox.SearchQuery{Subject: "Gamma"})
	want := []string{"Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Subject=Gamma matched %v, want %v", got, want)
	}
}

// TestMemorySearchFrom verifies the From filter.
func TestMemorySearchFrom(t *testing.T) {
	b := newFixtureBackend(t)
	got := searchSubjects(t, b, mailbox.SearchQuery{From: "ann@example.test"})
	want := []string{"Alpha report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("From=ann matched %v, want %v", got, want)
	}
}

// TestMemorySearchToMatchesCc verifies that the To filter matches
// a recipient in To or in Cc.
func TestMemorySearchToMatchesCc(t *testing.T) {
	b := newFixtureBackend(t)
	got := searchSubjects(t, b, mailbox.SearchQuery{To: "bob@example.test"})
	want := []string{"Alpha report", "Epsilon report"}
	if !equalSubjects(got, want) {
		t.Errorf("To=bob matched %v, want %v", got, want)
	}
	got = searchSubjects(t, b, mailbox.SearchQuery{To: "eve@example.test"})
	want = []string{"Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("To=eve (Cc only) matched %v, want %v", got, want)
	}
}

// TestMemorySearchCriteriaAreANDed verifies that the criteria are
// ANDed, not OR'd.
func TestMemorySearchCriteriaAreANDed(t *testing.T) {
	b := newFixtureBackend(t)
	got := searchSubjects(t, b, mailbox.SearchQuery{Subject: "Gamma", From: "carol@example.test"})
	if len(got) != 0 {
		t.Errorf("Subject=Gamma AND From=carol matched %v, want no results", got)
	}
	got = searchSubjects(t, b, mailbox.SearchQuery{Subject: "Gamma", To: "eve@example.test"})
	want := []string{"Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Subject=Gamma AND To=eve matched %v, want %v", got, want)
	}
}

// TestMemorySearchDateGranularity verifies the day-granularity
// date semantics, matching the IMAP backend.
func TestMemorySearchDateGranularity(t *testing.T) {
	b := newFixtureBackend(t)

	// Since 2024-08-12T23:00 truncates to the day, so the
	// 12th's messages (09:00 and 10:00) are included.
	since := time.Date(2024, 8, 12, 23, 0, 0, 0, time.UTC)
	got := searchSubjects(t, b, mailbox.SearchQuery{Since: &since})
	want := []string{"Alpha report", "Beta report", "Delta report", "Epsilon report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Since=2024-08-12T23:00 matched %v, want %v (the time of day must be discarded)", got, want)
	}

	// Before 2024-08-14 excludes the 14th.
	before := time.Date(2024, 8, 14, 0, 0, 0, 0, time.UTC)
	got = searchSubjects(t, b, mailbox.SearchQuery{Before: &before})
	want = []string{"Alpha report", "Beta report", "Gamma report"}
	if !equalSubjects(got, want) {
		t.Errorf("Before=2024-08-14 matched %v, want %v", got, want)
	}
}

// TestMemorySearchEmptyQuery verifies the empty-query behaviour.
func TestMemorySearchEmptyQuery(t *testing.T) {
	b := newFixtureBackend(t)
	got := searchSubjects(t, b, mailbox.SearchQuery{})
	if len(got) != 5 {
		t.Errorf("empty query matched %d messages, want 5", len(got))
	}
}

// TestMemorySearchLimit verifies the limit is honoured.
func TestMemorySearchLimit(t *testing.T) {
	b := newFixtureBackend(t)
	got := searchSubjects(t, b, mailbox.SearchQuery{Subject: "report", Limit: 2})
	if len(got) != 2 {
		t.Errorf("Limit=2 matched %d messages, want 2", len(got))
	}
}
