// Package tools defines the MCP tool surface for TheMailroom.
//
// The tools expose read-only operations: list_accounts, list_mailboxes,
// list_messages, read_message, read_attachment, and search_messages, plus
// authenticated outbound submission through send_message.
//
// Every tool:
//   - validates input against a JSON Schema derived from the args struct;
//   - performs a server-side capability check before any backend call;
//   - returns a structured result with no credential material;
//   - returns typed errors which are mapped to MCP tool errors.
package tools

import (
	"errors"
	"fmt"

	"github.com/flowbyte-com/themailroom/internal/attachments"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/mailcompose"
	"github.com/flowbyte-com/themailroom/internal/permissions"
	"github.com/flowbyte-com/themailroom/internal/recipients"
	"github.com/flowbyte-com/themailroom/internal/smtpclient"
)

// Tool error codes. These match the codes advertised in docs/TOOLS.md.
const (
	codeInvalidInput       = "InvalidInput"
	codeAccountNotFound    = "AccountNotFound"
	codePermissionDenied   = "PermissionDenied"
	codeBackendError       = "BackendError"
	codeLimitExceeded      = "RateLimited"
	codeUidMismatch        = "UidMismatch"
	codeNotImplemented     = "NotImplemented"
	codeMailboxNotFound    = "MailboxNotFound"
	codeMessageNotFound    = "MessageNotFound"
	codeAttachmentNotFound = "AttachmentNotFound"
	codeInternalError      = "InternalError"
	codeMessageTooLarge    = "MessageTooLarge"
	codeNestingTooDeep     = "NestingTooDeep"
	codeTooManyParts       = "TooManyParts"
	codeTooManyAttachments = "TooManyAttachments"
	codeAttachmentTooLarge = "AttachmentTooLarge"
	codeMalformed          = "Malformed"

	codeRecipientNotAllowed  = "RecipientNotAllowed"
	codeTooManyRecipients    = "TooManyRecipients"
	codeSmtpAuthFailed       = "SmtpAuthFailed"
	codeSmtpTLSFailed        = "SmtpTLSFailed"
	codeSenderRejected       = "SenderRejected"
	codeRecipientRejected    = "RecipientRejected"
	codeSmtpMessageRejected  = "SmtpMessageRejected"
	codeSmtpConnectionFailed = "SmtpConnectionFailed"
	codeSmtpTimeout          = "SmtpTimeout"
	codeSmtpAmbiguousFailure = "SmtpAmbiguousFailure"
)

// TypedError is returned by tool handlers. The MCP layer treats it as a
// tool error (not a protocol error) and surfaces code + message + details.
type TypedError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *TypedError) Error() string { return e.Message }

// typed wraps an internal error into a TypedError. It MUST NOT include
// any secret material in Message or Details.
func typed(code, msg string, details map[string]any) *TypedError {
	return &TypedError{Code: code, Message: msg, Details: details}
}

