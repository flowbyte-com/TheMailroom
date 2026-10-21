package imapclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// --- credential command helper process ---
//
// The credential command under test is a child process. To drive its
// output exactly, the tests re-execute this test binary as a helper and
// let it produce the requested bytes. The helper is selected by an
// environment variable so that the command line stays a plain argument
// vector with no shell involved.

const (
	helperEnvMode   = "THEMAILROOM_CREDENTIAL_MODE"
	helperEnvBytes  = "THEMAILROOM_CREDENTIAL_BYTES"
	helperEnvMarker = "THEMAILROOM_CREDENTIAL_MARKER"
)

// Helper modes. Each one produces a different observable behaviour.
const (
	helperModeStdout       = "stdout"        // n bytes on stdout, exit 0
	helperModeStderr       = "stderr"        // n bytes on stderr, exit 0
	helperModeBoth         = "both"          // a password on stdout and n bytes on stderr, at once
	helperModeBigBoth      = "big-both"      // n bytes on stdout while stderr is active
	helperModePartial      = "partial"       // a few bytes on stdout, then exit 3
	helperModeExit         = "exit"          // exit 1 with no output
	helperModeSleep        = "sleep"         // run for a long time
	helperModeSecret       = "secret"        // the server's test password
	helperModeTrailingLF   = "trailing-lf"   // password plus a trailing LF
	helperModeTrailingCRLF = "trailing-crlf" // password plus a trailing CRLF
	helperModeSecretStderr = "secret-stderr" // a secret on stderr, then exit 1
	helperModeEmpty        = "empty"         // no output at all
)

// TestCredentialHelperProcess is not a real test. It is the child process
// used by the credential-command tests; it only does work when the parent
// sets the sentinel environment variable.
func TestCredentialHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnvMode)
	if mode == "" {
		t.Skip("not a credential-helper invocation")
	}

	// Record the invocation so the parent can assert that the command ran
	// exactly once.
	if marker := os.Getenv(helperEnvMarker); marker != "" {
		if f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.WriteString("invoked\n")
			_ = f.Close()
		}
	}

	n, _ := strconv.Atoi(os.Getenv(helperEnvBytes))
	payload := []byte(strings.Repeat("S", n))

	switch mode {
	case helperModeStdout:
		writeAll(os.Stdout, payload)
	case helperModeStderr:
		writeAll(os.Stderr, payload)
	case helperModeBoth:
		// Both pipes are written concurrently, so neither can fill up
		// while the other is still being produced. The stdout stays
		// within the limit so that the password is still extractable.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); writeAll(os.Stdout, []byte("password")) }()
		go func() { defer wg.Done(); writeAll(os.Stderr, payload) }()
		wg.Wait()
	case helperModeBigBoth:
		// Oversized stdout while stderr stays active.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); writeAll(os.Stdout, payload) }()
		go func() { defer wg.Done(); writeAll(os.Stderr, []byte("stderr noise\n")) }()
		wg.Wait()
	case helperModePartial:
		writeAll(os.Stdout, payload[:min(len(payload), 32)])
		os.Exit(3)
	case helperModeExit:
		os.Exit(1)
	case helperModeSleep:
		// Far longer than any test timeout or cancellation, so the
		// command can only end because it was killed. Bounded so that a
		// failure to kill it does not hang the test indefinitely.
		time.Sleep(60 * time.Second)
	case helperModeSecret:
		writeAll(os.Stdout, []byte("password"))
	case helperModeTrailingLF:
		writeAll(os.Stdout, []byte("password\n"))
	case helperModeTrailingCRLF:
		writeAll(os.Stdout, []byte("password\r\n"))
	case helperModeSecretStderr:
		writeAll(os.Stderr, []byte("fatal: could not read secret hunter2-do-not-leak\n"))
		os.Exit(1)
	case helperModeEmpty:
		// No output at all.
	default:
		os.Exit(9)
	}
	os.Exit(0)
}

// writeAll writes every byte, tolerating short writes.
func writeAll(w *os.File, b []byte) {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return
		}
		b = b[n:]
	}
}

// credentialCommand returns a password_command that re-executes this test
// binary as the helper process in the given mode.
func credentialCommand(t *testing.T, mode string, n int) []string {
	t.Helper()
	t.Setenv(helperEnvMode, mode)
	t.Setenv(helperEnvBytes, strconv.Itoa(n))
	return []string{os.Args[0], "-test.run=^TestCredentialHelperProcess$"}
}

