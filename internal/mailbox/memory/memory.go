// Package memory provides an in-process Backend implementation that loads
// RFC 822 (.eml) files from a directory. It is intended for development
// and tests; production deployments use the IMAP backend.
package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
)

// Backend is a Backend implementation that reads messages from a directory.
type Backend struct {
	mu     sync.RWMutex
	root   string
	loaded map[string][]*mailbox.Message // mailbox -> messages (uid=index+1)
	limits config.LimitsConfig
}

// New returns a Backend that loads .eml files from root. The directory is
// scanned synchronously; pass an already-populated directory. The default
// limits are applied; use NewWithLimits to override.
func New(root string) (*Backend, error) {
	return NewWithLimits(root, config.LimitsConfig{})
}

// NewWithLimits returns a Backend with explicit limits. Tests use this to
// trigger size / depth / part-count cap behaviour without rebuilding the
// configuration pipeline. The limits are applied during the initial load
// so that messages exceeding the cap are rejected with a parse error.
func NewWithLimits(root string, limits config.LimitsConfig) (*Backend, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("memory: stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("memory: %s is not a directory", root)
	}
	b := &Backend{
		root:   root,
		loaded: make(map[string][]*mailbox.Message),
		limits: withDefaults(limits),
	}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
}

// withDefaults fills in zero-valued limit fields with the same defaults
// the loader uses. The defaults match the documented package-level
// constants.
func withDefaults(l config.LimitsConfig) config.LimitsConfig {
	if l.MaxMessageBytes == 0 {
		l.MaxMessageBytes = config.DefaultMaxMessageBytes
	}
	if l.MaxAttachmentBytes == 0 {
		l.MaxAttachmentBytes = config.DefaultMaxAttachmentBytes
	}
	if l.MaxTotalAttachmentBytes == 0 {
		l.MaxTotalAttachmentBytes = config.DefaultMaxTotalAttachmentBytes
	}
	if l.MaxAttachments == 0 {
		l.MaxAttachments = config.DefaultMaxAttachments
	}
	if l.MaxMIMENesting == 0 {
		l.MaxMIMENesting = config.DefaultMaxMIMENesting
	}
	if l.MaxMIMEParts == 0 {
		l.MaxMIMEParts = config.DefaultMaxMIMEParts
	}
	if l.MaxSearchResults == 0 {
		l.MaxSearchResults = config.DefaultMaxSearchResults
	}
	if l.MaxBatchOperations == 0 {
		l.MaxBatchOperations = config.DefaultMaxBatchOperations
	}
	return l
}

// load reads every *.eml file in root (recursively) and indexes it under
// the relative directory name (or INBOX if at the top level).
//
// Symlink entries (both regular files and directories) are rejected.
// filepath.WalkDir does not follow symlinks, so this prevents the
// mailbox from accidentally exposing files outside the configured root.
func (b *Backend) load() error {
	// Resolve the root to its canonical absolute path so subsequent
	// containment checks have a stable reference. We do this once
	// rather than per-entry to avoid TOCTOU surprises during the walk.
	canonicalRoot, err := filepath.EvalSymlinks(b.root)
	if err != nil {
		return fmt.Errorf("memory: resolve root symlinks: %w", err)
	}
	b.root = canonicalRoot

	entries := []string{}
	walkErr := filepath.WalkDir(b.root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Reject any symlink entry, even if the symlink would not
		// actually be followed by WalkDir. The user gets a clear
		// error instead of a silently-skipped file.
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink entries are not allowed in mailbox_root: %s", path)
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".eml") {
			return nil
		}
		entries = append(entries, path)
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("memory: walk: %w", walkErr)
	}
	sort.Strings(entries)

	for _, path := range entries {
		// Defence in depth: verify the file is under the resolved
		// root. EvalSymlinks on each file detects any race where a
		// symlink was created between WalkDir and ReadFile.
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("memory: resolve %s: %w", path, err)
		}
		if !strings.HasPrefix(real, canonicalRoot+string(filepath.Separator)) && real != canonicalRoot {
			return fmt.Errorf("memory: %s escapes mailbox root", path)
		}
		data, err := os.ReadFile(real)
		if err != nil {
			return fmt.Errorf("memory: read %s: %w", path, err)
		}
		msg, err := b.parse(data)
		if err != nil {
			return fmt.Errorf("memory: parse %s: %w", path, err)
		}
		mailboxName := mailboxNameFor(b.root, real)
		b.loaded[mailboxName] = append(b.loaded[mailboxName], msg)
	}
	// Assign sequential UIDs per mailbox, newest first. UIDVALIDITY is
	// a constant per process; real IMAP servers change it on mailbox
	// re-creation. The memory backend has only one process lifetime so
	// 1 is a stable value.
	for name, msgs := range b.loaded {
		sort.SliceStable(msgs, func(i, j int) bool {
			return msgs[i].Headers.Date.After(msgs[j].Headers.Date)
		})
		for i, m := range msgs {
			m.UID = uint32(i + 1)
			m.UIDValidity = 1
		}
		b.loaded[name] = msgs
	}
	return nil
}

