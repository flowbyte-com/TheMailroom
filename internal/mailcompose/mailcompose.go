// Package mailcompose builds bounded RFC 5322 / MIME messages.
//
// The composer is a pure, in-memory message-construction component. It
// accepts already-frozen attachments, never opens files, never talks to an
// SMTP server, and never writes to disk. The output is a complete serialized
// message and the generated Message-ID. The SMTP envelope is assembled
// separately by the SMTP sender; Bcc recipients are accepted for validation
// but are never serialized into the message headers.
package mailcompose

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/flowbyte-com/themailroom/internal/attachments"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/recipients"
)

// Errors returned by Compose. Callers should use errors.Is rather than
// matching strings.
var (
	// ErrInvalidMessage is returned for invalid message or header input.
	ErrInvalidMessage = errors.New("mailcompose: invalid message")

	// ErrInvalidAttachment is returned for invalid attachment metadata.
	ErrInvalidAttachment = errors.New("mailcompose: invalid attachment metadata")

	// ErrTooManyAttachments is returned when the attachment count exceeds
	// max_attachments.
	ErrTooManyAttachments = errors.New("mailcompose: too many attachments")

	// ErrAttachmentTooLarge is returned when one attachment exceeds
	// max_attachment_bytes.
	ErrAttachmentTooLarge = errors.New("mailcompose: attachment too large")

	// ErrTotalAttachmentTooLarge is returned when the combined attachment
	// bytes exceed max_total_attachment_bytes.
	ErrTotalAttachmentTooLarge = errors.New("mailcompose: total attachment size too large")

	// ErrMessageTooLarge is returned when the final serialized message
	// exceeds max_message_bytes.
	ErrMessageTooLarge = errors.New("mailcompose: serialized message too large")

	// ErrMIME is returned when MIME serialization fails for a reason other
	// than a caller input or size-limit error.
	ErrMIME = errors.New("mailcompose: MIME composition failed")

	// ErrInvalidLimits is returned when the supplied limits cannot be used.
	ErrInvalidLimits = errors.New("mailcompose: invalid limits")
)

// Request is the typed input to Compose. From must be the configured account
// identity, not an agent-supplied sender. To, Cc and Bcc are canonicalized
// with the same address parser and local-part-case rules as the recipient
// policy. Bcc is validated but deliberately omitted from serialized headers.
type Request struct {
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	Text        string
	HTML        string
	Attachments []attachments.File
}

// Result is a completely serialized message plus the Message-ID generated for
// it. MessageID is returned without angle brackets, matching the value exposed
// by mimeparse and suitable for reporting an ambiguous delivery outcome.
type Result struct {
	Message   []byte
	MessageID string
}

// Compose serializes req using the supplied per-account limits.
//
// The returned message is complete and ready for SMTP DATA. On any error the
// zero Result is returned: no partial message bytes are exposed. A message
// whose serialized size is exactly MaxMessageBytes succeeds; one byte larger
// fails with ErrMessageTooLarge.
func Compose(req Request, limits config.LimitsConfig) (result Result, err error) {
	// go-message panics if its multipart boundary generator cannot read
	// randomness. Treat any composition panic as a typed MIME failure rather
	// than allowing it to escape the composer boundary.
	defer func() {
		if recovered := recover(); recovered != nil {
			result = Result{}
			err = fmt.Errorf("%w: internal composition panic", ErrMIME)
		}
	}()
	return compose(req, limits, time.Now, generateMessageID)
}

