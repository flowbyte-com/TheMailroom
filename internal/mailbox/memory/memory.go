// Package memory provides an in-process Backend implementation that loads
// RFC 822 (.eml) files from a directory. It is intended for development
// and tests; production deployments use the IMAP backend.
package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
	"github.com/flowbyte-com/themailroom/internal/searchspec"
)

// Backend is a Backend implementation that reads messages from a directory.
type Backend struct {
	mu     sync.RWMutex
	root   string
	loaded map[string][]*storedMessage // mailbox -> messages (uid=index+1)
	limits config.LimitsConfig
}

// storedMessage keeps the parsed message view and, for messages that
// expose attachments, a private immutable snapshot of the raw RFC 5322
// bytes. The snapshot is never returned to callers; attachment
// retrieval re-parses it instead of re-reading the source file.
type storedMessage struct {
	msg *mailbox.Message
	raw []byte
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
		loaded: make(map[string][]*storedMessage),
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
		rec := &storedMessage{msg: msg}
		if len(msg.Attachments) > 0 {
			rec.raw = data
		}
		b.loaded[mailboxName] = append(b.loaded[mailboxName], rec)
	}
	// Assign sequential UIDs per mailbox, newest first. UIDVALIDITY is
	// a constant per process; real IMAP servers change it on mailbox
	// re-creation. The memory backend has only one process lifetime so
	// 1 is a stable value.
	for name, recs := range b.loaded {
		sort.SliceStable(recs, func(i, j int) bool {
			return recs[i].msg.Headers.Date.After(recs[j].msg.Headers.Date)
		})
		for i, rec := range recs {
			rec.msg.UID = uint32(i + 1)
			rec.msg.UIDValidity = 1
		}
		b.loaded[name] = recs
	}
	return nil
}

// parse decodes a single .eml using the shared mimeparse package so
// memory and IMAP backends expose the same structured view. Limits
// are applied per account.
func (b *Backend) parse(data []byte) (*mailbox.Message, error) {
	decoded, err := mimeparse.Parse(data, b.mimeLimits())
	if err != nil {
		return nil, err
	}
	msg := &mailbox.Message{
		Headers:     decoded.Headers,
		Text:        decoded.Text,
		HTML:        decoded.HTML,
		Attachments: convertAttachments(decoded.Attachments),
		// Flags come from the conventional mbox Status: and
		// X-Status: headers: Status R means \Seen; X-Status
		// D/F/A mean \Deleted, \Flagged and \Answered. A
		// message with neither header has no flags, which
		// means it is unseen and not flagged. This is what
		// lets the memory backend answer the Flagged and
		// Unseen search filters the same way the IMAP backend
		// does.
		Flags: searchspec.FlagsFromStatusHeaders(
			rawHeader(data, "Status"),
			rawHeader(data, "X-Status"),
		),
	}
	if msg.Headers.Date.IsZero() {
		msg.Headers.Date = timeNow()
	}
	return msg, nil
}

// rawHeader returns the value of the named header from the raw
// RFC 5322 bytes, or "" if the header is absent. Only the
// header block is scanned; the body is never read.
//
// It is used for the conventional Status: and X-Status: headers,
// which the MIME parser does not surface because they are not
// part of the public message view.
func rawHeader(data []byte, name string) string {
	// The header block ends at the first empty line.
	end := len(data)
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 && i < end {
		end = i
	} else if i := bytes.Index(data, []byte("\n\n")); i >= 0 && i < end {
		end = i
	}
	block := data[:end]
	// Unfold continuation lines (a header line starting with
	// whitespace continues the previous one).
	lines := strings.Split(string(block), "\n")
	var unfolded []string
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if len(ln) > 0 && (ln[0] == ' ' || ln[0] == '\t') && len(unfolded) > 0 {
			unfolded[len(unfolded)-1] += " " + strings.TrimSpace(ln)
			continue
		}
		unfolded = append(unfolded, ln)
	}
	for _, ln := range unfolded {
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// mimeLimits returns the parser limits shared by message loading and
// attachment retrieval.
func (b *Backend) mimeLimits() mimeparse.Limits {
	return mimeparse.Limits{
		MaxMessageBytes:         b.limits.MaxMessageBytes,
		MaxAttachmentBytes:      b.limits.MaxAttachmentBytes,
		MaxTotalAttachmentBytes: b.limits.MaxTotalAttachmentBytes,
		MaxMIMENesting:          b.limits.MaxMIMENesting,
		MaxMIMEParts:            b.limits.MaxMIMEParts,
		MaxAttachments:          b.limits.MaxAttachments,
	}
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
		out[i] = convertAttachment(a, i)
	}
	return out
}

func convertAttachment(a mimeparse.Attachment, index int) mailbox.Attachment {
	return mailbox.Attachment{
		Index:       index,
		Part:        a.Part,
		Filename:    a.Filename,
		MimeType:    a.MimeType,
		Size:        a.Size,
		ContentID:   a.ContentID,
		Inline:      a.IsInline,
		Disposition: a.Disposition,
	}
}

