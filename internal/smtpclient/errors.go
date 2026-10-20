package smtpclient

import (
	"errors"
	"fmt"
)

// Public sentinel errors. The tools layer maps these onto stable MCP error
// codes. Error text is intentionally generic: SMTP server responses, command
// output, credentials and recipient lists are never included.
var (
	// ErrInvalidRequest is returned before any network connection when the
	// sender request is malformed or contains an unsupported envelope
	// address.
	ErrInvalidRequest = errors.New("smtpclient: invalid request")

	// ErrConnectionFailed is returned when a connection cannot be
	// established or a pre-DATA SMTP command fails for a non-timeout reason.
	ErrConnectionFailed = errors.New("smtpclient: connection failed")

	// ErrTLSFailed is returned when verified TLS cannot be established or
	// the server does not support the required STARTTLS negotiation.
	ErrTLSFailed = errors.New("smtpclient: TLS failed")

	// ErrAuthFailed is returned when credentials cannot be resolved, the
	// server does not advertise a supported authentication mechanism, or
	// authentication is rejected.
	ErrAuthFailed = errors.New("smtpclient: authentication failed")

	// ErrSenderRejected is returned when the server rejects MAIL FROM.
	ErrSenderRejected = errors.New("smtpclient: sender rejected")

	// ErrRecipientRejected is returned when the server rejects any RCPT TO.
	// No DATA command follows a rejected recipient.
	ErrRecipientRejected = errors.New("smtpclient: recipient rejected")

	// ErrMessageRejected is returned when the server explicitly rejects the
	// DATA command or returns an explicit 4xx/5xx final response.
	ErrMessageRejected = errors.New("smtpclient: message rejected")

	// ErrTimeout is returned for a timeout or cancellation before message
	// data transmission begins. After DATA begins, the failure is reported
	// as an AmbiguousError instead.
	ErrTimeout = errors.New("smtpclient: submission timed out or was cancelled")
)

// AmbiguousError reports that message-data transmission began, but the
// client did not obtain a definitive final SMTP response. The message may or
// may not have been accepted by the server. Callers must not automatically
// retry it.
type AmbiguousError struct {
	// MessageID is the Message-ID of the prepared message whose submission
	// outcome is uncertain. It is preserved so the caller can report it.
	MessageID string
}

func (e *AmbiguousError) Error() string {
	if e.MessageID == "" {
		return "smtpclient: ambiguous submission: message-data transmission began but final server acceptance was not confirmed; may_have_been_sent=true"
	}
	return fmt.Sprintf("smtpclient: ambiguous submission for Message-ID <%s>: message-data transmission began but final server acceptance was not confirmed; may_have_been_sent=true", e.MessageID)
}

// Is allows errors.Is(err, ErrAmbiguousSubmission) and keeps the error
// class discoverable without embedding the server response.
func (e *AmbiguousError) Is(target error) bool {
	return target == ErrAmbiguousSubmission
}

// ErrAmbiguousSubmission is the sentinel matched by AmbiguousError.
var ErrAmbiguousSubmission = errors.New("smtpclient: ambiguous submission")

func invalidRequest(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, reason)
}
