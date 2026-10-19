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
	ListMessages(ctx context.Context, accountID, mailbox string, opts ListOptions) ([]MessageSummary, error)

	// FetchMessage returns a full message body. Implementations MUST
	// enforce the per-account max_message_bytes limit when decoding.
	FetchMessage(ctx context.Context, accountID, mailbox string, uid uint32) (*Message, error)

	// SearchMessages returns matching message summaries.
	SearchMessages(ctx context.Context, accountID, mailbox string, q SearchQuery) ([]MessageSummary, error)
}

// Mailbox describes one folder on the server.
type Mailbox struct {
	Name        string   `json:"name"`
	Attributes  []string `json:"attributes,omitempty"`
	Delimiter   string   `json:"delimiter,omitempty"`
	UIDValidity uint32   `json:"uid_validity,omitempty"`
	UIDNext     uint32   `json:"uid_next,omitempty"`
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
// ignored; an all-empty query returns no messages.
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
	Headers     MessageHeaders `json:"headers"`
	Text        string         `json:"text,omitempty"`
	HTML        string         `json:"html,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
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
	Index     int    `json:"index"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mime_type"`
	Size      int64  `json:"size"`
	ContentID string `json:"content_id,omitempty"`
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
	ErrUIDMismatch          = errors.New("mailbox: uid/uid_validity mismatch")
	ErrLimitExceeded        = errors.New("mailbox: limit exceeded")
)