// parse decodes a single .eml using the shared mimeparse package so
// memory and IMAP backends expose the same structured view. Limits
// are applied per account.
func (b *Backend) parse(data []byte) (*mailbox.Message, error) {
	decoded, err := mimeparse.Parse(data, mimeparse.Limits{
		MaxMessageBytes:         b.limits.MaxMessageBytes,
		MaxAttachmentBytes:      b.limits.MaxAttachmentBytes,
		MaxTotalAttachmentBytes: b.limits.MaxTotalAttachmentBytes,
		MaxMIMENesting:          b.limits.MaxMIMENesting,
		MaxMIMEParts:            b.limits.MaxMIMEParts,
		MaxAttachments:          b.limits.MaxAttachments,
	})
	if err != nil {
		return nil, err
	}
	msg := &mailbox.Message{
		Headers:     decoded.Headers,
		Text:        decoded.Text,
		HTML:        decoded.HTML,
		Attachments: convertAttachments(decoded.Attachments),
	}
	if msg.Headers.Date.IsZero() {
		msg.Headers.Date = timeNow()
	}
	return msg, nil
}

// convertAttachments maps the mimeparse attachment view onto the
// internal representation. The internal field set is deliberately
// smaller: it is the API the tools layer commits to.
func convertAttachments(in []mimeparse.Attachment) []mailbox.Attachment {
	if len(in) == 0 {
		return nil
	}
	out := make([]mailbox.Attachment, len(in))
	for i, a := range in {
		out[i] = mailbox.Attachment{
			Index:       i,
			Part:        a.Part,
			Filename:    a.Filename,
			MimeType:    a.MimeType,
			Size:        a.Size,
			ContentID:   a.ContentID,
			Inline:      a.IsInline,
			Disposition: a.Disposition,
		}
	}
	return out
}

func mailboxNameFor(root, path string) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." {
		return "INBOX"
	}
	return filepath.ToSlash(rel)
}

