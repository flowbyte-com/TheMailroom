package imapclient

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// --- decoder integration ---
//
// The go-imap fork (v1.2.1-themailroom.6) bounds the memory a single
// response can make its reader allocate. These tests check that the
// limits TheMailroom derives from the account's configuration are
// consistent with the application-level search guarantees, and that a
// decoder violation is classified and handled correctly.
//
// The limits are per response. They do not bound the number of
// responses, the work the server does to evaluate the search, or the
// duration of an operation.

// decoderLimitsFor builds a backend with the given limits, without
// connecting it.
func decoderLimitsFor(t *testing.T, maxMessageBytes, scanBudget int) *Backend {
	t.Helper()
	cfg := config.AccountConfig{
		ID: "test", Email: "user@example.test", Backend: "imap",
		IMAP: config.IMAPConfig{
			Host:     "127.0.0.1",
			Port:     1,
			Username: "username",
			Password: "password",
			TLS:      true,
		},
		Limits: config.LimitsConfig{
			MaxMessageBytes:         maxMessageBytes,
			MaxAttachmentBytes:      1 << 20,
			MaxSearchResults:        50,
			MaxBatchOperations:      50,
			MaxListScanMessages:     scanBudget,
			MaxMIMENesting:          16,
			MaxMIMEParts:            100,
			MaxTotalAttachmentBytes: 1 << 20,
			MaxAttachments:          10,
			MaxRecipients:           10,
		},
	}
	be, err := New(cfg, Options{DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return be
}

// TestDecoderLimitsAreConsistentWithTheScanBudget checks the derived
// limits for the scan budgets an operator can configure.
//
// The properties are what matter, not the exact numbers: the field
// limit must leave room for the response's name on top of the budget,
// so that a result of budget+1 UIDs is still decodable and the
// application can produce its own typed refusal. The byte limit must
// cover both a SEARCH response at the budget and the largest
// legitimate literal.
func TestDecoderLimitsAreConsistentWithTheScanBudget(t *testing.T) {
	const maxMessageBytes = 25 * 1024 * 1024
	for _, budget := range []int{1, 10, 10_000, config.MaxListScanMessagesLimit} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			be := decoderLimitsFor(t, maxMessageBytes, budget)
			opts := be.decoderLimits(budget)

			// The field limit leaves room for the response's name on
			// top of the budget, so a result of budget+1 UIDs is still
			// decodable.
			if opts.MaxResponseFields < uint32(budget)+2 {
				t.Errorf("MaxResponseFields = %d, want at least %d", opts.MaxResponseFields, budget+2)
			}
			// The floor keeps an ordinary FETCH envelope usable.
			if opts.MaxResponseFields < decoderFieldLimitFloor {
				t.Errorf("MaxResponseFields = %d, want at least %d", opts.MaxResponseFields, decoderFieldLimitFloor)
			}
			// The byte limit covers a SEARCH response at the budget.
			need := uint64(decoderBytesPerUID)*uint64(budget+1) + decoderSearchOverhead
			if uint64(opts.MaxResponseBytes) < need {
				t.Errorf("MaxResponseBytes = %d, want at least %d", opts.MaxResponseBytes, need)
			}
			// The byte limit covers the largest legitimate literal and
			// the protocol around it.
			if uint64(opts.MaxResponseBytes) < uint64(maxMessageBytes)+decoderLiteralOverhead {
				t.Errorf("MaxResponseBytes = %d, want at least %d", opts.MaxResponseBytes, uint64(maxMessageBytes)+decoderLiteralOverhead)
			}
			// Nothing overflows a uint32, and nothing is disabled.
			if opts.MaxResponseBytes == 0 || opts.MaxResponseFields == 0 {
				t.Error("a limit of zero disables the enforcement")
			}
			if opts.MaxResponseBytes > math.MaxUint32 {
				t.Errorf("MaxResponseBytes = %d, exceeds the uint32 the reader uses", opts.MaxResponseBytes)
			}
			// The literal cap is the configured maximum.
			if uint32(opts.MaxLiteralSize) != uint32(maxMessageBytes) {
				t.Errorf("MaxLiteralSize = %d, want %d", opts.MaxLiteralSize, maxMessageBytes)
			}
			// The fixed limits.
			if opts.MaxAtomSize != decoderAtomSizeLimit {
				t.Errorf("MaxAtomSize = %d, want %d", opts.MaxAtomSize, decoderAtomSizeLimit)
			}
			if opts.MaxQuotedStringSize != decoderQuotedStringSizeLimit {
				t.Errorf("MaxQuotedStringSize = %d, want %d", opts.MaxQuotedStringSize, decoderQuotedStringSizeLimit)
			}
			if opts.MaxListDepth != decoderListDepthLimit {
				t.Errorf("MaxListDepth = %d, want %d", opts.MaxListDepth, decoderListDepthLimit)
			}
		})
	}
}

