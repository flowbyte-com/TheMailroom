package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

var listMailboxesTool = &mcp.Tool{
	Name:        "list_mailboxes",
	Description: "List mailboxes (folders) for an account. Requires the 'read' capability.",
}

type ListMailboxesArgs struct {
	AccountID string `json:"account_id" jsonschema:"the account to list mailboxes for"`
}

type ListMailboxesResult struct {
	AccountID string        `json:"account_id"`
	Mailboxes []mailboxView `json:"mailboxes"`
}
