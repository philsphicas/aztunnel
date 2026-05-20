package azure

import "testing"

func TestSlugAlias(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"alice", "alice"},
		{"Alice.Smith", "alice-smith"},
		{"firstname.lastname", "firstname-lastname"},
		{"weird+plus", "weird-plus"},
		{"a_b_c", "a-b-c"},
		{"héllo", "hello"},
		{"François", "francois"},
		{"--double--dash--", "double-dash"},
		{"trailing.", "trailing"},
		{".leading", "leading"},
		{"...---...", ""},
		{"", ""},
		{"a", "a"},
		{"a@b", "a-b"},
		{"Alice O'Brien", "alice-o-brien"},
		{"日本語user", "user"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, // capped at 30
		{"alias-with-many-chars-then-end---", "alias-with-many-chars-then-end"},
	}
	for _, c := range cases {
		got := SlugAlias(c.in)
		if got != c.want {
			t.Errorf("SlugAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugAliasLengthCap(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	got := SlugAlias(long)
	if len(got) > maxAliasLen {
		t.Errorf("SlugAlias did not cap length: got %d chars", len(got))
	}
}

func TestAliasFromUPN(t *testing.T) {
	cases := []struct {
		upn, want string
	}{
		{"alice@example.com", "alice"},
		{"firstname.lastname@contoso.com", "firstname-lastname"},
		{"weird+plus@x.com", "weird-plus"},
		{"héllo@x.com", "hello"},
		{"no-at-sign", "no-at-sign"},
		{"", ""},
		{"@onlyat", ""},
	}
	for _, c := range cases {
		got := AliasFromUPN(c.upn)
		if got != c.want {
			t.Errorf("AliasFromUPN(%q) = %q, want %q", c.upn, got, c.want)
		}
	}
}

func TestAliasFromObjectID(t *testing.T) {
	cases := []struct {
		oid, want string
	}{
		{"00000000-0000-0000-0000-000000000000", "000000000000"},
		{"AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA", "aaaaaaaaaaaa"},
		{"abcd", "abcd"},
		{"", ""},
	}
	for _, c := range cases {
		got := AliasFromObjectID(c.oid)
		if got != c.want {
			t.Errorf("AliasFromObjectID(%q) = %q, want %q", c.oid, got, c.want)
		}
	}
}

func TestDeriveAlias_PrefersUPN(t *testing.T) {
	got := DeriveAlias("00000000-0000-0000-0000-000000000000", "alice@example.com")
	if got != "alice" {
		t.Errorf("DeriveAlias = %q, want \"alice\"", got)
	}
}

func TestDeriveAlias_FallsBackToOID(t *testing.T) {
	got := DeriveAlias("00000000-0000-0000-0000-000000000000", "@")
	if got != "000000000000" {
		t.Errorf("DeriveAlias = %q, want \"000000000000\"", got)
	}
}

func TestDeriveAlias_EmptyOnBothEmpty(t *testing.T) {
	got := DeriveAlias("", "")
	if got != "" {
		t.Errorf("DeriveAlias = %q, want \"\"", got)
	}
}
