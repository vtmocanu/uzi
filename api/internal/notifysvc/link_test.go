package notifysvc

import "testing"

func TestSafeLinkURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://gitlab.example.com/grp/proj/-/pipelines/9": "https://gitlab.example.com/grp/proj/-/pipelines/9",
		"  http://forge:3000/p/actions/runs/4  ":            "http://forge:3000/p/actions/runs/4",
		"https://forge.example/p?a=1&b=2":                   "https://forge.example/p?a=1&b=2",
		"":                                                  "",
		"https://forge.example/p|label":                     "",
		"https://forge.example/<p>":                         "",
		"https://forge.example/p q":                         "",
		"https://forge.example/p\tq":                        "",
		"https://forge.example/p\x00":                       "",
		"https://forge.example/p\u0085q":                    "",
		"javascript:alert(1)":                               "",
		"ftp://forge.example/p":                             "",
		"/grp/proj/-/pipelines/9":                           "",
		"https:///grp/proj":                                 "",
		"forge.example/p":                                   "",
		"https://forge.example/%zz-invalid":                 "",
	} {
		if got := SafeLinkURL(in); got != want {
			t.Errorf("SafeLinkURL(%q) = %q, want %q", in, got, want)
		}
	}
}
