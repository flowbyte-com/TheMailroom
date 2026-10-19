package tools

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var listMessagesTool = &mcp.Tool{
	Name:        "list_messages",
	Description: "List message summaries in a mailbox, newest first. Requires the 'read' capability.",
}

type ListMessagesArgs struct {
	AccountID string     `json:"account_id"      jsonschema:"the account to read from"`
	Mailbox   string     `json:"mailbox"          jsonschema:"the mailbox to list (e.g. INBOX)"`
	Limit     int        `json:"limit,omitempty"  jsonschema:"maximum number of messages to return; 0 means account default"`
	Offset    int        `json:"offset,omitempty" jsonschema:"skip the first N messages; 0 means start from the most recent"`
	Since     *time.Time `json:"since,omitempty"  jsonschema:"RFC 3339 timestamp; only messages on or after this time"`
	Before    *time.Time `json:"before,omitempty" jsonschema:"RFC 3339 timestamp; only messages before this time"`
}

type ListMessagesResult struct {
	AccountID   string            `json:"account_id"`
	Mailbox     string            `json:"mailbox"`
	UIDValidity uint32            `json:"uid_validity"`
	Messages    []messageSummaryV `json:"messages"`
}
