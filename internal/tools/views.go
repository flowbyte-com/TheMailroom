package tools

import (
	"time"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// accountView is the redacted MCP-visible shape of an account. It is a
// distinct type from accounts.View so the wire format is decoupled from
// the runtime representation; if we ever need to add a field that must
// not be exposed, we cannot accidentally leak it through MCP.
type accountView struct {
	ID                  string     `json:"id"`
	Email               string     `json:"email"`
	Provider            string     `json:"provider,omitempty"`
	Permissions         []string   `json:"permissions"`
	Limits              limitsView `json:"limits"`
	AttachmentSendPaths []string   `json:"attachment_send_paths"`
}

type limitsView struct {
	MaxMessageBytes    int `json:"max_message_bytes"`
	MaxAttachments     int `json:"max_attachments"`
	MaxSearchResults   int `json:"max_search_results"`
	MaxBatchOperations int `json:"max_batch_operations"`
}

func accountViewFrom(a *accounts.Account) accountView {
	perms := a.Permissions.List()
	permStrs := make([]string, len(perms))
	for i, c := range perms {
		permStrs[i] = string(c)
	}
	paths := a.AttachmentSendDirs
	if paths == nil {
		paths = []string{}
	}
	return accountView{
		ID:                  a.ID,
		Email:               a.Email,
		Provider:            a.Provider,
		Permissions:         permStrs,
		Limits:              limitsFrom(a.Limits),
		AttachmentSendPaths: paths,
	}
}

func limitsFrom(l config.LimitsConfig) limitsView {
	return limitsView{
		MaxMessageBytes:    l.MaxMessageBytes,
		MaxAttachments:     l.MaxAttachments,
		MaxSearchResults:   l.MaxSearchResults,
		MaxBatchOperations: l.MaxBatchOperations,
	}
}

// mailboxView is the wire shape of a mailbox. It deliberately omits
// IMAP-only fields we do not yet populate.
type mailboxView struct {
	Name        string   `json:"name"`
	Attributes  []string `json:"attributes,omitempty"`
	Delimiter   string   `json:"delimiter,omitempty"`
	UIDValidity uint32   `json:"uid_validity,omitempty"`
}

func mailboxViewFrom(m mailbox.Mailbox) mailboxView {
	attrs := m.Attributes
	if attrs == nil {
		attrs = []string{}
	}
	return mailboxView{
		Name:        m.Name,
		Attributes:  attrs,
		Delimiter:   m.Delimiter,
		UIDValidity: m.UIDValidity,
	}
}

// messageSummaryV is the wire shape of a message summary.
type messageSummaryV struct {
	UID           uint32    `json:"uid"`
	Flags         []string  `json:"flags,omitempty"`
	InternalDate  time.Time `json:"internal_date"`
	Size          uint32    `json:"size"`
	From          string    `json:"from,omitempty"`
	To            []string  `json:"to,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	HasAttachment bool      `json:"has_attachments"`
}

func messageSummaryFrom(m mailbox.MessageSummary) messageSummaryV {
	flags := m.Flags
	if flags == nil {
		flags = []string{}
	}
	to := m.To
	if to == nil {
		to = []string{}
	}
	return messageSummaryV{
		UID:           m.UID,
		Flags:         flags,
		InternalDate:  m.InternalDate,
		Size:          m.Size,
		From:          m.From,
		To:            to,
		Subject:       m.Subject,
		HasAttachment: m.HasAttachment,
	}
}

// headersView is the wire shape of message headers.
type headersView struct {
	MessageID  string    `json:"message_id,omitempty"`
	From       string    `json:"from,omitempty"`
	To         []string  `json:"to,omitempty"`
	Cc         []string  `json:"cc,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	Date       time.Time `json:"date"`
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	References []string  `json:"references,omitempty"`
}

func headersFrom(h mailbox.MessageHeaders) headersView {
	to := h.To
	if to == nil {
		to = []string{}
	}
	cc := h.Cc
	if cc == nil {
		cc = []string{}
	}
	refs := h.References
	if refs == nil {
		refs = []string{}
	}
	return headersView{
		MessageID:  h.MessageID,
		From:       h.From,
		To:         to,
		Cc:         cc,
		Subject:    h.Subject,
		Date:       h.Date,
		InReplyTo:  h.InReplyTo,
		References: refs,
	}
}

// attachmentView is the wire shape of an attachment.
type attachmentView struct {
	Index     int    `json:"index"`
	Filename  string `json:"filename,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	Size      int64  `json:"size"`
	ContentID string `json:"content_id,omitempty"`
}

func attachmentFrom(a mailbox.Attachment) attachmentView {
	return attachmentView{
		Index:     a.Index,
		Filename:  a.Filename,
		MimeType:  a.MimeType,
		Size:      a.Size,
		ContentID: a.ContentID,
	}
}
