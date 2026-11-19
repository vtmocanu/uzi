package providers

import (
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

func TestValidateKey(t *testing.T) {
	ok := strings.Repeat("k", 16)
	bad := map[string]string{
		"7 chars":       "abcdefg",
		"15 chars":      strings.Repeat("k", 15),
		"too long":      strings.Repeat("k", MaxKeyLen+1),
		"space":         ok + " x",
		"tab":           ok + "\tx",
		"newline":       ok + "\n",
		"non-ascii":     ok + "é",
		"del":           ok + "\x7f",
		"leading space": " " + ok,
	}
	if err := ValidateKey(ok); err != nil {
		t.Errorf("16-char key: %v", err)
	}
	if err := ValidateKey(strings.Repeat("k", MaxKeyLen)); err != nil {
		t.Errorf("max-length key: %v", err)
	}
	for name, k := range bad {
		err := ValidateKey(k)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), k) && len(k) > 4 {
			t.Errorf("%s: error echoes the key", name)
		}
	}
}

func TestValidateModelMap(t *testing.T) {
	good := ModelMap{"haiku": {Model: "corp-haiku"}}
	if err := ValidateModelMap(good); err != nil {
		t.Fatalf("minimal map: %v", err)
	}
	full := ModelMap{
		"opus": {Model: "corp-opus", ContextWindow: intp(200000)}, "sonnet": {Model: "a/b:c@d.e_f-g"},
		"haiku": {Model: "h", ContextWindow: intp(MinContextWindow)}, "fable": {Model: "f", ContextWindow: intp(MaxContextWindow)},
	}
	if err := ValidateModelMap(full); err != nil {
		t.Fatalf("full map: %v", err)
	}
	bad := map[string]ModelMap{
		"nil":              {},
		"no haiku":         {"opus": {Model: "x"}},
		"unknown key":      {"haiku": {Model: "x"}, "gpt": {Model: "y"}},
		"empty model":      {"haiku": {Model: ""}},
		"long model":       {"haiku": {Model: strings.Repeat("a", MaxModelLen+1)}},
		"space in model":   {"haiku": {Model: "a b"}},
		"quote in model":   {"haiku": {Model: "a\"b"}},
		"non-ascii model":  {"haiku": {Model: "mødel"}},
		"window too small": {"haiku": {Model: "x", ContextWindow: intp(MinContextWindow - 1)}},
		"window too large": {"haiku": {Model: "x", ContextWindow: intp(MaxContextWindow + 1)}},
		"window negative":  {"haiku": {Model: "x", ContextWindow: intp(-1)}},
		"too many entries": func() ModelMap {
			m := ModelMap{"haiku": {Model: "x"}}
			for i := 0; i < 8; i++ {
				m[string(rune('a'+i))] = ModelEntry{Model: "x"}
			}
			return m
		}(),
	}
	for name, m := range bad {
		if err := ValidateModelMap(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateModelMap(ModelMap{"haiku": {Model: strings.Repeat("a", MaxModelLen)}}); err != nil {
		t.Errorf("max-length model: %v", err)
	}
}

func TestValidateAuthStyle(t *testing.T) {
	for _, s := range []string{"bearer", "x_api_key"} {
		if err := ValidateAuthStyle(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "Bearer", "basic", "x-api-key"} {
		if ValidateAuthStyle(s) == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
