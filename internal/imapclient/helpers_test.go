package imapclient

import (
	"errors"
	"testing"

	"github.com/emersion/go-imap"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
)

// TestMapErrMimeparse is the regression for the mimeparse
// error mapping. Every mimeparse typed error must map to
// the corresponding mailbox typed error.
func TestMapErrMimeparse(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"message too large", mimeparse.ErrMessageTooLarge, mailbox.ErrMessageTooLarge},
		{"nesting too deep", mimeparse.ErrNestingTooDeep, mailbox.ErrNestingTooDeep},
		{"too many parts", mimeparse.ErrTooManyParts, mailbox.ErrTooManyParts},
		{"too many attachments", mimeparse.ErrTooManyAttachments, mailbox.ErrTooManyAttachments},
		{"attachment too large", mimeparse.ErrAttachmentTooLarge, mailbox.ErrAttachmentTooLarge},
		{"total attachment too large", mimeparse.ErrTotalAttachmentTooLarge, mailbox.ErrTotalAttachmentTooLarge},
		{"malformed", mimeparse.ErrMalformed, mailbox.ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapErr(tc.in)
			if !errors.Is(got, tc.want) {
				t.Errorf("mapErr(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestMapErrLiteralSizeEvidence is the regression for
// PR #3 review item #4: only a literal-cap error with
// concrete evidence ("literal exceeding maximum size")
// must be translated to ErrMessageTooLarge. Generic parse
// errors such as "expected a space" or "error reading
// response" must NOT be misreported as ErrMessageTooLarge.
// Disguising a protocol parse failure as a size-limit
// violation would mislead callers into believing the size
// control succeeded when in fact the protocol desynced.
//
// The string-match for the literal-cap evidence is the
// only signal we can test from outside the go-imap
// package (its *parseError type is unexported). The
// imap.IsParseError branch in mapErr is exercised in
// integration tests when the in-process server produces a
// real parse error.
func TestMapErrLiteralSizeEvidence(t *testing.T) {
	t.Run("concrete size cap error maps to ErrMessageTooLarge", func(t *testing.T) {
		// The only string go-imap produces from its own
		// MaxLiteralSize check. This is the concrete
		// evidence of an oversized literal.
		got := mapErr(errors.New("imap: literal exceeding maximum size"))
		if !errors.Is(got, mailbox.ErrMessageTooLarge) {
			t.Errorf("mapErr = %v, want ErrMessageTooLarge", got)
		}
	})

	t.Run("generic parse errors do not map to ErrMessageTooLarge", func(t *testing.T) {
		// These strings are NOT evidence of an oversized
		// literal. They must NOT be translated to
		// ErrMessageTooLarge. (They may map to
		// ErrMalformed only if imap.IsParseError is true;
		// for plain errors.New they are returned verbatim.
		// Either way, they are NOT ErrMessageTooLarge.)
		cases := []error{
			errors.New("imap: expected a space"),
			errors.New("imap/client: error reading response: expected a space"),
			errors.New("some other error"),
		}
		for _, in := range cases {
			got := mapErr(in)
			if errors.Is(got, mailbox.ErrMessageTooLarge) {
				t.Errorf("mapErr(%v) = %v, must not be ErrMessageTooLarge", in, got)
			}
		}
	})
}

// TestMapErrConnectionClosed is the regression for the
// connection-closed path. The go-imap client returns
// "imap: connection closed" when the executor sees the
// loggedOut channel close; this is the signal for any
// unrecoverable transport failure, including the
// protocol-level literal cap. mapErr must wrap it so
// the caller can distinguish it from the application-level
// cap (which returns ErrMessageTooLarge directly).
func TestMapErrConnectionClosed(t *testing.T) {
	got := mapErr(errors.New("imap: connection closed"))
	if got == nil {
		t.Fatal("mapErr returned nil")
	}
	// The wrapped message must still contain the original.
	if !contains(got.Error(), "imap: connection closed") {
		t.Errorf("mapErr(connection closed) = %v, want the original message to be preserved", got)
	}
	// A connection-closed error must NOT be misreported as
	// ErrMessageTooLarge: the transport failure is not
	// evidence of a size-limit violation.
	if errors.Is(got, mailbox.ErrMessageTooLarge) {
		t.Errorf("mapErr(connection closed) = %v, must not be ErrMessageTooLarge (it is a transport failure, not a size signal)", got)
	}
}

// TestMapErrMailboxNotFound is a sanity check that the
// existing mailbox-not-found detection still works.
func TestMapErrMailboxNotFound(t *testing.T) {
	got := mapErr(errors.New("Mailbox doesn't exist: NoSuchMailbox"))
	if !errors.Is(got, mailbox.ErrMailboxNotFound) {
		t.Errorf("mapErr = %v, want ErrMailboxNotFound", got)
	}
}

// TestMapErrPassThroughUnknown is a sanity check that
// errors we don't recognise are returned verbatim (not
// translated to a typed error).
func TestMapErrPassThroughUnknown(t *testing.T) {
	in := errors.New("some other error")
	got := mapErr(in)
	if got != in {
		t.Errorf("mapErr(%v) = %v, want %v (verbatim)", in, got, in)
	}
}

// TestMapErrNil is a sanity check for the nil case.
func TestMapErrNil(t *testing.T) {
	if got := mapErr(nil); got != nil {
		t.Errorf("mapErr(nil) = %v, want nil", got)
	}
}

// contains is a small helper for substring checks.
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Ensure the unused import is referenced in a way that the
// compiler accepts even when the file is changed.
var _ imap.FetchItem
