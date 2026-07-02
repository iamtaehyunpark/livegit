package shellq

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"":         "''",
		"plain":    "'plain'",
		"a b":      "'a b'",
		"a;b":      "'a;b'",
		"$HOME":    "'$HOME'",
		"it's":     `'it'\''s'`,
		"a*b":      "'a*b'",
		"/abs/dir": "'/abs/dir'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoin(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"ls"}, "'ls'"},
		{[]string{"ls", "-la"}, "'ls' '-la'"},
		{[]string{"grep", "foo bar", "."}, "'grep' 'foo bar' '.'"},
		{[]string{"echo", "a;b"}, "'echo' 'a;b'"}, // ';' stays a literal arg
	}
	for _, c := range cases {
		if got := Join(c.args); got != c.want {
			t.Errorf("Join(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}
