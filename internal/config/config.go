// Package config loads and validates the TheMailroom YAML configuration file.
//
// The configuration defines the server-level settings and a list of accounts.
// Each account has its own credentials, protocol endpoints, permissions and
// limits. Validation is strict: unknown fields are tolerated but invalid
// values are rejected with a clear error before the server starts.
package config

import (
	"fmt"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strings"

	"github.com/flowbyte-com/themailroom/internal/recipients"
)

// Default values applied when a field is left blank.
const (
	DefaultIMAPPort         = 993
	DefaultIMAPPortStartTLS = 143
	DefaultSMTPPort         = 587
	DefaultSMTPPortImplicit = 465

	DefaultMaxMessageBytes         = 25 * 1024 * 1024 // 25 MiB
	DefaultMaxAttachmentBytes      = 6 * 1024 * 1024  // 6 MiB
	DefaultMaxTotalAttachmentBytes = 25 * 1024 * 1024 // 25 MiB
	DefaultMaxAttachments          = 50
	DefaultMaxRecipients           = 50
	DefaultMaxSearchResults        = 500
	DefaultMaxBatchOperations      = 50
	DefaultMaxMIMENesting          = 16
	DefaultMaxMIMEParts            = 500

	DefaultLogLevel = "info"
)

// Config is the root of the YAML configuration.
type Config struct {
	Server   ServerConfig    `yaml:"server"`
	Accounts []AccountConfig `yaml:"accounts"`
}

// ServerConfig holds server-level settings.
type ServerConfig struct {
	// LogLevel is one of "debug", "info", "warn", "error".
	LogLevel string `yaml:"log_level"`
}

// AccountConfig describes one email account.
type AccountConfig struct {
	// ID is the stable opaque identifier exposed to MCP tools.
	ID string `yaml:"id"`

	// Backend selects the implementation. Empty defaults to "imap" once
	// Phase 2 ships. "memory" is supported now and is intended for
	// development and tests; it loads from MailboxRoot.
	Backend string `yaml:"backend,omitempty"`

	// Provider is an informational label only (e.g. "purelymail",
	// "generic"). No provider-specific behaviour is wired in Phase 1.
	Provider string `yaml:"provider,omitempty"`

	// Email is the primary email address associated with the account.
	Email string `yaml:"email"`

	// IMAP holds the IMAP connection settings. Required when Backend is
	// "imap".
	IMAP IMAPConfig `yaml:"imap,omitempty"`

	// SMTP holds the SMTP submission settings. Required when the account
	// is granted the "send" capability (Phase 2).
	SMTP SMTPConfig `yaml:"smtp,omitempty"`

	// Permissions lists granted capabilities. Unknown names cause a load
	// error.
	Permissions []string `yaml:"permissions,omitempty"`

	// AllowedRecipients is an optional exact-address allow-list for
	// outbound recipients. An empty list permits any recipient. After RFC
	// 5322 parsing, local-part case is preserved and significant while the
	// domain is compared case-insensitively; wildcards are not supported.
	AllowedRecipients []string `yaml:"allowed_recipients,omitempty"`

	// AttachmentSendPaths lists absolute directory paths that may be
	// used as attachment sources. Empty means attachments cannot be sent.
	// Roots are opened with os.Root when attachment_send is granted.
	AttachmentSendPaths []string `yaml:"attachment_send_paths,omitempty"`

	// Limits overrides default safety limits. Zero values are replaced
	// with defaults.
	Limits LimitsConfig `yaml:"limits,omitempty"`

	// MailboxRoot is required when Backend = "memory". It points to a
	// directory of RFC 822 (.eml) files used as the mailbox contents.
	MailboxRoot string `yaml:"mailbox_root,omitempty"`
}

// IMAPConfig configures an IMAP connection.
type IMAPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port,omitempty"`
	TLS      bool   `yaml:"tls,omitempty"`
	StartTLS bool   `yaml:"starttls,omitempty"`
	Username string `yaml:"username,omitempty"`

	// Exactly one of Password or PasswordCommand must be set. Both
	// fields are tagged json:"-" so an accidental JSON serialisation
	// never leaks the resolved credential to a log line, panic dump
	// or test fixture.
	Password        string   `yaml:"password,omitempty"        json:"-"`
	PasswordCommand []string `yaml:"password_command,omitempty" json:"-"`
}

