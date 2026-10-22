package imapclient

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// --- prospective scan-budget enforcement ---
//
// The scan budget is enforced twice:
//
//   - Before the fetch, from the mailbox size the server reports in the
//     EXAMINE response (the preflight check).
//   - During the fetch, from the number of messages the FETCH handler
//     actually delivers (the prospective check).
//
// The prospective check is what makes the bound real: the preflight
// depends on the server's own count, which a server may under-report.
// The prospective check observes budget+1 deliveries, aborts the
// transfer by closing the socket, and never returns a partial or
// approximate page.
//
// The tests below use local, deterministic fixtures only.

// budgetMessages builds n fixture messages whose internal dates descend
// with the UID, so the newest message is the first one.
func budgetMessages(n int) []listFixtureMessage {
	msgs := make([]listFixtureMessage, n)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i+1))
	}
	return msgs
}

// requireListTooLarge asserts that err is the typed list-too-large error.
func requireListTooLarge(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the typed list-too-large error, got nil")
	}
	if !errors.Is(err, mailbox.ErrListTooLarge) {
		t.Fatalf("err = %v, want it to wrap %v", err, mailbox.ErrListTooLarge)
	}
}

// blockedFetchGoroutines returns the stacks of the goroutines that are
// still inside the go-imap fetch machinery: either blocked sending a
// response on the message channel, or blocked waiting for a command to
// complete.
//
// A correct implementation leaves none: the response channel is drained
// and the command is awaited, so both goroutines have returned.
func blockedFetchGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "responses.(*Fetch).Handle") ||
			strings.Contains(g, "client.(*Client).execute") {
			out = append(out, g)
		}
	}
	return out
}

// assertNoBlockedFetchGoroutine waits briefly for the fetch machinery to
// unwind and then fails if any goroutine is still inside it.
func assertNoBlockedFetchGoroutine(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		gs := blockedFetchGoroutines()
		if len(gs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d goroutine(s) still inside the IMAP fetch machinery:\n%s",
				len(gs), strings.Join(gs, "\n"))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFor polls cond until it holds or the timeout expires. It is used
// for the observations the server makes asynchronously: the server
// goroutine may still be writing when the client has already returned.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestListScanBudgetExactlyBudgetSucceeds checks that a mailbox that
// delivers exactly the budget messages is listed normally. The bound is
// inclusive: budget messages are allowed, budget+1 are not.
func TestListScanBudgetExactlyBudgetSucceeds(t *testing.T) {
	const budget = 20
	srv := &listFixtureServer{messages: budgetMessages(budget)}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: budget})
	if err != nil {
		t.Fatalf("ListMessages at exactly the budget: %v", err)
	}
	if len(res.Messages) != budget {
		t.Fatalf("got %d messages, want %d", len(res.Messages), budget)
	}
	// The whole page must be there: nothing was dropped.
	want := make([]string, budget)
	for i := range want {
		want[i] = fmt.Sprintf("m%d", i+1)
	}
	if got := subjects(res.Messages); !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestListScanBudgetExceededRefusesWithoutPartialPage checks the two
// ways a listing can exceed the budget.
//
// The preflight refusal happens before any FETCH is sent, because the
// server itself reported a mailbox over the budget.
//
// The observed refusal happens when the server under-reports its
// mailbox size and then delivers more messages than the budget allows.
// The call fails with the typed error and returns no page at all.
func TestListScanBudgetExceededRefusesWithoutPartialPage(t *testing.T) {
	const budget = 20
	msgs := budgetMessages(budget + 1)

	t.Run("preflight refuses before any fetch", func(t *testing.T) {
		srv := &listFixtureServer{messages: msgs, exists: budget + 1}
		addr, cleanup := srv.start(t)
		defer cleanup()

		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
		requireListTooLarge(t, err)
		if len(res.Messages) != 0 {
			t.Errorf("got %d messages, want 0: a refused mailbox must not return a partial page", len(res.Messages))
		}
		if n := srv.fetchCount(); n != 0 {
			t.Errorf("server saw %d fetches, want 0: the over-budget mailbox must be refused before any work", n)
		}
	})

	t.Run("observed overrun refuses the page", func(t *testing.T) {
		srv := &listFixtureServer{messages: msgs, exists: 1}
		addr, cleanup := srv.start(t)
		defer cleanup()

		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
		requireListTooLarge(t, err)
		if len(res.Messages) != 0 {
			t.Errorf("got %d messages, want 0: an over-budget listing must not return a partial page", len(res.Messages))
		}
		// The server really did deliver more than the budget.
		waitFor(t, 5*time.Second, "the server to deliver more than the budget", func() bool {
			return srv.servedCount() >= budget+1
		})
	})
}

