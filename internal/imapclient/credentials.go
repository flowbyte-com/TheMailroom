package imapclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// maxCredentialOutput bounds the password_command stdout. The command is
// operator-configured, but its output is still untrusted input: it must not
// be allowed to grow memory without bound, and it must not be able to
// block the child process by filling a pipe.
//
// The value matches the SMTP credential limit. A password is a short opaque
// secret, so the limit exists only to bound untrusted output; a separate
// IMAP value would add a knob without adding security.
const maxCredentialOutput = 8 * 1024

// defaultPasswordCommandTimeout caps one credential command run. This is
// the pre-existing bound and is deliberately unchanged.
const defaultPasswordCommandTimeout = 30 * time.Second

// errNoPasswordSource is returned when neither a password nor a command is
// configured. Configuration validation rejects that case before start-up.
var errNoPasswordSource = errors.New("imap: no password source configured")

// errEmptyCredentialOutput is returned when the command succeeded but
// produced no password.
var errEmptyCredentialOutput = errors.New("imap: password_command produced empty output")

// resolvePassword returns the password from the configured source.
//
// A directly configured password is returned unchanged. A password_command
// is executed directly, never through a shell, with bounded stdout, a
// discarded stderr and a bounded wait for inherited pipe handles. Neither
// the command's stdout nor its stderr is ever included in the returned
// error.
//
// ctx bounds the command: it is cancelled together with the IMAP operation
// that needed the credential.
func resolvePassword(ctx context.Context, cfg config.IMAPConfig) (string, error) {
	return resolvePasswordWithin(ctx, cfg, defaultPasswordCommandTimeout)
}

// resolvePasswordWithin is resolvePassword with an explicit timeout. It
// exists so the timeout can be exercised in tests without waiting for the
// production bound.
func resolvePasswordWithin(ctx context.Context, cfg config.IMAPConfig, timeout time.Duration) (string, error) {
	if cfg.Password != "" {
		return cfg.Password, nil
	}
	if len(cfg.PasswordCommand) == 0 {
		return "", errNoPasswordSource
	}
	if timeout <= 0 {
		timeout = defaultPasswordCommandTimeout
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, cfg.PasswordCommand[0], cfg.PasswordCommand[1:]...)
	var stdout boundedBuffer
	stdout.limit = maxCredentialOutput
	cmd.Stdout = &stdout
	// stderr is discarded rather than captured. It is arbitrary
	// operator-supplied text that may embed secrets, and it is not needed
	// to classify the failure. Discarding also keeps the child's stderr
	// pipe drained, so a chatty command cannot block on it.
	cmd.Stderr = io.Discard
	// The command must never be able to block waiting for input.
	cmd.Stdin = nil
	// A child process can outlive cancellation while holding the stdout
	// pipe open. Bound the wait for pipe I/O as well as the process run,
	// so a cancelled or timed-out command cannot wedge the caller.
	cmd.WaitDelay = time.Second

	err := cmd.Run()
	if err != nil {
		switch {
		case errors.Is(cmdCtx.Err(), context.DeadlineExceeded):
			return "", fmt.Errorf("imap: password_command timed out after %s: %w", timeout, context.DeadlineExceeded)
		case errors.Is(cmdCtx.Err(), context.Canceled):
			return "", fmt.Errorf("imap: password_command canceled: %w", context.Canceled)
		default:
			// err is the exec failure, for example "exit status 1". It
			// carries no command output: cmd.Stderr is non-nil, so
			// os/exec does not capture stderr into the ExitError, and
			// the exit status is the only diagnostic retained.
			return "", fmt.Errorf("imap: password_command failed: %w", err)
		}
	}
	if stdout.overflow {
		return "", fmt.Errorf("imap: password_command output exceeded %d bytes", maxCredentialOutput)
	}
	// Trim the trailing newlines the command may have appended, then copy
	// once into an exactly-sized string so the retained buffer does not
	// outlive the password it produced.
	out := string(bytes.TrimRight(stdout.Bytes(), "\r\n"))
	if out == "" {
		return "", errEmptyCredentialOutput
	}
	return out, nil
}

// boundedBuffer stores at most limit bytes and discards the rest while
// still reporting successful writes to the child process, so a command that
// produces more output than the limit is never blocked by a full pipe.
// overflow records that at least one byte was discarded.
type boundedBuffer struct {
	buf      []byte
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.overflow = b.overflow || len(p) > 0
		return len(p), nil
	}
	remaining := b.limit - len(b.buf)
	if remaining > 0 {
		n := len(p)
		if n > remaining {
			n = remaining
		}
		b.buf = append(b.buf, p[:n]...)
	}
	if len(p) > remaining {
		b.overflow = true
	}
	return len(p), nil
}

// Bytes returns the retained bytes. The slice aliases the buffer and must
// not be retained beyond the caller's use.
func (b *boundedBuffer) Bytes() []byte { return b.buf }
