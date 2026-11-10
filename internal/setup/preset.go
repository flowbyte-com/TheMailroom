package setup

import (
	"fmt"
	"strings"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// Preset is a set of interview defaults for a known mail provider.
//
// A preset only supplies the values the interview offers as defaults. Every
// value stays editable, nothing is detected automatically, and no part of
// TheMailroom outside this package learns that a preset was chosen. The
// provider name, when set, is written as the informational account field
// documented as having no runtime effect.
type Preset struct {
	// Name is the preset identifier, empty for the generic interview.
	Name string

	// IMAPHost is the suggested incoming server.
	IMAPHost string
	// IMAPImplicitTLS selects implicit TLS as the suggested transport.
	IMAPImplicitTLS bool
	// IMAPImplicitPort and IMAPStartTLSPort are the suggested ports for
	// the two transports.
	IMAPImplicitPort int
	IMAPStartTLSPort int

	// SMTPHost is the suggested submission server.
	SMTPHost string
	// SMTPImplicitTLS selects implicit TLS as the suggested transport.
	SMTPImplicitTLS bool
	// SMTPImplicitPort and SMTPStartTLSPort are the suggested ports for
	// the two transports.
	SMTPImplicitPort int
	SMTPStartTLSPort int
}

// Generic is the provider-neutral interview.
//
// Its defaults are the ones the setup command has always used: implicit TLS
// on 993 for IMAP, and STARTTLS on 587 for SMTP.
var Generic = Preset{
	Name:             "",
	IMAPHost:         "",
	IMAPImplicitTLS:  true,
	IMAPImplicitPort: config.DefaultIMAPPort,
	IMAPStartTLSPort: config.DefaultIMAPPortStartTLS,
	SMTPHost:         "",
	SMTPImplicitTLS:  false,
	SMTPImplicitPort: config.DefaultSMTPPortImplicit,
	SMTPStartTLSPort: config.DefaultSMTPPort,
}

// Purelymail is an optional convenience preset for Purelymail.
//
// The values are the ones Purelymail publishes at
// https://purelymail.com/docs/setup/technical : IMAP on imap.purelymail.com
// port 993 with implicit TLS, and SMTP on smtp.purelymail.com port 465 with
// implicit TLS, or port 587 with STARTTLS where the client needs it. The
// login name is the full email address, and an app password is required when
// two-factor authentication is enabled.
//
// The preset does not create or manage Purelymail accounts, domains, DNS
// records or credentials, and it cannot verify them.
var Purelymail = Preset{
	Name:             "purelymail",
	IMAPHost:         "imap.purelymail.com",
	IMAPImplicitTLS:  true,
	IMAPImplicitPort: 993,
	IMAPStartTLSPort: 143,
	SMTPHost:         "smtp.purelymail.com",
	SMTPImplicitTLS:  true,
	SMTPImplicitPort: 465,
	SMTPStartTLSPort: 587,
}

// LookupPreset resolves a -preset value.
//
// An empty name selects the generic interview, so the flag is optional. An
// unknown name is a usage error rather than a silent fallback: a typo must
// not quietly produce a configuration with the wrong servers.
func LookupPreset(name string) (Preset, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return Generic, nil
	case "purelymail":
		return Purelymail, nil
	default:
		return Preset{}, fmt.Errorf(
			"unknown preset %q; supported presets: purelymail", strings.TrimSpace(name))
	}
}