// Mailboxes returns the list of mailboxes (always present, even if empty).
func (b *Backend) Mailboxes(ctx context.Context, accountID string) ([]mailbox.Mailbox, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := []mailbox.Mailbox{}
	for name, msgs := range b.loaded {
		out = append(out, mailbox.Mailbox{
			Name:        name,
			Attributes:  []string{},
			UIDValidity: 1,
			UIDNext:     uint32(len(msgs) + 1),
		})
	}
	if len(out) == 0 {
		out = append(out, mailbox.Mailbox{Name: "INBOX", UIDValidity: 1, UIDNext: 1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListMessages returns summaries, newest first. The memory
// backend always reports UIDVALIDITY=1; that value is returned
// in ListResult so callers can pair it with the listed UIDs.
func (b *Backend) ListMessages(ctx context.Context, accountID, mbx string, opts mailbox.ListOptions) (mailbox.ListResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs, ok := b.loaded[mbx]
	if !ok {
		return mailbox.ListResult{}, mailbox.ErrMailboxNotFound
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = b.limits.MaxBatchOperations
	}
	out := make([]mailbox.MessageSummary, 0, len(msgs))
	for i, m := range msgs {
		if opts.Since != nil && m.Headers.Date.Before(*opts.Since) {
			continue
		}
		if opts.Before != nil && !m.Headers.Date.Before(*opts.Before) {
			continue
		}
		if int(opts.Offset) > i {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, mailbox.MessageSummary{
			UID:           m.UID,
			Flags:         copyStrings(m.FlagsSlice()),
			InternalDate:  m.Headers.Date,
			Size:          uint32(m.Size()),
			From:          m.Headers.From,
			To:            copyStrings(m.Headers.To),
			Subject:       m.Headers.Subject,
			HasAttachment: len(m.Attachments) > 0,
		})
	}
	return mailbox.ListResult{UIDValidity: 1, Messages: out}, nil
}

// FetchMessage returns a deep copy of the message. The memory
// backend always reports UIDVALIDITY=1; callers that pass any
// other value receive ErrUIDMismatch to keep the contract
// identical with the IMAP backend.
func (b *Backend) FetchMessage(ctx context.Context, accountID, mbx string, uid uint32, uidValidity uint32) (*mailbox.Message, error) {
	if uidValidity == 0 {
		return nil, mailbox.ErrUIDMissing
	}
	if uidValidity != 1 {
		return nil, mailbox.ErrUIDMismatch
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs, ok := b.loaded[mbx]
	if !ok {
		return nil, mailbox.ErrMailboxNotFound
	}
	for _, m := range msgs {
		if m.UID == uid {
			clone := *m
			clone.Attachments = copyAttachments(m.Attachments)
			return &clone, nil
		}
	}
	return nil, mailbox.ErrMessageNotFound
}

// SearchMessages does a simple substring match against the available
// fields. It is intentionally naive — production search uses IMAP SEARCH.
// The memory backend always reports UIDVALIDITY=1.
func (b *Backend) SearchMessages(ctx context.Context, accountID, mbx string, q mailbox.SearchQuery) (mailbox.ListResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs, ok := b.loaded[mbx]
	if !ok {
		return mailbox.ListResult{}, mailbox.ErrMailboxNotFound
	}
	limit := q.Limit
	if limit <= 0 {
		limit = b.limits.MaxSearchResults
	}
	out := []mailbox.MessageSummary{}
	for _, m := range msgs {
		if !searchMatches(m, q) {
			continue
		}
		out = append(out, mailbox.MessageSummary{
			UID:           m.UID,
			Flags:         copyStrings(m.FlagsSlice()),
			InternalDate:  m.Headers.Date,
			Size:          uint32(m.Size()),
			From:          m.Headers.From,
			To:            copyStrings(m.Headers.To),
			Subject:       m.Headers.Subject,
			HasAttachment: len(m.Attachments) > 0,
		})
		if len(out) >= limit {
			break
		}
	}
	return mailbox.ListResult{UIDValidity: 1, Messages: out}, nil
}

func searchMatches(m *mailbox.Message, q mailbox.SearchQuery) bool {
	if q.Subject != "" && !strings.Contains(strings.ToLower(m.Headers.Subject), strings.ToLower(q.Subject)) {
		return false
	}
	if q.From != "" && !strings.Contains(strings.ToLower(m.Headers.From), strings.ToLower(q.From)) {
		return false
	}
	if q.To != "" && !anyContainsCI(m.Headers.To, q.To) {
		return false
	}
	if q.Body != "" && !strings.Contains(strings.ToLower(m.Text), strings.ToLower(q.Body)) {
		return false
	}
	if q.Since != nil && m.Headers.Date.Before(*q.Since) {
		return false
	}
	if q.Before != nil && !m.Headers.Date.Before(*q.Before) {
		return false
	}
	if q.Unseen {
		// Memory backend semantics: all messages are treated
		// as \\Seen. An Unseen filter therefore matches
		// nothing, which is the only safe answer.
		return false
	}
	// q.Flagged is a pre-existing gap: searchMatches does
	// not consider q.Flagged. The field is accepted but
	// ignored. The wire schema marks it omitempty so callers
	// that do not set it see no behaviour change; callers
	// that do set it get the same result as the unfiltered
	// query.
	return true
}

func anyContainsCI(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(strings.ToLower(s), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func copyStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyAttachments(in []mailbox.Attachment) []mailbox.Attachment {
	if in == nil {
		return nil
	}
	out := make([]mailbox.Attachment, len(in))
	copy(out, in)
	return out
}