// credentialCommandWithMarker is credentialCommand plus a file that records
// every invocation of the command.
func credentialCommandWithMarker(t *testing.T, mode string, n int, marker string) []string {
	t.Helper()
	t.Setenv(helperEnvMarker, marker)
	return credentialCommand(t, mode, n)
}

// invocationCount returns the number of recorded helper invocations.
func invocationCount(t *testing.T, marker string) int {
	t.Helper()
	b, err := os.ReadFile(marker)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read marker: %v", err)
	}
	return strings.Count(string(b), "invoked")
}

// --- tests ---

func TestCredentialCommandSuccess(t *testing.T) {
	got, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeSecret, 0),
	})
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "password" {
		t.Errorf("password = %q, want %q", got, "password")
	}
}

func TestCredentialCommandEmptyOutputRejected(t *testing.T) {
	_, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeEmpty, 0),
	})
	if err == nil {
		t.Fatal("expected an error for empty output")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want it to mention empty output", err)
	}
}

// TestCredentialCommandExactMaximumAccepted checks the boundary: exactly
// maxCredentialOutput bytes are accepted.
func TestCredentialCommandExactMaximumAccepted(t *testing.T) {
	got, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeStdout, maxCredentialOutput),
	})
	if err != nil {
		t.Fatalf("resolvePassword at the exact limit: %v", err)
	}
	if len(got) != maxCredentialOutput {
		t.Errorf("len(password) = %d, want %d", len(got), maxCredentialOutput)
	}
}

// TestCredentialCommandMaximumPlusOneRejected checks the other side of the
// boundary: one byte over the limit is rejected.
func TestCredentialCommandMaximumPlusOneRejected(t *testing.T) {
	_, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeStdout, maxCredentialOutput+1),
	})
	if err == nil {
		t.Fatal("expected an error for output one byte over the limit")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v, want it to mention the exceeded limit", err)
	}
}

// TestBoundedBufferDiscardsBeyondLimit is the unit-level guarantee that
// retained memory does not grow with the amount of output.
func TestBoundedBufferDiscardsBeyondLimit(t *testing.T) {
	var b boundedBuffer
	b.limit = 8

	if n, err := b.Write([]byte("12345678")); n != 8 || err != nil {
		t.Fatalf("Write(exact limit) = %d, %v; want 8, nil", n, err)
	}
	if b.overflow {
		t.Error("overflow set after writing exactly the limit")
	}
	if len(b.Bytes()) != 8 {
		t.Errorf("retained %d bytes, want 8", len(b.Bytes()))
	}

	// One byte over the limit must be reported as an overflow and must
	// not be retained, but the write must still succeed so that the
	// child process is never blocked by a full pipe.
	if n, err := b.Write([]byte("9")); n != 1 || err != nil {
		t.Fatalf("Write(over limit) = %d, %v; want 1, nil", n, err)
	}
	if !b.overflow {
		t.Error("overflow not set after writing one byte over the limit")
	}
	if len(b.Bytes()) != 8 {
		t.Errorf("retained %d bytes, want 8", len(b.Bytes()))
	}

	// A single huge write must not be retained either.
	var c boundedBuffer
	c.limit = 8
	huge := make([]byte, 4*1024*1024)
	if n, err := c.Write(huge); n != len(huge) || err != nil {
		t.Fatalf("Write(huge) = %d, %v; want %d, nil", n, err, len(huge))
	}
	if len(c.Bytes()) > 8 {
		t.Errorf("retained %d bytes from a %d-byte write, want at most 8", len(c.Bytes()), len(huge))
	}
	if !c.overflow {
		t.Error("overflow not set after a huge write")
	}
}

