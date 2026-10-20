// Package mimeparse decodes RFC 5322 messages into a structured
// representation safe to surface to MCP tools. It is built on
// github.com/emersion/go-message which handles charset, transfer
// encoding, multipart nesting and RFC 2047 header decoding.
//
// The package is read-only: it never writes to disk, never resends
// anything and never interprets message content. The untrusted body
// is returned as a string (decoded to UTF-8 where possible) and the
// agent is responsible for any rendering. The package does not
// concatenate parts into a "natural language" blob that could be
// confused with instructions.
//
// Limits enforced:
//
//   - MaxMessageBytes caps the total raw RFC 5322 byte size the
//     parser will read. It is checked before header parsing.
//   - MaxHeaderBytes caps the header block. Bodies can be much
//     smaller than the raw size, but pathological headers are still
//     rejected up front.
//   - MaxMIMENesting caps the depth of multipart/* nesting. A
//     multipart structure deeper than this returns ErrNestingTooDeep.
//   - MaxMIMEParts caps the total number of MIME parts visited
//     during a single decode. zip / multipart bombs are rejected
//     before they can fan out into large memory consumption.
//   - MaxAttachmentBytes caps the size of any single attachment.
//     The cap is enforced by a counting reader; large attachments
//     return ErrAttachmentTooLarge and the body is released.
//   - MaxTotalAttachmentBytes caps the sum of decoded attachment
//     sizes. Exceeding it returns ErrTotalAttachmentTooLarge.
//
// Parse exposes attachment metadata and a per-part reference
// (mailbox + UIDVALIDITY + UID + part path); it does not retain
// attachment bodies. ExtractAttachment retains at most the one
// selected attachment while traversing and returns it only after the
// complete message and every limit have been validated. Bodies are
// read through a counting reader that stops as soon as the cap is
// exceeded, so a multi-gigabyte attachment consumes only a small
// amount of memory before being rejected.
package mimeparse

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message"
	// Register the charset reader so non-UTF-8 messages decode.
	// The blank import has the side effect of populating
	// message.CharsetReader.
	_ "github.com/emersion/go-message/charset"
	gomail "github.com/emersion/go-message/mail"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// keep bufio import for future use; the helper uses message.Read
// directly.
var _ = bytes.NewReader

// Limits caps the parser. The zero value uses safe defaults; pass a
// custom config to override per call.
type Limits struct {
	// MaxMessageBytes caps the total raw message size. Zero means
	// the default (25 MiB). Negative means no cap (use only in
	// tests).
	MaxMessageBytes int

	// MaxHeaderBytes caps the header block. Zero means go-message
	// default (1 MiB). Negative means no cap.
	MaxHeaderBytes int64

	// MaxMIMENesting caps multipart depth. Zero defaults to 16.
	MaxMIMENesting int

	// MaxMIMEParts caps total parts visited. Zero defaults to 500.
	MaxMIMEParts int

	// MaxAttachments caps the number of attachments the parser
	// will return. Zero defaults to 50.
	MaxAttachments int

	// MaxAttachmentBytes caps a single attachment. Zero defaults to
	// 6 MiB.
	MaxAttachmentBytes int

	// MaxTotalAttachmentBytes caps the sum of all attachments. Zero
	// defaults to 25 MiB.
	MaxTotalAttachmentBytes int
}

func (l Limits) withDefaults() Limits {
	if l.MaxMessageBytes == 0 {
		l.MaxMessageBytes = 25 * 1024 * 1024
	}
	if l.MaxMIMENesting == 0 {
		l.MaxMIMENesting = 16
	}
	if l.MaxMIMEParts == 0 {
		l.MaxMIMEParts = 500
	}
	if l.MaxAttachments == 0 {
		l.MaxAttachments = 50
	}
	if l.MaxAttachmentBytes == 0 {
		l.MaxAttachmentBytes = 6 * 1024 * 1024
	}
	if l.MaxTotalAttachmentBytes == 0 {
		l.MaxTotalAttachmentBytes = 25 * 1024 * 1024
	}
	return l
}

