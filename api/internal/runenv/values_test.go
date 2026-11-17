package runenv

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestValidateValue(t *testing.T) {
	tests := []struct {
		name    string
		secret  bool
		value   string
		wantErr string
	}{
		{"empty plain accepted", false, "", ""},
		{"short plain accepted", false, "x", ""},
		{"plain url without userinfo", false, "https://example.test/path?mail=a@b", ""},
		{"plain url with at in the path only", false, "https://example.test/@scope/pkg", ""},
		{"7 character secret refused", true, "1234567", "at least 8 characters"},
		{"8 character secret accepted", true, "12345678", ""},
		{"empty secret refused", true, "", "at least 8 characters"},
		// 4 astral code points are 8 UTF-16 units; 3 are 6.
		{"4 astral runes count as 8", true, "\U0001F600\U0001F600\U0001F600\U0001F600", ""},
		{"3 astral runes count as 6", true, "\U0001F600\U0001F600\U0001F600", "at least 8 characters"},
		{"NUL refused in plain", false, "a\x00b", "NUL"},
		{"NUL refused in secret", true, "abcdefgh\x00", "NUL"},
		{"invalid utf-8 refused", false, "a\xffb", "UTF-8"},
		{"plain over the byte limit", false, strings.Repeat("a", MaxValueBytes+1), "at most 65536 bytes"},
		{"plain at the byte limit", false, strings.Repeat("a", MaxValueBytes), ""},
		{"secret over the byte limit", true, strings.Repeat("a", MaxValueBytes+1), "at most 65536 bytes"},
		{"secret at the byte limit", true, strings.Repeat("a", MaxValueBytes), ""},
		{"userinfo with password as plain", false, "postgres://app:pw@db.example.test/app", "carrying a user or password"},
		{"userinfo user only as plain", false, "https://token@example.test/", "carrying a user or password"},
		{"userinfo with leading space as plain", false, "  https://u:p@example.test", "carrying a user or password"},
		{"unparsable userinfo as plain", false, "https://u:%zz@example.test", "carrying a user or password"},
		{"userinfo url as secret accepted", true, "postgres://app:pw@db.example.test/app", ""},
	}
	for _, tc := range tests {
		err := ValidateValue(tc.secret, tc.value)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: got %v, want an error naming %q", tc.name, err, tc.wantErr)
		}
		if err != nil && strings.Contains(err.Error(), tc.value) && tc.value != "" {
			t.Errorf("%s: the error echoes the value", tc.name)
		}
	}
}

func TestValidateDescription(t *testing.T) {
	if err := ValidateDescription(strings.Repeat("a", MaxDescriptionBytes)); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := ValidateDescription(strings.Repeat("a", MaxDescriptionBytes+1)); err == nil {
		t.Error("one byte past the limit accepted")
	}
	// 512 two-byte runes are exactly 1024 bytes; one more rune is past it.
	if err := ValidateDescription(strings.Repeat("é", 512)); err != nil {
		t.Errorf("1024 bytes of UTF-8: %v", err)
	}
	if err := ValidateDescription(strings.Repeat("é", 513)); err == nil {
		t.Error("1026 bytes of UTF-8 accepted")
	}
	if err := ValidateDescription(""); err != nil {
		t.Errorf("empty description: %v", err)
	}
}

func TestAADKindSeparatesContexts(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	seen := map[string]string{}
	for label, k := range map[string]string{
		"repo a secret X":  AADKind(ScopeRepo, &a, true, "X"),
		"repo a plain X":   AADKind(ScopeRepo, &a, false, "X"),
		"repo b secret X":  AADKind(ScopeRepo, &b, true, "X"),
		"repo a secret Y":  AADKind(ScopeRepo, &a, true, "Y"),
		"user secret X":    AADKind(ScopeUser, nil, true, "X"),
		"user plain X":     AADKind(ScopeUser, nil, false, "X"),
		"repo a secret XY": AADKind(ScopeRepo, &a, true, "XY"),
	} {
		if prev, dup := seen[k]; dup {
			t.Errorf("%s and %s share a sealing context", label, prev)
		}
		seen[k] = label
	}
	first, second := AADKind(ScopeRepo, &a, true, "X"), AADKind(ScopeRepo, &a, true, "X")
	if first != second {
		t.Error("AADKind is not deterministic")
	}
	want := "uzi-run-env-v1\x00user\x00\x001\x00\x00\x00\x00\x03ABC"
	if got := AADKind(ScopeUser, nil, true, "ABC"); got != want {
		t.Errorf("layout = %q, want %q", got, want)
	}
}
