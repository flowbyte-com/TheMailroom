// Package searchspec defines the shared search semantics for
// TheMailroom's mailbox backends.
//
// The IMAP backend delegates the matching to the IMAP server;
// the memory backend matches locally. For the two to honour the
// same public contract they must agree on:
//
//   - which criteria are ANDed and which are ORed;
//   - the granularity of the date criteria;
//   - the meaning of the flag criteria;
//   - what an empty query means.
//
// This package holds the parts that can be shared verbatim: the
// date truncation (IMAP date search is day-granularity, see
// RFC 3501 section 6.4.4 and the "2-Jan-2006" search date
// layout), and the flag predicates.
//
// # Date granularity
//
// RFC 3501's SINCE / BEFORE / SENTSINCE / SENTBEFORE compare
// *dates*, not timestamps: the search key carries a date with no
// time and no timezone, and the server interprets it in the
// server's own local timezone. A search for
// "since 2024-08-15T10:11:12Z" therefore behaves as "the
// message's date is on or after 2024-08-15" and can include
// messages sent earlier on that day.
//
// TheMailroom's public API is explicit about this: the Since and
// Before criteria are truncated to the UTC day boundary before
// they are applied.
//
// Why UTC: the IMAP search date goes on the wire as a bare
// calendar date ("2-Jan-2006"), and the reference server
// (go-imap) parses it as UTC midnight and truncates the
// message's Date: header to UTC midnight before comparing.
// Truncating the search value to UTC midnight therefore
// reproduces the server's comparison exactly. A server in
// another timezone interprets the bare date in its own local
// time and can differ by up to one day; that is inherent to
// IMAP's date-only search keys and is documented as a
// limitation rather than papered over.
//
// Both backends apply TruncateDate, so they agree exactly.
//
// # Flag semantics
//
// Unseen means "the message does not carry the \Seen flag".
// Flagged means "the message carries the \Flagged flag". A
// message with no flags at all is unseen and not flagged. This
// matches the IMAP definitions (RFC 3501 section 2.3.2) and the
// memory backend derives its flags from the RFC 2156 Status:
// header so it can answer the same questions.
package searchspec

import (
	"strings"
	"time"
)

// Canonical IMAP system flag names.
const (
	FlagSeen     = "\\Seen"
	FlagFlagged  = "\\Flagged"
	FlagDeleted  = "\\Deleted"
	FlagAnswered = "\\Answered"
)

// TruncateDate reduces t to the start of its UTC day, which
// is the granularity an IMAP SEARCH date key carries. The
// time-of-day and the timezone are both discarded, exactly as
// the "2-Jan-2006" search date layout does on the wire.
//
// The message side is truncated the same way by the IMAP
// server before the comparison, so truncating the search
// value to UTC midnight reproduces the server's comparison
// exactly.
func TruncateDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// HasFlag reports whether the flag set contains the given
// system flag. The comparison is case-insensitive because IMAP
// system flags are case-insensitive, and the flag set may come
// from a server that uses a different case.
func HasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, flag) {
			return true
		}
	}
	return false
}

// MatchesUnseen reports whether a message with the given flags
// satisfies the "unseen only" criterion. Unseen means the
// message does not carry \Seen.
func MatchesUnseen(flags []string, unseen bool) bool {
	if !unseen {
		return true
	}
	return !HasFlag(flags, FlagSeen)
}

// MatchesFlagged reports whether a message with the given flags
// satisfies the "flagged only" criterion.
func MatchesFlagged(flags []string, flagged bool) bool {
	if !flagged {
		return true
	}
	return HasFlag(flags, FlagFlagged)
}

// FlagsFromStatusHeader derives IMAP system flags from an RFC 2156
// Status: header value.
//
// The Status: header is the standard way for a local store to
// record message state. Its characters are:
//
//	R - the message has been read (\Seen)
//	O - the message is old (not \Recent)
//	F - the message is flagged (\Flagged)
//	D - the message is deleted (\Deleted)
//	A - the message has been answered (\Answered)
//
// A message with no Status: header has no flags, which means it
// is unseen and not flagged.
func FlagsFromStatusHeader(status string) []string {
	var flags []string
	for _, r := range status {
		switch r {
		case 'R', 'r':
			flags = append(flags, FlagSeen)
		case 'F', 'f':
			flags = append(flags, FlagFlagged)
		case 'D', 'd':
			flags = append(flags, FlagDeleted)
		case 'A', 'a':
			flags = append(flags, FlagAnswered)
		}
	}
	return flags
}
