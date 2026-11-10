// Package doctor implements TheMailroom's diagnostic command.
//
// It performs two clearly separated groups of checks. Local checks read the
// configuration, validate it with the same loader the server uses, exercise
// the same registry construction the server performs, and report credential
// sources without revealing them. Network checks are opt-in: they resolve
// names, open TCP connections, verify TLS and authenticate, using the
// production IMAP and SMTP paths wherever those can be reused safely.
//
// The package never prints a credential, never executes a password_command
// during local checks, never disables TLS verification, and never sends a
// message or mutates a mailbox.
package doctor

import (
	"fmt"
	"io"
	"strings"
)

// Status is the outcome of one check.
type Status string

const (
	// StatusOK means the check passed.
	StatusOK Status = "ok"
	// StatusWarn means the check passed but the configuration or the
	// environment deserves attention. A warning never fails the run.
	StatusWarn Status = "warn"
	// StatusFail means the check failed. Any failure fails the run.
	StatusFail Status = "fail"
	// StatusSkip means the check did not run. A skip that merely records
	// "not applicable" does not fail the run; a skip that records a
	// requested diagnostic which could not complete does, and is marked
	// Incomplete.
	StatusSkip Status = "skip"
)

// Check is one diagnostic result.
//
// Detail and Remedy are operator-facing text. They must never contain a
// credential, the arguments of a credential command, or raw untrusted
// document or protocol content.
type Check struct {
	// Name is a stable identifier such as "account/work/imap".
	Name string
	// Status is the outcome.
	Status Status
	// Detail is a one-line explanation.
	Detail string
	// Remedy is a one-line, sanitised suggestion. It is set for warnings
	// and failures.
	Remedy string
	// Incomplete marks a check that did not run because a requested
	// diagnostic was cancelled or timed out. It is displayed as skipped,
	// but it fails the run: a requested diagnostic that did not complete
	// is not a success. A check that is skipped because it does not apply
	// — no networked account, a STARTTLS endpoint with no plaintext TLS
	// probe — leaves this false.
	Incomplete bool
}

// Report is the complete result of a diagnostic run.
type Report struct {
	// ConfigPath is the configuration file the run used.
	ConfigPath string
	// Network reports whether network checks were requested.
	Network bool
	// Checks holds every result in a stable order.
	Checks []Check
}

// Add appends a check to the report.
func (r *Report) Add(c Check) { r.Checks = append(r.Checks, c) }

// Count returns the number of checks with the given status.
func (r Report) Count(s Status) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == s {
			n++
		}
	}
	return n
}

// IncompleteCount returns the number of checks that did not run because the
// requested diagnostic was cancelled or timed out.
func (r Report) IncompleteCount() int {
	n := 0
	for _, c := range r.Checks {
		if c.Incomplete {
			n++
		}
	}
	return n
}

// Failed reports whether the run failed.
//
// A failed check fails the run, and so does an incomplete one: a requested
// diagnostic that was cancelled or timed out has not established that the
// configuration is healthy.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail || c.Incomplete {
			return true
		}
	}
	return false
}

// WriteText renders the report as operator-facing text.
//
// The layout is stable so it can be read in a terminal and diffed between
// runs. Failures and warnings print their remedy on an indented line.
func (r Report) WriteText(w io.Writer) error {
	var b strings.Builder
	b.WriteString("themailroom doctor\n\n")
	fmt.Fprintf(&b, "Config:  %s\n", r.ConfigPath)
	if r.Network {
		b.WriteString("Mode:    local and network diagnostics\n")
	} else {
		b.WriteString("Mode:    local only (pass -network to test connectivity)\n")
	}
	b.WriteString("\n")

	const nameWidth = 34
	for _, c := range r.Checks {
		name := c.Name
		if len(name) > nameWidth {
			name = name[:nameWidth]
		}
		fmt.Fprintf(&b, "  %-5s %-*s %s\n", c.Status, nameWidth, name, c.Detail)
		if c.Remedy != "" && (c.Status == StatusWarn || c.Status == StatusFail) {
			fmt.Fprintf(&b, "        %-*s %s\n", nameWidth, "", c.Remedy)
		}
	}

	b.WriteString("\n")
	if n := r.IncompleteCount(); n > 0 {
		fmt.Fprintf(&b, "Note: %d requested check(s) did not complete; the network phase was cancelled or timed out.\n", n)
	}
	fmt.Fprintf(&b, "Summary: %d checks, %d ok, %d warning(s), %d failure(s), %d skipped (%d incomplete)\n",
		len(r.Checks), r.Count(StatusOK), r.Count(StatusWarn), r.Count(StatusFail), r.Count(StatusSkip), r.IncompleteCount())

	_, err := io.WriteString(w, b.String())
	return err
}
