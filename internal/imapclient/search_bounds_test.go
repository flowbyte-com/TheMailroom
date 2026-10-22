package imapclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// --- bounded IMAP search workload ---
//
// The search workload is bounded by the same scan budget as the
// listing workload: the per-account limits.max_list_scan_messages.
//
// The bound has two enforcement points:
//
//  1. The UID SEARCH response. The application counts the UIDs the
//     server returns and refuses the search when it returns more than
//     the budget allows, before any FETCH is sent.
//  2. The UID FETCH response. The application counts the messages the
//     FETCH handler delivers and aborts the transfer when the server
//     delivers more than the budget allows.
//
// The second point is the second line of defence: the go-imap response
// handler discards any message whose UID is not in the requested
// sequence set, so the number of delivered messages cannot exceed the
// number of requested UIDs for a well-behaved server. A server that
// repeats a UID can exceed it, and is caught there.
//
// The tests below use local, deterministic fixtures only.

// requireSearchTooLarge asserts that err is the typed search-too-large
// error.
func requireSearchTooLarge(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the typed search-too-large error, got nil")
	}
	if !errors.Is(err, mailbox.ErrSearchTooLarge) {
		t.Fatalf("err = %v, want it to wrap %v", err, mailbox.ErrSearchTooLarge)
	}
}

// searchUIDsOf returns the UIDs of the given fixture messages.
func searchUIDsOf(msgs []listFixtureMessage) []uint32 {
	out := make([]uint32, len(msgs))
	for i, m := range msgs {
		out[i] = m.uid
	}
	return out
}

// startSearchFixture starts the fixture server with the given messages
// and the given UID SEARCH result.
func startSearchFixture(t *testing.T, msgs []listFixtureMessage, uids []uint32) (*listFixtureServer, string, func()) {
	t.Helper()
	srv := &listFixtureServer{messages: msgs, searchUIDs: uids}
	addr, cleanup := srv.start(t)
	return srv, addr, cleanup
}

// TestSearchZeroMatches checks that a search that matches nothing
// returns an empty result and does no fetch.
func TestSearchZeroMatches(t *testing.T) {
	srv, addr, cleanup := startSearchFixture(t, nil, nil)
	defer cleanup()
	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{Subject: "nothing"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(res.Messages))
	}
	if n := srv.searchCount(); n != 1 {
		t.Errorf("server saw %d searches, want 1", n)
	}
	if n := srv.fetchCount(); n != 0 {
		t.Errorf("server saw %d fetches, want 0: a search with no match must not fetch", n)
	}
}

// TestSearchOneMatch checks the single-match case.
func TestSearchOneMatch(t *testing.T) {
	msgs := []listFixtureMessage{fixtureMessage(1, baseTime, "m1")}
	_, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{Subject: "m1"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(res.Messages))
	}
	if res.Messages[0].Subject != "m1" {
		t.Errorf("subject = %q, want %q", res.Messages[0].Subject, "m1")
	}
	if res.Messages[0].UID != 1 {
		t.Errorf("uid = %d, want 1", res.Messages[0].UID)
	}
}

// TestSearchExactlyAtBudgetSucceeds checks that a search whose result
// is exactly the budget is allowed, and that every match is returned.
func TestSearchExactlyAtBudgetSucceeds(t *testing.T) {
	const budget = 20
	msgs := make([]listFixtureMessage, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i+1))
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages at the budget: %v", err)
	}
	if len(res.Messages) != budget {
		t.Fatalf("got %d messages, want %d", len(res.Messages), budget)
	}
	if n := srv.fetchCount(); n != 1 {
		t.Errorf("server saw %d fetches, want 1", n)
	}
}

// TestSearchBudgetPlusOneIsRefused checks that a search whose result is
// one over the budget is refused with the typed error, that no partial
// result is returned, and that no FETCH is sent.
func TestSearchBudgetPlusOneIsRefused(t *testing.T) {
	const budget = 20
	msgs := make([]listFixtureMessage, budget+1)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i+1))
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireSearchTooLarge(t, err)
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a refused search must not return a partial result", len(res.Messages))
	}
	// The refusal happens before the FETCH, so the server is never
	// asked for the envelopes of the matches.
	if n := srv.fetchCount(); n != 0 {
		t.Errorf("server saw %d fetches, want 0: an over-budget search must be refused before any work", n)
	}
}

