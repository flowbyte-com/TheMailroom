package config

import "testing"

// TestEffectiveMaxListScanMessages pins the resolution of the scan
// budget.
//
// The reported budget and the enforced budget must be the same number.
// The resolution therefore lives in one place, and this test pins it.
func TestEffectiveMaxListScanMessages(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		want       int
	}{
		{name: "unset selects the default", configured: 0, want: DefaultMaxListScanMessages},
		{name: "negative selects the default", configured: -1, want: DefaultMaxListScanMessages},
		{name: "configured value is used", configured: 4242, want: 4242},
		{name: "the default is used when configured explicitly", configured: DefaultMaxListScanMessages, want: DefaultMaxListScanMessages},
		{name: "the maximum is used when configured explicitly", configured: MaxListScanMessagesLimit, want: MaxListScanMessagesLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := LimitsConfig{MaxListScanMessages: tc.configured}
			if got := l.EffectiveMaxListScanMessages(); got != tc.want {
				t.Errorf("EffectiveMaxListScanMessages() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEffectiveMaxListScanMessagesMatchesParsedDefault pins that a
// parsed configuration without an explicit limit and one that is
// resolved through the helper agree. The account registry stores the
// parsed limits, so the value list_accounts reports and the value the
// client enforces must be the same.
func TestEffectiveMaxListScanMessagesMatchesParsedDefault(t *testing.T) {
	yaml := `
accounts:
  - id: a
    backend: memory
    email: a@example.test
    mailbox_root: ./testdata
    permissions: [read]
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	limits := cfg.Accounts[0].Limits
	if got := limits.EffectiveMaxListScanMessages(); got != DefaultMaxListScanMessages {
		t.Errorf("EffectiveMaxListScanMessages() = %d, want %d", got, DefaultMaxListScanMessages)
	}
}
