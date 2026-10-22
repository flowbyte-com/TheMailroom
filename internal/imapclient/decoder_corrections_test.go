package imapclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// --- the corrections found in the final review ---

// TestMalformedResponseInsideReadRespIsClassifiedAsMalformed checks
// that a response which fails *inside* the IMAP reader is classified as
// a malformed response.
//
// The fork's IsParseError does not unwrap: it inspects the error
// itself. The executor wraps a reader error in its typed
// connection-closed error, so a parse error that failed inside ReadResp
// reaches the application one level down, and the direct type
// assertion misses it.
//
// The response used here fails in the parser: a field that is not
// separated by a space. It is not merely a syntactically valid SEARCH
// response with a non-numeric field, which the *handler* rejects
// rather than the parser.
func TestMalformedResponseInsideReadRespIsClassifiedAsMalformed(t *testing.T) {
	// The field separator is a quote, so the parser fails with
	// "fields are not separated by a space".
	srv := &listFixtureServer{
		searchUIDs:        []uint32{1},
		rawSearchResponse: "* SEARCH 1\"x\"\r\n",
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatalf("SearchMessages returned %v, want an error", res)
	}
	if !errors.Is(err, mailbox.ErrMalformed) {
		t.Fatalf("err = %v, want errors.Is(err, mailbox.ErrMalformed)", err)
	}
	// A decoder violation keeps its own classification.
	if errors.Is(err, mailbox.ErrResponseTooLarge) {
		t.Errorf("err = %v, want it not to be a decoder-limit violation", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a malformed response must not produce a result", len(res.Messages))
	}
	// The desynchronised connection is discarded.
	be.mu.Lock()
	client, conn := be.client, be.conn
	be.mu.Unlock()
	if client != nil {
		t.Error("the client was retained after the malformed response")
	}
	if conn != nil {
		t.Error("the socket was retained after the malformed response")
	}
	// A later valid operation reconnects and succeeds.
	srv.setSearchUIDs([]uint32{1})
	srv.setMessages([]listFixtureMessage{fixtureMessage(1, baseTime, "m1")}, 0)
	srv.rawSearchResponse = ""
	res, err = be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages after the malformed response: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Errorf("got %d messages, want 1", len(res.Messages))
	}
}

// TestTransportFailureIsNotAMalformedResponse checks that a plain
// transport failure is not reported as a malformed response.
//
// The reader's error chain must not turn an EOF, a timeout or a reset
// into ErrMalformed: the executor only records a cause when the reader
// reported a parse error, so a transport failure arrives without one.
func TestTransportFailureIsNotAMalformedResponse(t *testing.T) {
	// The fixture closes the connection without answering.
	srv := &listFixtureServer{searchUIDs: []uint32{1}}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	// Close the listener's accepted connection by stopping the server.
	cleanup()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatal("expected an error for a closed connection")
	}
	if errors.Is(err, mailbox.ErrMalformed) {
		t.Errorf("err = %v, want it not to be a malformed-response error", err)
	}
}

// TestDecoderErrorMessageDoesNotRecommendTheScanBudget checks the
// wording of the decoder's refusal.
//
// A decoder violation can be any of the per-response limits - the number
// of fields, the size of an atom or of a quoted string, the total
// number of bytes, or the nesting depth of the lists - so neither
// narrowing the query nor raising the scan budget necessarily resolves
// it. The message must not claim that it does.
func TestDecoderErrorMessageDoesNotRecommendTheScanBudget(t *testing.T) {
	big := strings.Repeat("x", int(decoderAtomSizeLimit)+16)
	srv := &listFixtureServer{searchUIDs: []uint32{1}, rawSearchResponse: "* " + big + "\r\n"}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireResponseTooLarge(t, err)
	if strings.Contains(err.Error(), "max_list_scan_messages") {
		t.Errorf("err = %q, want it not to recommend raising the scan budget: "+
			"a decoder violation is not necessarily a scan-budget problem", err)
	}
	if strings.Contains(err.Error(), "narrow the query") {
		t.Errorf("err = %q, want it not to recommend narrowing the query", err)
	}
}

