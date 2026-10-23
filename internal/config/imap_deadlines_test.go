package config

// Regression tests for the IMAP deadline configuration.
//
// Every deadline is finite: zero means "use the default", and a value
// outside the accepted range is a configuration error rather than a
// silent clamp. An operator must never believe a bound is in force when
// it is not.

import (
	"strings"
	"testing"
	"time"
)

func TestValidateIMAPDeadlinesAcceptsTheBoundaries(t *testing.T) {
	cases := []IMAPConfig{
		{},
		{OperationAllowance: MinIMAPOperationAllowance},
		{OperationAllowance: MaxIMAPOperationAllowance},
		{ControlCommandTimeout: MinIMAPControlCommandTimeout},
		{ControlCommandTimeout: MaxIMAPControlCommandTimeout},
		{TransferRate: MinIMAPTransferRate},
		{TransferRate: MaxIMAPTransferRate},
		{MaxOperationTimeout: MinIMAPMaxOperationTimeout},
		{MaxOperationTimeout: MaxIMAPMaxOperationTimeout},
		{
			OperationAllowance:    MinIMAPOperationAllowance,
			ControlCommandTimeout: MaxIMAPControlCommandTimeout,
			TransferRate:          MaxIMAPTransferRate,
			MaxOperationTimeout:   MaxIMAPMaxOperationTimeout,
		},
	}
	for i, c := range cases {
		if err := validateIMAPDeadlines(c); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestValidateIMAPDeadlinesRejectsOutOfRangeValues(t *testing.T) {
	cases := []struct {
		name string
		cfg  IMAPConfig
		want string
	}{
		{"allowance negative", IMAPConfig{OperationAllowance: -time.Second}, "operation_allowance"},
		{"allowance too large", IMAPConfig{OperationAllowance: MaxIMAPOperationAllowance + time.Second}, "operation_allowance"},
		{"control timeout negative", IMAPConfig{ControlCommandTimeout: -time.Second}, "control_command_timeout"},
		{"control timeout too large", IMAPConfig{ControlCommandTimeout: MaxIMAPControlCommandTimeout + time.Second}, "control_command_timeout"},
		{"transfer rate too small", IMAPConfig{TransferRate: MinIMAPTransferRate - 1}, "transfer_rate"},
		{"transfer rate too large", IMAPConfig{TransferRate: MaxIMAPTransferRate + 1}, "transfer_rate"},
		{"operation timeout too small", IMAPConfig{MaxOperationTimeout: MinIMAPMaxOperationTimeout - time.Second}, "max_operation_timeout"},
		{"operation timeout too large", IMAPConfig{MaxOperationTimeout: MaxIMAPMaxOperationTimeout + time.Second}, "max_operation_timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIMAPDeadlines(tc.cfg)
			if err == nil {
				t.Fatal("an out-of-range value was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// TestIMAPDeadlineDefaultsAreFinite checks that every deadline has a
// finite default and that zero resolves to it.
func TestIMAPDeadlineDefaultsAreFinite(t *testing.T) {
	acc := AccountConfig{IMAP: IMAPConfig{}}.WithDefaults()
	if acc.IMAP.OperationAllowance != DefaultIMAPOperationAllowance {
		t.Errorf("operation allowance = %s, want %s", acc.IMAP.OperationAllowance, DefaultIMAPOperationAllowance)
	}
	if acc.IMAP.ControlCommandTimeout != DefaultIMAPControlCommandTimeout {
		t.Errorf("control command timeout = %s, want %s", acc.IMAP.ControlCommandTimeout, DefaultIMAPControlCommandTimeout)
	}
	if acc.IMAP.TransferRate != DefaultIMAPTransferRate {
		t.Errorf("transfer rate = %d, want %d", acc.IMAP.TransferRate, DefaultIMAPTransferRate)
	}
	if acc.IMAP.MaxOperationTimeout != DefaultIMAPMaxOperationTimeout {
		t.Errorf("maximum operation timeout = %s, want %s", acc.IMAP.MaxOperationTimeout, DefaultIMAPMaxOperationTimeout)
	}
	for name, d := range map[string]time.Duration{
		"operation allowance":       acc.IMAP.OperationAllowance,
		"control command timeout":   acc.IMAP.ControlCommandTimeout,
		"maximum operation timeout": acc.IMAP.MaxOperationTimeout,
	} {
		if d <= 0 {
			t.Errorf("the %s default is %s, want a finite positive value", name, d)
		}
	}
	if acc.IMAP.TransferRate <= 0 {
		t.Errorf("the transfer rate default is %d, want a finite positive value", acc.IMAP.TransferRate)
	}
}
