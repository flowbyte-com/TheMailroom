// Package accounts builds the in-process account registry from a validated
// configuration. Each registered account exposes a typed AccountView
// (suitable for MCP tool output) and a mailbox.Backend implementation.
package accounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/imapclient"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	mem "github.com/flowbyte-com/themailroom/internal/mailbox/memory"
	"github.com/flowbyte-com/themailroom/internal/permissions"
)

// ErrIMAPNotImplemented is kept for backwards compatibility; the
// IMAP backend is now wired up via imapclient. This variable is no
// longer returned by New; the import remains to document the
// migration in one place.
var ErrIMAPNotImplemented = errors.New("accounts: imap backend not implemented")

// Account is the runtime representation of one configured account.
// Credentials are NEVER copied into this type; they remain in config and
// are resolved at the IMAP/SMTP client boundary (Phase 2).
type Account struct {
	ID                 string
	Email              string
	Provider           string
	Permissions        permissions.Set
	Limits             config.LimitsConfig
	AttachmentSendDirs []string
	backend            mailbox.Backend
}

// Backend returns the mailbox backend for this account.
func (a *Account) Backend() mailbox.Backend { return a.backend }

// SetBackendForTest replaces the backend. It is intended for tests
// that need to inject an alternative configuration after the
// registry has been built. Calling it from production code is a bug.
func (a *Account) SetBackendForTest(b mailbox.Backend) { a.backend = b }

// View is the redacted MCP-visible representation of an Account. It
// never contains credentials.
type View struct {
	ID                  string     `json:"id"`
	Email               string     `json:"email"`
	Provider            string     `json:"provider"`
	Permissions         []string   `json:"permissions"`
	Limits              ViewLimits `json:"limits"`
	AttachmentSendPaths []string   `json:"attachment_send_paths"`
}

// ViewLimits is the JSON-serialisable view of config.LimitsConfig.
type ViewLimits struct {
	MaxMessageBytes    int `json:"max_message_bytes"`
	MaxAttachments     int `json:"max_attachments"`
	MaxSearchResults   int `json:"max_search_results"`
	MaxBatchOperations int `json:"max_batch_operations"`
}

// Registry holds the resolved accounts. It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	accounts map[string]*Account
}

// New builds a Registry from a validated config.
//
// For backend=memory it loads the mailbox from the configured root
// directory. For backend=imap it returns ErrIMAPNotImplemented — the IMAP
// backend is scheduled for Phase 2.
func New(cfg config.Config) (*Registry, error) {
	r := &Registry{accounts: make(map[string]*Account, len(cfg.Accounts))}
	for _, acc := range cfg.Accounts {
		perms, unknown := permissions.NewSetFromStrings(acc.Permissions)
		if len(unknown) > 0 {
			return nil, fmt.Errorf("account %q: unknown permissions: %s", acc.ID, strings.Join(unknown, ","))
		}
		a := &Account{
			ID:                 acc.ID,
			Email:              acc.Email,
			Provider:           acc.Provider,
			Permissions:        perms,
			Limits:             acc.Limits,
			AttachmentSendDirs: append([]string(nil), acc.AttachmentSendPaths...),
		}
		switch acc.Backend {
		case "memory":
			root, err := filepath.Abs(acc.MailboxRoot)
			if err != nil {
				return nil, fmt.Errorf("account %q: resolve mailbox_root: %w", acc.ID, err)
			}
			if _, err := os.Stat(root); err != nil {
				return nil, fmt.Errorf("account %q: mailbox_root: %w", acc.ID, err)
			}
			be, err := mem.New(root)
			if err != nil {
				return nil, fmt.Errorf("account %q: %w", acc.ID, err)
			}
			a.backend = be
		case "imap":
			be, err := imapclient.New(acc, imapclient.Options{})
			if err != nil {
				return nil, fmt.Errorf("account %q: %w", acc.ID, err)
			}
			a.backend = be
		default:
			return nil, fmt.Errorf("account %q: unknown backend %q", acc.ID, acc.Backend)
		}
		r.accounts[a.ID] = a
	}
	return r, nil
}

// Get returns the account with the given id, or ErrAccountNotFound.
func (r *Registry) Get(id string) (*Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.accounts[id]
	if !ok {
		return nil, mailbox.ErrAccountNotFound
	}
	return a, nil
}

// All returns every account in an unspecified order. The slice is safe
// for the caller to read but must not be mutated. The returned slice is
// freshly allocated on each call.
func (r *Registry) All() []*Account {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		out = append(out, a)
	}
	return out
}

// LimitsFor returns a copy of the limits for the given account.
func (r *Registry) LimitsFor(id string) config.LimitsConfig {
	a, err := r.Get(id)
	if err != nil {
		return config.LimitsConfig{}
	}
	return a.Limits
}

// Views returns redacted views of every account, sorted by id.
func (r *Registry) Views() []View {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.accounts))
	for id := range r.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]View, 0, len(ids))
	for _, id := range ids {
		out = append(out, viewOf(r.accounts[id]))
	}
	return out
}

func viewOf(a *Account) View {
	return View{
		ID:          a.ID,
		Email:       a.Email,
		Provider:    a.Provider,
		Permissions: stringsFromCaps(a.Permissions),
		Limits: ViewLimits{
			MaxMessageBytes:    a.Limits.MaxMessageBytes,
			MaxAttachments:     a.Limits.MaxAttachments,
			MaxSearchResults:   a.Limits.MaxSearchResults,
			MaxBatchOperations: a.Limits.MaxBatchOperations,
		},
		AttachmentSendPaths: append([]string(nil), a.AttachmentSendDirs...),
	}
}

func stringsFromCaps(s permissions.Set) []string {
	cs := s.List()
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}
