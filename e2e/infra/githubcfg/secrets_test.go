package githubcfg

import "testing"

func TestSplitRepo(t *testing.T) {
	good := map[string][2]string{
		"philsphicas/aztunnel": {"philsphicas", "aztunnel"},
		"a/b":                  {"a", "b"},
		"o/r-with-dashes":      {"o", "r-with-dashes"},
	}
	for in, want := range good {
		o, r, err := splitRepo(in)
		if err != nil {
			t.Errorf("splitRepo(%q): unexpected error %v", in, err)
			continue
		}
		if o != want[0] || r != want[1] {
			t.Errorf("splitRepo(%q) = %q, %q; want %q, %q", in, o, r, want[0], want[1])
		}
	}
	bad := []string{"", "noslash", "/leading", "trailing/", "/"}
	for _, in := range bad {
		if _, _, err := splitRepo(in); err == nil {
			t.Errorf("splitRepo(%q): expected error", in)
		}
	}
}