// TestListScanBudgetUnderReportedExistsCannotExceedBudget checks that a
// server which under-reports its mailbox size cannot make the
// application accept more than the delivery budget.
//
// The preflight alone would not catch this: the server claims one
// message. The prospective check does, because it counts the messages
// the FETCH handler delivers.
func TestListScanBudgetUnderReportedExistsCannotExceedBudget(t *testing.T) {
	const budget = 50
	const total = 500
	srv := &listFixtureServer{messages: budgetMessages(total), exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
	requireListTooLarge(t, err)
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: the application must not accept more than the delivery budget", len(res.Messages))
	}
	// The boundary is exact: a server that under-reports its size but
	// stays within the budget is still listed.
	srv.setMessages(budgetMessages(budget), 1)
	res, err = be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("ListMessages at the budget with an under-reported size: %v", err)
	}
	if len(res.Messages) != 5 {
		t.Errorf("got %d messages, want 5", len(res.Messages))
	}
}

// TestListScanBudgetReaderKeepsDrainingAfterSocketClose checks that the
// client keeps draining the response channel after it closes the socket.
//
// The go-imap response handler sends each message on the channel and
// blocks if nobody reads. A client that stopped reading at the abort
// would block the handler, which stops the reader, which means the
// command never completes and the call never returns.
func TestListScanBudgetReaderKeepsDrainingAfterSocketClose(t *testing.T) {
	const budget = 20
	const total = 400
	srv := &listFixtureServer{messages: budgetMessages(total), exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
		done <- err
	}()
	select {
	case err := <-done:
		requireListTooLarge(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("ListMessages did not return: the reader stopped draining after the socket was closed")
	}
	// The server delivered more than the budget, so the client had to
	// abort while the response was still streaming.
	waitFor(t, 5*time.Second, "the server to deliver more than the budget", func() bool {
		return srv.servedCount() >= budget+1
	})
	assertNoBlockedFetchGoroutine(t)
}

// TestListScanBudgetFetchCompletesWithoutBlockedGoroutine checks that
// the in-flight FETCH completes after the abort and that no goroutine is
// left blocked in the fetch machinery.
func TestListScanBudgetFetchCompletesWithoutBlockedGoroutine(t *testing.T) {
	const budget = 20
	srv := &listFixtureServer{messages: budgetMessages(budget * 10), exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
	requireListTooLarge(t, err)
	assertNoBlockedFetchGoroutine(t)
}

// TestListScanBudgetSocketCloseDoesNotRace repeats the abort so that the
// race detector has many opportunities to observe the socket close
// against the fork's command write and the connection cleanup.
//
// The close of a socket writes nothing, so it cannot race the command
// writer; the repeated runs are what turn that argument into evidence.
func TestListScanBudgetSocketCloseDoesNotRace(t *testing.T) {
	const budget = 10
	for round := 0; round < 10; round++ {
		srv := &listFixtureServer{messages: budgetMessages(budget * 5), exists: 1}
		addr, cleanup := srv.start(t)

		be := listFixtureBackend(t, addr, budget)
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
		requireListTooLarge(t, err)
		_ = be.Close()
		cleanup()
	}
}

// TestListScanBudgetConcurrentCallsDoNotOverlap checks that concurrent
// calls on the same backend are serialised by the per-account mutex and
// that an abort never closes a replacement connection.
//
// Every call over-refuses and aborts, so the backend reconnects between
// calls. A socket closed by an earlier call must not close the socket
// of a later one.
func TestListScanBudgetConcurrentCallsDoNotOverlap(t *testing.T) {
	const budget = 10
	srv := &listFixtureServer{messages: budgetMessages(budget * 5), exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Errorf("worker %d: expected the typed list-too-large error, got nil", i)
			continue
		}
		if !errors.Is(err, mailbox.ErrListTooLarge) {
			t.Errorf("worker %d: err = %v, want it to wrap %v", i, err, mailbox.ErrListTooLarge)
		}
	}
	// Each call aborted its own connection, so the server saw one
	// connection per call and none of them was closed by a foreign
	// abort.
	if got, want := srv.connectionCount(), workers; got != want {
		t.Errorf("server saw %d connections, want %d", got, want)
	}
	assertNoBlockedFetchGoroutine(t)
}

// TestListScanBudgetStallingServerDoesNotHang checks that a server
// which stops mid-response does not leave the call hanging.
//
// The abort closes the socket, which unblocks the reader that is
// waiting for the rest of the response. Without the abort the call
// would wait for the command timeout.
func TestListScanBudgetStallingServerDoesNotHang(t *testing.T) {
	const budget = 20
	const commandTimeout = 30 * time.Second
	srv := &listFixtureServer{
		messages:   budgetMessages(budget * 10),
		exists:     1,
		stallAfter: budget + 1,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackendMode(t, addr, budget, false, commandTimeout)
	defer be.Close()

	start := time.Now()
	_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
	elapsed := time.Since(start)
	requireListTooLarge(t, err)
	if elapsed > commandTimeout/2 {
		t.Errorf("the stalled call took %s, want it to abort well before the %s command timeout", elapsed, commandTimeout)
	}
	// The server observed the client closing the socket, which is what
	// ended the stall.
	if got := srv.stalledCount(); got != 1 {
		t.Errorf("server stalled %d times, want 1", got)
	}
	waitFor(t, 5*time.Second, "the server to observe the client close", func() bool {
		return srv.peerClosedCount() == 1
	})
	assertNoBlockedFetchGoroutine(t)
}

// TestListScanBudgetAbortedConnectionIsDiscardedAndNextCallReconnects
// checks that the aborted connection is not reused.
//
// The client that was aborted is unusable, so the backend must discard
// it and the next call must dial a fresh connection.
func TestListScanBudgetAbortedConnectionIsDiscardedAndNextCallReconnects(t *testing.T) {
	const budget = 20
	srv := &listFixtureServer{messages: budgetMessages(budget * 5), exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
	requireListTooLarge(t, err)
	if got := srv.connectionCount(); got != 1 {
		t.Fatalf("server saw %d connections after the abort, want 1", got)
	}

	// The next call must dial a new connection and succeed.
	srv.setMessages(budgetMessages(3), 3)
	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages after the abort: %v", err)
	}
	if len(res.Messages) != 3 {
		t.Errorf("got %d messages, want 3", len(res.Messages))
	}
	if got := srv.connectionCount(); got != 2 {
		t.Errorf("server saw %d connections, want 2: the aborted connection must be discarded", got)
	}
}

// TestListScanBudgetAbortUnderImplicitTLSAndSTARTTLS checks that the
// abort and the cleanup work the same way in both TLS modes.
//
// The socket that is closed is the TCP socket in both cases: with
// implicit TLS the client speaks over a TLS connection that wraps it,
// and with STARTTLS the connection is upgraded in place.
func TestListScanBudgetAbortUnderImplicitTLSAndSTARTTLS(t *testing.T) {
	const budget = 20
	for _, mode := range []struct {
		name     string
		startTLS bool
	}{
		{name: "implicit TLS", startTLS: false},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			srv := &listFixtureServer{
				messages:   budgetMessages(budget * 5),
				exists:     1,
				stallAfter: budget + 1,
				startTLS:   mode.startTLS,
			}
			addr, cleanup := srv.start(t)
			defer cleanup()

			be := listFixtureBackendMode(t, addr, budget, mode.startTLS, 30*time.Second)
			defer be.Close()

			_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
			requireListTooLarge(t, err)
			waitFor(t, 5*time.Second, "the server to observe the client close", func() bool {
				return srv.peerClosedCount() == 1
			})
			assertNoBlockedFetchGoroutine(t)

			// The aborted connection is discarded and the next call
			// reconnects over the same TLS mode.
			srv.setMessages(budgetMessages(3), 3)
			res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
			if err != nil {
				t.Fatalf("ListMessages after the abort: %v", err)
			}
			if len(res.Messages) != 3 {
				t.Errorf("got %d messages, want 3", len(res.Messages))
			}
			if got := srv.connectionCount(); got != 2 {
				t.Errorf("server saw %d connections, want 2", got)
			}
		})
	}
}

