package tools

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var searchMessagesTool = &mcp.Tool{
	Name:        "search_messages",
	Description: "Search a mailbox. All populated criteria are ANDed together. Empty query fields are ignored; an empty query returns every message in the mailbox, bounded by the account limit. Requires the 'read' capability.",
}

type SearchMessagesArgs struct {
	AccountID string              `json:"account_id" jsonschema:"the account to search"`
	Mailbox   string              `json:"mailbox"     jsonschema:"the mailbox to search"`
	Query     SearchMessagesQuery `json:"query"       jsonschema:"search criteria"`
}

// SearchMessagesQuery mirrors mailbox.SearchQuery with JSON tags and a
// jsonschema description per field. It is intentionally a separate type
// so the wire format can evolve independently of the backend.
//
// All populated criteria are ANDed. The date criteria have
// day granularity: the value is truncated to the UTC day before it
// is applied, because an IMAP search date key carries no time. A
// "since" of 2024-08-15T23:00:00Z therefore includes messages sent
// earlier on 2024-08-15.
type SearchMessagesQuery struct {
	Subject string     `json:"subject,omitempty" jsonschema:"case-insensitive substring match against the Subject header"`
	From    string     `json:"from,omitempty"    jsonschema:"case-insensitive substring match against the From header"`
	To      string     `json:"to,omitempty"      jsonschema:"case-insensitive substring match against any recipient in To or Cc"`
	Body    string     `json:"body,omitempty"    jsonschema:"case-insensitive substring match against the plain-text body"`
	Since   *time.Time `json:"since,omitempty"   jsonschema:"RFC 3339 timestamp; only messages on or after this date (truncated to the UTC day)"`
	Before  *time.Time `json:"before,omitempty"  jsonschema:"RFC 3339 timestamp; only messages before this date (truncated to the UTC day)"`
	Flagged bool       `json:"flagged,omitempty" jsonschema:"only messages carrying the \\\\Flagged flag"`
	Unseen  bool       `json:"unseen,omitempty"  jsonschema:"only messages not carrying the \\\\Seen flag"`
	Limit   int        `json:"limit,omitempty"   jsonschema:"maximum results; 0 means account default"`
}

type SearchMessagesResult struct {
	AccountID   string            `json:"account_id"`
	Mailbox     string            `json:"mailbox"`
	UIDValidity uint32            `json:"uid_validity"`
	Matches     []messageSummaryV `json:"matches"`
}