// SMTPConfig configures an SMTP submission connection.
type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port,omitempty"`
	TLS      bool   `yaml:"tls,omitempty"`
	StartTLS bool   `yaml:"starttls,omitempty"`
	Username string `yaml:"username,omitempty"`

	// Exactly one of Password or PasswordCommand must be set. Both
	// fields are tagged json:"-" so an accidental JSON serialisation
	// never leaks the resolved credential.
	Password        string   `yaml:"password,omitempty"        json:"-"`
	PasswordCommand []string `yaml:"password_command,omitempty" json:"-"`
}

// LimitsConfig enforces per-account safety limits. Zero values are replaced
// with defaults during validation.
type LimitsConfig struct {
	MaxMessageBytes         int `yaml:"max_message_bytes,omitempty"`
	MaxAttachmentBytes      int `yaml:"max_attachment_bytes,omitempty"`
	MaxTotalAttachmentBytes int `yaml:"max_total_attachment_bytes,omitempty"`
	MaxAttachments          int `yaml:"max_attachments,omitempty"`
	MaxRecipients           int `yaml:"max_recipients,omitempty"`
	MaxSearchResults        int `yaml:"max_search_results,omitempty"`
	MaxBatchOperations      int `yaml:"max_batch_operations,omitempty"`
	MaxMIMENesting          int `yaml:"max_mime_nesting,omitempty"`
	MaxMIMEParts            int `yaml:"max_mime_parts,omitempty"`
}