// TestSearchBudgetErrorMessageKeepsItsRecourse checks that the
// application's own scan-budget refusal keeps its specific recourse.
//
// The two conditions are distinct: a search that matched too many
// messages is resolved by narrowing the query or by raising the scan
// budget, and the message says so.
func TestSearchBudgetErrorMessageKeepsItsRecourse(t *testing.T) {
	const budget = 5
	msgs := make([]listFixtureMessage, budget+1)
	uids := make([]uint32, budget+1)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	_, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	requireSearchTooLarge(t, err)
	if !strings.Contains(err.Error(), "max_list_scan_messages") {
		t.Errorf("err = %q, want it to name the limit to raise", err)
	}
	if !strings.Contains(err.Error(), "narrow the query") {
		t.Errorf("err = %q, want it to mention narrowing the query", err)
	}
}

// TestMaximumSizeLiteralOverTheWire checks that a literal of exactly
// the configured maximum message size is accepted, and that a literal
// beyond the size cap is refused.
//
// The test goes through the normal backend connection path, so the
// decoder's limits are the ones the backend derives from the
// configuration, not a manually constructed reader.
//
// The limit is modest - a quarter of a mebibyte - so that the fixture
// stays cheap; the boundary is the same code path for any limit.
func TestMaximumSizeLiteralOverTheWire(t *testing.T) {
	// The raw message is exactly the maximum size. It must be a valid
	// message, because the backend parses it after the fetch.
	const maxMessageBytes = 256 << 10
	const header = "From: a@example.test\r\nSubject: boundary\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n"
	body := strings.Repeat("A", maxMessageBytes-len(header))
	raw := header + body
	if len(raw) != maxMessageBytes {
		t.Fatalf("the message is %d bytes, want %d", len(raw), maxMessageBytes)
	}

	for _, mode := range []struct {
		name     string
		startTLS bool
	}{
		{name: "implicit TLS", startTLS: false},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("at the limit", func(t *testing.T) {
				srv := &listFixtureServer{
					fetchLiteralBody: []byte(raw),
					startTLS:         mode.startTLS,
				}
				addr, cleanup := srv.start(t)
				defer cleanup()

				be := listFixtureBackendMode(t, addr, 100, mode.startTLS, 30*time.Second)
				defer be.Close()
				be.limits.MaxMessageBytes = maxMessageBytes

				msg, err := be.FetchMessage(t.Context(), "test", "INBOX", 1, 1)
				if err != nil {
					t.Fatalf("FetchMessage at the limit: %v", err)
				}
				if msg == nil {
					t.Fatal("FetchMessage returned no message")
				}
				if got := msg.Headers.Subject; got != "boundary" {
					t.Errorf("subject = %q, want %q", got, "boundary")
				}
			})

			t.Run("beyond the literal size cap", func(t *testing.T) {
				// The server announces a literal one byte over the cap
				// and never sends the body: the client must refuse the
				// announcement before it allocates a buffer for it.
				srv := &listFixtureServer{
					fetchLiteralAnnounce: maxMessageBytes + 1,
					startTLS:             mode.startTLS,
				}
				addr, cleanup := srv.start(t)
				defer cleanup()

				be := listFixtureBackendMode(t, addr, 100, mode.startTLS, 30*time.Second)
				defer be.Close()
				be.limits.MaxMessageBytes = maxMessageBytes

				_, err := be.FetchMessage(t.Context(), "test", "INBOX", 1, 1)
				if err == nil {
					t.Fatal("expected an error for an over-cap literal")
				}
				if !errors.Is(err, mailbox.ErrMessageTooLarge) {
					t.Errorf("err = %v, want errors.Is(err, mailbox.ErrMessageTooLarge)", err)
				}
			})
		})
	}
}
