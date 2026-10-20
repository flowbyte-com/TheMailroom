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
// time and no timezone. A search for
// "since 2024-08-15T10:11:12Z" therefore behaves as "the
// message's date is on or after 2024-08-15" and can include
// messages sent earlier on that day.
//
// Two different normalisations are involved:
//
//   - The search value is truncated to its UTC day boundary
//     (TruncateDate). That is the date placed on the wire.
//   - The message's Date: header is reduced to the calendar day
//     named by its own date fields, ignoring the time and the
//     timezone offset (CalendarDay). This mirrors the reference
//     IMAP server's comparison.
//
// Both backends must use the same two normalisations. In
// particular, do not use TruncateDate on a message Date:
// converting it to UTC first can move it to the previous or next
// calendar day. A server that interprets the bare search date
// in its own local timezone can still differ by up to one day;
// that is inherent to IMAP's date-only search keys and is
// documented as a limitation rather than papered over.
//
// # Flag semantics
//
// Unseen means "the message does not carry the \Seen flag".
// Flagged means "the message carries the \Flagged flag". A
// message with no flags at all is unseen and not flagged. This
// matches the IMAP definitions (RFC 3501 section 2.3.2) and the
// memory backend derives its flags from the conventional mbox
// Status: and X-Status: headers so it can answer the same
// questions.
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
// timestamp is converted to UTC and its time-of-day is then
// discarded, exactly as the "2-Jan-2006" search date layout does
// on the wire.
//
// TruncateDate is for query values (Since / Before), not for
// message Date headers. Converting a message timestamp to UTC
// before taking its day can move it to a different calendar
// day; use CalendarDay for the message side.
func TruncateDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// CalendarDay reduces t to the calendar day named by its own
// date fields, returned as UTC midnight. The time of day and the
// timezone offset are ignored, so 2024-08-15 00:30 +0200 and
// 2024-08-15 23:30 -0500 both map to 2024-08-15 UTC.
//
// This is the message-side normalisation for IMAP date search:
// the reference server compares the Date: header's calendar day
// and ignores its time and zone. It is deliberately different
// from TruncateDate, which converts to UTC first.
func CalendarDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
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

// FlagsFromStatusHeaders derives IMAP system flags from the
// conventional mbox Status: and X-Status: header values.
//
// The conventions are:
//
//	Status:   R - the message has been read (\Seen)
//	          O - the message is old (no flag; we do not model \Recent)
//	X-Status: D - the message is deleted (\Deleted)
//	          F - the message is flagged (\Flagged)
//	          A - the message has been answered (\Answered)
//
// These headers are an mbox convention, not an RFC 2156
// definition (RFC 2156 defines MIXER, not local mailbox state).
// A message with neither header has no flags, which means it is
// unseen and not flagged.
func FlagsFromStatusHeaders(status, xstatus string) []string {
	var flags []string
	for _, r := range status {
		switch r {
		case 'R', 'r':
			flags = append(flags, FlagSeen)
		case 'O', 'o':
			// Old: not \Recent. The memory backend does
			// not model \Recent, so this contributes no
			// flag.
		}
	}
	for _, r := range xstatus {
		switch r {
		case 'D', 'd':
			flags = append(flags, FlagDeleted)
		case 'F', 'f':
			flags = append(flags, FlagFlagged)
		case 'A', 'a':
			flags = append(flags, FlagAnswered)
		}
	}
	return flags
}
