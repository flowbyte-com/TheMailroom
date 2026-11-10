package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
)

// LocalResult is the outcome of the local checks.
type LocalResult struct {
	// Path is the absolute path the checks examined.
	Path string
	// Config is the validated configuration. It is meaningful only when
	// Loaded is true.
	Config config.Config
	// Loaded reports whether the configuration parsed and validated.
	Loaded bool
	// Checks holds every local result.
	Checks []Check
}

// RunLocal performs the local diagnostics for the configuration at path.
//
// It never touches the network and never executes a password_command. A
// credential source is reported by kind only; the secret and the arguments
// of the command are never read into the output.
func RunLocal(path string) LocalResult {
	var out LocalResult

	// 1. Discovery, existence and permissions.
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	out.Path = abs
	info, statErr := os.Stat(abs)
	if statErr != nil {
		out.Checks = append(out.Checks, Check{
			Name:   "config.file",
			Status: StatusFail,
			Detail: fmt.Sprintf("%s is not readable: %s", abs, classifyErr(statErr)),
			Remedy: "pass -config with the path of the configuration file",
		})
		out.Checks = append(out.Checks, Check{
			Name:   "config.parse",
			Status: StatusSkip,
			Detail: "not attempted because the file could not be read",
		})
		return out
	}
	out.Checks = append(out.Checks, Check{
		Name:   "config.file",
		Status: StatusOK,
		Detail: fmt.Sprintf("%s (%d bytes)", abs, info.Size()),
	})
	out.Checks = append(out.Checks, checkFilePermissions(abs, info))

	// 2. Parsing and validation, through the loader the server uses.
	cfg, err := config.Load(abs)
	if err != nil {
		out.Checks = append(out.Checks, Check{
			Name:   "config.parse",
			Status: StatusFail,
			Detail: "configuration is invalid: " + redactConfigError(err.Error(), abs),
			Remedy: "correct the reported setting and run doctor again",
		})
		return out
	}
	out.Loaded = true
	out.Config = cfg
	out.Checks = append(out.Checks, Check{
		Name:   "config.parse",
		Status: StatusOK,
		Detail: fmt.Sprintf("parsed and validated; %d account(s)", len(cfg.Accounts)),
	})

	// 3. Effective server bounds.
	out.Checks = append(out.Checks, Check{
		Name:   "config.server",
		Status: StatusOK,
		Detail: fmt.Sprintf("log_level=%s max_request_bytes=%d max_response_bytes=%d",
			cfg.Server.LogLevel, cfg.Server.MaxRequestBytes, cfg.Server.MaxResponseBytes),
	})

	// 4. Registry construction: the same step the server performs at
	// start-up. For backend=memory this loads the mailbox, and for
	// attachment_send this opens the configured roots.
	reg, err := accounts.New(cfg)
	if err != nil {
		out.Checks = append(out.Checks, Check{
			Name:   "registry",
			Status: StatusFail,
			Detail: "accounts could not be constructed: " + redactConfigError(err.Error(), abs),
			Remedy: "correct the reported account setting and run doctor again",
		})
	} else {
		out.Checks = append(out.Checks, Check{
			Name:   "registry",
			Status: StatusOK,
			Detail: fmt.Sprintf("%d account(s) constructed", len(reg.All())),
		})
		_ = reg.Close()
	}

	// 5. Per-account configuration.
	for _, acc := range cfg.Accounts {
		out.Checks = append(out.Checks, accountChecks(acc)...)
	}
	return out
}