// WithDefaults returns a copy of c with zero-valued fields filled in.
// The receiver is not modified.
func (c AccountConfig) WithDefaults() AccountConfig {
	if c.Backend == "" {
		// Default depends on whether IMAP is configured; the loader
		// resolves the effective default after defaults are applied.
		c.Backend = "imap"
	}
	// IMAP: default to implicit TLS on 993.
	if c.IMAP.Port == 0 {
		switch {
		case c.IMAP.TLS:
			c.IMAP.Port = DefaultIMAPPort
		case c.IMAP.StartTLS:
			c.IMAP.Port = DefaultIMAPPortStartTLS
		default:
			c.IMAP.TLS = true
			c.IMAP.Port = DefaultIMAPPort
		}
	}
	// SMTP: default to STARTTLS on the submission port 587.
	if c.SMTP.Port == 0 {
		switch {
		case c.SMTP.TLS:
			c.SMTP.Port = DefaultSMTPPortImplicit
		case c.SMTP.StartTLS:
			c.SMTP.Port = DefaultSMTPPort
		default:
			c.SMTP.StartTLS = true
			c.SMTP.Port = DefaultSMTPPort
		}
	}
	if c.Limits.MaxMessageBytes == 0 {
		c.Limits.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if c.Limits.MaxAttachmentBytes == 0 {
		c.Limits.MaxAttachmentBytes = DefaultMaxAttachmentBytes
	}
	if c.Limits.MaxTotalAttachmentBytes == 0 {
		c.Limits.MaxTotalAttachmentBytes = DefaultMaxTotalAttachmentBytes
	}
	if c.Limits.MaxAttachments == 0 {
		c.Limits.MaxAttachments = DefaultMaxAttachments
	}
	if c.Limits.MaxRecipients == 0 {
		c.Limits.MaxRecipients = DefaultMaxRecipients
	}
	if c.Limits.MaxSearchResults == 0 {
		c.Limits.MaxSearchResults = DefaultMaxSearchResults
	}
	if c.Limits.MaxBatchOperations == 0 {
		c.Limits.MaxBatchOperations = DefaultMaxBatchOperations
	}
	if c.Limits.MaxMIMENesting == 0 {
		c.Limits.MaxMIMENesting = DefaultMaxMIMENesting
	}
	if c.Limits.MaxMIMEParts == 0 {
		c.Limits.MaxMIMEParts = DefaultMaxMIMEParts
	}
	return c
}

// Validate checks the configuration for consistency and required fields.
// On success it returns the normalised Config (with defaults applied). On
// failure it returns a typed error wrapping the first violation found.
func (c Config) Validate() (Config, error) {
	if strings.TrimSpace(c.Server.LogLevel) == "" {
		c.Server.LogLevel = DefaultLogLevel
	}
	if !isLogLevel(c.Server.LogLevel) {
		return Config{}, fmt.Errorf("server.log_level %q invalid; want one of debug, info, warn, error", c.Server.LogLevel)
	}
	if len(c.Accounts) == 0 {
		return Config{}, fmt.Errorf("at least one account is required")
	}
	seen := make(map[string]struct{}, len(c.Accounts))
	for i := range c.Accounts {
		acc := c.Accounts[i].WithDefaults()
		if err := validateAccount(i, acc); err != nil {
			return Config{}, err
		}
		if _, dup := seen[acc.ID]; dup {
			return Config{}, fmt.Errorf("duplicate account id %q", acc.ID)
		}
		seen[acc.ID] = struct{}{}
		c.Accounts[i] = acc
	}
	return c, nil
}

func validateAccount(idx int, a AccountConfig) error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("accounts[%d].id is required", idx)
	}
	if strings.ContainsAny(a.ID, " \n") {
		return fmt.Errorf("accounts[%d].id %q must not contain whitespace", idx, a.ID)
	}
	if err := validateSenderIdentity(a.Email); err != nil {
		return fmt.Errorf("accounts[%d].email: %w", idx, err)
	}
	if err := validatePermissions(a.Permissions); err != nil {
		return fmt.Errorf("accounts[%d].%w", idx, err)
	}
	switch a.Backend {
	case "imap", "memory":
		// ok
	default:
		return fmt.Errorf("accounts[%d].backend %q invalid; want \"imap\" or \"memory\"", idx, a.Backend)
	}
	if a.Backend == "memory" {
		if strings.TrimSpace(a.MailboxRoot) == "" {
			return fmt.Errorf("accounts[%d].mailbox_root is required when backend=memory", idx)
		}
		// We check existence; canonicalisation happens in the loader.
		info, err := os.Stat(a.MailboxRoot)
		if err != nil {
			return fmt.Errorf("accounts[%d].mailbox_root: %w", idx, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("accounts[%d].mailbox_root %q is not a directory", idx, a.MailboxRoot)
		}
	} else {
		if err := validateEndpoint("imap", a.IMAP.Host, a.IMAP.Port); err != nil {
			return fmt.Errorf("accounts[%d].%w", idx, err)
		}
		if err := validateAuth("imap", a.IMAP.Username, a.IMAP.Password, a.IMAP.PasswordCommand); err != nil {
			return fmt.Errorf("accounts[%d].%w", idx, err)
		}
	}
	if err := validateLimits(a.Limits); err != nil {
		return fmt.Errorf("accounts[%d].%w", idx, err)
	}
	if _, err := recipients.NewPolicy(a.AllowedRecipients, a.Limits.MaxRecipients); err != nil {
		return fmt.Errorf("accounts[%d].%w", idx, err)
	}
	// SMTP settings are only required for accounts that can actually be
	// asked to send. Read-only accounts retain their previous configuration
	// requirements and may leave SMTP entirely empty.
	if hasPermission(a.Permissions, "send") {
		if err := validateSMTPConfig(a.SMTP); err != nil {
			return fmt.Errorf("accounts[%d].%w", idx, err)
		}
	}
	if err := validateAttachmentPaths(a.AttachmentSendPaths, hasPermission(a.Permissions, "attachment_send")); err != nil {
		return fmt.Errorf("accounts[%d].%w", idx, err)
	}
	return nil
}

// validateSMTPConfig validates the submission endpoint for an account with
// the send capability. It deliberately accepts only implicit TLS or mandatory
// STARTTLS; no plaintext mode or skip-verification option exists.
func validateSMTPConfig(s SMTPConfig) error {
	if s.TLS && s.StartTLS {
		return fmt.Errorf("smtp: only one of tls or starttls may be enabled")
	}
	if !s.TLS && !s.StartTLS {
		return fmt.Errorf("smtp: one of tls or starttls must be enabled")
	}
	if hasControl(s.Host) {
		return fmt.Errorf("smtp.host contains a control character")
	}
	if err := validateEndpoint("smtp", s.Host, s.Port); err != nil {
		return err
	}
	if hasControl(s.Username) {
		return fmt.Errorf("smtp.username contains a control character")
	}
	if err := validateAuth("smtp", s.Username, s.Password, s.PasswordCommand); err != nil {
		return err
	}
	return nil
}

func validateSenderIdentity(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("is required")
	}
	if hasControl(raw) {
		return fmt.Errorf("contains a control character")
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address == "" || hasControl(addr.Address) {
		return fmt.Errorf("is invalid")
	}
	return nil
}