// mapError converts an internal error into a TypedError using a small
// table. Anything not matched becomes InternalError.
func mapError(err error) *TypedError {
	if err == nil {
		return nil
	}
	var te *TypedError
	if errors.As(err, &te) {
		return te
	}
	var ambiguous *smtpclient.AmbiguousError
	if errors.As(err, &ambiguous) {
		return typed(codeSmtpAmbiguousFailure,
			fmt.Sprintf("SmtpAmbiguousFailure: SMTP submission status is uncertain for Message-ID <%s>; may_have_been_sent=true; the server may have accepted the message and it must not be retried automatically", ambiguous.MessageID),
			map[string]any{
				"message_id":         ambiguous.MessageID,
				"may_have_been_sent": true,
			})
	}
	var pde *permissions.ErrPermissionDenied
	if errors.As(err, &pde) {
		return typed(codePermissionDenied, err.Error(), map[string]any{
			"account_id": pde.Account,
			"capability": string(pde.Capability),
		})
	}
	switch {
	case errors.Is(err, mailbox.ErrAccountNotFound):
		return typed(codeAccountNotFound, "account not found", nil)
	case errors.Is(err, mailbox.ErrMailboxNotFound):
		return typed(codeMailboxNotFound, "mailbox not found", nil)
	case errors.Is(err, mailbox.ErrMessageNotFound):
		return typed(codeMessageNotFound, "message not found", nil)
	case errors.Is(err, mailbox.ErrAttachmentNotFound):
		return typed(codeAttachmentNotFound, "attachment not found", nil)
	case errors.Is(err, mailbox.ErrInvalidPart):
		return typed(codeInvalidInput, "invalid attachment part", nil)
	case errors.Is(err, mailbox.ErrUIDMismatch):
		return typed(codeUidMismatch, "uid/uid_validity mismatch", nil)
	case errors.Is(err, mailbox.ErrLimitExceeded):
		return typed(codeLimitExceeded, "limit exceeded", nil)
	case errors.Is(err, mailbox.ErrListTooLarge):
		// The message carries the counts, the scan budget and the
		// recourse, so the caller can raise the limit instead of
		// retrying unchanged. Narrowing the query with the date
		// filters does not help: they run after the preflight check
		// and after the fetch, so they cannot make an oversized
		// mailbox listable.
		return typed(codeLimitExceeded, err.Error(), nil)
	case errors.Is(err, mailbox.ErrSearchTooLarge):
		// The message carries the budget and the recourse, so the
		// caller can narrow the query or raise the limit instead of
		// retrying unchanged. Narrowing the query does help here: the
		// server evaluates the criteria, so a narrower query matches
		// fewer messages.
		return typed(codeLimitExceeded, err.Error(), nil)
	case errors.Is(err, mailbox.ErrMessageTooLarge):
		return typed(codeMessageTooLarge, "message exceeds size limit", nil)
	case errors.Is(err, mailbox.ErrNestingTooDeep):
		return typed(codeNestingTooDeep, "mime nesting exceeds limit", nil)
	case errors.Is(err, mailbox.ErrTooManyParts):
		return typed(codeTooManyParts, "too many mime parts", nil)
	case errors.Is(err, mailbox.ErrTooManyAttachments):
		return typed(codeTooManyAttachments, "too many attachments", nil)
	case errors.Is(err, mailbox.ErrAttachmentTooLarge):
		return typed(codeAttachmentTooLarge, "attachment exceeds size limit", nil)
	case errors.Is(err, mailbox.ErrTotalAttachmentTooLarge):
		return typed(codeAttachmentTooLarge, "total attachment size exceeds limit", nil)
	case errors.Is(err, mailbox.ErrMalformed):
		return typed(codeMalformed, "malformed message", nil)

	case errors.Is(err, recipients.ErrRecipientNotAllowed):
		return typed(codeRecipientNotAllowed, "recipient is not allowed by this account's policy", nil)
	case errors.Is(err, recipients.ErrTooManyRecipients):
		return typed(codeTooManyRecipients, "too many unique recipients", nil)
	case errors.Is(err, recipients.ErrInvalidAddress), errors.Is(err, recipients.ErrNoRecipients):
		return typed(codeInvalidInput, "invalid recipient list", nil)

	case errors.Is(err, attachments.ErrTooMany):
		return typed(codeTooManyAttachments, "too many attachments", nil)
	case errors.Is(err, attachments.ErrTooLarge), errors.Is(err, attachments.ErrTotalTooLarge):
		return typed(codeAttachmentTooLarge, "attachment exceeds size limit", nil)
	case errors.Is(err, attachments.ErrNotFound):
		return typed(codeAttachmentNotFound, "attachment not found", nil)
	case errors.Is(err, attachments.ErrInvalidPath), errors.Is(err, attachments.ErrAmbiguous), errors.Is(err, attachments.ErrNotRegular):
		return typed(codeInvalidInput, "invalid attachment reference", nil)

	case errors.Is(err, mailcompose.ErrMessageTooLarge):
		return typed(codeMessageTooLarge, "serialized message exceeds size limit", nil)
	case errors.Is(err, mailcompose.ErrTooManyAttachments):
		return typed(codeTooManyAttachments, "too many attachments", nil)
	case errors.Is(err, mailcompose.ErrAttachmentTooLarge), errors.Is(err, mailcompose.ErrTotalAttachmentTooLarge):
		return typed(codeAttachmentTooLarge, "attachment exceeds size limit", nil)
	case errors.Is(err, mailcompose.ErrInvalidMessage), errors.Is(err, mailcompose.ErrInvalidAttachment):
		return typed(codeInvalidInput, "invalid message input", nil)

	case errors.Is(err, smtpclient.ErrInvalidRequest):
		return typed(codeInvalidInput, "invalid submission request", nil)
	case errors.Is(err, smtpclient.ErrTLSFailed):
		return typed(codeSmtpTLSFailed, "verified TLS could not be established", nil)
	case errors.Is(err, smtpclient.ErrAuthFailed):
		return typed(codeSmtpAuthFailed, "SMTP authentication failed", nil)
	case errors.Is(err, smtpclient.ErrSenderRejected):
		return typed(codeSenderRejected, "SMTP server rejected the sender", nil)
	case errors.Is(err, smtpclient.ErrRecipientRejected):
		return typed(codeRecipientRejected, "SMTP server rejected a recipient; no message data was sent", nil)
	case errors.Is(err, smtpclient.ErrMessageRejected):
		return typed(codeSmtpMessageRejected, "SMTP server rejected the message", nil)
	case errors.Is(err, smtpclient.ErrTimeout):
		return typed(codeSmtpTimeout, "SMTP submission timed out or was cancelled before message data was sent", nil)
	case errors.Is(err, smtpclient.ErrConnectionFailed):
		return typed(codeSmtpConnectionFailed, "SMTP connection failed", nil)

	default:
		// Avoid leaking internal details.
		return typed(codeInternalError, "internal error", nil)
	}
}

// invalidInput is a convenience constructor.
func invalidInput(format string, a ...any) *TypedError {
	return typed(codeInvalidInput, fmt.Sprintf(format, a...), nil)
}
