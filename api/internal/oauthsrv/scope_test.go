package oauthsrv

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseScopes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		ok   bool
	}{
		{"one", "jobs:run", []string{"jobs:run"}, true},
		{"two in order", "jobs:read jobs:run", []string{"jobs:read", "jobs:run"}, true},
		{"empty", "", nil, false},
		{"only a space", " ", nil, false},
		{"leading space", " jobs:run", nil, false},
		{"trailing space", "jobs:run ", nil, false},
		{"doubled space", "jobs:run  jobs:read", nil, false},
		{"unknown", "jobs:run admin", nil, false},
		{"case differs", "Jobs:Run", nil, false},
		{"duplicate", "jobs:run jobs:run", nil, false},
		{"comma separated is one unknown token", "jobs:run,jobs:read", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseScopes(tc.in)
			if tc.ok != (err == nil) {
				t.Fatalf("ParseScopes(%q) err = %v, want ok=%v", tc.in, err, tc.ok)
			}
			if tc.ok && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseScopes(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateScopes(t *testing.T) {
	if _, err := ValidateScopes(nil); err == nil {
		t.Fatal("nil scopes accepted")
	}
	if _, err := ValidateScopes([]string{}); err == nil {
		t.Fatal("empty scopes accepted")
	}
	if _, err := ValidateScopes([]string{"jobs:run", ""}); err == nil {
		t.Fatal("empty element accepted")
	}
	got, err := ValidateScopes([]string{"jobs:run", "jobs:read"})
	if err != nil || !reflect.DeepEqual(got, []string{"jobs:run", "jobs:read"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestValidateScopesBoundsEchoedValue(t *testing.T) {
	_, err := ValidateScopes([]string{strings.Repeat("x", 5000)})
	if err == nil || len(err.Error()) > 300 {
		t.Fatalf("err = %v, want a bounded message", err)
	}
}