// TestSearchVeryLargeResultSetIsRefused checks that a search matching
// far more messages than the budget is refused, and that the work
// stops at the search.
func TestSearchVeryLargeResultSetIsRefused(t *testing.T) {
	const budget = 50
	const matches = 5000
	msgs := make([]listFixtureMessage, matches)
	uids := make([]uint32, matches)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Second), fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireSearchTooLarge(t, err)
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(res.Messages))
	}
	if n := srv.fetchCount(); n != 0 {
		t.Errorf("server saw %d fetches, want 0", n)
	}
}

// TestSearchEmptyCriteriaIsBounded checks the empty-criteria case,
// which matches every message in the mailbox.
//
// The criteria are empty, so the command carries no search key and the
// server is free to answer with every UID in the mailbox. The budget
// must still bound the operation.
func TestSearchEmptyCriteriaIsBounded(t *testing.T) {
	const budget = 10

	t.Run("within the budget", func(t *testing.T) {
		msgs := make([]listFixtureMessage, budget)
		for i := range msgs {
			msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		}
		srv, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
		defer cleanup()

		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
		if err != nil {
			t.Fatalf("SearchMessages: %v", err)
		}
		if len(res.Messages) != budget {
			t.Errorf("got %d messages, want %d", len(res.Messages), budget)
		}
		// The command carries no search key at all.
		cmd := srv.lastSearch()
		if !strings.Contains(strings.ToUpper(cmd), "SEARCH") {
			t.Fatalf("search command = %q, want a UID SEARCH", cmd)
		}
		for _, key := range []string{"SUBJECT", "FROM", "TO", "BODY", "FLAGGED", "UNSEEN", "SENTSINCE", "SENTBEFORE"} {
			if strings.Contains(strings.ToUpper(cmd), key) {
				t.Errorf("search command = %q, want it to carry no %s key for an empty query", cmd, key)
			}
		}
	})

	t.Run("over the budget", func(t *testing.T) {
		msgs := make([]listFixtureMessage, budget+1)
		uids := make([]uint32, budget+1)
		for i := range msgs {
			msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
			uids[i] = uint32(i + 1)
		}
		srv, addr, cleanup := startSearchFixture(t, msgs, uids)
		defer cleanup()

		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
		requireSearchTooLarge(t, err)
		if n := srv.fetchCount(); n != 0 {
			t.Errorf("server saw %d fetches, want 0", n)
		}
	})
}

// TestSearchOrdersByInternalDateNotUID checks that the result order is
// the internal-date order, not the UID order.
//
// The search returns the UIDs in ascending order, but the internal dates
// are placed so that the UID order and the result order differ.
func TestSearchOrdersByInternalDateNotUID(t *testing.T) {
	// UID 1 is the oldest, UID 2 the newest, UID 3 in between.
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime.Add(-5*time.Hour), "uid1-oldest"),
		fixtureMessage(2, baseTime.Add(-1*time.Hour), "uid2-newest"),
		fixtureMessage(3, baseTime.Add(-3*time.Hour), "uid3-middle"),
	}
	_, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	want := []string{"uid2-newest", "uid3-middle", "uid1-oldest"}
	if got := subjects(res.Messages); !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSearchEqualTimestampsBreakByUIDDescending checks the tie-break
// on equal internal dates.
func TestSearchEqualTimestampsBreakByUIDDescending(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(7, baseTime, "uid7"),
		fixtureMessage(3, baseTime, "uid3"),
		fixtureMessage(9, baseTime, "uid9"),
		fixtureMessage(5, baseTime, "uid5"),
	}
	_, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	want := []string{"uid9", "uid7", "uid5", "uid3"}
	if got := subjects(res.Messages); !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSearchResultLimitIsPreserved checks that the result limit still
// truncates the page, and that it applies after the ordering.
func TestSearchResultLimitIsPreserved(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime.Add(-1*time.Hour), "m1"),
		fixtureMessage(2, baseTime.Add(-2*time.Hour), "m2"),
		fixtureMessage(3, baseTime.Add(-3*time.Hour), "m3"),
		fixtureMessage(4, baseTime.Add(-4*time.Hour), "m4"),
	}
	_, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{Limit: 2})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	want := []string{"m1", "m2"}
	if got := subjects(res.Messages); !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSearchCommandCarriesEveryCriterion checks that the single search
