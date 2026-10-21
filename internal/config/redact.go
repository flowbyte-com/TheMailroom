package config

import "strings"

// Redacted returns a printable representation of the configuration with
// every credential field replaced by "***". Use this for diagnostics
// and structured logging; never log the raw Config.
func (c Config) Redacted() string {
	var b strings.Builder
	b.WriteString("server{log_level=")
	b.WriteString(c.Server.LogLevel)
	b.WriteString(", max_request_bytes=")
	b.WriteString(itoa(c.Server.MaxRequestBytes))
	b.WriteString("} accounts=[")
	for i, a := range c.Accounts {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.redacted())
	}
	b.WriteString("]")
	return b.String()
}

func (a AccountConfig) redacted() string {
	return "{" +
		"id=" + a.ID +
		", email=" + a.Email +
		", backend=" + a.Backend +
		", provider=" + a.Provider +
		", imap=" + a.IMAP.redacted() +
		", smtp=" + a.SMTP.redacted() +
		", permissions=" + strings.Join(a.Permissions, ",") +
		", attachment_send_paths=" + strings.Join(a.AttachmentSendPaths, ",") +
		", mailbox_root=" + a.MailboxRoot +
		", limits=" + a.Limits.redacted() +
		"}"
}

func (i IMAPConfig) redacted() string {
	return "{host=" + i.Host +
		", port=" + itoa(i.Port) +
		", tls=" + boolStr(i.TLS) +
		", starttls=" + boolStr(i.StartTLS) +
		", username=" + i.Username +
		", password=***" +
		", password_command_set=" + boolStr(len(i.PasswordCommand) > 0) +
		"}"
}

func (s SMTPConfig) redacted() string {
	return "{host=" + s.Host +
		", port=" + itoa(s.Port) +
		", tls=" + boolStr(s.TLS) +
		", starttls=" + boolStr(s.StartTLS) +
		", username=" + s.Username +
		", password=***" +
		", password_command_set=" + boolStr(len(s.PasswordCommand) > 0) +
		"}"
}

func (l LimitsConfig) redacted() string {
	return "{max_message_bytes=" + itoa(l.MaxMessageBytes) +
		", max_attachment_bytes=" + itoa(l.MaxAttachmentBytes) +
		", max_total_attachment_bytes=" + itoa(l.MaxTotalAttachmentBytes) +
		", max_attachments=" + itoa(l.MaxAttachments) +
		", max_recipients=" + itoa(l.MaxRecipients) +
		", max_search_results=" + itoa(l.MaxSearchResults) +
		", max_batch_operations=" + itoa(l.MaxBatchOperations) +
		", max_mime_nesting=" + itoa(l.MaxMIMENesting) +
		", max_mime_parts=" + itoa(l.MaxMIMEParts) +
		"}"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