// TestListScanBudgetDateFiltersCannotBypassBudget checks that the date
// filters do not reduce the work the budget bounds.
//
// The filters run in the client, after the fetch. A filter that would
// exclude every message therefore does not stop the budget from being
// enforced.
func TestListScanBudgetDateFiltersCannotBypassBudget(t *testing.T) {
	const budget = 20
	msgs := budgetMessages(budget * 5)
	srv := &listFixtureServer{messages: msgs, exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	// A since filter in the future excludes every message.
	since := baseTime.Add(time.Hour)
	res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
		Since: &since,
		Limit: 5,
	})
	requireListTooLarge(t, err)
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a date filter must not bypass the scan budget", len(res.Messages))
	}

	// A before filter in the past does the same.
	before := baseTime.Add(-24 * time.Hour)
	_, err = be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
		Before: &before,
		Limit:  5,
	})
	requireListTooLarge(t, err)
}

// TestListScanBudgetLargeOffsetStaysBounded checks that a large offset
// does not change the budget accounting and does not grow the retained
// set.
func TestListScanBudgetLargeOffsetStaysBounded(t *testing.T) {
	const budget = 20
	srv := &listFixtureServer{messages: budgetMessages(budget * 5), exists: 1}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	for _, offset := range []int{1 << 30, 1 << 40} {
		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{
			Offset: offset,
			Limit:  5,
		})
		requireListTooLarge(t, err)
		if len(res.Messages) != 0 {
			t.Errorf("offset %d returned %d messages, want 0", offset, len(res.Messages))
		}
	}
}