// Public errors. The tools layer maps them onto typed MCP errors.
var (
	ErrMessageTooLarge         = errors.New("mime: message exceeds size limit")
	ErrNestingTooDeep          = errors.New("mime: nesting exceeds limit")
	ErrTooManyParts            = errors.New("mime: part count exceeds limit")
	ErrTooManyAttachments      = errors.New("mime: attachment count exceeds limit")
	ErrAttachmentTooLarge      = errors.New("mime: attachment exceeds size limit")
	ErrTotalAttachmentTooLarge = errors.New("mime: total attachment size exceeds limit")
	ErrAttachmentNotFound      = errors.New("mime: attachment not found")
	ErrMalformed               = errors.New("mime: malformed message")
)

// Attachment is the structured view of a single MIME part that the
// caller considers an attachment. The Part field is the IMAP-style
// part path (1-indexed) and ContentID is taken from the
// Content-ID header. Filename and MimeType are decoded in a
// charset-aware manner. Size is the decoded size in bytes; the
// server never returns the raw attachment body inline.
type Attachment struct {
	Part        string `json:"part"`
	Filename    string `json:"filename,omitempty"`
	MimeType    string `json:"mime_type"`
	Size        int64  `json:"size"`
	ContentID   string `json:"content_id,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	IsText      bool   `json:"is_text"`
	IsInline    bool   `json:"is_inline"`
}

// Decoded is the parser result. Headers are normalised; Text and
// HTML are decoded to UTF-8 where possible and represent the
// best-effort view the agent will use for display. Attachments
// carry metadata only.
type Decoded struct {
	Headers     mailbox.MessageHeaders `json:"headers"`
	Text        string                 `json:"text,omitempty"`
	HTML        string                 `json:"html,omitempty"`
	Attachments []Attachment           `json:"attachments,omitempty"`
	Truncated   bool                   `json:"truncated,omitempty"`
	Warnings    []string               `json:"warnings,omitempty"`
}

// AttachmentContent is the selected attachment's metadata and decoded
// bytes. Content is transfer-decoded; text parts are charset-decoded
// to UTF-8 where possible, so text content is not guaranteed to be
// byte-identical to the sender's original file.
type AttachmentContent struct {
	Attachment
	Content  []byte
	Warnings []string
}

// Parse decodes a single message. The raw input is the full RFC 5322
// bytes (headers + body). Limits are applied as described on the
// package doc; see Errors for the typed failures.
//
// Parse returns attachment metadata only. Use ExtractAttachment to
// materialise one selected attachment.
func Parse(raw []byte, limits Limits) (*Decoded, error) {
	return parse(raw, limits, "", nil)
}

// ExtractAttachment decodes a message, validates the complete MIME
// structure and every resource limit, and returns the decoded content
// of the single attachment whose part path matches part. It never
// returns partial content: if any later part fails validation, the
// whole call fails.
//
// part must be the 1-indexed IMAP-style path returned in
// Decoded.Attachments[].Part. Multipart containers, ordinary body
// parts and parts hidden inside an opaque message/rfc822 attachment
// are not attachments and return ErrAttachmentNotFound.
func ExtractAttachment(raw []byte, part string, limits Limits) (*AttachmentContent, error) {
	if part == "" {
		return nil, ErrAttachmentNotFound
	}
	capture := &attachmentCapture{}
	d, err := parse(raw, limits, part, capture)
	if err != nil {
		return nil, err
	}
	if !capture.found {
		return nil, ErrAttachmentNotFound
	}
	for _, a := range d.Attachments {
		if a.Part == part {
			return &AttachmentContent{
				Attachment: a,
				Content:    capture.content,
				Warnings:   d.Warnings,
			}, nil
		}
	}
	return nil, ErrAttachmentNotFound
}

type attachmentCapture struct {
	content []byte
	found   bool
}

// parse is the shared traversal used by Parse and ExtractAttachment.
// When capture is non-nil, the decoded bytes of the attachment whose
// part path equals capturePart are recorded. Traversal continues after
// capture so later sibling limits still apply.
func parse(raw []byte, limits Limits, capturePart string, capture *attachmentCapture) (*Decoded, error) {
	limits = limits.withDefaults()
	if limits.MaxMessageBytes > 0 && len(raw) > limits.MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	opts := &message.ReadOptions{MaxHeaderBytes: limits.MaxHeaderBytes}
	rootEntity, err := message.ReadWithOptions(bytes.NewReader(raw), opts)
	var warnings []string
	if err != nil {
		if isHeaderTooBig(err) {
			return nil, ErrMessageTooLarge
		}
		if !(message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)) {
			return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		// Soft error: the entity is still usable, but we surface
		// the issue as a warning so the agent knows the body or
		// some headers were not decoded.
		warnings = append(warnings, err.Error())
	}

	d := &Decoded{Headers: mailbox.MessageHeaders{}}
	partCount := 0
	totalAttachmentBytes := int64(0)
	attachmentCount := 0
	var text, html string
	var atts []Attachment
	appendAttachment := func(a Attachment, data []byte) {
		atts = append(atts, a)
		if capture != nil && capturePart == a.Part {
			capture.content = data
			capture.found = true
		}
	}
	// attachmentLimit is the per-attachment cap (int64 for the
	// counting reader).
	attachmentCap := int64(limits.MaxAttachmentBytes)
	// textCap bounds the captured text/html bodies so a giant
	// text/plain part is still rejected (the same cap as
	// attachments).
	textCap := attachmentCap

	walkErr := rootEntity.Walk(func(path []int, entity *message.Entity, err error) error {
		if err != nil && (message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)) {
			warnings = append(warnings, err.Error())
			err = nil
		}
		if err != nil {
			warnings = append(warnings, "part: "+err.Error())
			return nil
		}
		// Depth: the root has a nil path and is depth 0; the
		// children of a multipart at depth D have paths of
		// length D, so depth = len(path)+1.
		if len(path)+1 > limits.MaxMIMENesting {
			return ErrNestingTooDeep
		}
		partCount++
		if partCount > limits.MaxMIMEParts {
			return ErrTooManyParts
		}

		mediaType, mediaParams, _ := entity.Header.ContentType()
		disp, dispParams, _ := entity.Header.ContentDisposition()
		cid := strings.Trim(entity.Header.Get("Content-ID"), "<> ")

		// The root entity carries the top-level headers.
		if len(path) == 0 {
			mh := gomail.Header{Header: entity.Header}
			date, derr := mh.Date()
			if derr == nil {
				d.Headers.Date = date
			} else {
				d.Headers.Date = time.Now().UTC()
			}
			if v, _ := mh.MessageID(); v != "" {
				d.Headers.MessageID = v
			}
			if v, _ := mh.Subject(); v != "" {
				d.Headers.Subject = v
			}
			if v, _ := mh.AddressList("From"); len(v) > 0 {
				d.Headers.From = formatAddress(v[0])
			}
			if v, _ := mh.AddressList("To"); len(v) > 0 {
				d.Headers.To = formatAddressList(v)
			}
			if v, _ := mh.AddressList("Cc"); len(v) > 0 {
				d.Headers.Cc = formatAddressList(v)
			}
			if v, _ := entity.Header.Text("In-Reply-To"); v != "" {
				d.Headers.InReplyTo = v
			}
			if v, _ := mh.MsgIDList("References"); len(v) > 0 {
				d.Headers.References = v
			}
		}

		// Multipart containers are walked into; nothing to
		// capture for the container itself.
		if strings.HasPrefix(mediaType, "multipart/") {
			return nil
		}

		// Filename resolution: prefer Content-Disposition, then
		// Content-Type name.
		filename := dispParams["filename"]
		if filename == "" {
			filename = mediaParams["name"]
		}
		// The mail.AttachmentHeader helper decodes any RFC 2047
		// encoded-word in the filename.
		ah := gomail.AttachmentHeader{Header: entity.Header}
		if dec, ferr := ah.Filename(); ferr == nil && dec != "" {
			filename = dec
		}
		filename = SanitizeFilename(filename)

		isAttachment := disp == "attachment"
		isInline := disp == "inline"
		// Heuristic for "looks like an attachment" when no
		// Content-Disposition is set: a non-text part with a
		// filename is treated as an attachment. We do not
		// promote a part that explicitly declared itself
		// inline to an attachment.
		if !isAttachment && !isInline && filename != "" && !strings.HasPrefix(mediaType, "text/") {
			isAttachment = true
		}

		// Read the body, respecting the per-attachment cap.
		// We use the same LimitReader + 1 trick as
		// ParseFromReader: read up to cap+1 bytes; if we
		// got more than cap, the body exceeded the cap.
		cap := attachmentCap
		if !isAttachment {
			cap = textCap
		}
		limited := io.LimitReader(entity.Body, cap+1)
		data, rerr := io.ReadAll(limited)
		if rerr != nil {
			warnings = append(warnings, fmt.Sprintf("part read: %v", rerr))
			return nil
		}
		if int64(len(data)) > cap {
			if !isAttachment {
				// For text parts we still surface what we
				// read (truncated) and set Truncated: true.
				d.Truncated = true
				if isTextPlain(mediaType) {
					text = string(data)
				} else if isTextHTML(mediaType) {
					html = string(data)
				}
				return nil
			}
			return ErrAttachmentTooLarge
		}

		if isAttachment {
			if totalAttachmentBytes+int64(len(data)) > int64(limits.MaxTotalAttachmentBytes) {
				return ErrTotalAttachmentTooLarge
			}
			if attachmentCount+1 > limits.MaxAttachments {
				return ErrTooManyAttachments
			}
			totalAttachmentBytes += int64(len(data))
			attachmentCount++
			appendAttachment(Attachment{
				Part:        partPath(path),
				Filename:    filename,
				MimeType:    mediaType,
				Size:        int64(len(data)),
				ContentID:   cid,
				Disposition: "attachment",
				IsText:      isTextLike(mediaType),
				IsInline:    false,
			}, data)
			return nil
		}

		// Inline (text) part.
		if isTextPlain(mediaType) {
			// An explicit Content-Disposition: attachment with
			// a filename on a text/plain part is an
			// attachment (e.g. a .txt file attached to the
			// message), not the rendered body.
			if disp == "attachment" && filename != "" {
				if attachmentCount+1 > limits.MaxAttachments {
					return ErrTooManyAttachments
				}
				if totalAttachmentBytes+int64(len(data)) > int64(limits.MaxTotalAttachmentBytes) {
					return ErrTotalAttachmentTooLarge
				}
				attachmentCount++
				totalAttachmentBytes += int64(len(data))
				appendAttachment(Attachment{
					Part:        partPath(path),
					Filename:    filename,
					MimeType:    mediaType,
					Size:        int64(len(data)),
					ContentID:   cid,
					Disposition: "attachment",
					IsText:      true,
					IsInline:    false,
				}, data)
				return nil
			}
			text = string(data)
		} else if isTextHTML(mediaType) {
			// Same as text/plain: explicit attachment
			// disposition + filename is an attachment, not
			// the rendered body.
			if disp == "attachment" && filename != "" {
				if attachmentCount+1 > limits.MaxAttachments {
					return ErrTooManyAttachments
				}
				if totalAttachmentBytes+int64(len(data)) > int64(limits.MaxTotalAttachmentBytes) {
					return ErrTotalAttachmentTooLarge
				}
				attachmentCount++
				totalAttachmentBytes += int64(len(data))
				appendAttachment(Attachment{
					Part:        partPath(path),
					Filename:    filename,
					MimeType:    mediaType,
					Size:        int64(len(data)),
					ContentID:   cid,
					Disposition: "attachment",
					IsText:      true,
					IsInline:    false,
				}, data)
				return nil
			}
			html = string(data)
		} else if isInline {
			// Inline non-text part. We retain every inline
			// part (including those without a filename) so
			// the agent can render Content-ID references in
			// the HTML body. The Content-ID is the stable
			// wire identifier; filename is best-effort.
			if attachmentCount+1 > limits.MaxAttachments {
				return ErrTooManyAttachments
			}
			if totalAttachmentBytes+int64(len(data)) > int64(limits.MaxTotalAttachmentBytes) {
				return ErrTotalAttachmentTooLarge
			}
			attachmentCount++
			totalAttachmentBytes += int64(len(data))
			appendAttachment(Attachment{
				Part:        partPath(path),
				Filename:    filename, // may be empty for unnamed inline parts
				MimeType:    mediaType,
				Size:        int64(len(data)),
				ContentID:   cid, // may be empty
				Disposition: "inline",
				IsText:      isTextLike(mediaType),
				IsInline:    true,
			}, data)
		}
		// Anything else (e.g. an application/octet-stream
		// without a Content-Disposition) is intentionally
		// dropped: per RFC it cannot be displayed, and we do
		// not want to expose arbitrary bytes as "body".
		return nil
	})

	d.Text = text
	d.HTML = html
	d.Attachments = atts
	d.Warnings = warnings
	if walkErr != nil {
		// Mark the partial decode as truncated so callers
		// cannot mistake it for a complete read. The error
		// is returned alongside the partial decode; callers
		// that want a strict "no partial on failure"
		// contract should discard the decode themselves.
		d.Truncated = true
		d.Warnings = append(d.Warnings, walkErr.Error())
		return d, walkErr
	}
	return d, nil
}

// ParseFromReader is like Parse but reads the body incrementally so
// the caller can stream from a network source. The same Limits
// apply; the overall size cap is enforced on the input via a
// LimitReader, so a hostile server cannot stream gigabytes
// through. A message whose size is *exactly* the cap is valid
// and is parsed successfully; a message that exceeds the cap is
// rejected with ErrMessageTooLarge.
func ParseFromReader(r io.Reader, limits Limits) (*Decoded, error) {
	limits = limits.withDefaults()
	cap := int64(limits.MaxMessageBytes)
	if cap < 0 {
		cap = 0
	}
	// io.LimitReader returns io.EOF after cap bytes. If the
	// source is exactly cap bytes, ReadAll returns those
	// bytes plus io.EOF (io.ReadAll treats io.EOF as
	// success). To distinguish "source is exactly cap" from
	// "source is larger than cap" we read one extra byte
	// beyond the cap; if the extra byte is present the
	// source was longer.
	limited := io.LimitReader(r, cap+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("mime: read: %w", err)
	}
	if int64(len(raw)) > cap {
		return nil, ErrMessageTooLarge
	}
	return Parse(raw, limits)
}

func isTextPlain(mt string) bool { return strings.HasPrefix(mt, "text/plain") }
func isTextHTML(mt string) bool  { return strings.HasPrefix(mt, "text/html") }
func isTextLike(mt string) bool  { return strings.HasPrefix(mt, "text/") }

// partPath renders an IMAP-style 1-indexed path. go-message uses
// 0-indexed paths internally; we 1-index to match IMAP so an
// attachment reference is stable across the wire.
func partPath(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = strconv.Itoa(p + 1)
	}
	return strings.Join(parts, ".")
}

func formatAddress(a *gomail.Address) string {
	if a == nil {
		return ""
	}
	if a.Name != "" {
		return a.Name + " <" + a.Address + ">"
	}
	return a.Address
}

func formatAddressList(addrs []*gomail.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a == nil {
			continue
		}
		out = append(out, formatAddress(a))
	}
	return out
}

// isHeaderTooBig detects the unexported go-message error by
// string. The library does not expose a sentinel, so a best-effort
// match is the only portable way.
func isHeaderTooBig(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "header exceeds") || strings.Contains(s, "exceeds maximum size")
}

// SanitizeFilename strips path separators and NUL bytes from a
// filename returned in attachment metadata. We never use the raw
// filename for filesystem operations; this is defence in depth in
// case a future tool surfaces the filename in a context that does.
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

// MIMEInfo is a small helper that returns the top-level
// Content-Type and parameters for a raw message, without walking
// the body. It is used by callers that want to log the structure
// before parsing.
func MIMEInfo(raw []byte) (mediaType string, params map[string]string, err error) {
	if len(raw) == 0 {
		return "", nil, errors.New("mime: empty message")
	}
	// Use a header parser that mirrors go-message's behaviour.
	e, err := message.ReadWithOptions(bytes.NewReader(raw), &message.ReadOptions{MaxHeaderBytes: -1})
	if err != nil && !(message.IsUnknownCharset(err) || message.IsUnknownEncoding(err)) {
		return "", nil, fmt.Errorf("mime: read header: %w", err)
	}
	ct := e.Header.Get("Content-Type")
	if ct == "" {
		return "text/plain", map[string]string{"charset": "us-ascii"}, nil
	}
	return mime.ParseMediaType(ct)
}
