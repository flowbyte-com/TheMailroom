package tools

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var searchMessagesTool = &mcp.Tool{
	Name:        "search_messages",
	Description: "Search a mailbox. Empty query fields are ignored. Requires the 'read' capability.",
}

type SearchMessagesArgs struct {
	AccountID string              `json:"account_id" jsonschema:"the account to search"`
	Mailbox   string              `json:"mailbox"     jsonschema:"the mailbox to search"`
	Query     SearchMessagesQuery `json:"query"       jsonschema:"search criteria"`
}

// SearchMessagesQuery mirrors mailbox.SearchQuery with JSON tags and a
// jsonschema description per field. It is intentionally a separate type
// so the wire format can evolve independently of the backend.
type SearchMessagesQuery struct {
	Subject string     `json:"subject,omitempty" jsonschema:"substring match against the Subject header"`
	From    string     `json:"from,omitempty"    jsonschema:"substring match against the From header"`
	To      string     `json:"to,omitempty"      jsonschema:"substring match against any recipient in To or Cc"`
	Body    string     `json:"body,omitempty"    jsonschema:"substring match against the plain-text body"`
	Since   *time.Time `json:"since,omitempty"   jsonschema:"RFC 3339 timestamp; only messages on or after this time"`
	Before  *time.Time `json:"before,omitempty"  jsonschema:"RFC 3339 timestamp; only messages before this time"`
	Flagged bool       `json:"flagged,omitempty" jsonschema:"only flagged messages"`
	Unseen  bool       `json:"unseen,omitempty"  jsonschema:"only unseen messages"`
	Limit   int        `json:"limit,omitempty"   jsonschema:"maximum results; 0 means account default"`
}

type SearchMessagesResult struct {
	AccountID   string            `json:"account_id"`
	Mailbox     string            `json:"mailbox"`
	UIDValidity uint32            `json:"uid_validity"`
	Matches     []messageSummaryV `json:"matches"`
}
