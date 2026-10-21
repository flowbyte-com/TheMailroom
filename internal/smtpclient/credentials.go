package smtpclient

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// maxCredentialOutput bounds the password_command stdout. The command is
// operator-configured, but its output is still untrusted input and must not
// be allowed to grow memory without bound.
const maxCredentialOutput = 8 * 1024

var errCredentialSource = errors.New("smtpclient: credential source failed")

// resolveCredential returns the SMTP password from the configured source.
// password_command is executed directly, never through a shell, with a
// bounded timeout and bounded stdout. Its output is not included in errors.
func resolveCredential(ctx context.Context, cfg config.SMTPConfig, timeout time.Duration) (string, error) {
	if cfg.Password != "" {
		return cfg.Password, nil
	}
	if len(cfg.PasswordCommand) == 0 {
		return "", errCredentialSource
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, cfg.PasswordCommand[0], cfg.PasswordCommand[1:]...)
	var stdout boundedBuffer
	stdout.limit = maxCredentialOutput
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	cmd.Stdin = nil
	// A child process can outlive cancellation while holding the stdout
	// pipe open. Bound the wait for pipe I/O as well as the process run.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if cmdCtx.Err() != nil {
		// Preserve the context outcome so the caller can classify it as a
		// timeout/cancellation without exposing command output.
		return "", cmdCtx.Err()
	}
	if err != nil {
		return "", errCredentialSource
	}
	if stdout.overflow {
		return "", errCredentialSource
	}
	pw := strings.TrimRight(stdout.String(), "\r\n")
	if pw == "" {
		return "", errCredentialSource
	}
	return pw, nil
}

// boundedBuffer stores at most limit bytes and discards the rest while still
// reporting successful writes to the child process. overflow is set if any
// byte was discarded.
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

func (b *boundedBuffer) String() string { return string(b.buf) }