// mapMimeError maps parser sentinels onto the public mailbox errors.
// The memory backend validates every loaded message, so the size
// errors are not expected during retrieval; mapping them keeps the
// backend contract identical if limits or parser behaviour change.
func mapMimeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mimeparse.ErrMessageTooLarge):
		return mailbox.ErrMessageTooLarge
	case errors.Is(err, mimeparse.ErrNestingTooDeep):
		return mailbox.ErrNestingTooDeep
	case errors.Is(err, mimeparse.ErrTooManyParts):
		return mailbox.ErrTooManyParts
	case errors.Is(err, mimeparse.ErrTooManyAttachments):
		return mailbox.ErrTooManyAttachments
	case errors.Is(err, mimeparse.ErrAttachmentTooLarge):
		return mailbox.ErrAttachmentTooLarge
	case errors.Is(err, mimeparse.ErrTotalAttachmentTooLarge):
		return mailbox.ErrTotalAttachmentTooLarge
	case errors.Is(err, mimeparse.ErrAttachmentNotFound):
		return mailbox.ErrAttachmentNotFound
	case errors.Is(err, mimeparse.ErrMalformed):
		return mailbox.ErrMalformed
	default:
		return err
	}
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
	for i, rec := range msgs {
		m := rec.msg
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
	for _, rec := range msgs {
		m := rec.msg
		if m.UID == uid {
			clone := *m
			clone.Attachments = copyAttachments(m.Attachments)
			return &clone, nil
		}
	}
	return nil, mailbox.ErrMessageNotFound
}

// FetchAttachment returns one selected attachment from the retained
// raw-message snapshot. It never re-reads the source file and never
// returns the snapshot itself; mimeparse produces fresh decoded
// content for the requested part.
func (b *Backend) FetchAttachment(ctx context.Context, accountID, mbx string, uid uint32, uidValidity uint32, part string) (*mailbox.AttachmentData, error) {
	if uidValidity == 0 {
		return nil, mailbox.ErrUIDMissing
	}
	if uidValidity != 1 {
		return nil, mailbox.ErrUIDMismatch
	}
	if _, err := mailbox.ParsePartPath(part, b.limits.MaxMIMENesting, b.limits.MaxMIMEParts); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs, ok := b.loaded[mbx]
	if !ok {
		return nil, mailbox.ErrMailboxNotFound
	}
	for _, rec := range msgs {
		if rec.msg.UID != uid {
			continue
		}
		if rec.raw == nil {
			return nil, mailbox.ErrAttachmentNotFound
		}
		ac, err := mimeparse.ExtractAttachment(rec.raw, part, b.mimeLimits())
		if err != nil {
			return nil, mapMimeError(err)
		}
		return &mailbox.AttachmentData{
			UID:         rec.msg.UID,
			UIDValidity: 1,
			Attachment:  convertAttachment(ac.Attachment, 0),
			Content:     ac.Content,
			Warnings:    ac.Warnings,
		}, nil
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
	if limit <= 0 || limit > b.limits.MaxSearchResults {
		limit = b.limits.MaxSearchResults
	}
	out := []mailbox.MessageSummary{}
	for _, rec := range msgs {
		m := rec.msg
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

// searchMatches applies the shared search contract to a memory
// message. The semantics must match the IMAP backend's (see
// internal/searchspec):
//
//   - Subject, From and To are ANDed and are case-insensitive
//     substring matches. The To filter matches any recipient in
//     To or Cc.
//   - Body is a case-insensitive substring match on the decoded
//     plain-text body.
//   - Since / Before use day granularity: the query value is
//     truncated to its UTC day, and the message Date is reduced
//     to the calendar day named by its own fields, matching the
//     IMAP search date comparison.
//   - Flagged requires the \Flagged flag; Unseen requires the
//     absence of the \Seen flag.
func searchMatches(m *mailbox.Message, q mailbox.SearchQuery) bool {
	if q.Subject != "" && !strings.Contains(strings.ToLower(m.Headers.Subject), strings.ToLower(q.Subject)) {
		return false
	}
	if q.From != "" && !strings.Contains(strings.ToLower(m.Headers.From), strings.ToLower(q.From)) {
		return false
	}
	if q.To != "" && !anyContainsCI(m.Headers.To, q.To) && !anyContainsCI(m.Headers.Cc, q.To) {
		return false
	}
	if q.Body != "" && !strings.Contains(strings.ToLower(m.Text), strings.ToLower(q.Body)) {
		return false
	}
	// Date search is at calendar-day granularity. The query
	// value is reduced to its UTC day, while the message Date
	// header keeps the calendar day named by its own fields;
	// converting the message date to UTC first would shift it
	// near midnight. This matches the reference IMAP server.
	msgDay := searchspec.CalendarDay(m.Headers.Date)
	if q.Since != nil {
		since := searchspec.TruncateDate(*q.Since)
		if msgDay.Before(since) {
			return false
		}
	}
	if q.Before != nil {
		before := searchspec.TruncateDate(*q.Before)
		if !msgDay.Before(before) {
			return false
		}
	}
	if !searchspec.MatchesUnseen(m.Flags, q.Unseen) {
		return false
	}
	if !searchspec.MatchesFlagged(m.Flags, q.Flagged) {
		return false
	}
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
