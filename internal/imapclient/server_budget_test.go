package imapclient

// --- the aggregate server data budget ---
//
// The decoder's limits are per response. The server data budget is the
// aggregate: it bounds the inbound protocol bytes one operation may
// consume, including the unsolicited responses that arrive while its
// commands run.
//
// The tests below use local fixtures and deterministic sizes: no test
// sleeps, and none of them allocates more than a few megabytes.

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// TestServerDataBudgetBoundary checks the wrapper's boundary.
//
// The budget must refuse to read past its allowance, so the bytes beyond
// it are never handed to the reader and never parsed.
func TestServerDataBudgetBoundary(t *testing.T) {
	for _, limit := range []int64{99, 100, 101} {
		t.Run(fmt.Sprintf("allowance %d", limit), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			budget := &serverDataBudget{Conn: client}
			budget.setLimit(limit)

			// The server offers one byte more than the allowance.
			go func() {
				server.Write(make([]byte, limit+1))
			}()

			// The reader never gets more than the allowance, and the
			// read that would exceed it fails instead.
			buf := make([]byte, 64)
			total := int64(0)
			for total < limit {
				n, err := budget.Read(buf)
				total += int64(n)
				if err != nil {
					t.Fatalf("read %d bytes: %v", total, err)
				}
			}
			if total != limit {
				t.Errorf("read %d bytes, want exactly the allowance %d", total, limit)
			}
			if _, err := budget.Read(buf); !errors.Is(err, errServerDataBudgetExceeded) {
				t.Errorf("the read past the allowance returned %v, want errServerDataBudgetExceeded", err)
			}
			if !budget.wasExceeded() {
				t.Error("the budget did not record the violation")
			}
		})
	}
}

// TestServerDataBudgetIsNotDisabledByZero checks that a limit of zero
// means "no limit" and that the clamped allowances are never zero.
func TestServerDataBudgetIsNotDisabledByZero(t *testing.T) {
	if got := clampDataAllowance(0); got != 1 {
		t.Errorf("clampDataAllowance(0) = %d, want 1: zero would disable the enforcement", got)
	}
	if got := clampDataAllowance(-5); got != 1 {
		t.Errorf("clampDataAllowance(-5) = %d, want 1", got)
	}
	if got := summaryDataAllowanceFor(0); got <= 0 {
		t.Errorf("summaryDataAllowanceFor(0) = %d, want a positive allowance", got)
	}
	if got := messageDataAllowanceFor(0); got <= 0 {
		t.Errorf("messageDataAllowanceFor(0) = %d, want a positive allowance", got)
	}
}

// TestSummaryDataAllowanceIsDerivedFromTheScanBudget checks the
// derivation of the summary allowance.
//
// The allowance must grow with the scan budget, so that a larger budget
// is not refused by the aggregate bound, and it must not overflow.
func TestSummaryDataAllowanceIsDerivedFromTheScanBudget(t *testing.T) {
	small := summaryDataAllowanceFor(10)
	large := summaryDataAllowanceFor(10000)
	if large <= small {
		t.Errorf("the allowance did not grow with the budget: %d then %d", small, large)
	}
	if large < int64(10000)*summaryDataAllowance {
		t.Errorf("the allowance %d does not cover the budget times the per-summary allowance", large)
	}
	// The largest budget the configuration allows must not overflow.
	max := summaryDataAllowanceFor(1000000)
	if max <= 0 {
		t.Error("the allowance at the maximum budget is not positive")
	}
	if got := summaryDataAllowanceFor(-1); got <= 0 {
		t.Errorf("the allowance for a negative budget is %d, want a positive one", got)
	}
}

// TestSearchFloodIsRefusedWithATypedError checks that a server which
// floods unsolicited responses during a command is stopped, that the
// failure is typed, that no partial result is returned, that the
// connection is discarded, and that a later operation reconnects.
func TestSearchFloodIsRefusedWithATypedError(t *testing.T) {
	// The flood is larger than both the idle allowance and the summary
	// allowance of a small budget, so the refusal does not depend on
	// which of the two happens to be in force.
	const budget = 10
	const flood = 10000

	srv := &listFixtureServer{
		messages:       budgetMessages(budget),
		searchUIDs:     []uint32{1},
		floodResponses: flood,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatalf("SearchMessages returned %d messages, want a refusal", len(res.Messages))
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Fatalf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a refused search must not return a partial result", len(res.Messages))
	}
	// The decoder's violation keeps its own classification.
	if errors.Is(err, mailbox.ErrResponseTooLarge) {
		t.Errorf("err = %v, want it not to be a decoder-limit violation", err)
	}
	if errors.Is(err, mailbox.ErrSearchTooLarge) {
		t.Errorf("err = %v, want it not to be a scan-budget refusal", err)
	}
	// The client and its socket are discarded together.
	be.mu.Lock()
	client, conn := be.client, be.conn
	be.mu.Unlock()
	if client != nil {
		t.Error("the client was retained after the flood")
	}
	if conn != nil {
		t.Error("the socket was retained after the flood")
	}
	assertNoBlockedFetchGoroutine(t)
}