// command carries every supported criterion, with the To filter as an
// explicit OR over To and Cc.
func TestSearchCommandCarriesEveryCriterion(t *testing.T) {
	since := baseTime.Add(-24 * time.Hour)
	before := baseTime.Add(24 * time.Hour)
	srv, addr, cleanup := startSearchFixture(t, nil, nil)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{
		Subject: "report",
		From:    "ann@example.test",
		To:      "bob@example.test",
		Body:    "body text",
		Flagged: true,
		Unseen:  true,
		Since:   &since,
		Before:  &before,
	}); err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if n := srv.searchCount(); n != 1 {
		t.Fatalf("server saw %d searches, want 1: the search must be one logical command", n)
	}
	cmd := strings.ToUpper(srv.lastSearch())
	if !strings.Contains(cmd, "UID SEARCH") {
		t.Fatalf("search command = %q, want a UID SEARCH", cmd)
	}
	for _, key := range []string{
		"SUBJECT", "FROM", "BODY", "FLAGGED", "UNSEEN",
		"SENTSINCE", "SENTBEFORE", "OR", "TO", "CC",
	} {
		if !strings.Contains(cmd, key) {
			t.Errorf("search command = %q, missing %q", cmd, key)
		}
	}
	if strings.Contains(cmd, "TEXT ") {
		t.Errorf("search command = %q, want it not to use the obsolete TEXT key", cmd)
	}
}

// TestSearchIgnoresInaccurateMessageCount checks that the
// server-reported mailbox size does not affect the search bound.
//
// The search bound comes from the number of UIDs the server returns,
// not from the size it reports in the EXAMINE response, so an
// inaccurate count changes neither the result nor the bound.
func TestSearchIgnoresInaccurateMessageCount(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
		fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
		fixtureMessage(3, baseTime.Add(-2*time.Hour), "m3"),
	}
	for _, reported := range []int{0, 1, 3, 1000000} {
		srv := &listFixtureServer{messages: msgs, searchUIDs: searchUIDsOf(msgs), exists: reported}
		addr, cleanup := srv.start(t)

		be := listFixtureBackend(t, addr, 100)
		res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
		if err != nil {
			t.Fatalf("SearchMessages with a reported size of %d: %v", reported, err)
		}
		if len(res.Messages) != 3 {
			t.Errorf("reported size %d: got %d messages, want 3", reported, len(res.Messages))
		}
		_ = be.Close()
		cleanup()
	}
}

// TestSearchTruncatedResponseIsAnError checks that a SEARCH response
// without its tagged completion is an error and never a partial
// result.
func TestSearchTruncatedResponseIsAnError(t *testing.T) {
	msgs := []listFixtureMessage{fixtureMessage(1, baseTime, "m1")}
	srv := &listFixtureServer{messages: msgs, searchUIDs: searchUIDsOf(msgs), searchTruncate: true}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatal("expected an error for a truncated SEARCH response")
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a truncated response must not produce a partial result", len(res.Messages))
	}
}

// TestSearchMalformedResponseIsAnError checks that a SEARCH response
// with a non-numeric field is reported as a malformed response and
// never a partial result.
func TestSearchMalformedResponseIsAnError(t *testing.T) {
	msgs := []listFixtureMessage{fixtureMessage(1, baseTime, "m1")}
	srv := &listFixtureServer{messages: msgs, searchUIDs: searchUIDsOf(msgs), searchMalformed: true}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatal("expected an error for a malformed SEARCH response")
	}
	if !errors.Is(err, mailbox.ErrMalformed) {
		t.Errorf("err = %v, want it to be the malformed-response error", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(res.Messages))
	}
}

// TestSearchFetchDisconnectionIsAnError checks that a FETCH that is cut
// short is an error and never a partial result.
func TestSearchFetchDisconnectionIsAnError(t *testing.T) {
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
		fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
	}
	srv := &listFixtureServer{messages: msgs, searchUIDs: searchUIDsOf(msgs), disconnectAfter: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatal("expected an error for a disconnected FETCH")
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a truncated fetch must not produce a partial result", len(res.Messages))
	}
}