// TestCredentialCommandOutputAllocationIsNotProportional checks that the
// memory used to consume command output does not scale with the amount of
// output. It compares the allocation for a small output against a much
// larger one; the difference must be far smaller than the extra output.
func TestCredentialCommandOutputAllocationIsNotProportional(t *testing.T) {
	const small = 1 << 20  // 1 MiB
	const large = 32 << 20 // 32 MiB

	measure := func(n int) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := resolvePassword(context.Background(), config.IMAPConfig{
			PasswordCommand: credentialCommand(t, helperModeStdout, n),
		})
		if err == nil {
			t.Fatalf("expected an error for %d bytes of output", n)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	smallAlloc := measure(small)
	largeAlloc := measure(large)

	// The allowance is deliberately generous: the implementation must not
	// retain the output, so the extra 31 MiB of output must not cost
	// anything close to 31 MiB of allocation.
	const allowance = 8 << 20 // 8 MiB
	if delta := int64(largeAlloc) - int64(smallAlloc); delta > allowance {
		t.Errorf("allocating for %d bytes cost %d more bytes than for %d bytes; "+
			"want <= %d (allocation must not scale with output)",
			large, delta, small, allowance)
	}
}

// TestCredentialCommandLargeStderrIsDiscarded checks that a very large
// stderr neither reaches the error nor is retained.
func TestCredentialCommandLargeStderrIsDiscarded(t *testing.T) {
	const large = 32 << 20 // 32 MiB

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeStderr, large),
	})
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("expected an error: stdout was empty")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want it to mention empty output", err)
	}
	// The stderr text must not appear in the error at all.
	if strings.Contains(err.Error(), "SSSS") {
		t.Error("the error contains stderr output")
	}
	const allowance = 8 << 20 // 8 MiB
	if used := after.TotalAlloc - before.TotalAlloc; used > allowance {
		t.Errorf("allocating %d bytes of stderr used %d bytes; want <= %d "+
			"(stderr must be discarded, not buffered)", large, used, allowance)
	}
}

// TestCredentialCommandBothStreamsActive checks that both streams are
// consumed concurrently: the command writes the password to stdout and a
// large amount of noise to stderr at the same time, and the password is
// still extracted.
func TestCredentialCommandBothStreamsActive(t *testing.T) {
	const noise = 1 << 20 // 1 MiB on stderr

	got, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeBoth, noise),
	})
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "password" {
		t.Errorf("password = %q, want %q", got, "password")
	}
}

// TestCredentialCommandNonZeroExit checks that a failing command is
// reported with its exit status and without any command output.
func TestCredentialCommandNonZeroExit(t *testing.T) {
	_, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeExit, 0),
	})
	if err == nil {
		t.Fatal("expected an error from a failing command")
	}
	if !strings.Contains(err.Error(), "password_command failed") {
		t.Errorf("err = %v, want it to mention password_command failed", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("err = %v, want it to carry the exit status", err)
	}
}

// TestCredentialCommandTimeout checks that a command that never finishes is
// bounded and classified as a timeout.
func TestCredentialCommandTimeout(t *testing.T) {
	start := time.Now()
	_, err := resolvePasswordWithin(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeSleep, 0),
	}, 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error for a command that never finishes")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want it to report a timeout", err)
	}
	// The command must be bounded by the timeout plus the pipe wait
	// allowance, not by the helper's own sleep.
	if elapsed > 30*time.Second {
		t.Errorf("resolvePassword took %v; the timeout did not bound the command", elapsed)
	}
}

// TestCredentialCommandCancellation checks that cancelling the caller's
// context stops the command and is reported as a cancellation.
func TestCredentialCommandCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := credentialCommand(t, helperModeSleep, 0)

	// Cancel once the helper has certainly started.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := resolvePassword(ctx, config.IMAPConfig{PasswordCommand: cmd})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error for a cancelled command")
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Errorf("err = %v, want it to report a cancellation", err)
	}
	if elapsed > 30*time.Second {
		t.Errorf("resolvePassword took %v; the cancellation did not bound the command", elapsed)
	}
}

// TestCredentialCommandPartialOutputBeforeFailure checks that output
// produced before a failure is discarded and does not reach the error.
func TestCredentialCommandPartialOutputBeforeFailure(t *testing.T) {
	_, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModePartial, 1024),
	})
	if err == nil {
		t.Fatal("expected an error for a command that failed after partial output")
	}
	if !strings.Contains(err.Error(), "password_command failed") {
		t.Errorf("err = %v, want it to mention password_command failed", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("err = %v, want it to carry the exit status", err)
	}
	if strings.Contains(err.Error(), "SSSS") {
		t.Error("the error contains the partial stdout of the failed command")
	}
}