// compose is the deterministic test seam. Production callers use Compose.
func compose(req Request, limits config.LimitsConfig, now func() time.Time, nextID func(time.Time, string) (string, error)) (Result, error) {
	if err := validateLimits(limits); err != nil {
		return Result{}, err
	}
	if err := validateHeaderPreparation(req, limits.MaxMessageBytes); err != nil {
		return Result{}, err
	}

	from, err := canonicalAddress("from", req.From)
	if err != nil {
		return Result{}, err
	}
	to, err := canonicalAddresses("to", req.To)
	if err != nil {
		return Result{}, err
	}
	cc, err := canonicalAddresses("cc", req.Cc)
	if err != nil {
		return Result{}, err
	}
	if _, err := canonicalAddresses("bcc", req.Bcc); err != nil {
		return Result{}, err
	}

	if err := validateHeaderText("subject", req.Subject); err != nil {
		return Result{}, err
	}
	if !utf8.ValidString(req.Text) {
		return Result{}, fmt.Errorf("%w: text body is not valid UTF-8", ErrInvalidMessage)
	}
	if !utf8.ValidString(req.HTML) {
		return Result{}, fmt.Errorf("%w: HTML body is not valid UTF-8", ErrInvalidMessage)
	}

	files, err := validateAttachments(req.Attachments, limits)
	if err != nil {
		return Result{}, err
	}
	if err := validatePreparedHeaders(from, to, cc, req.Subject, files, limits.MaxMessageBytes); err != nil {
		return Result{}, err
	}

	at := now()
	if at.IsZero() {
		return Result{}, fmt.Errorf("%w: message timestamp is zero", ErrMIME)
	}
	at = at.UTC()

	domain, err := addressDomain(from)
	if err != nil {
		return Result{}, err
	}
	messageID, err := nextID(at, domain)
	if err != nil {
		return Result{}, fmt.Errorf("%w: message-id generation failed", ErrMIME)
	}
	if err := validateGeneratedMessageID(messageID); err != nil {
		return Result{}, err
	}

	header := gomail.Header{}
	header.Set("From", formatAddressList([]string{from}))
	if len(to) > 0 {
		header.Set("To", formatAddressList(to))
	}
	if len(cc) > 0 {
		header.Set("Cc", formatAddressList(cc))
	}
	header.Set("Subject", encodeSubject(req.Subject))
	header.SetDate(at)
	header.SetMessageID(messageID)
	header.Set("MIME-Version", "1.0")

	var buf bytes.Buffer
	limited := &limitedWriter{dst: &buf, remaining: int64(limits.MaxMessageBytes)}
	if err := writeMessage(limited, header.Header, req, files); err != nil {
		return Result{}, mapComposeError(err)
	}

	return Result{Message: buf.Bytes(), MessageID: messageID}, nil
}

func validateLimits(limits config.LimitsConfig) error {
	if limits.MaxMessageBytes <= 0 {
		return fmt.Errorf("%w: max_message_bytes must be positive", ErrInvalidLimits)
	}
	if limits.MaxAttachmentBytes < 0 {
		return fmt.Errorf("%w: max_attachment_bytes must not be negative", ErrInvalidLimits)
	}
	if limits.MaxTotalAttachmentBytes < 0 {
		return fmt.Errorf("%w: max_total_attachment_bytes must not be negative", ErrInvalidLimits)
	}
	if limits.MaxAttachments < 0 {
		return fmt.Errorf("%w: max_attachments must not be negative", ErrInvalidLimits)
	}
	return nil
}

