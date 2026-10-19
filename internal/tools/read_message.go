package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

var readMessageTool = &mcp.Tool{
	Name:        "read_message",
	Description: "Read a single message, returning headers, text and HTML bodies, and an attachment list. Requires the 'read' capability.",
}

type ReadMessageArgs struct {
	AccountID   string `json:"account_id"             jsonschema:"the account to read from"`
	Mailbox     string `json:"mailbox"                 jsonschema:"the mailbox the message lives in"`
	UID         uint32 `json:"uid"                     jsonschema:"IMAP UID of the message"`
	UIDValidity uint32 `json:"uid_validity,omitempty"  jsonschema:"UIDVALIDITY of the mailbox; rejected if it does not match"`
}

type ReadMessageResult struct {
	AccountID   string           `json:"account_id"`
	Mailbox     string           `json:"mailbox"`
	UID         uint32           `json:"uid"`
	UIDValidity uint32           `json:"uid_validity"`
	Headers     headersView      `json:"headers"`
	Text        string           `json:"text,omitempty"`
	HTML        string           `json:"html,omitempty"`
	Attachments []attachmentView `json:"attachments,omitempty"`
	Truncated   bool             `json:"truncated,omitempty"`
}
