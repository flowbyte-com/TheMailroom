// Package permissions defines the capability model for TheMailroom.
//
// Every operation is gated by a Capability. A tool call requests a capability
// on behalf of an account and is rejected with ErrPermissionDenied if the
// account does not grant it. The check is enforced inside TheMailroom; the
// MCP client is not trusted.
package permissions

import (
	"fmt"
	"sort"
	"strings"
)

// Capability names a class of operation.
type Capability string

// All defined capabilities. Adding a new value here must be matched by adding
// its name to AllCapabilities.
const (
	// CapRead grants listing mailboxes, listing and reading messages and
	// attachments, and searching.
	CapRead Capability = "read"

	// CapSend grants composing and sending new messages, replies and
	// forwards.
	CapSend Capability = "send"

	// CapModify grants flagging, moving and archiving messages without
	// deletion.
	CapModify Capability = "modify"

	// CapDelete grants deletion (move to Trash or expunge).
	CapDelete Capability = "delete"

	// CapAttachmentSend is required in addition to CapSend whenever a
	// message includes one or more attachments sourced from the local
	// filesystem.
	CapAttachmentSend Capability = "attachment_send"
)

// AllCapabilities lists every defined capability, in declaration order.
var AllCapabilities = []Capability{
	CapRead,
	CapSend,
	CapModify,
	CapDelete,
	CapAttachmentSend,
}

// Set is the set of capabilities granted to an account.
type Set struct {
	caps map[Capability]struct{}
}

// NewSet returns a Set containing the given capabilities.
func NewSet(caps ...Capability) Set {
	s := Set{caps: make(map[Capability]struct{}, len(caps))}
	for _, c := range caps {
		s.caps[c] = struct{}{}
	}
	return s
}

// NewSetFromStrings parses capability names. Unknown names are returned
// separately so the caller can surface a config error rather than silently
// ignoring them.
func NewSetFromStrings(names []string) (Set, []string) {
	s := Set{caps: make(map[Capability]struct{}, len(names))}
	known := make(map[Capability]struct{}, len(AllCapabilities))
	for _, c := range AllCapabilities {
		known[c] = struct{}{}
	}
	var unknown []string
	for _, n := range names {
		c := Capability(strings.ToLower(strings.TrimSpace(n)))
		if _, ok := known[c]; ok {
			s.caps[c] = struct{}{}
		} else {
			unknown = append(unknown, n)
		}
	}
	return s, unknown
}

// Has reports whether c is granted.
func (s Set) Has(c Capability) bool {
	if s.caps == nil {
		return false
	}
	_, ok := s.caps[c]
	return ok
}

// List returns sorted capability names.
func (s Set) List() []Capability {
	out := make([]Capability, 0, len(s.caps))
	for c := range s.caps {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// String returns a comma-separated list of granted capabilities.
func (s Set) String() string {
	cs := s.List()
	strs := make([]string, len(cs))
	for i, c := range cs {
		strs[i] = string(c)
	}
	return strings.Join(strs, ",")
}

// IsEmpty reports whether the set has no capabilities.
func (s Set) IsEmpty() bool { return len(s.caps) == 0 }

// ErrPermissionDenied is returned when a capability check fails. It carries
// no secret material and is safe to return to MCP clients.
type ErrPermissionDenied struct {
	Account    string
	Capability Capability
}

func (e *ErrPermissionDenied) Error() string {
	return fmt.Sprintf("account %q lacks capability %q", e.Account, e.Capability)
}

// Check returns *ErrPermissionDenied if c is not granted to the account
// identified by name. The account name is taken only for error messages.
func Check(name string, granted Set, c Capability) error {
	if granted.Has(c) {
		return nil
	}
	return &ErrPermissionDenied{Account: name, Capability: c}
}