// validateHeaderPreparation bounds agent-controlled header input before any
// MIME, RFC 2047 or RFC 2231 encoding allocates expanded values. The final
// serialized-message limit is the budget: raw subject/filename bytes, the
// number of address values and the aggregate raw address input may not exceed
// it. Display names are included in the raw address input because parsing them
// is part of header preparation; recipient authorization remains in the
// existing recipient policy and is not reimplemented here.
func validateHeaderPreparation(req Request, maxMessageBytes int) error {
	max := int64(maxMessageBytes)

	if int64(len(req.Subject)) > max {
		return fmt.Errorf("%w: subject exceeds max_message_bytes", ErrMessageTooLarge)
	}
	if int64(len(req.Attachments)) > max {
		return fmt.Errorf("%w: attachment count exceeds max_message_bytes", ErrMessageTooLarge)
	}

	var filenameBytes int64
	for i, file := range req.Attachments {
		n := int64(len(file.Filename))
		if n > max-filenameBytes {
			return fmt.Errorf("%w: attachment[%d] filename exceeds max_message_bytes", ErrMessageTooLarge, i)
		}
		filenameBytes += n
	}

	addressCount := int64(1) // From
	addressCount += int64(len(req.To))
	addressCount += int64(len(req.Cc))
	addressCount += int64(len(req.Bcc))
	if addressCount > max {
		return fmt.Errorf("%w: address count exceeds max_message_bytes", ErrMessageTooLarge)
	}

	var addressBytes int64
	addAddresses := func(field string, values []string) error {
		for _, value := range values {
			n := int64(len(value))
			if n > max-addressBytes {
				return fmt.Errorf("%w: %s address input exceeds max_message_bytes", ErrMessageTooLarge, field)
			}
			addressBytes += n
		}
		return nil
	}
	if err := addAddresses("from", []string{req.From}); err != nil {
		return err
	}
	if err := addAddresses("to", req.To); err != nil {
		return err
	}
	if err := addAddresses("cc", req.Cc); err != nil {
		return err
	}
	if err := addAddresses("bcc", req.Bcc); err != nil {
		return err
	}
	return nil
}

// validatePreparedHeaders checks the canonical and decoded header values that
// will actually be handed to the MIME writer. Encoded subjects and RFC 2231
// filenames may expand further, but the writer still enforces the exact final
// serialized-message boundary. This pass bounds the list joins and ensures the
// prepared values alone cannot exceed the message budget.
func validatePreparedHeaders(from string, to, cc []string, subject string, files []attachmentInfo, maxMessageBytes int) error {
	max := int64(maxMessageBytes)
	total := int64(0)
	add := func(n int64) error {
		if n > max-total {
			return fmt.Errorf("%w: prepared header values exceed max_message_bytes", ErrMessageTooLarge)
		}
		total += n
		return nil
	}

	if err := add(int64(len(from)) + 2); err != nil { // From is always emitted as <addr>.
		return err
	}
	if err := add(int64(len(subject))); err != nil {
		return err
	}
	for _, file := range files {
		if err := add(int64(len(file.filename))); err != nil {
			return err
		}
	}
	addList := func(addrs []string) error {
		for i, addr := range addrs {
			n := int64(len(addr)) + 2 // <addr>
			if i > 0 {
				n += 2 // ", "
			}
			if err := add(n); err != nil {
				return err
			}
		}
		return nil
	}
	if err := addList(to); err != nil {
		return err
	}
	if err := addList(cc); err != nil {
		return err
	}
	return nil
}

func canonicalAddress(field, raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidMessage, field)
	}
	addr, err := recipients.Canonical(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrInvalidMessage, field, err)
	}
	if !utf8.ValidString(addr) {
		return "", fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidMessage, field)
	}
	// Keep a single address well below the RFC 5322 998-byte physical-line
	// limit. Addresses longer than this cannot be folded without changing
	// their value and are not valid RFC 5321 envelope addresses in practice.
	if len(addr) > 900 {
		return "", fmt.Errorf("%w: %s address is too long", ErrInvalidMessage, field)
	}
	return addr, nil
}

func canonicalAddresses(field string, raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		addr, err := canonicalAddress(field, value)
		if err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, nil
}

func validateHeaderText(field, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidMessage, field)
	}
	if recipients.HasControl(value) {
		return fmt.Errorf("%w: %s contains a control character", ErrInvalidMessage, field)
	}
	return nil
}

// encodeSubject leaves ordinary subjects readable, uses RFC 2047 for
// non-ASCII text, and force-encodes an overlong unbreakable ASCII word. The
// last case matters because the header writer cannot fold a whitespace-free
// run longer than its hard line limit without inserting a space into the
// subject.
func encodeSubject(subject string) string {
	if encoded := mime.QEncoding.Encode("utf-8", subject); encoded != subject {
		return encoded
	}
	if !subjectNeedsForcedEncoding(subject) {
		return subject
	}
	const (
		encodedWordPrefix = "=?utf-8?q?"
		encodedWordSuffix = "?="
		maxPayload        = 63 // leaves room for prefix and suffix under 75
	)
	var words []string
	var payload strings.Builder
	flush := func() {
		if payload.Len() > 0 {
			words = append(words, encodedWordPrefix+payload.String()+encodedWordSuffix)
			payload.Reset()
		}
	}
	for _, r := range subject {
		encoded := qEncodeRune(r)
		if payload.Len()+len(encoded) > maxPayload {
			flush()
		}
		payload.WriteString(encoded)
	}
	flush()
	return strings.Join(words, " ")
}

