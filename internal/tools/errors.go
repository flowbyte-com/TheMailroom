// Package tools defines the MCP tool surface for TheMailroom.
//
// The tools expose the read-only operations in Phase 1: list_accounts,
// list_mailboxes, list_messages, read_message, and search_messages.
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

	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/permissions"
)

// Tool error codes. These match the codes advertised in docs/TOOLS.md.
const (
	codeInvalidInput     = "InvalidInput"
	codeAccountNotFound  = "AccountNotFound"
	codePermissionDenied = "PermissionDenied"
	codeBackendError     = "BackendError"
	codeLimitExceeded    = "RateLimited"
	codeUidMismatch      = "UidMismatch"
	codeNotImplemented   = "NotImplemented"
	codeMailboxNotFound  = "MailboxNotFound"
	codeMessageNotFound  = "MessageNotFound"
	codeInternalError    = "InternalError"
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
	case errors.Is(err, mailbox.ErrUIDMismatch):
		return typed(codeUidMismatch, "uid/uid_validity mismatch", nil)
	case errors.Is(err, mailbox.ErrLimitExceeded):
		return typed(codeLimitExceeded, "limit exceeded", nil)
	default:
		// Avoid leaking internal details.
		return typed(codeInternalError, "internal error", nil)
	}
}

// invalidInput is a convenience constructor.
func invalidInput(format string, a ...any) *TypedError {
	return typed(codeInvalidInput, fmt.Sprintf(format, a...), nil)
}
