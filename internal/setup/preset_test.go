package setup

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// TestLookupPreset covers preset resolution, including the case-insensitive
// and whitespace-tolerant forms and the usage error for an unknown name.
func TestLookupPreset(t *testing.T) {
	for _, name := range []string{"", "  "} {
		got, err := LookupPreset(name)
		if err != nil {
			t.Errorf("LookupPreset(%q) = %v, want the generic preset", name, err)
		}
		if got.Name != "" {
			t.Errorf("LookupPreset(%q).Name = %q, want empty", name, got.Name)
		}
	}

	for _, name := range []string{"purelymail", "Purelymail", " PURELYMAIL "} {
		got, err := LookupPreset(name)
		if err != nil {
			t.Errorf("LookupPreset(%q) = %v, want the Purelymail preset", name, err)
		}
		if got.Name != "purelymail" {
			t.Errorf("LookupPreset(%q).Name = %q, want purelymail", name, got.Name)
		}
	}

	for _, name := range []string{"nope", "purely", "gmail", "purelymail.com"} {
		if _, err := LookupPreset(name); err == nil {
			t.Errorf("LookupPreset(%q) = nil, want a usage error", name)
		} else {
			if !strings.Contains(err.Error(), "unknown preset") {
				t.Errorf("LookupPreset(%q) error does not say unknown: %v", name, err)
			}
			if !strings.Contains(err.Error(), "purelymail") {
				t.Errorf("LookupPreset(%q) error does not list the supported presets: %v", name, err)
			}
		}
	}
}

// TestGenericPresetKeepsTheExistingDefaults pins the provider-neutral
// interview to the values the setup command already used.
func TestGenericPresetKeepsTheExistingDefaults(t *testing.T) {
	if Generic.Name != "" {
		t.Errorf("Generic.Name = %q, want empty", Generic.Name)
	}
	if Generic.IMAPHost != "" {
		t.Errorf("Generic.IMAPHost = %q, want empty so the operator must supply it", Generic.IMAPHost)
	}
	if !Generic.IMAPImplicitTLS {
		t.Error("Generic must prefer implicit TLS for IMAP")
	}
	if Generic.IMAPImplicitPort != config.DefaultIMAPPort {
		t.Errorf("Generic IMAP implicit port = %d, want %d", Generic.IMAPImplicitPort, config.DefaultIMAPPort)
	}
	if Generic.IMAPStartTLSPort != config.DefaultIMAPPortStartTLS {
		t.Errorf("Generic IMAP STARTTLS port = %d, want %d", Generic.IMAPStartTLSPort, config.DefaultIMAPPortStartTLS)
	}
	if Generic.SMTPHost != "" {
		t.Errorf("Generic.SMTPHost = %q, want empty", Generic.SMTPHost)
	}
	if Generic.SMTPImplicitTLS {
		t.Error("Generic must prefer STARTTLS for SMTP")
	}
	if Generic.SMTPStartTLSPort != config.DefaultSMTPPort {
		t.Errorf("Generic SMTP STARTTLS port = %d, want %d", Generic.SMTPStartTLSPort, config.DefaultSMTPPort)
	}
	if Generic.SMTPImplicitPort != config.DefaultSMTPPortImplicit {
		t.Errorf("Generic SMTP implicit port = %d, want %d", Generic.SMTPImplicitPort, config.DefaultSMTPPortImplicit)
	}
}

// TestPurelymailPresetValues pins the documented provider values.
func TestPurelymailPresetValues(t *testing.T) {
	if Purelymail.Name != "purelymail" {
		t.Errorf("Name = %q", Purelymail.Name)
	}
	if Purelymail.IMAPHost != "imap.purelymail.com" {
		t.Errorf("IMAPHost = %q", Purelymail.IMAPHost)
	}
	if Purelymail.IMAPImplicitPort != 993 || !Purelymail.IMAPImplicitTLS {
		t.Errorf("IMAP = %d implicit=%v, want 993 implicit", Purelymail.IMAPImplicitPort, Purelymail.IMAPImplicitTLS)
	}
	if Purelymail.IMAPStartTLSPort != 143 {
		t.Errorf("IMAP STARTTLS port = %d, want 143", Purelymail.IMAPStartTLSPort)
	}
	if Purelymail.SMTPHost != "smtp.purelymail.com" {
		t.Errorf("SMTPHost = %q", Purelymail.SMTPHost)
	}
	if Purelymail.SMTPImplicitPort != 465 || !Purelymail.SMTPImplicitTLS {
		t.Errorf("SMTP = %d implicit=%v, want 465 implicit", Purelymail.SMTPImplicitPort, Purelymail.SMTPImplicitTLS)
	}
	if Purelymail.SMTPStartTLSPort != 587 {
		t.Errorf("SMTP STARTTLS port = %d, want 587", Purelymail.SMTPStartTLSPort)
	}
}
