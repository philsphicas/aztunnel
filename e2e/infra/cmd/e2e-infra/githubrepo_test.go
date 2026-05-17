package main

import "testing"

func TestParseGitConfigRepo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "https with .git",
			in:   "[remote \"origin\"]\n\turl = https://github.com/foo/bar.git\n",
			want: "foo/bar",
		},
		{
			name: "https without .git",
			in:   "[remote \"origin\"]\n\turl = https://github.com/foo/bar\n",
			want: "foo/bar",
		},
		{
			name: "ssh with .git",
			in:   "[remote \"origin\"]\n\turl = git@github.com:foo/bar.git\n",
			want: "foo/bar",
		},
		{
			name: "non-github url",
			in:   "[remote \"origin\"]\n\turl = https://gitlab.com/foo/bar\n",
			want: "",
		},
		{
			name: "no url",
			in:   "[core]\n\trepositoryformatversion = 0\n",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "urlInsteadOf should not match",
			in:   "[url \"git@github.com:other/repo.git\"]\n\tinsteadOf = https://github.com/other/repo\n",
			want: "",
		},
		{
			name: "pushurl should not match",
			in:   "[remote \"origin\"]\n\tpushurl = https://github.com/foo/bar.git\n",
			want: "",
		},
		{
			name: "url= without spaces",
			in:   "[remote \"origin\"]\n\turl=https://github.com/foo/bar.git\n",
			want: "foo/bar",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGitConfigRepo(tc.in)
			if got != tc.want {
				t.Errorf("parseGitConfigRepo: got %q, want %q", got, tc.want)
			}
		})
	}
}