// TestSearchUIDValidityComesFromTheExamine checks that the UIDVALIDITY
// is the one observed during the EXAMINE that produced the results.
func TestSearchUIDValidityComesFromTheExamine(t *testing.T) {
	msgs := []listFixtureMessage{fixtureMessage(1, baseTime, "m1")}
	_, addr, cleanup := startSearchFixture(t, msgs, searchUIDsOf(msgs))
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if res.UIDValidity != 1 {
		t.Errorf("UIDValidity = %d, want 1", res.UIDValidity)
	}
	if uv := be.SelectedUIDValidity(); uv != 1 {
		t.Errorf("SelectedUIDValidity = %d, want 1", uv)
	}
}

// TestSearchFetchOverrunIsAborted checks the second enforcement point:
// a server that delivers more messages than the budget allows, even
// though it was asked for fewer, is aborted.
//
// The go-imap response handler discards a message whose UID is not in
// the requested sequence set, so the only way to exceed the budget is
// to repeat a UID. The abort is the close of the underlying socket.
func TestSearchFetchOverrunIsAborted(t *testing.T) {
	const budget = 2
	// The search asks for UIDs 1 and 2; the server delivers 1, 2 and
	// then repeats UID 1.
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
		fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
		fixtureMessage(1, baseTime.Add(-2*time.Hour), "m1-again"),
	}
	_, addr, cleanup := startSearchFixture(t, msgs, []uint32{1, 2})
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireSearchTooLarge(t, err)
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: an aborted search must not return a partial result", len(res.Messages))
	}
	assertNoBlockedFetchGoroutine(t)
}

// TestSearchAbortUnderImplicitTLSAndSTARTTLS checks that the abort and
// the cleanup work the same way in both TLS modes.
func TestSearchAbortUnderImplicitTLSAndSTARTTLS(t *testing.T) {
	const budget = 2
	for _, mode := range []struct {
		name     string
		startTLS bool
	}{
		{name: "implicit TLS", startTLS: false},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			msgs := []listFixtureMessage{
				fixtureMessage(1, baseTime, "m1"),
				fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
				fixtureMessage(1, baseTime.Add(-2*time.Hour), "m1-again"),
			}
			srv := &listFixtureServer{
				messages:            msgs,
				searchUIDs:          []uint32{1, 2},
				startTLS:            mode.startTLS,
				terminateAfterFetch: true,
			}
			addr, cleanup := srv.start(t)
			defer cleanup()

			be := listFixtureBackendMode(t, addr, budget, mode.startTLS, 30*time.Second)
			defer be.Close()

			_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
			requireSearchTooLarge(t, err)
			assertNoBlockedFetchGoroutine(t)

			// The aborted connection is discarded and the next call
			// reconnects over the same TLS mode.
			srv.setSearchUIDs([]uint32{1})
			srv.setMessages([]listFixtureMessage{fixtureMessage(1, baseTime, "m1")}, 0)
			res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
			if err != nil {
				t.Fatalf("SearchMessages after the abort: %v", err)
			}
			if len(res.Messages) != 1 {
				t.Errorf("got %d messages, want 1", len(res.Messages))
			}
		})
	}
}

// TestSearchRefusedConnectionIsDiscardedAndReconnects checks that a
// refused search discards the connection and that the next call
// reconnects.
func TestSearchRefusedConnectionIsDiscardedAndReconnects(t *testing.T) {
	const budget = 5
	msgs := make([]listFixtureMessage, budget+1)
	uids := make([]uint32, budget+1)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireSearchTooLarge(t, err)
	if got := srv.connectionCount(); got != 1 {
		t.Fatalf("server saw %d connections after the refusal, want 1", got)
	}

	// The next call must dial a new connection and succeed.
	srv.setSearchUIDs([]uint32{1})
	srv.setMessages([]listFixtureMessage{fixtureMessage(1, baseTime, "m1")}, 0)
	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages after the refusal: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Errorf("got %d messages, want 1", len(res.Messages))
	}
	if got := srv.connectionCount(); got != 2 {
		t.Errorf("server saw %d connections, want 2: the refused connection must be discarded", got)
	}
}