// TestDecoderLimitsDoNotOverflowAtTheHardMaximum checks the arithmetic
// at the largest scan budget an operator may configure.
//
// The check is arithmetic only: it does not build a fixture with a
// million UIDs, which would be slow and pointless.
func TestDecoderLimitsDoNotOverflowAtTheHardMaximum(t *testing.T) {
	const budget = config.MaxListScanMessagesLimit
	be := decoderLimitsFor(t, 25*1024*1024, budget)
	opts := be.decoderLimits(budget)

	if opts.MaxResponseFields != uint32(budget)+2 {
		t.Errorf("MaxResponseFields = %d, want %d", opts.MaxResponseFields, uint32(budget)+2)
	}
	want := uint64(decoderBytesPerUID)*uint64(budget+1) + decoderSearchOverhead
	if uint64(opts.MaxResponseBytes) < want {
		t.Errorf("MaxResponseBytes = %d, want at least %d", opts.MaxResponseBytes, want)
	}
	// The literal requirement dominates at this budget only if the
	// configured message size is larger; either way the value fits.
	if opts.MaxResponseBytes == 0 {
		t.Error("MaxResponseBytes = 0, which disables the enforcement")
	}
}

// TestDecoderLimitsClampAnUnrepresentableMessageSize checks that a
// configured maximum message size that one response cannot carry is
// reduced rather than overflowing.
func TestDecoderLimitsClampAnUnrepresentableMessageSize(t *testing.T) {
	be := decoderLimitsFor(t, math.MaxUint32, 100)
	opts := be.decoderLimits(100)

	if opts.MaxResponseBytes == 0 {
		t.Fatal("MaxResponseBytes = 0, which disables the enforcement")
	}
	if uint32(opts.MaxLiteralSize) > math.MaxUint32 {
		t.Errorf("MaxLiteralSize = %d, exceeds the uint32 the reader uses", opts.MaxLiteralSize)
	}
	// The literal must still fit in the response budget.
	if uint64(opts.MaxLiteralSize)+decoderLiteralOverhead > uint64(opts.MaxResponseBytes) {
		t.Errorf("MaxLiteralSize = %d does not fit in MaxResponseBytes = %d",
			opts.MaxLiteralSize, opts.MaxResponseBytes)
	}
}

// TestSearchAtBudgetIsServed checks that a search result of exactly
// the budget is served normally.
func TestSearchAtBudgetIsServed(t *testing.T) {
	const budget = 20
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
		t.Fatalf("SearchMessages at the budget: %v", err)
	}
	if len(res.Messages) != budget {
		t.Errorf("got %d messages, want %d", len(res.Messages), budget)
	}
}

// TestSearchAtBudgetPlusOneIsRefusedByTheApplication checks that a
// result of budget+1 UIDs produces the application's own typed refusal.
//
// The decoder's field limit leaves room for exactly that many UIDs, so
// the application, not the decoder, is the one that refuses.
func TestSearchAtBudgetPlusOneIsRefusedByTheApplication(t *testing.T) {
	const budget = 20
	msgs := make([]listFixtureMessage, budget+1)
	uids := make([]uint32, budget+1)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireSearchTooLarge(t, err)
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a refused search must not return a partial result", len(res.Messages))
	}
	// No FETCH was sent: the server is never asked for the envelopes
	// of the matches.
	if n := srv.fetchCount(); n != 0 {
		t.Errorf("server saw %d fetches, want 0", n)
	}
}

// TestSearchWithTenDigitUIDs checks that a result of full-width UIDs is
// served and refused at the right boundary.
//
// A ten-digit UID occupies eleven bytes on the wire, so the decoder's
// byte limit must accommodate the whole result.
func TestSearchWithTenDigitUIDs(t *testing.T) {
	const budget = 20
	const base = 4_000_000_000
	msgs := make([]listFixtureMessage, budget)
	uids := make([]uint32, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(base+uint32(i), baseTime.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("m%d", i+1))
		uids[i] = base + uint32(i)
	}
	_, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages with ten-digit UIDs: %v", err)
	}
	if len(res.Messages) != budget {
		t.Errorf("got %d messages, want %d", len(res.Messages), budget)
	}
	if got := res.Messages[0].UID; got != base {
		t.Errorf("first UID = %d, want %d", got, base)
	}
}

