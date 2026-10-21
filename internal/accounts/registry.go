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

	"github.com/flowbyte-com/themailroom/internal/attachments"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/imapclient"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	mem "github.com/flowbyte-com/themailroom/internal/mailbox/memory"
	"github.com/flowbyte-com/themailroom/internal/permissions"
	"github.com/flowbyte-com/themailroom/internal/recipients"
)

// ErrIMAPNotImplemented is kept for backwards compatibility; the
// IMAP backend is now wired up via imapclient. This variable is no
// longer returned by New; the import remains to document the
// migration in one place.
var ErrIMAPNotImplemented = errors.New("accounts: imap backend not implemented")

// Account is the runtime representation of one configured account.
// Credentials are retained only in unexported fields for use at the
// protocol boundary; they are never part of View or any MCP-visible shape.
type Account struct {
	ID              string
	Email           string
	Provider        string
	Permissions     permissions.Set
	Limits          config.LimitsConfig
	RecipientPolicy recipients.Policy

	backend            mailbox.Backend
	attachmentResolver *attachments.Resolver
	smtp               config.SMTPConfig
}

// Backend returns the mailbox backend for this account.
func (a *Account) Backend() mailbox.Backend { return a.backend }

// SetBackendForTest replaces the backend. It is intended for tests
// that need to inject an alternative configuration after the
// registry has been built. Calling it from production code is a bug.
func (a *Account) SetBackendForTest(b mailbox.Backend) { a.backend = b }

// AttachmentSendEnabled reports whether this account has a usable confined
// attachment resolver. It is true only when attachment_send is granted and at
// least one attachment root was configured and opened.
func (a *Account) AttachmentSendEnabled() bool { return a.attachmentResolver != nil }

// AttachmentResolver returns the confined outbound attachment resolver, or
// nil when attachment sending is not configured for this account.
func (a *Account) AttachmentResolver() *attachments.Resolver { return a.attachmentResolver }

// SMTPSubmissionConfig returns a copy of the account's SMTP submission
// configuration for the sender. It contains credentials and must never be
// exposed through an MCP-visible structure or logged.
func (a *Account) SMTPSubmissionConfig() config.SMTPConfig {
	cfg := a.smtp
	cfg.PasswordCommand = append([]string(nil), a.smtp.PasswordCommand...)
	return cfg
}

// Close releases resources owned by the account, currently the outbound
// attachment roots. It is idempotent.
func (a *Account) Close() error {
	if a.attachmentResolver == nil {
		return nil
	}
	return a.attachmentResolver.Close()
}

// View is the redacted MCP-visible representation of an Account. It
// never contains credentials or absolute filesystem roots.
type View struct {
	ID                    string     `json:"id"`
	Email                 string     `json:"email"`
	Provider              string     `json:"provider"`
	Permissions           []string   `json:"permissions"`
	Limits                ViewLimits `json:"limits"`
	AttachmentSendEnabled bool       `json:"attachment_send_enabled"`
}

// ViewLimits is the JSON-serialisable view of config.LimitsConfig.
type ViewLimits struct {
	MaxMessageBytes         int `json:"max_message_bytes"`
	MaxAttachmentBytes      int `json:"max_attachment_bytes"`
	MaxTotalAttachmentBytes int `json:"max_total_attachment_bytes"`
	MaxAttachments          int `json:"max_attachments"`
	MaxRecipients           int `json:"max_recipients"`
	MaxSearchResults        int `json:"max_search_results"`
	MaxBatchOperations      int `json:"max_batch_operations"`
	MaxMIMENesting          int `json:"max_mime_nesting"`
	MaxMIMEParts            int `json:"max_mime_parts"`
}

// Registry holds the resolved accounts. It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	accounts map[string]*Account
}

