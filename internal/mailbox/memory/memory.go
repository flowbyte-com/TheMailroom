// Package memory provides an in-process Backend implementation that loads
// RFC 822 (.eml) files from a directory. It is intended for development
// and tests; production deployments use the IMAP backend.
package memory

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// Backend is a Backend implementation that reads messages from a directory.
type Backend struct {
	mu     sync.RWMutex
	root   string
	loaded map[string][]*mailbox.Message // mailbox -> messages (uid=index+1)
}

// New returns a Backend that loads .eml files from root. The directory is
// scanned synchronously; pass an already-populated directory.
func New(root string) (*Backend, error) {
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
	}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
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
		msg, err := parseMessage(data)
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

func mailboxNameFor(root, path string) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." {
		return "INBOX"
	}
	// Convert "foo/bar" to "foo/bar". Use forward slashes consistently.
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
	// Always surface INBOX so a client can probe before any messages
	// exist.
	if len(out) == 0 {
		out = append(out, mailbox.Mailbox{Name: "INBOX", UIDValidity: 1, UIDNext: 1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListMessages returns summaries, newest first.
func (b *Backend) ListMessages(ctx context.Context, accountID, mbx string, opts mailbox.ListOptions) ([]mailbox.MessageSummary, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs, ok := b.loaded[mbx]
	if !ok {
		return nil, mailbox.ErrMailboxNotFound
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
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
			Flags:         []string{},
			InternalDate:  m.Headers.Date,
			Size:          uint32(m.Size()),
			From:          m.Headers.From,
			To:            m.Headers.To,
			Subject:       m.Headers.Subject,
			HasAttachment: len(m.Attachments) > 0,
		})
	}
	return out, nil
}

// FetchMessage returns a deep copy of the message.
func (b *Backend) FetchMessage(ctx context.Context, accountID, mbx string, uid uint32) (*mailbox.Message, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs, ok := b.loaded[mbx]
	if !ok {
		return nil, mailbox.ErrMailboxNotFound
	}
	for _, m := range msgs {
		if m.UID == uid {
			// Deep copy of the body fields; the message itself is
			// immutable from the caller's perspective.
			clone := *m
			return &clone, nil
		}
	}
	return nil, mailbox.ErrMessageNotFound
}

// SearchMessages does a simple substring match against the available
// fields. It is intentionally naive — production search uses IMAP SEARCH.
func (b *Backend) SearchMessages(ctx context.Context, accountID, mbx string, q mailbox.SearchQuery) ([]mailbox.MessageSummary, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs, ok := b.loaded[mbx]
	if !ok {
		return nil, mailbox.ErrMailboxNotFound
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	out := []mailbox.MessageSummary{}
	for _, m := range msgs {
		if !searchMatches(m, q) {
			continue
		}
		out = append(out, mailbox.MessageSummary{
			UID:           m.UID,
			Flags:         []string{},
			InternalDate:  m.Headers.Date,
			Size:          uint32(m.Size()),
			From:          m.Headers.From,
			To:            m.Headers.To,
			Subject:       m.Headers.Subject,
			HasAttachment: len(m.Attachments) > 0,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
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
		// Memory backend treats all messages as Seen; Unseen=false.
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

// parseMessage decodes an RFC 822 message into a mailbox.Message using
// only the standard library. It supports multipart/alternative and
// multipart/mixed sufficiently for tests; exotic structures are
// best-effort.
func parseMessage(data []byte) (*mailbox.Message, error) {
	msg, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(data)))
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	out := &mailbox.Message{Headers: mailbox.MessageHeaders{}}
	out.Headers.MessageID = msg.Header.Get("Message-Id")
	out.Headers.Subject = msg.Header.Get("Subject")
	out.Headers.From = msg.Header.Get("From")
	out.Headers.InReplyTo = msg.Header.Get("In-Reply-To")
	if v := msg.Header.Get("References"); v != "" {
		out.Headers.References = strings.Fields(v)
	}
	if v := msg.Header.Get("To"); v != "" {
		out.Headers.To = splitAddresses(v)
	}
	if v := msg.Header.Get("Cc"); v != "" {
		out.Headers.Cc = splitAddresses(v)
	}
	if t, err := mail.ParseDate(msg.Header.Get("Date")); err == nil {
		out.Headers.Date = t
	} else {
		out.Headers.Date = time.Now().UTC()
	}

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		// Plain text fallback.
		body, _ := io.ReadAll(msg.Body)
		out.Text = string(body)
		return out, nil
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		body, _ := io.ReadAll(msg.Body)
		out.Text = string(body)
		return out, nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(msg.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				break
			}
			pCT := part.Header.Get("Content-Type")
			mt, pParams, _ := mime.ParseMediaType(pCT)
			disposition, dParams, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			isAttachment := disposition == "attachment" || (dParams["filename"] != "" && disposition != "inline")
			partBody, _ := io.ReadAll(part)
			if isAttachment {
				out.Attachments = append(out.Attachments, mailbox.Attachment{
					Index:    len(out.Attachments),
					Filename: dParams["filename"],
					MimeType: mt,
					Size:     int64(len(partBody)),
				})
				continue
			}
			switch {
			case strings.HasPrefix(mt, "text/plain"):
				out.Text = string(partBody)
			case strings.HasPrefix(mt, "text/html"):
				out.HTML = string(partBody)
			case strings.HasPrefix(mt, "multipart/"):
				// Best-effort nested multipart handling.
				nested := &mailbox.Message{}
				nested.Headers = out.Headers
				nested.Text = out.Text
				nested.HTML = out.HTML
				nested.Attachments = out.Attachments
				if err := mergePart(nested, mt, pParams["boundary"], partBody); err == nil {
					out = nested
				}
			}
		}
		return out, nil
	}

	// Single-part body.
	body, _ := io.ReadAll(msg.Body)
	if strings.HasPrefix(mediaType, "text/plain") {
		out.Text = string(body)
	} else if strings.HasPrefix(mediaType, "text/html") {
		out.HTML = string(body)
	} else {
		out.Attachments = append(out.Attachments, mailbox.Attachment{
			Index:    0,
			Filename: params["name"],
			MimeType: mediaType,
			Size:     int64(len(body)),
		})
	}
	return out, nil
}

func mergePart(m *mailbox.Message, mediaType, boundary string, body []byte) error {
	if boundary == "" {
		return errors.New("multipart with no boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		ct := part.Header.Get("Content-Type")
		mt, _, _ := mime.ParseMediaType(ct)
		data, _ := io.ReadAll(part)
		switch {
		case strings.HasPrefix(mt, "text/plain"):
			m.Text = string(data)
		case strings.HasPrefix(mt, "text/html"):
			m.HTML = string(data)
		}
	}
}

func splitAddresses(s string) []string {
	addrs, err := mail.ParseAddressList(s)
	if err != nil {
		return strings.Fields(s)
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.Address
	}
	return out
}