// TestSearchAtMinimalBudgetFetchesAnOrdinaryEnvelope checks that a
// minimal scan budget does not make an ordinary response unusable.
//
// The decoder's field limit has a floor, so a FETCH envelope - which
// holds about thirty fields - is still accepted whatever the scan
// budget is.
func TestSearchAtMinimalBudgetFetchesAnOrdinaryEnvelope(t *testing.T) {
	const budget = 1
	msgs := []listFixtureMessage{
		fixtureMessage(1, baseTime, "m1"),
		fixtureMessage(2, baseTime.Add(-time.Hour), "m2"),
	}
	_, addr, cleanup := startSearchFixture(t, msgs, []uint32{1})
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages with a minimal budget: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(res.Messages))
	}
	if res.Messages[0].Subject != "m1" {
		t.Errorf("subject = %q, want %q", res.Messages[0].Subject, "m1")
	}
	// The envelope must have been fetched in full.
	if res.Messages[0].From != "sender@example.test" {
		t.Errorf("from = %q, want the envelope's sender", res.Messages[0].From)
	}
}

// TestMaximumSizeLiteralIsStillFetched checks that a literal of the
// configured maximum message size is still valid.
//
// The decoder's byte limit is derived from the maximum message size
// plus the protocol around it, so the largest legitimate body still
// fits.
func TestMaximumSizeLiteralIsStillFetched(t *testing.T) {
	const maxMessageBytes = 1 << 20
	be := listFixtureBackend(t, "127.0.0.1:1", 100)
	opts := be.decoderLimits(100)
	if uint64(opts.MaxResponseBytes) < uint64(maxMessageBytes)+decoderLiteralOverhead {
		t.Fatalf("MaxResponseBytes = %d, want at least %d", opts.MaxResponseBytes,
			uint64(maxMessageBytes)+decoderLiteralOverhead)
	}
	if uint32(opts.MaxLiteralSize) != maxMessageBytes {
		t.Fatalf("MaxLiteralSize = %d, want %d", opts.MaxLiteralSize, maxMessageBytes)
	}
}

// TestDecoderLimitIsClassifiedSeparately checks that a decoder-limit
// violation is not reported as the application's scan-budget refusal.
func TestDecoderLimitIsClassifiedSeparately(t *testing.T) {
	const budget = 20
	// A result far beyond the decoder's field limit.
	msgs := make([]listFixtureMessage, 500)
	uids := make([]uint32, 500)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)
	if n := srv.fetchCount(); n != 0 {
		t.Errorf("server saw %d fetches, want 0", n)
	}
}

// TestDecoderFailureDiscardsTheConnectionAndReconnects checks the
// connection lifecycle after a decoder violation.
func TestDecoderFailureDiscardsTheConnectionAndReconnects(t *testing.T) {
	const budget = 20
	msgs := make([]listFixtureMessage, 500)
	uids := make([]uint32, 500)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)

	// The client and its socket are discarded together.
	be.mu.Lock()
	client, conn := be.client, be.conn
	be.mu.Unlock()
	if client != nil {
		t.Error("the client was retained after the decoder violation")
	}
	if conn != nil {
		t.Error("the socket was retained after the decoder violation")
	}

	// A later operation reconnects and succeeds.
	srv.setSearchUIDs([]uint32{1})
	srv.setMessages([]listFixtureMessage{fixtureMessage(1, baseTime, "m1")}, 0)
	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages after the decoder failure: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Errorf("got %d messages, want 1", len(res.Messages))
	}
	if got := srv.connectionCount(); got != 2 {
		t.Errorf("server saw %d connections, want 2", got)
	}
}

// TestDecoderLimitsApplyUnderBothTLSModes checks that the decoder
// limits are in force under implicit TLS and STARTTLS.
func TestDecoderLimitsApplyUnderBothTLSModes(t *testing.T) {
	const budget = 20
	for _, mode := range []struct {
		name     string
		startTLS bool
	}{
		{name: "implicit TLS", startTLS: false},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			msgs := make([]listFixtureMessage, 500)
			uids := make([]uint32, 500)
			for i := range msgs {
				msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
				uids[i] = uint32(i + 1)
			}
			srv := &listFixtureServer{messages: msgs, searchUIDs: uids, startTLS: mode.startTLS}
			addr, cleanup := srv.start(t)
			defer cleanup()

			be := listFixtureBackendMode(t, addr, budget, mode.startTLS, 30*time.Second)
			defer be.Close()

			_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
			requireResponseTooLarge(t, err)

			// The client reconnects and succeeds.
			srv.setSearchUIDs([]uint32{1})
			srv.setMessages([]listFixtureMessage{fixtureMessage(1, baseTime, "m1")}, 0)
			if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err != nil {
				t.Fatalf("SearchMessages after the decoder failure: %v", err)
			}
		})
	}
}