// New builds a Registry from a validated config.
//
// For backend=memory it loads the mailbox from the configured root
// directory. For backend=imap it constructs the IMAP backend. Accounts with
// the attachment_send capability also open their configured attachment roots;
// callers must Close the registry to release them.
func New(cfg config.Config) (reg *Registry, err error) {
	r := &Registry{accounts: make(map[string]*Account, len(cfg.Accounts))}
	defer func() {
		if err != nil {
			_ = r.Close()
		}
	}()

	for _, acc := range cfg.Accounts {
		perms, unknown := permissions.NewSetFromStrings(acc.Permissions)
		if len(unknown) > 0 {
			return nil, fmt.Errorf("account %q: unknown permissions: %s", acc.ID, strings.Join(unknown, ","))
		}
		policy, err := recipients.NewPolicy(acc.AllowedRecipients, acc.Limits.MaxRecipients)
		if err != nil {
			return nil, fmt.Errorf("account %q: %w", acc.ID, err)
		}
		smtpConfig := acc.SMTP
		smtpConfig.PasswordCommand = append([]string(nil), acc.SMTP.PasswordCommand...)
		a := &Account{
			ID:              acc.ID,
			Email:           acc.Email,
			Provider:        acc.Provider,
			Permissions:     perms,
			Limits:          acc.Limits,
			RecipientPolicy: policy,
			smtp:            smtpConfig,
		}
		if perms.Has(permissions.CapAttachmentSend) && len(acc.AttachmentSendPaths) > 0 {
			resolver, err := attachments.NewResolver(acc.AttachmentSendPaths, attachments.Limits{
				MaxAttachmentBytes:      int64(acc.Limits.MaxAttachmentBytes),
				MaxTotalAttachmentBytes: int64(acc.Limits.MaxTotalAttachmentBytes),
				MaxAttachments:          acc.Limits.MaxAttachments,
			})
			if err != nil {
				return nil, fmt.Errorf("account %q: %w", acc.ID, err)
			}
			a.attachmentResolver = resolver
		}
		switch acc.Backend {
		case "memory":
			root, err := filepath.Abs(acc.MailboxRoot)
			if err != nil {
				_ = a.Close()
				return nil, fmt.Errorf("account %q: resolve mailbox_root: %w", acc.ID, err)
			}
			if _, err := os.Stat(root); err != nil {
				_ = a.Close()
				return nil, fmt.Errorf("account %q: mailbox_root: %w", acc.ID, err)
			}
			be, err := mem.New(root)
			if err != nil {
				_ = a.Close()
				return nil, fmt.Errorf("account %q: %w", acc.ID, err)
			}
			a.backend = be
		case "imap":
			be, err := imapclient.New(acc, imapclient.Options{})
			if err != nil {
				_ = a.Close()
				return nil, fmt.Errorf("account %q: %w", acc.ID, err)
			}
			a.backend = be
		default:
			_ = a.Close()
			return nil, fmt.Errorf("account %q: unknown backend %q", acc.ID, acc.Backend)
		}
		if _, exists := r.accounts[a.ID]; exists {
			_ = a.Close()
			return nil, fmt.Errorf("account %q: duplicate account id", a.ID)
		}
		r.accounts[a.ID] = a
	}
	return r, nil
}

// Close releases resources owned by every account. It is safe to call
// multiple times.
func (r *Registry) Close() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var errs []error
	for _, a := range r.accounts {
		if err := a.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
			MaxMessageBytes:         a.Limits.MaxMessageBytes,
			MaxAttachmentBytes:      a.Limits.MaxAttachmentBytes,
			MaxTotalAttachmentBytes: a.Limits.MaxTotalAttachmentBytes,
			MaxAttachments:          a.Limits.MaxAttachments,
			MaxRecipients:           a.Limits.MaxRecipients,
			MaxSearchResults:        a.Limits.MaxSearchResults,
			MaxBatchOperations:      a.Limits.MaxBatchOperations,
			MaxMIMENesting:          a.Limits.MaxMIMENesting,
			MaxMIMEParts:            a.Limits.MaxMIMEParts,
		},
		AttachmentSendEnabled: a.AttachmentSendEnabled(),
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