func subjectNeedsForcedEncoding(subject string) bool {
	// Header folding treats runs of whitespace specially. Encoding a
	// subject with repeated spaces preserves the exact value once the field
	// is long enough to be folded.
	if len(subject) > 60 && strings.Contains(subject, "  ") {
		return true
	}
	if len(subject) <= 980 {
		return false
	}
	run := 0
	for _, r := range subject {
		if r == ' ' || r == '\t' {
			run = 0
			continue
		}
		run++
		if run > 980 {
			return true
		}
	}
	return false
}

func qEncodeRune(r rune) string {
	if r == ' ' {
		return "_"
	}
	if r >= 33 && r <= 126 && r != '=' && r != '?' && r != '_' {
		return string(r)
	}
	var encoded strings.Builder
	for _, b := range []byte(string(r)) {
		fmt.Fprintf(&encoded, "=%02X", b)
	}
	return encoded.String()
}

type attachmentInfo struct {
	filename    string
	data        []byte
	contentType string
	params      map[string]string
}

func validateAttachments(files []attachments.File, limits config.LimitsConfig) ([]attachmentInfo, error) {
	if len(files) > limits.MaxAttachments {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooManyAttachments, len(files), limits.MaxAttachments)
	}

	out := make([]attachmentInfo, 0, len(files))
	var total int64
	for i, file := range files {
		if err := validateFilename(file.Filename); err != nil {
			return nil, fmt.Errorf("%w: attachment[%d]", err, i)
		}
		size := int64(len(file.Data))
		if size > int64(limits.MaxAttachmentBytes) {
			return nil, fmt.Errorf("%w: attachment[%d] exceeds max_attachment_bytes", ErrAttachmentTooLarge, i)
		}
		total += size
		if total > int64(limits.MaxTotalAttachmentBytes) {
			return nil, fmt.Errorf("%w: total exceeds max_total_attachment_bytes", ErrTotalAttachmentTooLarge)
		}
		contentType, params := attachmentContentType(file.Filename)
		out = append(out, attachmentInfo{
			filename:    file.Filename,
			data:        file.Data,
			contentType: contentType,
			params:      params,
		})
	}
	return out, nil
}

