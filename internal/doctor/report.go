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
	// StatusSkip means the check did not run, usually because it needs
	// --network or does not apply to the account.
	StatusSkip Status = "skip"
)

// Check is one diagnostic result.
//
// Detail and Remedy are operator-facing text. They must never contain a
// credential, the arguments of a credential command, or raw protocol
// server output.
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

// Failed reports whether any check failed.
func (r Report) Failed() bool { return r.Count(StatusFail) > 0 }

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
	fmt.Fprintf(&b, "Summary: %d checks, %d ok, %d warning(s), %d failure(s), %d skipped\n",
		len(r.Checks), r.Count(StatusOK), r.Count(StatusWarn), r.Count(StatusFail), r.Count(StatusSkip))

	_, err := io.WriteString(w, b.String())
	return err
}
