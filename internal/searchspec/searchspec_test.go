package searchspec

import (
	"testing"
	"time"
)

// TestTruncateDate verifies the UTC day-granularity truncation
// that both backends apply to the date criteria.
func TestTruncateDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"midnight stays", "2024-08-12T00:00:00Z", "2024-08-12T00:00:00Z"},
		{"end of day truncates down", "2024-08-12T23:59:59Z", "2024-08-12T00:00:00Z"},
		{"midday truncates down", "2024-08-12T12:34:56Z", "2024-08-12T00:00:00Z"},
		{"one second past midnight truncates down", "2024-08-12T00:00:01Z", "2024-08-12T00:00:00Z"},
		{"month boundary", "2024-09-01T08:00:00Z", "2024-09-01T00:00:00Z"},
		{"year boundary", "2025-01-01T08:00:00Z", "2025-01-01T00:00:00Z"},
		{"leap day", "2024-02-29T10:00:00Z", "2024-02-29T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := time.Parse(time.RFC3339, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatal(err)
			}
			got := TruncateDate(in)
			if !got.Equal(want) {
				t.Errorf("TruncateDate(%s) = %s, want %s", tc.in, got.Format(time.RFC3339), tc.want)
			}
		})
	}
}

// TestTruncateDateUsesUTC verifies that the truncation is in
// UTC regardless of the input's location, so the value the client
// puts on the wire and the value the server parses agree.
func TestTruncateDateUsesUTC(t *testing.T) {
	loc := time.FixedZone("UTC+7", 7*3600)
	in := time.Date(2024, 8, 12, 2, 0, 0, 0, loc) // 2024-08-11T19:00Z in UTC
	got := TruncateDate(in)
	want := time.Date(2024, 8, 11, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("TruncateDate = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestFlagPredicates verifies the flag semantics.
func TestFlagPredicates(t *testing.T) {
	seen := []string{FlagSeen}
	flagged := []string{FlagFlagged}
	both := []string{FlagSeen, FlagFlagged}
	none := []string{}

	cases := []struct {
		name        string
		flags       []string
		unseen      bool
		flagged     bool
		wantUnseen  bool
		wantFlagged bool
	}{
		{"no filters, no flags", none, false, false, true, true},
		{"no filters, seen", seen, false, false, true, true},
		{"unseen filter, seen message", seen, true, false, false, true},
		{"unseen filter, unseen message", none, true, false, true, true},
		{"unseen filter, flagged but not seen", flagged, true, false, true, true},
		{"flagged filter, flagged message", flagged, false, true, true, true},
		{"flagged filter, not flagged", none, false, true, true, false},
		{"flagged filter, seen but not flagged", seen, false, true, true, false},
		{"both filters, flagged and not seen", flagged, true, true, true, true},
		{"both filters, seen and flagged", both, true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesUnseen(tc.flags, tc.unseen); got != tc.wantUnseen {
				t.Errorf("MatchesUnseen(%v, %v) = %v, want %v", tc.flags, tc.unseen, got, tc.wantUnseen)
			}
			if got := MatchesFlagged(tc.flags, tc.flagged); got != tc.wantFlagged {
				t.Errorf("MatchesFlagged(%v, %v) = %v, want %v", tc.flags, tc.flagged, got, tc.wantFlagged)
			}
		})
	}
}

// TestHasFlagIsCaseInsensitive verifies that the flag comparison
// tolerates a server that uses a different case for the flag
// names.
func TestHasFlagIsCaseInsensitive(t *testing.T) {
	flags := []string{"\\SEEN", "\\flagged"}
	if !HasFlag(flags, FlagSeen) {
		t.Error("HasFlag did not match \\SEEN against \\Seen")
	}
	if !HasFlag(flags, FlagFlagged) {
		t.Error("HasFlag did not match \\flagged against \\Flagged")
	}
	if HasFlag(flags, FlagDeleted) {
		t.Error("HasFlag matched \\Deleted, which is absent")
	}
}

// TestFlagsFromStatusHeader verifies the RFC 2156 Status: header
// translation.
func TestFlagsFromStatusHeader(t *testing.T) {
	cases := []struct {
		status string
		want   []string
	}{
		{"", nil},
		{"R", []string{FlagSeen}},
		{"F", []string{FlagFlagged}},
		{"RF", []string{FlagSeen, FlagFlagged}},
		{"D", []string{FlagDeleted}},
		{"A", []string{FlagAnswered}},
		{"RFA", []string{FlagSeen, FlagFlagged, FlagAnswered}},
		// "O" means "old" and has no \Recent counterpart here.
		{"O", nil},
		// Lowercase is accepted too.
		{"rf", []string{FlagSeen, FlagFlagged}},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			got := FlagsFromStatusHeader(tc.status)
			if len(got) != len(tc.want) {
				t.Fatalf("FlagsFromStatusHeader(%q) = %v, want %v", tc.status, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("FlagsFromStatusHeader(%q) = %v, want %v", tc.status, got, tc.want)
				}
			}
		})
	}
}