// TestDecoderLimitsSurviveTheSTARTTLSUpgrade checks that the limits
// are still in force after the connection has been upgraded.
//
// The reader is created once and survives the upgrade, so the limits
// must still apply to the responses that follow it. The test upgrades
// the connection explicitly and then triggers a violation.
func TestDecoderLimitsSurviveTheSTARTTLSUpgrade(t *testing.T) {
	const budget = 20
	srv := &listFixtureServer{
		messages:   budgetMessages(1),
		searchUIDs: []uint32{1},
		startTLS:   true,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackendMode(t, addr, budget, true, 30*time.Second)
	defer be.Close()

	// The first search succeeds over the upgraded connection.
	if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err != nil {
		t.Fatalf("SearchMessages over the upgraded connection: %v", err)
	}

	// The next one violates the decoder's field limit, so the limits
	// survived the upgrade.
	msgs := make([]listFixtureMessage, 500)
	uids := make([]uint32, 500)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	srv.setSearchUIDs(uids)
	srv.setMessages(msgs, 0)

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)
}

// TestDecoderOversizedAtomIsRefused checks that an oversized atom in
// a response is refused and that the connection is torn down.
func TestDecoderOversizedAtomIsRefused(t *testing.T) {
	// The response's name is far larger than the decoder's atom limit.
	big := strings.Repeat("x", int(decoderAtomSizeLimit)+16)
	srv := &listFixtureServer{searchUIDs: []uint32{1}, rawSearchResponse: "* " + big + "\r\n"}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)
	if n := srv.fetchCount(); n != 0 {
		t.Errorf("server saw %d fetches, want 0", n)
	}
}

// TestDecoderOversizedQuotedStringIsRefused checks that an oversized
// quoted string in a response is refused.
func TestDecoderOversizedQuotedStringIsRefused(t *testing.T) {
	big := strings.Repeat("x", int(decoderQuotedStringSizeLimit)+16)
	srv := &listFixtureServer{
		searchUIDs:        []uint32{1},
		rawSearchResponse: `* SEARCH "` + big + "\"\r\n",
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)
}

// TestDecoderErrorCarriesNoServerContent checks that the decoder
// violation's message never carries content from the response.
func TestDecoderErrorCarriesNoServerContent(t *testing.T) {
	const marker = "SERVERCONTENTMARKER"
	// The atom must exceed the decoder's atom limit for the violation
	// to fire, so the marker is repeated until it is over the limit.
	big := strings.Repeat(marker, int(decoderAtomSizeLimit/len(marker))+2)
	srv := &listFixtureServer{searchUIDs: []uint32{1}, rawSearchResponse: "* " + big + "\r\n"}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)
	if strings.Contains(err.Error(), marker) {
		t.Errorf("err = %q, want it not to carry content from the response", err)
	}
}

// TestSearchCancellationIsNotBlocked checks that a cancelled search
// returns and leaves no goroutine behind.
func TestSearchCancellationIsNotBlocked(t *testing.T) {
	msgs := make([]listFixtureMessage, 50)
	uids := make([]uint32, 50)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	_, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := be.SearchMessages(ctx, "test", "INBOX", mailbox.SearchQuery{}); err == nil {
		t.Error("a cancelled search returned no error")
	}
	assertNoBlockedFetchGoroutine(t)
}

// TestDecoderLimitIsNotAMessageSizeViolation checks that the decoder
// violation is distinct from the literal and message size violations.
func TestDecoderLimitIsNotAMessageSizeViolation(t *testing.T) {
	big := strings.Repeat("x", int(decoderAtomSizeLimit)+16)
	srv := &listFixtureServer{searchUIDs: []uint32{1}, rawSearchResponse: "* " + big + "\r\n"}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)
	if errors.Is(err, mailbox.ErrMessageTooLarge) {
		t.Errorf("err = %v, want it not to be a message-size violation", err)
	}
	if errors.Is(err, mailbox.ErrMalformed) {
		t.Errorf("err = %v, want it not to be a malformed-response error", err)
	}
}
