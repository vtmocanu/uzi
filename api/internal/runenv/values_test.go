package runenv

import (
	"errors"
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
		{"jdbc scheme chain with userinfo", false, "jdbc:postgresql://u:p@db.example/app", "carrying a user or password"},
		{"quoted url with userinfo", false, "\"https://u:p@h.example\"", "carrying a user or password"},
		{"url after a comma with userinfo", false, "primary,https://u:p@h.example", "carrying a user or password"},
		{"url on a later line with userinfo", false, "x\nhttps://u:p@h.example", "carrying a user or password"},
		{"at sign after the host path accepted", false, "https://h.example/path?x=a@b", ""},
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

// ctx builds "a<rune>b" from a code point, so the test source holds no invisible character.
func ctx(r rune) string { return "a" + string(r) + "b" }

// TestValidateDescriptionRejectsControlAndFormatCharacters pins the carried M2 W0 rule: a
// description is shown on the run page and put in the lead's prompt, so it must be plain text.
func TestValidateDescriptionRejectsControlAndFormatCharacters(t *testing.T) {
	bad := map[string]string{
		"TAB":                ctx(0x09),
		"LF":                 ctx(0x0a),
		"CR":                 ctx(0x0d),
		"SOH":                ctx(0x01),
		"US (last C0)":       ctx(0x1f),
		"ESC":                "\x1b[31mred",
		"DEL":                ctx(0x7f),
		"first C1":           ctx(0x80),
		"NEL (C1)":           ctx(0x85),
		"last C1":            ctx(0x9f),
		"ZWSP":               ctx(0x200b),
		"ZWNJ":               ctx(0x200c),
		"RLM (last of 200F)": ctx(0x200f),
		"LINE SEPARATOR":     ctx(0x2028),
		"PARA SEPARATOR":     ctx(0x2029),
		"LRE":                ctx(0x202a),
		"RLO (last of 202E)": ctx(0x202e),
		"WORD JOINER":        ctx(0x2060),
		"INVISIBLE PLUS":     ctx(0x2064),
		"LRI":                ctx(0x2066),
		"PDI":                ctx(0x2069),
		"last of 2066..206F": ctx(0x206f),
		"BOM":                ctx(0xfeff),
	}
	for label, d := range bad {
		err := ValidateDescription(d)
		var rule *RuleError
		if !errors.As(err, &rule) || rule.Field != "description" {
			t.Errorf("%s: ValidateDescription = %v, want a description rule error", label, err)
		}
	}
	ok := map[string]string{
		"ascii":                       "Base URL of the stub service",
		"tilde, last printable ASCII": ctx(0x7e),
		"NBSP, first after C1":        ctx(0xa0),
		"e acute":                     ctx(0xe9),
		"just before 200B":            ctx(0x200a),
		"just after 200F":             ctx(0x2010),
		"just after 2029":             ctx(0x202f),
		"just before 2060":            ctx(0x205f),
		"2065 unassigned":             ctx(0x2065),
		"just after 206F":             ctx(0x2070),
		"just before BOM":             ctx(0xfefe),
		"CJK and emoji":               ctx(0x6d4b) + ctx(0x1f600),
	}
	for label, d := range ok {
		if err := ValidateDescription(d); err != nil {
			t.Errorf("%s: ValidateDescription rejected a plain description: %v", label, err)
		}
	}
}

func TestAADKindV2BindsVarIDAndRev(t *testing.T) {
	a := uuid.New()
	v1, v2 := uuid.New(), uuid.New()
	seen := map[string]string{}
	for label, k := range map[string]string{
		"base":          AADKindV2(ScopeRepo, &a, true, "X", v1, 1),
		"other rev":     AADKindV2(ScopeRepo, &a, true, "X", v1, 2),
		"other var":     AADKindV2(ScopeRepo, &a, true, "X", v2, 1),
		"plain":         AADKindV2(ScopeRepo, &a, false, "X", v1, 1),
		"other name":    AADKindV2(ScopeRepo, &a, true, "Y", v1, 1),
		"user scope":    AADKindV2(ScopeUser, nil, true, "X", v1, 1),
		"v1 context":    AADKind(ScopeRepo, &a, true, "X"),
		"rev 12":        AADKindV2(ScopeRepo, &a, true, "X", v1, 12),
		"name X1 rev 2": AADKindV2(ScopeRepo, &a, true, "X1", v1, 2),
		"name X rev 12": AADKindV2(ScopeRepo, &a, true, "X", v1, 112),
	} {
		if prev, dup := seen[k]; dup {
			t.Errorf("%s and %s share a sealing context", label, prev)
		}
		seen[k] = label
	}
	if !strings.HasPrefix(AADKindV2(ScopeUser, nil, true, "ABC", v1, 7), "uzi-run-env-v2") {
		t.Error("v2 context does not carry the v2 domain tag")
	}
}

func TestAADForPicksByVersion(t *testing.T) {
	a, v := uuid.New(), uuid.New()
	row := Row{AADVersion: AADVersion1, RepoID: &a, Secret: true, Name: "TOKEN", VarID: v, Rev: 3}
	if got, want := AADFor(row), AADKind(ScopeRepo, &a, true, "TOKEN"); got != want {
		t.Errorf("version 1 row: AADFor = %q, want the v1 context", got)
	}
	row.AADVersion = AADVersion2
	if got, want := AADFor(row), AADKindV2(ScopeRepo, &a, true, "TOKEN", v, 3); got != want {
		t.Errorf("version 2 row: AADFor = %q, want the v2 context", got)
	}
	row.RepoID = nil
	if got, want := AADFor(row), AADKindV2(ScopeUser, nil, true, "TOKEN", v, 3); got != want {
		t.Errorf("user-scope version 2 row: AADFor = %q, want the v2 user context", got)
	}
}