// TestListScanBudgetErrorMessageGivesAccurateRecourse checks the text of
// the refusal.
//
// The recourse is to raise the limit. It is not to narrow the query: the
// date filters run after the preflight and after the fetch, so they
// cannot make an oversized mailbox listable.
func TestListScanBudgetErrorMessageGivesAccurateRecourse(t *testing.T) {
	t.Run("preflight refusal", func(t *testing.T) {
		err := listTooLargeError("INBOX", 50000, 10000)
		msg := err.Error()
		if strings.Contains(msg, "since") || strings.Contains(msg, "before") {
			t.Errorf("msg = %q, want it not to suggest narrowing the query with date filters", msg)
		}
		if !strings.Contains(msg, "max_list_scan_messages") {
			t.Errorf("msg = %q, want it to name the limit to raise", msg)
		}
		if !strings.Contains(msg, "50000") || !strings.Contains(msg, "10000") {
			t.Errorf("msg = %q, want it to carry the message count and the budget", msg)
		}
	})

	t.Run("observed refusal", func(t *testing.T) {
		err := listScanBudgetExceededError("INBOX", 10000)
		msg := err.Error()
		if strings.Contains(msg, "since") || strings.Contains(msg, "before") {
			t.Errorf("msg = %q, want it not to suggest narrowing the query with date filters", msg)
		}
		if !strings.Contains(msg, "max_list_scan_messages") {
			t.Errorf("msg = %q, want it to name the limit to raise", msg)
		}
		// The observed count, not the server's EXISTS claim.
		if !strings.Contains(msg, "10000") {
			t.Errorf("msg = %q, want it to carry the observed budget", msg)
		}
		if strings.Contains(msg, "EXISTS") {
			t.Errorf("msg = %q, want it to report the observation rather than the server's EXISTS count", msg)
		}
	})
}
