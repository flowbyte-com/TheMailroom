package imapclient

import (
	"container/heap"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/flowbyte-com/go-imap"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
)

// mailboxFromInfo converts a server-reported MailboxInfo into the
// internal mailbox.Mailbox view.
func mailboxFromInfo(m *imap.MailboxInfo) mailbox.Mailbox {
	attrs := make([]string, len(m.Attributes))
	for i, a := range m.Attributes {
		attrs[i] = string(a)
	}
	return mailbox.Mailbox{
		Name:       m.Name,
		Attributes: attrs,
		Delimiter:  m.Delimiter,
	}
}

func sortMailboxes(mbs []mailbox.Mailbox) {
	sort.Slice(mbs, func(i, j int) bool { return mbs[i].Name < mbs[j].Name })
}

func sortSummaries(s []mailbox.MessageSummary) {
	sort.Slice(s, func(i, j int) bool {
		if !s[i].InternalDate.Equal(s[j].InternalDate) {
			return s[i].InternalDate.After(s[j].InternalDate)
		}
		return s[i].UID > s[j].UID
	})
}

// summaryBetter reports whether a is preferred over b in the list
// ordering: newest internal date first, higher UID first on a tie. It is
// the same comparison sortSummaries uses, so a bounded selection and a
// full sort agree on the order.
func summaryBetter(a, b mailbox.MessageSummary) bool {
	if !a.InternalDate.Equal(b.InternalDate) {
		return a.InternalDate.After(b.InternalDate)
	}
	return a.UID > b.UID
}

// summarySelector keeps the best k summaries in the list ordering while
// bounding the memory it uses.
//
// The ordering is a total order, because UIDs are unique within a
// mailbox. The selector therefore keeps exactly the k best summaries: the
// same page a full sort of every summary would produce, without holding
// every summary in memory.
//
// The worst kept summary sits at the root of the heap, so a summary that
// is better than it can replace it in constant time.
type summarySelector struct {
	items []mailbox.MessageSummary
	limit int
}

// Len implements heap.Interface.
func (s *summarySelector) Len() int { return len(s.items) }

// Less implements heap.Interface. The root is the worst kept summary, so
// that heap.Pop evicts it.
func (s *summarySelector) Less(i, j int) bool {
	return summaryBetter(s.items[j], s.items[i])
}

// Swap implements heap.Interface.
func (s *summarySelector) Swap(i, j int) { s.items[i], s.items[j] = s.items[j], s.items[i] }

// Push implements heap.Interface.
func (s *summarySelector) Push(x any) {
	s.items = append(s.items, x.(mailbox.MessageSummary))
}

// Pop implements heap.Interface.
func (s *summarySelector) Pop() any {
	old := s.items
	n := len(old)
	item := old[n-1]
	s.items = old[:n-1]
	return item
}

// Add offers a summary to the selector. It is retained only if it is
// better than the worst summary already kept.
func (s *summarySelector) Add(summary mailbox.MessageSummary) {
	if s.limit <= 0 {
		return
	}
	if len(s.items) < s.limit {
		heap.Push(s, summary)
		return
	}
	if summaryBetter(summary, s.items[0]) {
		s.items[0] = summary
		heap.Fix(s, 0)
	}
}

// Sorted returns the retained summaries in the list ordering.
func (s *summarySelector) Sorted() []mailbox.MessageSummary {
	out := make([]mailbox.MessageSummary, len(s.items))
	copy(out, s.items)
	sortSummaries(out)
	return out
}

func flagsToStrings(fs []string) []string {
	if len(fs) == 0 {
		return []string{}
	}
	out := make([]string, len(fs))
	copy(out, fs)
	return out
}

func envelopeFrom(env *imap.Envelope) string {
	if env == nil {
		return ""
	}
	if len(env.From) == 0 {
		return ""
	}
	a := env.From[0]
	if a.PersonalName != "" {
		return fmt.Sprintf("%s <%s@%s>", a.PersonalName, a.MailboxName, a.HostName)
	}
	return fmt.Sprintf("%s@%s", a.MailboxName, a.HostName)
}

func envelopeAddresses(env *imap.Envelope, kind string) []string {
	if env == nil {
		return nil
	}
	var addrs []*imap.Address
	switch strings.ToLower(kind) {
	case "to":
		addrs = env.To
	case "cc":
		addrs = env.Cc
	case "from":
		addrs = env.From
	}
	if len(addrs) == 0 {
		return []string{}
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		if a.PersonalName != "" {
			out[i] = fmt.Sprintf("%s <%s@%s>", a.PersonalName, a.MailboxName, a.HostName)
		} else {
			out[i] = fmt.Sprintf("%s@%s", a.MailboxName, a.HostName)
		}
	}
	return out
}

func envelopeSubject(env *imap.Envelope) string {
	if env == nil {
		return ""
	}
	return env.Subject
}

