package postgres

import "testing"

func TestParseMemberShortID(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"t-456", 456, true},
		{"t-1", 1, true},
		{"t-999999", 999999, true},
		{"t-0", 0, false},
		{"t-", 0, false},
		{"456", 0, false},
		{"t-abc", 0, false},
		{"", 0, false},
		{"t-12abc", 0, false},
	}
	for _, c := range cases {
		got, err := parseMemberShortID(c.in)
		if c.ok && err != nil {
			t.Errorf("parseMemberShortID(%q) unexpected err: %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("parseMemberShortID(%q) expected err, got nil", c.in)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parseMemberShortID(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestGenerateTeamInviteCode(t *testing.T) {
	code, err := generateTeamInviteCode(TeamInviteCodeLen)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(code) != TeamInviteCodeLen {
		t.Errorf("code length = %d, want %d", len(code), TeamInviteCodeLen)
	}
	// Codes must only use the unambiguous alphabet.
	const allowed = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	for _, c := range code {
		if !contains(allowed, byte(c)) {
			t.Errorf("code %q contains invalid char %q", code, c)
		}
	}
}

func contains(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}
