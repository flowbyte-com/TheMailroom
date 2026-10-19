package imapclient

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
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

// splitHeaders returns the header block as text plus the body
// bytes. The Mailbox.Message.Headers path expects pre-parsed values
// from the IMAP envelope where possible, so the raw headers are
// only used for fields the envelope does not provide (Message-ID,
// In-Reply-To, References).
func splitHeaders(raw []byte) (headersText string, body []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var b bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		b.WriteString(line)
		b.WriteString("\r\n")
		if line == "" {
			// End of headers.
			break
		}
	}
	return b.String(), raw[b.Len():]
}

// headersFromEnvelope fills MessageHeaders from an IMAP envelope and
// the raw header text. Fields the envelope does not provide are
// extracted by simple line scanning.
func headersFromEnvelope(env *imap.Envelope, headersText string) mailbox.MessageHeaders {
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
	// Extract Message-ID, In-Reply-To, References from raw headers.
	for _, line := range strings.Split(headersText, "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		switch key {
		case "message-id":
			out.MessageID = val
		case "in-reply-to":
			out.InReplyTo = val
		case "references":
			out.References = strings.Fields(val)
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
	// go-imap returns these as errors.New with a specific prefix.
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "imap: connection closed"):
		return fmt.Errorf("imap: connection closed: %w", err)
	case strings.Contains(msg, "Mailbox doesn't exist"):
		return mailbox.ErrMailboxNotFound
	}
	return err
}

// Unused but kept available for the SMTP and MIME slices that will
// follow; this prevents import drift if the file is later split.
var _ = errors.New
var _ = bytes.NewReader