// TestSearchStallingServerDoesNotHang checks that a server which stops
// after the untagged SEARCH response does not leave the call hanging
// forever.
//
// A stalled SEARCH response is not stopped by the scan budget: the
// budget counts the UIDs the server returns, and a stalled server
// returns none. What ends the call is the explicit per-command IMAP
// timeout, which is a separate control.
func TestSearchStallingServerDoesNotHang(t *testing.T) {
	const commandTimeout = 2 * time.Second
	msgs := []listFixtureMessage{fixtureMessage(1, baseTime, "m1")}
	srv := &listFixtureServer{
		messages:    msgs,
		searchUIDs:  searchUIDsOf(msgs),
		searchStall: true,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackendMode(t, addr, 100, false, commandTimeout)
	defer be.Close()

	start := time.Now()
	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error for a stalled SEARCH response")
	}
	if elapsed > 10*time.Second {
		t.Errorf("the stalled call took %s, want it to end on the %s command timeout", elapsed, commandTimeout)
	}
	waitFor(t, 5*time.Second, "the server to stall", func() bool {
		return srv.stalledCount() == 1
	})
}

// TestSearchErrorMessageGivesAccurateRecourse checks the text of the
// refusal.
//
// The recourse for a search is to narrow the query or to raise the
// limit. Narrowing the query does help here, unlike in the listing
// case, because the server evaluates the criteria.
func TestSearchErrorMessageGivesAccurateRecourse(t *testing.T) {
	err := searchTooLargeError(10000)
	msg := err.Error()
	if !strings.Contains(msg, "max_list_scan_messages") {
		t.Errorf("msg = %q, want it to name the limit to raise", msg)
	}
	if !strings.Contains(msg, "10000") {
		t.Errorf("msg = %q, want it to carry the budget", msg)
	}
	if !strings.Contains(msg, "narrow the query") {
		t.Errorf("msg = %q, want it to mention narrowing the query", msg)
	}
	if !errors.Is(err, mailbox.ErrSearchTooLarge) {
		t.Errorf("err = %v, want it to wrap %v", err, mailbox.ErrSearchTooLarge)
	}
}

// --- unit-level bounds ---

// TestUIDCollectorKeepsAtMostTheLimit checks the collector's own
// bound: it retains at most the limit and reports an overrun when the
// server returns more.
//
// Removing the limit check from the collector makes this test fail.
func TestUIDCollectorKeepsAtMostTheLimit(t *testing.T) {
	col := &uidCollector{limit: 3}
	if err := col.Handle(searchResponse(1, 2, 3)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if col.overrun {
		t.Error("the collector reported an overrun for a result at the limit")
	}
	if len(col.uids) != 3 {
		t.Fatalf("retained %d UIDs, want 3", len(col.uids))
	}
	for i, want := range []uint32{1, 2, 3} {
		if col.uids[i] != want {
			t.Errorf("uids[%d] = %d, want %d", i, col.uids[i], want)
		}
	}
}

// TestUIDCollectorReportsOverrun checks that the collector notices a
// result over the limit.
//
// Removing the limit check from the collector makes this test fail.
func TestUIDCollectorReportsOverrun(t *testing.T) {
	col := &uidCollector{limit: 3}
	if err := col.Handle(searchResponse(1, 2, 3, 4, 5)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !col.overrun {
		t.Error("the collector did not report the overrun")
	}
	if len(col.uids) != 3 {
		t.Errorf("retained %d UIDs, want 3", len(col.uids))
	}
}

// TestUIDCollectorIgnoresOtherResponses checks that a response that is
// not a SEARCH result is left for the executor to handle.
func TestUIDCollectorIgnoresOtherResponses(t *testing.T) {
	col := &uidCollector{limit: 3}
	other := &imap.DataResp{Tag: "*", Fields: []interface{}{"EXISTS", "42"}}
	if err := col.Handle(other); err == nil {
		t.Error("the collector handled a non-SEARCH response")
	}
	if col.overrun {
		t.Error("the collector reported an overrun for a non-SEARCH response")
	}
	if len(col.uids) != 0 {
		t.Errorf("retained %d UIDs, want 0", len(col.uids))
	}
}

// searchResponse builds the untagged SEARCH response the go-imap
// reader would produce for the given UIDs.
//
// The reader turns every atom into a string, so the fields are strings
// and the name is a plain string; that is what responses.Search and the
// application-side collector both see.
func searchResponse(uids ...uint32) imap.Resp {
	fields := []interface{}{"SEARCH"}
	for _, u := range uids {
		fields = append(fields, fmt.Sprint(u))
	}
	return &imap.DataResp{Tag: "*", Fields: fields}
}