// accountChecks returns the local checks for one account.
func accountChecks(acc config.AccountConfig) []Check {
	prefix := "account/" + acc.ID
	var checks []Check

	checks = append(checks, Check{
		Name:   prefix,
		Status: StatusOK,
		Detail: fmt.Sprintf("email=%s backend=%s permissions=%s", acc.Email, acc.Backend, permissionList(acc.Permissions)),
	})

	if acc.Backend == "memory" {
		checks = append(checks, Check{
			Name:   prefix + "/mailbox",
			Status: StatusOK,
			Detail: fmt.Sprintf("in-memory backend reading %s", acc.MailboxRoot),
		})
	}

	if acc.Backend == "imap" {
		checks = append(checks, checkIMAPTLS(prefix, acc.IMAP))
		checks = append(checks, Check{
			Name:   prefix + "/imap",
			Status: StatusOK,
			Detail: fmt.Sprintf("%s:%d %s", acc.IMAP.Host, acc.IMAP.Port, tlsMode(acc.IMAP.TLS, acc.IMAP.StartTLS)),
		})
		checks = append(checks, credentialCheck(prefix+"/credentials/imap", "imap", acc.IMAP.Password != "", acc.IMAP.PasswordCommand))
	}

	// SMTP is required only for accounts that may send, and is unused
	// otherwise. Reporting the difference prevents an operator from
	// believing that sending is configured when it is not.
	if hasPermission(acc.Permissions, "send") {
		checks = append(checks, Check{
			Name:   prefix + "/smtp",
			Status: StatusOK,
			Detail: fmt.Sprintf("%s:%d %s", acc.SMTP.Host, acc.SMTP.Port, tlsMode(acc.SMTP.TLS, acc.SMTP.StartTLS)),
		})
		checks = append(checks, credentialCheck(prefix+"/credentials/smtp", "smtp", acc.SMTP.Password != "", acc.SMTP.PasswordCommand))
	} else if acc.SMTP.Host != "" || acc.SMTP.Username != "" {
		checks = append(checks, Check{
			Name:   prefix + "/smtp",
			Status: StatusWarn,
			Detail: "SMTP is configured but the send capability is not granted; it will not be used",
			Remedy: "grant the send capability or remove the SMTP settings",
		})
	}

	// attachment_send without any configured root is granted but not
	// usable: the resolver is only built when at least one root exists.
	if hasPermission(acc.Permissions, "attachment_send") && len(acc.AttachmentSendPaths) == 0 {
		checks = append(checks, Check{
			Name:   prefix + "/attachments",
			Status: StatusWarn,
			Detail: "attachment_send is granted but attachment_send_paths is empty; attachments cannot be sent",
			Remedy: "add at least one absolute attachment_send_paths entry, or remove the attachment_send capability",
		})
	}
	return checks
}

// checkIMAPTLS reports an IMAP endpoint whose TLS mode is ambiguous.
//
// The configuration loader does not reject an explicit port with neither
// mode set, and it accepts both modes set at once. The server then either
// refuses the first connection or silently prefers implicit TLS. Both are
// reported here as a failure, because the effective security of the
// connection is not what the file appears to say.
func checkIMAPTLS(prefix string, cfg config.IMAPConfig) Check {
	name := prefix + "/imap/tls-mode"
	switch {
	case cfg.TLS && cfg.StartTLS:
		return Check{
			Name:   name,
			Status: StatusFail,
			Detail: "both imap.tls and imap.starttls are enabled; the connection mode is ambiguous",
			Remedy: "enable exactly one of imap.tls or imap.starttls",
		}
	case !cfg.TLS && !cfg.StartTLS:
		return Check{
			Name:   name,
			Status: StatusFail,
			Detail: "neither imap.tls nor imap.starttls is enabled; a connection would be refused",
			Remedy: "enable imap.tls for port 993 or imap.starttls for port 143; there is no plaintext mode",
		}
	default:
		return Check{
			Name:   name,
			Status: StatusOK,
			Detail: tlsMode(cfg.TLS, cfg.StartTLS),
		}
	}
}