// TestCredentialCommandOversizedStdoutWithActiveStderr checks that an
// oversized stdout is rejected while stderr is being produced, without
// deadlocking.
func TestCredentialCommandOversizedStdoutWithActiveStderr(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := resolvePassword(context.Background(), config.IMAPConfig{
			PasswordCommand: credentialCommand(t, helperModeBigBoth, maxCredentialOutput*4),
		})
		if err == nil {
			t.Error("expected an error for oversized output")
			return
		}
		if !strings.Contains(err.Error(), "exceeded") {
			t.Errorf("err = %v, want it to mention the exceeded limit", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("resolvePassword did not return; the command deadlocked")
	}
}

// TestCredentialCommandTrailingNewlines checks that the trailing newline
// handling of the command output is unchanged.
func TestCredentialCommandTrailingNewlines(t *testing.T) {
	for _, mode := range []string{helperModeTrailingLF, helperModeTrailingCRLF} {
		t.Run(mode, func(t *testing.T) {
			got, err := resolvePassword(context.Background(), config.IMAPConfig{
				PasswordCommand: credentialCommand(t, mode, 0),
			})
			if err != nil {
				t.Fatalf("resolvePassword: %v", err)
			}
			if got != "password" {
				t.Errorf("password = %q, want %q", got, "password")
			}
		})
	}
}

// TestCredentialCommandNoSecretInError checks that neither the command's
// stdout nor its stderr can reach the returned error.
func TestCredentialCommandNoSecretInError(t *testing.T) {
	_, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommand(t, helperModeSecretStderr, 0),
	})
	if err == nil {
		t.Fatal("expected an error from a failing command")
	}
	msg := err.Error()
	for _, forbidden := range []string{"hunter2-do-not-leak", "could not read secret", "stderr"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("error contains %q: %v", forbidden, msg)
		}
	}
	// The failure must still be diagnosable.
	if !strings.Contains(msg, "password_command failed") {
		t.Errorf("err = %v, want it to mention password_command failed", err)
	}
	if !strings.Contains(msg, "exit status 1") {
		t.Errorf("err = %v, want it to carry the exit status", err)
	}
}

// TestCredentialCommandInvokedOnce checks that a single credential
// operation runs the command exactly once, with no silent retry.
func TestCredentialCommandInvokedOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invocations")
	_, err := resolvePassword(context.Background(), config.IMAPConfig{
		PasswordCommand: credentialCommandWithMarker(t, helperModeSecret, 0, marker),
	})
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if n := invocationCount(t, marker); n != 1 {
		t.Errorf("command invoked %d times, want 1", n)
	}
}

// TestCredentialCommandDirectPasswordDoesNotRunCommand checks that a
// directly configured password is returned unchanged and that no command is
// executed.
func TestCredentialCommandDirectPasswordDoesNotRunCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invocations")
	t.Setenv(helperEnvMarker, marker)
	t.Setenv(helperEnvMode, helperModeSecret)

	got, err := resolvePassword(context.Background(), config.IMAPConfig{
		Password: "direct-password",
	})
	if err != nil {
		t.Fatalf("resolvePassword: %v", err)
	}
	if got != "direct-password" {
		t.Errorf("password = %q, want %q", got, "direct-password")
	}
	if n := invocationCount(t, marker); n != 0 {
		t.Errorf("command invoked %d times, want 0", n)
	}
}

// TestCredentialCommandOnAuthenticationPath is the regression test for the
// IMAP authentication path: the credential command supplies the password
// for a real IMAP login, and it is run exactly once.
func TestCredentialCommandOnAuthenticationPath(t *testing.T) {
	addr, cleanup := startCustomServer(t, nil)
	defer cleanup()

	host, port, _ := net.SplitHostPort(addr)
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)

	marker := filepath.Join(t.TempDir(), "invocations")
	cfg := config.AccountConfig{
		ID: "x", Email: "x@y", Backend: "imap",
		IMAP: config.IMAPConfig{
			Host:            host,
			Port:            p,
			Username:        "username",
			PasswordCommand: credentialCommandWithMarker(t, helperModeSecret, 0, marker),
			TLS:             true,
		},
		Limits: config.LimitsConfig{
			MaxMessageBytes:         1 << 20,
			MaxAttachmentBytes:      1 << 20,
			MaxSearchResults:        10,
			MaxBatchOperations:      10,
			MaxMIMENesting:          16,
			MaxMIMEParts:            100,
			MaxTotalAttachmentBytes: 1 << 20,
			MaxAttachments:          10,
			MaxRecipients:           10,
		},
	}
	be, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	if _, err := be.Mailboxes(context.Background(), "x"); err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	if n := invocationCount(t, marker); n != 1 {
		t.Errorf("credential command invoked %d times, want 1", n)
	}
}
