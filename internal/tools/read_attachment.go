package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

var readAttachmentTool = &mcp.Tool{
	Name:        "read_attachment",
	Description: "Read one explicitly requested attachment from a message. Returns transfer-decoded content as padded base64 together with its read_message part path, filename and MIME type. Text attachments may be charset-normalised to UTF-8 rather than byte-identical to the original file. Requires the 'read' capability.",
}

type ReadAttachmentArgs struct {
	AccountID   string `json:"account_id"   jsonschema:"the account to read from"`
	Mailbox     string `json:"mailbox"      jsonschema:"the mailbox the message lives in"`
	UID         uint32 `json:"uid"          jsonschema:"IMAP UID of the message"`
	UIDValidity uint32 `json:"uid_validity" jsonschema:"UIDVALIDITY of the mailbox as observed in a previous list/search result; required to prevent recycled UIDs from being silently targeted"`
	Part        string `json:"part"         jsonschema:"attachment part path exactly as returned by read_message, for example 1 or 2.1"`
}

type ReadAttachmentResult struct {
	AccountID     string   `json:"account_id"`
	Mailbox       string   `json:"mailbox"`
	UID           uint32   `json:"uid"`
	UIDValidity   uint32   `json:"uid_validity"`
	Part          string   `json:"part"`
	Filename      string   `json:"filename,omitempty"`
	MimeType      string   `json:"mime_type,omitempty"`
	Size          int64    `json:"size"`
	ContentBase64 string   `json:"content_base64"`
	Warnings      []string `json:"warnings,omitempty"`
}
