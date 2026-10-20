package mailbox

import "testing"

func TestParsePartPath(t *testing.T) {
	valid := []struct {
		part string
		max  int
	}{
		{"1", 1},
		{"2.1", 2},
		{"1.2.3", 3},
		{"16.500", 500},
	}
	for _, tc := range valid {
		got, err := ParsePartPath(tc.part, 16, tc.max)
		if err != nil {
			t.Errorf("ParsePartPath(%q): %v", tc.part, err)
			continue
		}
		if len(got) == 0 {
			t.Errorf("ParsePartPath(%q) returned no components", tc.part)
		}
	}

	invalid := []string{
		"", ".", "1.", ".1", "1..2", "0", "00", "01", "1.0",
		"+1", "-1", "1a", " 1", "1 ", "1. 2", "2147483648",
	}
	for _, part := range invalid {
		if _, err := ParsePartPath(part, 16, 500); err != ErrInvalidPart {
			t.Errorf("ParsePartPath(%q) err = %v, want ErrInvalidPart", part, err)
		}
	}

	if _, err := ParsePartPath("1.2.3", 2, 500); err != ErrInvalidPart {
		t.Errorf("depth overflow err = %v, want ErrInvalidPart", err)
	}
	if _, err := ParsePartPath("501", 16, 500); err != ErrInvalidPart {
		t.Errorf("part overflow err = %v, want ErrInvalidPart", err)
	}
	long := make([]byte, MaxPartPathBytes+1)
	for i := range long {
		long[i] = '1'
	}
	if _, err := ParsePartPath(string(long), 16, 1_000_000); err != ErrInvalidPart {
		t.Errorf("oversize path err = %v, want ErrInvalidPart", err)
	}
}
