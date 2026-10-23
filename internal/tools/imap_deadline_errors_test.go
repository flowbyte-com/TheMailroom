package tools

// Regression tests for the IMAP deadline and cancellation error codes.
//
// A cancelled or timed-out operation must be distinguishable from an
// internal error: the agent can act on "the request was withdrawn" and
// on "the operation timed out" in ways it cannot act on "internal
// error".

import (
	"context"
	"fmt"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

func TestMapErrorIMAPDeadlineClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{
			name: "operation deadline",
			err:  fmt.Errorf("%w: %w: the operation deadline was exceeded", mailbox.ErrTimeout, context.DeadlineExceeded),
			code: codeImapTimeout,
		},
		{
			name: "queue allowance",
			err:  fmt.Errorf("%w: %w: the account was busy for longer than the queue allowance", mailbox.ErrTimeout, context.DeadlineExceeded),
			code: codeImapTimeout,
		},
		{
			name: "caller cancellation",
			err:  fmt.Errorf("%w: %w", mailbox.ErrCanceled, context.Canceled),
			code: codeCanceled,
		},
		{
			name: "backend closed",
			err:  mailbox.ErrBackendClosed,
			code: codeBackendError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapError(tc.err)
			if got.Code != tc.code {
				t.Errorf("mapError(%v).Code = %q, want %q", tc.err, got.Code, tc.code)
			}
			if got.Message == "" {
				t.Error("the mapped error has no message")
			}
		})
	}
}

// TestMapErrorDeadlineIsNotAnInternalError checks that a deadline or a
// cancellation never falls through to the internal-error code.
func TestMapErrorDeadlineIsNotAnInternalError(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: %w", mailbox.ErrTimeout, context.DeadlineExceeded),
		fmt.Errorf("%w: %w", mailbox.ErrCanceled, context.Canceled),
	} {
		if got := mapError(err); got.Code == codeInternalError {
			t.Errorf("mapError(%v).Code = %q, want a specific code", err, got.Code)
		}
	}
}
