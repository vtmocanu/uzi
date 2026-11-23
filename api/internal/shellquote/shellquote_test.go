package shellquote

import "testing"

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "''",
		"prod":                "prod",
		"https://u.example/x": "https://u.example/x",
		"my ctx":              "'my ctx'",
		"o'brien":             `'o'\''brien'`,
		"$HOME":               "'$HOME'",
		"=foo":                "'=foo'",
		"a=b":                 "a=b",
		"--default":           "--default",
		"a`b`":                "'a`b`'",
		"$(id)":               "'$(id)'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s; want %s", in, got, want)
		}
	}
}
