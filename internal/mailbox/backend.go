// Package mailbox defines the provider-agnostic mail operations TheMailroom
// supports. Concrete implementations live in internal/imapclient (Phase 2)
// and internal/mailbox/memory (testing and development).
//
// The Backend interface is intentionally small. Phase 2 will extend it with
// sending, modifying and deleting operations.
package mailbox

import (
	"context"
	"errors"
	"time"
)

// Backend is the abstraction tools use to read mail.
type Backend interface {
	// Mailboxes lists mailboxes for the given account.
	Mailboxes(ctx context.Context, accountID string) ([]Mailbox, error)

	// ListMessages returns message summaries in a mailbox, ordered by
	// descending internal date (newest first). Implementations MUST
	// honour opts.Limit by returning no more than that many entries.
	// UIDValidity is the live UIDVALIDITY of the mailbox observed
	// during the same EXAMINE that produced the UIDs in Messages;
	// tools must not combine UIDs from one ListResult with a
	// UIDVALIDITY obtained from a later selection.
	ListMessages(ctx context.Context, accountID, mailbox string, opts ListOptions) (ListResult, error)

	// FetchMessage returns a full message body. Implementations MUST
	// enforce the per-account max_message_bytes limit when decoding.
	//
	// uidValidity is the UIDVALIDITY the caller last observed for
	// the mailbox. Implementations MUST validate it against the
	// current mailbox UIDVALIDITY and return ErrUIDMismatch if it
	// does not match, or ErrUIDMissing if the server does not
	// report a UIDVALIDITY (some broken servers return 0).
	FetchMessage(ctx context.Context, accountID, mailbox string, uid uint32, uidValidity uint32) (*Message, error)

	// FetchAttachment returns one attachment from an immutable
	// message. part is the 1-indexed IMAP-style part path previously
	// returned by FetchMessage/read_message.
	//
	// Implementations MUST validate uidValidity before returning
	// content and MUST NOT set the \Seen flag. Content is decoded:
	// transfer encodings are removed and text parts are charset-
	// decoded to UTF-8 where possible. Text attachments are therefore
	// not guaranteed to be byte-identical to the sender's original
	// file.
	FetchAttachment(ctx context.Context, accountID, mailbox string, uid uint32, uidValidity uint32, part string) (*AttachmentData, error)

	// SearchMessages returns matching message summaries. The
	// returned UIDValidity is the live value observed during the
	// same EXAMINE that produced the search results.
	SearchMessages(ctx context.Context, accountID, mailbox string, q SearchQuery) (ListResult, error)
}

// Mailbox describes one folder on the server.
type Mailbox struct {
	Name        string   `json:"name"`
	Attributes  []string `json:"attributes,omitempty"`
	Delimiter   string   `json:"delimiter,omitempty"`
	UIDValidity uint32   `json:"uid_validity,omitempty"`
	UIDNext     uint32   `json:"uid_next,omitempty"`
}

// ListResult is the return value of ListMessages and
// SearchMessages. It binds a set of message summaries to a
// UIDVALIDITY observed during the same IMAP EXAMINE; the
// caller MUST use this UIDVALIDITY for any subsequent
// FetchMessage call.
type ListResult struct {
	UIDValidity uint32
	Messages    []MessageSummary
}

// ListOptions controls how ListMessages returns data.
type ListOptions struct {
	// Limit is the maximum number of messages to return. Zero means
	// "use the account default"; implementations will clamp to the
	// configured per-account max_batch_operations.
	Limit int

	// Offset skips the first N messages in the (server-ordered) result
	// set. Zero means start from the most recent.
	Offset int

	// Since filters to messages with an internal date at or after t.
	Since *time.Time

	// Before filters to messages with an internal date strictly before t.
	Before *time.Time
}

