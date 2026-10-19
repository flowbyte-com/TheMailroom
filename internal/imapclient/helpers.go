package imapclient

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"

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

// compactStrings drops empty entries from a list of strings. Some
// IMAP servers reject SEARCH with empty criteria, others accept them;
// the safe path is to send only non-empty values.
func compactStrings(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
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
	if errors.Is(err, mimeparse.ErrMalformed) {
		return mailbox.ErrMalformed
	}
	// The IMAP literal-size evidence is concrete. The only
	// string go-imap produces for an over-cap literal is
	// "literal exceeding maximum size" from its own
	// MaxLiteralSize check (which is not currently set on
	// the client; see the package doc for the security
	// limitation). We translate that single, specific
	// string to ErrMessageTooLarge. Generic parse errors
	// such as "expected a space" or "error reading response"
	// are NOT evidence of an oversized literal; they are
	// surfaced as ErrMalformed so a tool-layer caller can
	// distinguish a real protocol error from a size-cap
	// control that succeeded.
	msg := err.Error()
	if strings.Contains(msg, "literal exceeding maximum size") {
		return mailbox.ErrMessageTooLarge
	}
	if imap.IsParseError(err) {
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