// TestSearchFloodUnderBothTLSModes checks the flood under implicit TLS
// and STARTTLS.
//
// The budget wraps the socket below the TLS layer, so it accounts the
// same bytes in both modes.
func TestSearchFloodUnderBothTLSModes(t *testing.T) {
	const budget = 10
	const flood = 10000
	for _, mode := range []struct {
		name     string
		startTLS bool
	}{
		{name: "implicit TLS", startTLS: false},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			srv := &listFixtureServer{
				messages:       budgetMessages(budget),
				searchUIDs:     []uint32{1},
				floodResponses: flood,
				startTLS:       mode.startTLS,
			}
			addr, cleanup := srv.start(t)
			defer cleanup()

			be := listFixtureBackendMode(t, addr, budget, mode.startTLS, 30*time.Second)
			defer be.Close()

			_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
			if err == nil {
				t.Fatal("expected the flood to be refused")
			}
			if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
				t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
			}
		})
	}
}

// TestUnsolicitedFloodIsRefused checks the idle case: a server that
// floods before the client has issued a command.
//
// The flood exceeds the small allowance of a plain operation, so it is
// refused whichever allowance happens to be in force.
func TestUnsolicitedFloodIsRefused(t *testing.T) {
	// The flood must exceed the small allowance of a plain operation,
	// so that it is refused whichever allowance happens to be in force.
	const flood = 30000
	srv := &listFixtureServer{
		messages:       budgetMessages(3),
		floodResponses: flood,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.Mailboxes(t.Context(), "test")
	if err == nil {
		t.Fatal("expected the flood to be refused")
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
	}
	// A later operation reconnects and succeeds.
	srv.setFloodResponses(0)
	srv.setSearchUIDs([]uint32{1})
	srv.setMessages([]listFixtureMessage{fixtureMessage(1, baseTime, "m1")}, 0)
	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages after the flood: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Errorf("got %d messages, want 1", len(res.Messages))
	}
	if got := srv.connectionCount(); got < 2 {
		t.Errorf("the server saw %d connections, want at least 2: the flooded connection must be discarded", got)
	}
}

// TestServerDataBudgetKeepsLegitimateResults checks that the budget
// does not interfere with an ordinary operation.
//
// A normal search transfers far less than the allowance, so it must
// succeed unchanged.
func TestServerDataBudgetKeepsLegitimateResults(t *testing.T) {
	const budget = 50
	msgs := make([]listFixtureMessage, budget)
	uids := make([]uint32, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	_, addr, cleanup := startSearchFixture(t, msgs, uids)
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
}

// TestLegitimateOperationLargerThanTheIdleAllowanceSucceeds checks
// that an operation whose legitimate traffic exceeds the idle
// allowance is still served.
//
// The idle allowance is deliberately small, because a server is
// expected to send almost nothing between operations. An operation's
// own allowance is larger, and it has to be armed: otherwise a
// legitimate summary listing would be refused.
func TestLegitimateOperationLargerThanTheIdleAllowanceSucceeds(t *testing.T) {
	const budget = 400
	// The summaries carry a long subject, so the operation transfers
	// comfortably more than the idle allowance.
	subject := strings.Repeat("s", 1024)
	msgs := make([]listFixtureMessage, budget)
	uids := make([]uint32, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, subject)
		uids[i] = uint32(i + 1)
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	// The operation must transfer more than the idle allowance.
	if got := int64(budget) * 1024; got <= idleDataAllowance {
		t.Fatalf("the test transfers %d bytes, which does not exceed the idle allowance %d", got, idleDataAllowance)
	}

	if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	// Every summary was fetched, so the whole transfer went through the
	// connection.
	if got := srv.servedCount(); got != budget {
		t.Errorf("the server served %d summaries, want %d", got, budget)
	}
}

// TestServerDataBudgetUnderConcurrentOperations checks that concurrent
// operations, which the backend serialises over one connection, are
// unaffected.
//
// The budget is armed per operation, and the per-account mutex
// serialises the operations, so the second operation must not inherit
// the first one's consumption.
func TestServerDataBudgetUnderConcurrentOperations(t *testing.T) {
	const budget = 20
	msgs := make([]listFixtureMessage, budget)
	uids := make([]uint32, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	_, addr, cleanup := startSearchFixture(t, msgs, uids)
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
			_, errs[i] = be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
}

// TestServerDataBudgetLeavesNoBlockedGoroutines checks that a refused
// operation leaves no goroutine behind.
func TestServerDataBudgetLeavesNoBlockedGoroutines(t *testing.T) {
	const budget = 10
	srv := &listFixtureServer{
		messages:       budgetMessages(budget),
		searchUIDs:     []uint32{1},
		floodResponses: 5000,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	_, _ = be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	assertNoBlockedFetchGoroutine(t)
}

// TestServerDataBudgetErrorMessageIsActionable checks the wording of the
// refusal.
func TestServerDataBudgetErrorMessageIsActionable(t *testing.T) {
	err := fmt.Errorf("%w: the server sent more protocol data than the operation allows, "+
		"so the connection was closed. Retry with a narrower request",
		mailbox.ErrServerDataBudgetExceeded)
	if !strings.Contains(err.Error(), "narrower request") {
		t.Errorf("err = %q, want it to carry the recourse", err)
	}
	if strings.Contains(err.Error(), "max_list_scan_messages") {
		t.Errorf("err = %q, want it not to name a limit that would not necessarily help", err)
	}
}