// SearchQuery holds the supported search criteria. Empty fields are
// ignored; an all-empty query returns every message in the mailbox,
// bounded by Limit or the account's max_search_results. All populated
// criteria are ANDed.
type SearchQuery struct {
	Subject string     `json:"subject,omitempty"`
	From    string     `json:"from,omitempty"`
	To      string     `json:"to,omitempty"`
	Body    string     `json:"body,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
	Before  *time.Time `json:"before,omitempty"`
	Flagged bool       `json:"flagged,omitempty"`
	Unseen  bool       `json:"unseen,omitempty"`
	Limit   int        `json:"limit,omitempty"`
}

// MessageSummary is the lightweight envelope used in listings and search
// results. It avoids decoding the full body.
type MessageSummary struct {
	UID           uint32    `json:"uid"`
	Flags         []string  `json:"flags,omitempty"`
	InternalDate  time.Time `json:"internal_date"`
	Size          uint32    `json:"size"`
	From          string    `json:"from,omitempty"`
	To            []string  `json:"to,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	HasAttachment bool      `json:"has_attachments"`
}

// Message is the full message body returned by FetchMessage.
type Message struct {
	UID         uint32         `json:"uid"`
	UIDValidity uint32         `json:"uid_validity"`
	Flags       []string       `json:"flags,omitempty"`
	Headers     MessageHeaders `json:"headers"`
	Text        string         `json:"text,omitempty"`
	HTML        string         `json:"html,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
	Warnings    []string       `json:"warnings,omitempty"`
	Truncated   bool           `json:"truncated,omitempty"`
}

// FlagsSlice returns the message's flags as a fresh slice, or an
// empty slice if the message has none. Callers should not mutate
// the returned slice.
func (m *Message) FlagsSlice() []string {
	if len(m.Flags) == 0 {
		return []string{}
	}
	out := make([]string, len(m.Flags))
	copy(out, m.Flags)
	return out
}

// MessageHeaders are the structured headers used by tools.
type MessageHeaders struct {
	MessageID  string    `json:"message_id,omitempty"`
	From       string    `json:"from,omitempty"`
	To         []string  `json:"to,omitempty"`
	Cc         []string  `json:"cc,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	Date       time.Time `json:"date"`
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	References []string  `json:"references,omitempty"`
}

// Attachment describes one MIME attachment.
type Attachment struct {
	Index       int    `json:"index"`
	Part        string `json:"part,omitempty"`
	Filename    string `json:"filename"`
	MimeType    string `json:"mime_type"`
	Size        int64  `json:"size"`
	ContentID   string `json:"content_id,omitempty"`
	Inline      bool   `json:"inline,omitempty"`
	Disposition string `json:"disposition,omitempty"`
}

// AttachmentData is the decoded content of one selected attachment,
// together with the metadata from the same MIME parse. Content is
// transfer-decoded; text parts are charset-decoded to UTF-8 where
// possible. Callers must not treat filenames or MIME types as trusted
// paths or executable content.
type AttachmentData struct {
	UID         uint32
	UIDValidity uint32
	Attachment  Attachment
	Content     []byte
	Warnings    []string
}

// Size returns an approximate total size of the message body used for
// listings. It avoids forcing callers to know about the on-wire format.
func (m *Message) Size() int {
	n := len(m.Text) + len(m.HTML)
	for _, a := range m.Attachments {
		n += int(a.Size)
	}
	return n
}

// Subject returns the message subject. It exists as a method so tests
// and callers do not have to reach into Headers for the common case.
func (m *Message) Subject() string { return m.Headers.Subject }

