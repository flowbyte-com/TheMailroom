package imapclient

// Cross-backend consistency tests.
//
// The IMAP backend delegates matching to the IMAP server; the
// memory backend matches locally. For the public API contract to
// mean anything, the two must agree on the queries that their
// respective capabilities allow.
//
// These tests run the same query set against both backends over
// the same fixture messages and assert that the results match.
// The fixture set is built from the same source data (the
// searchFixture type) so the two backends see identical input.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
	mem "github.com/flowbyte-com/themailroom/internal/mailbox/memory"
)

// memorySearchBackend builds an in-memory backend whose mailbox
// contains the same fixtures the IMAP test server uses.
//
// The fixtures are written as .eml files with an RFC 2156
// Status: header so the memory backend derives the same flags
// the IMAP backend sees.
func memorySearchBackend(t *testing.T) *mem.Backend {
	t.Helper()
	dir := t.TempDir()
	for i, f := range searchFixtures() {
		raw := f.raw()
		if len(f.flags) > 0 {
			// Translate the IMAP system flags into the
			// RFC 2156 Status: header so the memory
			// backend derives the same set.
			var status string
			for _, fl := range f.flags {
				switch fl {
				case "\\Seen":
					status += "R"
				case "\\Flagged":
					status += "F"
				case "\\Deleted":
					status += "D"
				case "\\Answered":
					status += "A"
				}
			}
			raw = injectStatusHeader(raw, status)
		}
		name := filepath.Join(dir, fmt.Sprintf("%02d.eml", i))
		if err := os.WriteFile(name, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := mem.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// injectStatusHeader inserts a Status: header into the header
// block of a raw message.
func injectStatusHeader(raw, status string) string {
	const marker = "\r\nMIME-Version:"
	i := indexOf(raw, marker)
	if i < 0 {
		return raw
	}
	return raw[:i] + "\r\nStatus: " + status + raw[i:]
}

// TestBackendsAgreeOnSearch runs the same queries against the IMAP
// backend and the memory backend and asserts the results are
// identical.
//
// The queries cover the criteria the two backends can both
// honour exactly: the header filters, the flag filters, the
// body filter, the date filters and the empty query.
func TestBackendsAgreeOnSearch(t *testing.T) {
	addr, cleanup := startSearchServer(t)
	defer cleanup()
	imapBe := testBackend(t, addr)
	defer imapBe.Close()
	memBe := memorySearchBackend(t)

	since12 := time.Date(2024, 8, 12, 0, 0, 0, 0, time.UTC)
	since13 := time.Date(2024, 8, 13, 0, 0, 0, 0, time.UTC)
	before14 := time.Date(2024, 8, 14, 0, 0, 0, 0, time.UTC)

	queries := []struct {
		name string
		q    mailbox.SearchQuery
	}{
		{"empty", mailbox.SearchQuery{}},
		{"subject report", mailbox.SearchQuery{Subject: "report"}},
		{"subject gamma", mailbox.SearchQuery{Subject: "Gamma"}},
		{"from ann", mailbox.SearchQuery{From: "ann@example.test"}},
		{"from carol", mailbox.SearchQuery{From: "carol@example.test"}},
		{"to bob", mailbox.SearchQuery{To: "bob@example.test"}},
		{"to eve (cc)", mailbox.SearchQuery{To: "eve@example.test"}},
		{"subject AND from", mailbox.SearchQuery{Subject: "Gamma", From: "carol@example.test"}},
		{"subject AND to", mailbox.SearchQuery{Subject: "Gamma", To: "eve@example.test"}},
		{"three way AND", mailbox.SearchQuery{Subject: "report", From: "ann@example.test", To: "bob@example.test"}},
		{"flagged", mailbox.SearchQuery{Flagged: true}},
		{"unseen", mailbox.SearchQuery{Unseen: true}},
		{"flagged AND unseen", mailbox.SearchQuery{Flagged: true, Unseen: true}},
		{"body gamma", mailbox.SearchQuery{Body: "gamma body"}},
		{"since 12th", mailbox.SearchQuery{Since: &since12}},
		{"since 13th", mailbox.SearchQuery{Since: &since13}},
		{"before 14th", mailbox.SearchQuery{Before: &before14}},
	}

	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			imapGot := imapSubjects(t, imapBe, tc.q)
			memGot := memorySubjects(t, memBe, tc.q)
			if !equalSubjects(imapGot, memGot) {
				t.Errorf("backends disagree:\n  imap:   %v\n  memory: %v", imapGot, memGot)
			}
		})
	}
}

// imapSubjects runs a search against the IMAP backend and
// returns the matching subjects, sorted, excluding the memory
// backend's seed message.
func imapSubjects(t *testing.T, be *Backend, q mailbox.SearchQuery) []string {
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

// memorySubjects runs a search against the memory backend and
// returns the matching subjects, sorted.
func memorySubjects(t *testing.T, be *mem.Backend, q mailbox.SearchQuery) []string {
	t.Helper()
	res, err := be.SearchMessages(context.Background(), "any", "INBOX", q)
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