func hasPermission(names []string, want string) bool {
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), want) {
			return true
		}
	}
	return false
}

func hasControl(s string) bool {
	return recipients.HasControl(s)
}

func validateEndpoint(proto, host string, port int) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%s.host is required", proto)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s.port %d invalid; want 1..65535", proto, port)
	}
	// Best-effort host syntax check; the OS resolver is the final word.
	if hasControl(host) || strings.ContainsAny(host, " /") {
		return fmt.Errorf("%s.host %q invalid", proto, host)
	}
	// The schema has a separate port field. Reject a host:port pair rather
	// than passing an ambiguous value to a future client. Bare IPv6 hosts
	// contain colons and are accepted when net.ParseIP recognises them.
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return fmt.Errorf("%s.host %q invalid; host and port must be separate", proto, host)
	}
	return nil
}

func validateAuth(proto, user, password string, cmd []string) error {
	if strings.TrimSpace(user) == "" {
		return fmt.Errorf("%s.username is required", proto)
	}
	hasPassword := password != ""
	hasCommand := len(cmd) > 0
	if hasPassword && hasCommand {
		return fmt.Errorf("%s: only one of password or password_command may be set", proto)
	}
	if !hasPassword && !hasCommand {
		return fmt.Errorf("%s: either password or password_command is required", proto)
	}
	if hasCommand && len(cmd) == 0 {
		return fmt.Errorf("%s.password_command must be a non-empty list", proto)
	}
	return nil
}

func validateLimits(l LimitsConfig) error {
	if l.MaxMessageBytes < 0 {
		return fmt.Errorf("limits.max_message_bytes must be non-negative")
	}
	if l.MaxAttachments < 0 {
		return fmt.Errorf("limits.max_attachments must be non-negative")
	}
	if l.MaxRecipients < 0 {
		return fmt.Errorf("limits.max_recipients must be non-negative")
	}
	if l.MaxSearchResults < 0 {
		return fmt.Errorf("limits.max_search_results must be non-negative")
	}
	if l.MaxBatchOperations < 0 {
		return fmt.Errorf("limits.max_batch_operations must be non-negative")
	}
	if l.MaxAttachmentBytes < 0 {
		return fmt.Errorf("limits.max_attachment_bytes must be non-negative")
	}
	if l.MaxTotalAttachmentBytes < 0 {
		return fmt.Errorf("limits.max_total_attachment_bytes must be non-negative")
	}
	if l.MaxMIMENesting < 0 {
		return fmt.Errorf("limits.max_mime_nesting must be non-negative")
	}
	if l.MaxMIMEParts < 0 {
		return fmt.Errorf("limits.max_mime_parts must be non-negative")
	}
	return nil
}

func validateAttachmentPaths(paths []string, requireExisting bool) error {
	seen := make(map[string]struct{}, len(paths))
	for i, p := range paths {
		if strings.Contains(p, "\x00") {
			return fmt.Errorf("attachment_send_paths[%d] contains NUL", i)
		}
		if hasControl(p) {
			return fmt.Errorf("attachment_send_paths[%d] contains a control character", i)
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("attachment_send_paths[%d] %q must be absolute", i, p)
		}
		clean := filepath.Clean(p)
		if _, duplicate := seen[clean]; duplicate {
			return fmt.Errorf("attachment_send_paths[%d] duplicates another root", i)
		}
		seen[clean] = struct{}{}
		if requireExisting {
			info, err := os.Stat(clean)
			if err != nil {
				return fmt.Errorf("attachment_send_paths[%d]: %w", i, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("attachment_send_paths[%d] %q is not a directory", i, p)
			}
		}
	}
	return nil
}

// validatePermissions rejects unknown capability names. Empty lists are
// valid (an account may exist without any capabilities).
func validatePermissions(names []string) error {
	known := map[string]struct{}{
		"read":            {},
		"send":            {},
		"modify":          {},
		"delete":          {},
		"attachment_send": {},
	}
	var bad []string
	for _, n := range names {
		if _, ok := known[strings.ToLower(strings.TrimSpace(n))]; !ok {
			bad = append(bad, n)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("permissions: unknown capability names: %s", strings.Join(bad, ", "))
	}
	return nil
}

func isLogLevel(s string) bool {
	switch strings.ToLower(s) {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}