// credentialCheck reports the configured credential source without
// revealing it.
//
// Only the kind of source and, for a command, the executable's base name
// are reported. The password is never read into the output, and the
// command's arguments are never shown: they can name the secret's location.
func credentialCheck(name, proto string, hasPassword bool, command []string) Check {
	switch {
	case hasPassword:
		return Check{
			Name:   name,
			Status: StatusWarn,
			Detail: fmt.Sprintf("%s password is stored in the configuration file", proto),
			Remedy: fmt.Sprintf("prefer %s.password_command so the secret is not stored in the file", proto),
		}
	case len(command) > 0:
		return Check{
			Name:   name,
			Status: StatusOK,
			Detail: fmt.Sprintf("%s password_command configured (executable: %s)", proto, filepath.Base(command[0])),
		}
	default:
		return Check{
			Name:   name,
			Status: StatusFail,
			Detail: fmt.Sprintf("no %s credential source is configured", proto),
			Remedy: fmt.Sprintf("set %s.password or %s.password_command", proto, proto),
		}
	}
}

// checkFilePermissions warns when the configuration file is accessible to
// anyone other than its owner.
//
// The check is meaningful only on a platform with POSIX-style permission
// bits. On Windows the mode bits do not describe the access control list,
// so the check is skipped rather than reported wrongly.
func checkFilePermissions(path string, info os.FileInfo) Check {
	name := "config.permissions"
	if runtime.GOOS == "windows" {
		return Check{Name: name, Status: StatusSkip, Detail: "file permissions are not checked on Windows"}
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return Check{
			Name:   name,
			Status: StatusWarn,
			Detail: fmt.Sprintf("mode %04o is accessible by group or other", mode),
			Remedy: "restrict the file to its owner: chmod 600 <config file>",
		}
	}
	return Check{Name: name, Status: StatusOK, Detail: fmt.Sprintf("mode %04o is owner-only", mode)}
}

// tlsMode renders the effective transport security of an endpoint.
func tlsMode(implicit, startTLS bool) string {
	switch {
	case implicit && startTLS:
		return "ambiguous TLS mode"
	case implicit:
		return "implicit TLS"
	case startTLS:
		return "mandatory STARTTLS"
	default:
		return "no TLS mode enabled"
	}
}

// permissionList renders the granted capabilities, or "none".
func permissionList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strings.ToLower(strings.TrimSpace(n)))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func hasPermission(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), want) {
			return true
		}
	}
	return false
}

// credentialValues collects the values of the credential fields in a raw
// configuration file.
//
// The values are read only so they can be removed from an error message;
// they are never printed and never returned to a caller.
func credentialValues(data []byte) []string {
	var vals []string
	inCommandList := false
	commandIndent := 0

	for _, raw := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))

		// A block-style item belonging to password_command.
		if inCommandList && strings.HasPrefix(trimmed, "- ") {
			if indent > commandIndent {
				vals = append(vals, unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))))
				continue
			}
			inCommandList = false
		}
		inCommandList = false

		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "password":
			if value != "" {
				vals = append(vals, unquote(value))
			}
		case "password_command":
			switch {
			case strings.HasPrefix(value, "["):
				vals = append(vals, flowListItems(value)...)
			case value != "":
				vals = append(vals, unquote(value))
			default:
				inCommandList = true
				commandIndent = indent
			}
		}
	}
	return vals
}

// flowListItems splits a YAML flow-style list into its scalar items.
func flowListItems(value string) []string {
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = unquote(strings.TrimSpace(item))
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// unquote strips a single pair of matching YAML quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// redactConfigError removes every credential value found in the raw
// configuration file from an error message.
//
// A YAML parse error can quote the offending scalar, so a password placed
// in the wrong field would otherwise be echoed back to the terminal. The
// values are read from the file only to be removed from the message; they
// are never printed and never returned.
func redactConfigError(msg, path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return msg
	}
	for _, value := range credentialValues(data) {
		// A short value would corrupt unrelated text by replacing a
		// common fragment, so only distinct values are redacted.
		if len(value) < 3 {
			continue
		}
		msg = strings.ReplaceAll(msg, value, "***")
	}
	return msg
}
