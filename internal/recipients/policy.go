// Package recipients implements the outbound recipient policy.
//
// The policy is deliberately small: recipients are parsed as RFC 5322
// addresses, canonicalised for exact comparison, checked against an optional
// allow-list, and counted against a hard limit. There are no wildcard or
// pattern-matching rules.
package recipients

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
)

var (
	// ErrNoRecipients is returned when a message has no envelope recipients.
	ErrNoRecipients = errors.New("recipients: no recipients")

	// ErrInvalidAddress is returned for an empty, malformed or control-
	// character-containing address.
	ErrInvalidAddress = errors.New("recipients: invalid address")

	// ErrTooManyRecipients is returned when the unique recipient count
	// exceeds the configured limit.
	ErrTooManyRecipients = errors.New("recipients: too many recipients")

	// ErrRecipientNotAllowed is returned for a recipient outside the
	// configured allow-list.
	ErrRecipientNotAllowed = errors.New("recipients: recipient not allowed")
)

// Policy is an immutable recipient policy. An empty allow-list permits any
// recipient; a non-empty allow-list permits only exact canonical addresses.
type Policy struct {
	allowed map[string]struct{}
	max     int
}

// NewPolicy builds a policy from an optional allow-list and a positive
// recipient limit. Allow-list entries are canonicalised and duplicate
// addresses are collapsed.
func NewPolicy(allowed []string, max int) (Policy, error) {
	if max <= 0 {
		return Policy{}, fmt.Errorf("recipients: max recipients must be positive")
	}
	p := Policy{max: max}
	if len(allowed) == 0 {
		return p, nil
	}
	p.allowed = make(map[string]struct{}, len(allowed))
	for i, raw := range allowed {
		addr, err := Canonical(raw)
		if err != nil {
			return Policy{}, fmt.Errorf("allowed_recipients[%d]: %w", i, err)
		}
		p.allowed[addr] = struct{}{}
	}
	return p, nil
}

// Max returns the configured unique-recipient limit.
func (p Policy) Max() int { return p.max }

// Allows reports whether addr is permitted by the allow-list. addr must be a
// canonical address returned by Canonical. An empty allow-list permits all
// canonical addresses.
func (p Policy) Allows(addr string) bool {
	if p.allowed == nil {
		return true
	}
	_, ok := p.allowed[addr]
	return ok
}

// Validate canonicalises and validates all recipients in to, cc and bcc.
//
// Duplicate addresses across the three fields are collapsed before the count
// is checked. The returned slice contains canonical envelope addresses in
// first-seen order. The allow-list and limit are applied to the combined set.
func (p Policy) Validate(to, cc, bcc []string) ([]string, error) {
	fields := []struct {
		name   string
		values []string
	}{
		{name: "to", values: to},
		{name: "cc", values: cc},
		{name: "bcc", values: bcc},
	}

	seen := make(map[string]struct{}, len(to)+len(cc)+len(bcc))
	out := make([]string, 0, len(to)+len(cc)+len(bcc))
	for _, field := range fields {
		for _, raw := range field.values {
			addr, err := Canonical(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", field.name, err)
			}
			if _, duplicate := seen[addr]; duplicate {
				continue
			}
			seen[addr] = struct{}{}
			if !p.Allows(addr) {
				return nil, fmt.Errorf("%w: %s", ErrRecipientNotAllowed, field.name)
			}
			out = append(out, addr)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRecipients
	}
	if len(out) > p.max {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooManyRecipients, len(out), p.max)
	}
	return out, nil
}

// Canonical parses raw as one RFC 5322 address and returns its canonical
// addr-spec.
//
// The canonical form is the parsed address with surrounding whitespace
// removed and the entire addr-spec lowercased. Matching is therefore
// case-insensitive and exact: no substring, prefix or wildcard matching is
// performed. Display names are accepted but are not part of the canonical
// value. Control characters are rejected before parsing so addresses cannot
// carry header or log injection sequences.
func Canonical(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidAddress)
	}
	if HasControl(raw) {
		return "", fmt.Errorf("%w: control character", ErrInvalidAddress)
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		// Do not include the parser error: it may quote the original input.
		return "", fmt.Errorf("%w: malformed", ErrInvalidAddress)
	}
	if addr.Address == "" || HasControl(addr.Address) {
		return "", fmt.Errorf("%w: empty or invalid addr-spec", ErrInvalidAddress)
	}
	if strings.ContainsAny(addr.Address, " \t\r\n") {
		return "", fmt.Errorf("%w: whitespace in addr-spec", ErrInvalidAddress)
	}
	return strings.ToLower(addr.Address), nil
}

// HasControl reports whether s contains an ASCII or Unicode control
// character. Tabs, line breaks, NULs and other controls are rejected because
// they have no legitimate place in an envelope address and are a common
// header-injection vector.
func HasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
