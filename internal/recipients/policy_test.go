package recipients

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonical(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare", in: "alice@example.test", want: "alice@example.test"},
		{name: "display name", in: "Alice <Alice@Example.TEST>", want: "alice@example.test"},
		{name: "surrounding whitespace", in: "  alice@example.test  ", want: "alice@example.test"},
		{name: "mixed case", in: "ALICE@EXAMPLE.TEST", want: "alice@example.test"},
		{name: "quoted local part", in: `"Alice" <alice@example.test>`, want: "alice@example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonical(tc.in)
			if err != nil {
				t.Fatalf("Canonical(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "whitespace", in: "   "},
		{name: "no at sign", in: "alice.example.test"},
		{name: "empty local part", in: "@example.test"},
		{name: "empty domain", in: "alice@"},
		{name: "two addresses", in: "alice@example.test, bob@example.test"},
		{name: "newline", in: "alice@example.test\nBcc: eve@example.test"},
		{name: "carriage return", in: "alice@example.test\r"},
		{name: "tab", in: "alice@example.test\t"},
		{name: "NUL", in: "alice@example.test\x00"},
		{name: "control in display name", in: "Alice\x01 <alice@example.test>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Canonical(tc.in)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, ErrInvalidAddress) {
				t.Errorf("err = %v, want ErrInvalidAddress", err)
			}
		})
	}
}

func TestCanonicalDoesNotReflectInput(t *testing.T) {
	// A malformed or control-containing input must not be echoed in the
	// error, because it may end up in logs.
	_, err := Canonical("alice@example.test\nsecret")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error reflected untrusted input: %q", err)
	}
}

func TestPolicyValidateAllowsAnyWhenListEmpty(t *testing.T) {
	p, err := NewPolicy(nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Validate(
		[]string{"alice@example.test"},
		nil,
		[]string{"bob@example.test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "alice@example.test" || got[1] != "bob@example.test" {
		t.Errorf("got %v", got)
	}
}

func TestPolicyValidateAllowListExact(t *testing.T) {
	p, err := NewPolicy([]string{"Alice@Example.Test", "team@example.test"}, 50)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.Validate([]string{"alice@example.test"}, []string{"TEAM@example.test"}, nil)
	if err != nil {
		t.Fatalf("allowed recipients rejected: %v", err)
	}
	if len(got) != 2 || got[0] != "alice@example.test" || got[1] != "team@example.test" {
		t.Errorf("canonical recipients = %v", got)
	}

	// No substring or suffix matching.
	for _, addr := range []string{
		"alice@example.test.attacker.test",
		"alice@example.testx",
		"xalice@example.test",
		"eve@example.test",
	} {
		_, err := p.Validate([]string{addr}, nil, nil)
		if !errors.Is(err, ErrRecipientNotAllowed) {
			t.Errorf("Validate(%q) err = %v, want ErrRecipientNotAllowed", addr, err)
		}
	}
}

func TestPolicyValidateChecksBccAndDeduplicates(t *testing.T) {
	p, err := NewPolicy([]string{"alice@example.test", "bob@example.test"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Validate(
		[]string{"alice@example.test"},
		[]string{"bob@example.test", "ALICE@example.test"},
		[]string{"bob@example.test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("duplicates were not collapsed: %v", got)
	}
}

func TestPolicyValidateLimit(t *testing.T) {
	p, err := NewPolicy(nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Validate(
		[]string{"one@example.test"},
		[]string{"two@example.test"},
		[]string{"three@example.test"},
	)
	if !errors.Is(err, ErrTooManyRecipients) {
		t.Errorf("err = %v, want ErrTooManyRecipients", err)
	}

	// Duplicates do not consume extra slots.
	got, err := p.Validate(
		[]string{"one@example.test", "ONE@example.test"},
		[]string{"two@example.test"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestPolicyValidateRejectsNoRecipients(t *testing.T) {
	p, err := NewPolicy(nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Validate(nil, nil, nil)
	if !errors.Is(err, ErrNoRecipients) {
		t.Errorf("err = %v, want ErrNoRecipients", err)
	}
}

func TestNewPolicyRejectsBadAllowList(t *testing.T) {
	cases := []string{"", "not-an-address", "a@example.test\nb@example.test"}
	for _, raw := range cases {
		if _, err := NewPolicy([]string{raw}, 50); !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("NewPolicy(%q) err = %v, want ErrInvalidAddress", raw, err)
		}
	}
	if _, err := NewPolicy(nil, 0); err == nil {
		t.Error("expected error for non-positive limit")
	}
}