// headersFromEnvelope fills MessageHeaders from an IMAP envelope and
// the raw message bytes. The envelope is the primary source for
// From / To / Cc / Subject / Date; the raw bytes are scanned for
// fields the envelope does not provide (Message-ID, In-Reply-To,
// References). The decoded MIME headers, if available from a prior
// parse, take precedence over both.
func headersFromEnvelope(env *imap.Envelope, raw []byte) mailbox.MessageHeaders {
	out := mailbox.MessageHeaders{}
	if env != nil {
		out.From = envelopeFrom(env)
		out.To = envelopeAddresses(env, "To")
		out.Cc = envelopeAddresses(env, "Cc")
		out.Subject = env.Subject
		if !env.Date.IsZero() {
			out.Date = env.Date
		} else {
			out.Date = time.Now().UTC()
		}
	}
	// Fall back to scanning the raw header block. The first blank
	// line ends the header section; we only look at the headers
	// here, not the body.
	for _, line := range strings.Split(string(raw), "\r\n") {
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		switch key {
		case "message-id":
			if out.MessageID == "" {
				out.MessageID = val
			}
		case "in-reply-to":
			if out.InReplyTo == "" {
				out.InReplyTo = val
			}
		case "references":
			if len(out.References) == 0 {
				out.References = strings.Fields(val)
			}
		}
	}
	return out
}

// convertAttachments maps the mimeparse attachment view onto the
// internal mailbox.Attachment representation. The wire field set
// is the union of the two; consumers that only need the basics
// (Filename, MimeType, Size) can ignore the new Part / ContentID
// / Inline fields.
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

// isParseError reports whether the error, or any error it wraps, is a
// parse error produced by the IMAP reader.
//
// The fork's imap.IsParseError inspects the error itself: it does not
// unwrap. The executor wraps a reader error in its typed
// connection-closed error, so a parse error that failed inside
// ReadResp reaches the application one level down. Walking the chain
// finds it.
//
// The walk cannot mistake a transport failure for a parse error: the
// executor only records a cause when the reader reported a parse
// error, so a plain EOF, a timeout or a reset arrives without a cause
// and is left alone.
func isParseError(err error) bool {
	for err != nil {
		if imap.IsParseError(err) {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// mapErr converts a low-level go-imap error into one of the
// package-level typed errors. Any unrecognised error is returned
// verbatim so callers can format it; the server is the source of
// truth for human-readable strings.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	// mimeparse errors are wrapped through; unwrap to the typed
	// public errors so the tool layer can map them.
	if errors.Is(err, mimeparse.ErrMessageTooLarge) {
		return mailbox.ErrMessageTooLarge
	}
	if errors.Is(err, mimeparse.ErrNestingTooDeep) {
		return mailbox.ErrNestingTooDeep
	}
	if errors.Is(err, mimeparse.ErrTooManyParts) {
		return mailbox.ErrTooManyParts
	}
	if errors.Is(err, mimeparse.ErrTooManyAttachments) {
		return mailbox.ErrTooManyAttachments
	}
	if errors.Is(err, mimeparse.ErrAttachmentTooLarge) {
		return mailbox.ErrAttachmentTooLarge
	}
	if errors.Is(err, mimeparse.ErrTotalAttachmentTooLarge) {
		return mailbox.ErrTotalAttachmentTooLarge
	}
	if errors.Is(err, mimeparse.ErrAttachmentNotFound) {
		return mailbox.ErrAttachmentNotFound
	}
	if errors.Is(err, mimeparse.ErrMalformed) {
		return mailbox.ErrMalformed
	}
	// A decoder-limit violation is not a malformed response and not a
	// message-size violation: the response was well formed, but it
	// exceeded one of the reader's per-response resource limits. It is
	// checked before the parse-error branch below, because the
	// executor wraps a reader error in the connection-closed error.
	//
	// The wording is deliberately neutral. The violation can be any of
	// the decoder's limits - the number of fields, the size of an atom
	// or of a quoted string, the total number of bytes, or the nesting
	// depth of the response's lists - so neither narrowing the query
	// nor raising the scan budget necessarily resolves it, and the
	// message does not claim that it does.
	//
	// The message is fixed text: it never carries content from the
	// server's response.
	if errors.Is(err, imap.ErrResponseTooLarge) {
		return fmt.Errorf("%w: the server sent an IMAP response that exceeded the IMAP client's per-response limits "+
			"(the number of fields, the size of an atom or of a quoted string, the total size of the response, "+
			"or the nesting depth of its lists). The response was refused before it was processed, "+
			"so the connection was closed",
			mailbox.ErrResponseTooLarge)
	}
	// The IMAP literal-size evidence is concrete and typed.
	// The go-imap fork (github.com/flowbyte-com/go-imap)
	// returns imap.ErrLiteralSizeExceeded when the announced
	// literal size exceeds the protocol-level cap. The
	// executor wraps it in a typed connection-closed error,
	// so errors.Is finds it through the Unwrap chain. We
	// translate that to ErrMessageTooLarge. Generic parse
	// errors are NOT evidence of an oversized literal; they
	// are surfaced as ErrMalformed so a tool-layer caller can
	// distinguish a real protocol error from a size-cap
	// control that succeeded.
	if errors.Is(err, imap.ErrLiteralSizeExceeded) {
		return mailbox.ErrMessageTooLarge
	}
	msg := err.Error()
	if isParseError(err) {
		return mailbox.ErrMalformed
	}
	switch {
	case strings.HasPrefix(msg, "imap: connection closed"):
		return fmt.Errorf("imap: connection closed: %w", err)
	case strings.Contains(msg, "Mailbox doesn't exist"):
		return mailbox.ErrMailboxNotFound
	}
	return err
}
