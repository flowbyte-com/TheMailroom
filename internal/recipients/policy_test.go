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
		{name: "display name preserves local case", in: "Alice <Alice@Example.TEST>", want: "Alice@example.test"},
		{name: "surrounding whitespace", in: "  alice@example.test  ", want: "alice@example.test"},
		{name: "mixed case preserves local part", in: "ALICE@EXAMPLE.TEST", want: "ALICE@example.test"},
		{name: "domain capitalization only", in: "alice@EXAMPLE.TEST", want: "alice@example.test"},
		{name: "quoted display name excluded", in: `"Alice" <alice@example.test>`, want: "alice@example.test"},
		{name: "quoted local part normalized", in: `"Sales"@Example.TEST`, want: "Sales@example.test"},
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
	p, err := NewPolicy([]string{
		"Sales@Example.Test",
		"sales@example.test",
		"team@example.test",
	}, 50)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.Validate(
		[]string{"Sales@example.test"},
		[]string{"sales@EXAMPLE.TEST"},
		[]string{"team@example.test"},
	)
	if err != nil {
		t.Fatalf("allowed recipients rejected: %v", err)
	}
	want := []string{"Sales@example.test", "sales@example.test", "team@example.test"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("canonical recipients = %v, want %v", got, want)
	}

	// Domain capitalization alone must not change authorization: the
	// local-part is preserved, the domain is normalized.
	if _, err := p.Validate([]string{"Sales@EXAMPLE.TEST"}, nil, nil); err != nil {
		t.Errorf("domain-only case variation rejected: %v", err)
	}

	// Local-part case is significant, and matching is not substring-based.
	for _, addr := range []string{
		"SALES@example.test",
		"Team@example.test",
		"TEAM@example.test",
		"Sales@example.test.attacker.test",
		"Sales@example.testx",
		"xSales@example.test",
		"eve@example.test",
	} {
		_, err := p.Validate([]string{addr}, nil, nil)
		if !errors.Is(err, ErrRecipientNotAllowed) {
			t.Errorf("Validate(%q) err = %v, want ErrRecipientNotAllowed", addr, err)
		}
	}
}

func TestPolicyAllowListLocalPartCase(t *testing.T) {
	p, err := NewPolicy([]string{"sales@example.test"}, 50)
	if err != nil {
		t.Fatal(err)
	}

	// A different local-part case must not be authorized by the allow-list.
	if _, err := p.Validate([]string{"Sales@example.test"}, nil, nil); !errors.Is(err, ErrRecipientNotAllowed) {
		t.Errorf("local-part case variant err = %v, want ErrRecipientNotAllowed", err)
	}

	// Domain capitalization alone must not change authorization.
	if _, err := p.Validate(nil, []string{"sales@EXAMPLE.TEST"}, nil); err != nil {
		t.Errorf("domain-only case variation rejected: %v", err)
	}
}

func TestPolicyValidateChecksBccAndDeduplicates(t *testing.T) {
	p, err := NewPolicy([]string{"alice@example.test", "bob@example.test"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Validate(
		[]string{"alice@example.test"},
		[]string{"bob@example.test", "alice@EXAMPLE.TEST"},
		[]string{"bob@example.test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "alice@example.test" || got[1] != "bob@example.test" {
		t.Errorf("duplicates were not collapsed correctly: %v", got)
	}
}

func TestPolicyValidatePreservesLocalPartCaseAcrossFields(t *testing.T) {
	p, err := NewPolicy([]string{
		"Sales@example.test",
		"sales@example.test",
		"SUPPORT@example.test",
	}, 50)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.Validate(
		[]string{"Sales@Example.TEST"},
		[]string{"sales@EXAMPLE.TEST"},
		[]string{"SUPPORT@example.test"},
	)
	if err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	want := []string{"Sales@example.test", "sales@example.test", "SUPPORT@example.test"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Exact duplicates, including domain-only case differences, collapse;
	// local-parts differing only by case stay distinct.
	got, err = p.Validate(
		[]string{"Sales@example.test"},
		[]string{"Sales@EXAMPLE.TEST", "sales@example.test"},
		[]string{"sales@Example.TEST"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "Sales@example.test" || got[1] != "sales@example.test" {
		t.Errorf("case-sensitive dedupe = %v", got)
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

	// Exact duplicates do not consume extra slots, but a local-part case
	// variation is a distinct recipient and does consume a slot.
	got, err := p.Validate(
		[]string{"one@example.test", "one@EXAMPLE.TEST"},
		[]string{"two@example.test"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v", got)
	}
	if _, err := p.Validate(
		[]string{"one@example.test", "ONE@example.test"},
		[]string{"two@example.test"},
		nil,
	); !errors.Is(err, ErrTooManyRecipients) {
		t.Errorf("case-distinct recipients err = %v, want ErrTooManyRecipients", err)
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