// Common errors. Wrapping these lets tools map backend errors onto the
// typed error codes in docs/TOOLS.md.
var (
	ErrBackendNotConfigured = errors.New("mailbox: backend not configured")
	ErrAccountNotFound      = errors.New("mailbox: account not found")
	ErrMailboxNotFound      = errors.New("mailbox: mailbox not found")
	ErrMessageNotFound      = errors.New("mailbox: message not found")
	ErrAttachmentNotFound   = errors.New("mailbox: attachment not found")
	ErrInvalidPart          = errors.New("mailbox: invalid attachment part")
	ErrUIDMismatch          = errors.New("mailbox: uid/uid_validity mismatch")
	ErrUIDMissing           = errors.New("mailbox: server did not report uid_validity")
	ErrLimitExceeded        = errors.New("mailbox: limit exceeded")
	// ErrListTooLarge is returned when a mailbox holds more messages
	// than the configured list scan budget. Listing is ordered by
	// internal date, which is unrelated to UID order, so an exact page
	// can only be produced by examining every message. Rather than
	// return an approximate page, the backend refuses the mailbox.
	//
	// The refusal is driven by the messages the server actually
	// delivers, not only by the mailbox size it reports: a server that
	// under-reports its size is caught while the listing is running.
	ErrListTooLarge = errors.New("mailbox: too many messages to list")
	// ErrSearchTooLarge is returned when a search matches more messages
	// than the configured scan budget. The search criteria are
	// evaluated by the server, so a result set can be arbitrarily
	// large; rather than fetch and process every match, the backend
	// refuses the search.
	ErrSearchTooLarge = errors.New("mailbox: too many messages to search")
	// ErrResponseTooLarge is returned when the IMAP response exceeded
	// one of the decoder's per-response resource limits: too many
	// fields, an oversized atom or quoted string, or too many bytes.
	// It is distinct from ErrSearchTooLarge, which is the
	// application's own scan budget, and from ErrMalformed, which is
	// a response that could not be parsed at all.
	ErrResponseTooLarge = errors.New("mailbox: imap response exceeded a decoder limit")
	// ErrServerDataBudgetExceeded is returned when the server sent more
	// protocol data than the operation allows.
	//
	// The decoder's limits bound one response; this bound is the
	// aggregate: the inbound bytes of the operation's commands and of
	// the unsolicited responses that arrive while they run. The server
	// sent too much data, so the connection was closed and the client,
	// its socket and its budget were discarded together.
	//
	// The message that wraps it names the recourse that fits the
	// operation: a search can be narrowed, because the server evaluates
	// the criteria; a summary listing cannot, because its date filters
	// run in the client after the fetch; a mailbox enumeration has no
	// narrower form; a raw message fetch is bounded by the message
	// size; and a violation detected between operations is an idle
	// violation.
	ErrServerDataBudgetExceeded = errors.New("mailbox: server sent more protocol data than the operation allows")
	ErrMessageTooLarge          = errors.New("mailbox: message too large")
	ErrNestingTooDeep           = errors.New("mailbox: mime nesting too deep")
	ErrTooManyParts             = errors.New("mailbox: too many mime parts")
	ErrAttachmentTooLarge       = errors.New("mailbox: attachment too large")
	ErrTotalAttachmentTooLarge  = errors.New("mailbox: total attachment size too large")
	ErrTooManyAttachments       = errors.New("mailbox: too many attachments")
	ErrMalformed                = errors.New("mailbox: malformed message")

	// ErrTimeout is returned when an operation exceeded its deadline.
	//
	// It covers the finite end-to-end operation deadline, the per-command
	// ceiling and the account's queue allowance. The three are
	// distinguished by the message that wraps this error, never by the
	// proximity of a failure to an expected deadline.
	//
	// It is distinct from ErrServerDataBudgetExceeded, which is a
	// volume bound and keeps its own classification.
	ErrTimeout = errors.New("mailbox: the IMAP operation timed out")
	// ErrCanceled is returned when the caller's context cancelled the
	// operation. It is distinct from ErrTimeout: a cancelled operation
	// was not slow, it was withdrawn.
	ErrCanceled = errors.New("mailbox: the IMAP operation was canceled")
	// ErrBackendClosed is returned when the backend was closed while an
	// operation was in progress or waiting to start.
	ErrBackendClosed = errors.New("mailbox: the IMAP backend is closed")
)
