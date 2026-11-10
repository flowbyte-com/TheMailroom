package doctor

import (
	"strings"
	"testing"
)

// TestValidationValueDoesNotCarryACredential covers a validation message
// that embeds an unquoted value containing the credential as a substring.
//
// The credential is two characters, so it is redacted only at token
// boundaries; the invalid capability name contains it inside a longer
// word, so the credential is carried into the diagnostic output.
func TestValidationValueDoesNotCarryACredential(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [prefixab]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password: "ab"
`
	out := renderLocal(t, body)

	if !strings.Contains(out, "config.parse") {
		t.Fatalf("the configuration was not rejected; output:\n%s", out)
	}
	if strings.Contains(out, "prefixab") {
		t.Errorf("the diagnostic carries a credential inside an unquoted validation value:\n%s", out)
	}
}