func validateFilename(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("%w: empty or reserved filename", ErrInvalidAttachment)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: filename is not valid UTF-8", ErrInvalidAttachment)
	}
	if recipients.HasControl(name) {
		return fmt.Errorf("%w: filename contains a control character", ErrInvalidAttachment)
	}
	if strings.ContainsAny(name, `/\`) ||
		filepath.IsAbs(name) ||
		filepath.VolumeName(name) != "" {
		return fmt.Errorf("%w: filename must be a basename", ErrInvalidAttachment)
	}
	return nil
}

// attachmentContentTypes is deliberately a fixed in-process table. The
// standard library's mime.TypeByExtension may read the host MIME database on
// first use, which would make composition depend on filesystem access and on
// host configuration. Unknown extensions fall back to the opaque binary type.
var attachmentContentTypes = map[string]string{
	".7z":   "application/x-7z-compressed",
	".avi":  "video/x-msvideo",
	".bmp":  "image/bmp",
	".bz2":  "application/x-bzip2",
	".css":  "text/css; charset=utf-8",
	".csv":  "text/csv; charset=utf-8",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".eml":  "message/rfc822",
	".gif":  "image/gif",
	".gz":   "application/gzip",
	".htm":  "text/html; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".ico":  "image/vnd.microsoft.icon",
	".ics":  "text/calendar; charset=utf-8",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json",
	".log":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".mp3":  "audio/mpeg",
	".mp4":  "video/mp4",
	".odp":  "application/vnd.oasis.opendocument.presentation",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odt":  "application/vnd.oasis.opendocument.text",
	".oga":  "audio/ogg",
	".ogg":  "audio/ogg",
	".ogv":  "video/ogg",
	".opus": "audio/ogg",
	".pdf":  "application/pdf",
	".png":  "image/png",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".rtf":  "application/rtf",
	".svg":  "image/svg+xml",
	".tar":  "application/x-tar",
	".text": "text/plain; charset=utf-8",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".tsv":  "text/tab-separated-values; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".vcf":  "text/vcard; charset=utf-8",
	".wav":  "audio/wav",
	".webm": "video/webm",
	".webp": "image/webp",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xml":  "application/xml",
	".xz":   "application/x-xz",
	".zip":  "application/zip",
}

func attachmentContentType(filename string) (string, map[string]string) {
	raw := attachmentContentTypes[strings.ToLower(filepath.Ext(filename))]
	if raw == "" {
		return "application/octet-stream", nil
	}
	mediaType, params, err := mime.ParseMediaType(raw)
	if err != nil || mediaType == "" {
		return "application/octet-stream", nil
	}
	return mediaType, params
}

// contentDispositionValue uses mime.FormatMediaType for ordinary filenames,
// but emits RFC 2231 continuations for long or internationalized names. A
// single long parameter value has no legal fold point, and the go-message
// header writer would otherwise insert whitespace inside it, corrupting the
// filename. Each continuation is kept well under the header line limit so the
// writer can fold at the parameter separators.
func contentDispositionValue(filename string) (string, error) {
	formatted := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if formatted == "" {
		return "", fmt.Errorf("%w: filename cannot be encoded", ErrInvalidAttachment)
	}
	// A short value, or a long value with no whitespace, can be written as
	// one legal line (or folded at "attachment;") without changing the
	// filename. A long value containing whitespace could be folded inside a
	// quoted string, collapsing repeated spaces, so use RFC 2231
	// continuations for it. The same applies above the safe line limit.
	if len(formatted) <= 70 ||
		(len(formatted) <= 900 && !strings.ContainsAny(filename, " \t")) {
		return formatted, nil
	}
	return "attachment; " + encodeRFC2231Parameter("filename", filename), nil
}

const rfc2231Chunk = 50

func encodeRFC2231Parameter(name, value string) string {
	var pieces []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			pieces = append(pieces, current.String())
			current.Reset()
		}
	}
	for _, r := range value {
		// Encode one complete rune at a time so a continuation boundary
		// never splits a multi-byte UTF-8 character.
		var encoded strings.Builder
		for _, b := range []byte(string(r)) {
			if rfc2231AttrChar(b) {
				encoded.WriteByte(b)
			} else {
				fmt.Fprintf(&encoded, "%%%02X", b)
			}
		}
		if current.Len()+encoded.Len() > rfc2231Chunk {
			flush()
		}
		current.WriteString(encoded.String())
	}
	flush()
	if len(pieces) == 0 {
		pieces = []string{""}
	}
	if len(pieces) == 1 {
		return name + "*=utf-8''" + pieces[0]
	}
	var b strings.Builder
	for i, piece := range pieces {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s*%d*=", name, i)
		if i == 0 {
			b.WriteString("utf-8''")
		}
		b.WriteString(piece)
	}
	return b.String()
}

func rfc2231AttrChar(b byte) bool {
	if 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' {
		return true
	}
	switch b {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

func addressDomain(addr string) (string, error) {
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return "", fmt.Errorf("%w: malformed sender address", ErrInvalidMessage)
	}
	return addr[at+1:], nil
}

func generateMessageID(at time.Time, domain string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x.%x@%s", at.UnixNano(), nonce[:], domain), nil
}

func validateGeneratedMessageID(id string) error {
	if id == "" ||
		!utf8.ValidString(id) ||
		recipients.HasControl(id) ||
		strings.ContainsAny(id, "<> \t\r\n") {
		return fmt.Errorf("%w: invalid generated message-id", ErrMIME)
	}
	return nil
}

func formatAddressList(addrs []string) string {
	out := make([]string, len(addrs))
	for i, addr := range addrs {
		// Canonical has already removed any display name and rejected
		// whitespace/controls. Angle brackets preserve the addr-spec exactly,
		// including quoted local-parts, without re-quoting it.
		out[i] = "<" + addr + ">"
	}
	return strings.Join(out, ", ")
}

func writeMessage(w io.Writer, header message.Header, req Request, files []attachmentInfo) error {
	hasText := req.Text != ""
	hasHTML := req.HTML != ""
	alternative := hasText && hasHTML

	if len(files) == 0 {
		switch {
		case alternative:
			return writeAlternative(w, header, req.Text, req.HTML)
		case hasHTML:
			return writeSingle(w, header, "text/html", req.HTML)
		default:
			return writeSingle(w, header, "text/plain", req.Text)
		}
	}

	header.SetContentType("multipart/mixed", nil)
	mw, err := message.CreateWriter(w, header)
	if err != nil {
		return err
	}
	if alternative {
		if err := writeAlternativePart(mw, req.Text, req.HTML); err != nil {
			return err
		}
	} else if hasHTML {
		if err := writeTextPart(mw, "text/html", req.HTML); err != nil {
			return err
		}
	} else {
		if err := writeTextPart(mw, "text/plain", req.Text); err != nil {
			return err
		}
	}
	for _, file := range files {
		if err := writeAttachmentPart(mw, file); err != nil {
			return err
		}
	}
	return mw.Close()
}

func writeSingle(w io.Writer, header message.Header, mediaType, body string) error {
	header.SetContentType(mediaType, map[string]string{"charset": "utf-8"})
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	mw, err := message.CreateWriter(w, header)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(mw, body); err != nil {
		return err
	}
	return mw.Close()
}

func writeAlternative(w io.Writer, header message.Header, text, html string) error {
	header.SetContentType("multipart/alternative", nil)
	mw, err := message.CreateWriter(w, header)
	if err != nil {
		return err
	}
	if err := writeTextPart(mw, "text/plain", text); err != nil {
		return err
	}
	if err := writeTextPart(mw, "text/html", html); err != nil {
		return err
	}
	return mw.Close()
}

func writeAlternativePart(parent *message.Writer, text, html string) error {
	header := message.Header{}
	header.SetContentType("multipart/alternative", nil)
	mw, err := parent.CreatePart(header)
	if err != nil {
		return err
	}
	if err := writeTextPart(mw, "text/plain", text); err != nil {
		return err
	}
	if err := writeTextPart(mw, "text/html", html); err != nil {
		return err
	}
	return mw.Close()
}

func writeTextPart(parent *message.Writer, mediaType, body string) error {
	header := message.Header{}
	header.SetContentType(mediaType, map[string]string{"charset": "utf-8"})
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := parent.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(part, body); err != nil {
		return err
	}
	return part.Close()
}

func writeAttachmentPart(parent *message.Writer, file attachmentInfo) error {
	header := message.Header{}
	header.SetContentType(file.contentType, file.params)
	disposition, err := contentDispositionValue(file.filename)
	if err != nil {
		return err
	}
	header.Set("Content-Disposition", disposition)
	header.Set("Content-Transfer-Encoding", "base64")

	part, err := parent.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(file.data); err != nil {
		return err
	}
	return part.Close()
}

type limitedWriter struct {
	dst       io.Writer
	remaining int64
	err       error
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = ErrMessageTooLarge
		return 0, w.err
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

func mapComposeError(err error) error {
	switch {
	case errors.Is(err, ErrMessageTooLarge),
		errors.Is(err, ErrInvalidAttachment),
		errors.Is(err, ErrMIME):
		return err
	default:
		return fmt.Errorf("%w: serialization failed", ErrMIME)
	}
}
