package codexprice

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCanonicalMirror(t *testing.T) {
	canonical, err := os.ReadFile("../../../agent/src/codex/codex-pricing.json")
	if err != nil {
		t.Fatalf("CodexPricingMirror: %v", err)
	}
	if !bytes.Equal(canonicalBytes, canonical) {
		t.Fatal("CodexPricingMirror: embedded bytes differ from canonical agent table; run task codex-pricing:sync")
	}
}

func TestCoverage(t *testing.T) {
	for _, tc := range []struct {
		model, clock string
		want         Status
	}{
		{"unknown", "2026-11-20T00:00:00Z", NoPrice},
		{"constructor", "2026-11-20T00:00:00Z", NoPrice},
		{"__proto__", "2026-11-20T00:00:00Z", NoPrice},
		{"gpt-5.6-sol", "2026-11-20T23:59:59Z", Priced},
		{"gpt-5.6-sol", "2026-11-21T00:00:00Z", PromoExpired},
		{"gpt-5.6-sol", "2026-11-22T00:00:00Z", PromoExpired},
		{"gpt-5.6-sol", "2026-11-20T19:00:00-05:00", PromoExpired},
		{"gpt-6.1-sol", "2027-01-01T00:00:00Z", Priced},
		{"gpt-6-sol", "2026-11-21T00:00:00Z", Priced},
		{"gpt-6-astra", "2026-11-21T00:00:00Z", Priced},
	} {
		now, err := time.Parse(time.RFC3339, tc.clock)
		if err != nil {
			t.Fatal(err)
		}
		if got := Coverage(tc.model, now); got != tc.want {
			t.Errorf("%s at %s = %s, want %s", tc.model, tc.clock, got, tc.want)
		}
	}
	before := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	after := before.AddDate(0, 0, 1)
	if got := PricedModels(before); !reflect.DeepEqual(got, []string{"gpt-5.6-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol"}) {
		t.Fatalf("before: %v", got)
	}
	if got := PricedModels(after); !reflect.DeepEqual(got, []string{"gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol"}) {
		t.Fatalf("after: %v", got)
	}
}

func changed(t *testing.T, path []string, value any, remove bool) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(canonicalBytes, &root); err != nil {
		t.Fatal(err)
	}
	parent := root
	for _, key := range path[:len(path)-1] {
		parent = parent[key].(map[string]any)
	}
	key := path[len(path)-1]
	if remove {
		delete(parent, key)
	} else {
		parent[key] = value
	}
	data, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStrictLoader(t *testing.T) {
	row := []string{"models", "gpt-5.6-sol"}
	paths := [][]string{{"version"}, {"input_tier_threshold_tokens"}, {"models"}}
	for _, key := range []string{"verified_at", "sources", "low", "high"} {
		paths = append(paths, append(append([]string{}, row...), key))
	}
	for _, tier := range []string{"low", "high"} {
		for _, bucket := range []string{"uncached_input", "cached_input", "cache_write", "output"} {
			paths = append(paths, []string{"models", "gpt-5.6-sol", tier, bucket})
		}
	}
	for _, path := range paths {
		for _, missing := range []bool{false, true} {
			t.Run(strings.Join(path, ".")+map[bool]string{true: "/missing", false: "/null"}[missing], func(t *testing.T) {
				if _, err := loadPricing(changed(t, path, nil, missing)); err == nil || !strings.Contains(err.Error(), path[len(path)-1]) {
					t.Fatalf("expected named rejection, got %v", err)
				}
			})
		}
	}
	cases := []struct {
		path   []string
		values []any
	}{
		{[]string{"version"}, []any{"", 1, false, []any{}, map[string]any{}}},
		{[]string{"input_tier_threshold_tokens"}, []any{0, -1, 0.5, 272000.5, "272000", false, []any{}, map[string]any{}}},
		{[]string{"models"}, []any{map[string]any{}, []any{}, "", 1, true}},
		{row, []any{nil, []any{}, "", 1, true}},
		{append(append([]string{}, row...), "sources"), []any{nil, "", map[string]any{}, false, []any{}}},
	}
	for _, tc := range cases {
		for _, value := range tc.values {
			if _, err := loadPricing(changed(t, tc.path, value, false)); err == nil {
				t.Errorf("accepted %v = %#v", tc.path, value)
			}
		}
	}
	for _, tier := range []string{"low", "high"} {
		tierPath := []string{"models", "gpt-5.6-sol", tier}
		for _, value := range []any{nil, []any{}, "", 1, true} {
			if _, err := loadPricing(changed(t, tierPath, value, false)); err == nil {
				t.Errorf("accepted nonobject %s", tier)
			}
		}
		for _, bucket := range []string{"uncached_input", "cached_input", "cache_write", "output"} {
			path := append(append([]string{}, tierPath...), bucket)
			for _, value := range []any{"1", false, []any{}, map[string]any{}, -0.1} {
				if _, err := loadPricing(changed(t, path, value, false)); err == nil {
					t.Errorf("accepted %v = %#v", path, value)
				}
			}
			if _, err := loadPricing(changed(t, path, 0, false)); err != nil {
				t.Errorf("zero %v: %v", path, err)
			}
		}
	}
	for _, path := range [][]string{{"extra"}, {"models", "extra"}, {"models", ""}, {"models", "gpt-5.6-sol", "extra"}, {"models", "gpt-5.6-sol", "low", "extra"}, {"models", "gpt-5.6-sol", "high", "extra"}} {
		if _, err := loadPricing(changed(t, path, 0, false)); err == nil {
			t.Errorf("accepted extra %v", path)
		}
	}
	for _, field := range []string{"verified_at", "promo_review_date"} {
		path := []string{"models", "gpt-5.6-sol", field}
		for _, value := range []any{nil, 20261001, false, []any{}, map[string]any{}, "", "2026-2-01", "2026-02-29", "2026-02-30", "1900-02-29", "0000-01-01", "10000-01-01", "2026-00-01", "2026-13-01", "2026-01-00", "2026-04-31", "2026-01-32", "2026-01-01T00:00:00Z", "2026-01-01\n"} {
			if _, err := loadPricing(changed(t, path, value, false)); err == nil {
				t.Errorf("accepted %s = %#v", field, value)
			}
		}
		for _, value := range []string{"0001-01-01", "9999-12-31", "0004-02-29", "2000-02-29", "2024-02-29", "1900-02-28"} {
			if _, err := loadPricing(changed(t, path, value, false)); err != nil {
				t.Errorf("%s %s: %v", field, value, err)
			}
		}
	}
	if _, err := loadPricing(changed(t, []string{"models", "gpt-5.6-sol", "promo_review_date"}, nil, true)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"null", "[]", "true", "1", `""`, string(canonicalBytes) + " {}", string(canonicalBytes) + " garbage"} {
		if _, err := loadPricing([]byte(raw)); err == nil {
			t.Errorf("accepted malformed/trailing document")
		}
	}
}

func TestSourcesPortableASCII(t *testing.T) {
	path := []string{"models", "gpt-5.6-sol", "sources"}
	bad := []any{nil, 1, false, map[string]any{}, []any{}, "", "http://example.com", "HTTPS://example.com", "https://", "https:///path", "https://?x", "https://#x", "https://example.com/a b", "https://example.com/\n", "https://example.com/\u0000", " https://example.com", "https://example.com/\\x", "https://user@example.com", "https://-bad.example", "https://bad-.example", "https://example..com", "https://example.com:", "https://example.com:0", "https://example.com:65536", "https://example.com/%zz", "https://example.com/%1", "https://999.1.1.1", "https://[::1]", "https://é.example", "https://" + strings.Repeat("a", 64), "https://" + strings.Repeat("abc.", 64) + "abc"}
	for _, value := range bad {
		if _, err := loadPricing(changed(t, path, []any{value}, false)); err == nil {
			t.Errorf("accepted source %#v", value)
		}
	}
	for _, value := range []string{"https://example.com", "https://example.com:443/a%20b?q=x#y", "https://127.0.0.1/path", "https://example.com:00001/", "https://example.com:65535/#%ff", "https://EXAMPLE.com/?a=~!$&'()*+,;=:@/?#%20+-", "https://" + strings.Repeat("a", 63)} {
		if _, err := loadPricing(changed(t, path, []any{value}, false)); err != nil {
			t.Errorf("valid source %s: %v", value, err)
		}
	}
}

func TestRawNumbersAndDuplicates(t *testing.T) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, canonicalBytes); err != nil {
		t.Fatal(err)
	}
	raw := compact.String()
	for _, target := range []string{`"input_tier_threshold_tokens":272000`, `"uncached_input":10`} {
		key := strings.Split(target, ":")[0]
		for _, value := range []string{"1e400", "-1e400", "NaN", "Infinity", "-Infinity"} {
			if _, err := loadPricing([]byte(strings.Replace(raw, target, key+":"+value, 1))); err == nil {
				t.Errorf("accepted %s = %s", key, value)
			}
		}
	}
	for _, pair := range []struct{ old, replacement string }{
		{`"version":`, `"version":null,"version":`},
		{`"models":`, `"models":{"bad":{"extra":1}},"models":`},
		{`"gpt-6-astra":`, `"gpt-6-astra":{"extra":1},"gpt-6-astra":`},
		{`"low":`, `"low":{"extra":1},"low":`},
		{`"uncached_input":10`, `"uncached_input":1e400,"uncached_input":10`},
		{`"gpt-6-astra":`, `"__proto__":`},
	} {
		if _, err := loadPricing([]byte(strings.Replace(raw, pair.old, pair.replacement, 1))); err != nil {
			t.Errorf("last-wins/own key: %v", err)
		}
	}
	if _, err := loadPricing([]byte(strings.Replace(raw, `"version":"openai-standard-2026-10-01"`, `"version":"ok","version":null`, 1))); err == nil {
		t.Fatal("accepted invalid final duplicate")
	}
}

func TestPricedModelsEmptyAndEarliestPromo(t *testing.T) {
	original := table
	t.Cleanup(func() { table = original })
	table = map[string]*time.Time{}
	if models := PricedModels(time.Now()); models == nil || len(models) != 0 {
		t.Fatalf("empty list = %#v", models)
	}
	loaded, err := loadPricing(changed(t, []string{"models", "gpt-5.6-sol", "promo_review_date"}, "0001-01-01", false))
	if err != nil {
		t.Fatal(err)
	}
	table = loaded
	if got := Coverage("gpt-5.6-sol", time.Time{}); got != PromoExpired {
		t.Fatalf("earliest promo: %s", got)
	}
}

func TestExactModelKeysRawUnicode(t *testing.T) {
	suite, err := os.ReadFile("../../../scripts/pricing-freshness.test.sh")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(suite), "cat <<'UNICODE_FIXTURES'\n", 2)
	if len(parts) != 2 {
		t.Fatal("missing shared raw corpus")
	}
	corpus := strings.SplitN(parts[1], "\nUNICODE_FIXTURES", 2)[0]
	var canonical map[string]any
	if err := json.Unmarshal(canonicalBytes, &canonical); err != nil {
		t.Fatal(err)
	}
	rowBytes, err := json.Marshal(canonical["models"].(map[string]any)["gpt-6-astra"])
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(corpus, "\n") {
		fields := strings.SplitN(line, "|", 3)
		t.Run(fields[0], func(t *testing.T) {
			_, err := loadPricing([]byte(strings.ReplaceAll(fields[2], "@ROW@", string(rowBytes))))
			if (err == nil) != (fields[1] == "accept") {
				t.Fatalf("%s: %v", fields[1], err)
			}
		})
	}
}

func TestRawUTF8Rejection(t *testing.T) {
	suite, err := os.ReadFile("../../../scripts/pricing-freshness.test.sh")
	if err != nil {
		t.Fatal(err)
	}
	corpus := strings.Split(strings.Split(string(suite), "cat <<'RAW_UTF8_FIXTURES'\n")[1], "\nRAW_UTF8_FIXTURES")[0]
	var canonical map[string]any
	if err := json.Unmarshal(canonicalBytes, &canonical); err != nil {
		t.Fatal(err)
	}
	row := canonical["models"].(map[string]any)["gpt-6-astra"].(map[string]any)
	rowBytes, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	row["promo_review_date"] = "2026-01-01"
	expiredBytes, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	original := table
	t.Cleanup(func() { table = original })
	// Fixed corpus and four placements; failures are reported independently.
	for _, line := range strings.Split(corpus, "\n") {
		fields := strings.Split(line, "|")
		t.Run(fields[0], func(t *testing.T) {
			raw, err := hex.DecodeString(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			prefix := `{"version":"fixture","input_tier_threshold_tokens":1,`
			placements := [][2]string{
				{`"models":{"`, `":` + string(rowBytes) + `,"�":` + string(expiredBytes) + "}}"},
				{`"models":{"�":` + string(expiredBytes) + `,"`, `":` + string(rowBytes) + "}}"},
				{`"version":"`, `","version":"fixture","models":{"model":` + string(rowBytes) + "}}"},
				{`"models":{"discard":{"text":"`, `"}},"models":{"model":` + string(rowBytes) + "}}"},
			}
			for i, placement := range placements {
				data := append([]byte(prefix+placement[0]), raw...)
				data = append(data, []byte(placement[1])...)
				loaded, err := loadPricing(data)
				if fields[1] == "reject" {
					if err == nil || !strings.Contains(err.Error(), "UTF-8") {
						t.Errorf("raw UTF8 rejection placement %d: %v", i, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					table = loaded
					if i < 2 {
						want := Priced
						if string(raw) == "�" && i == 0 {
							want = PromoExpired
						}
						if Coverage(string(raw), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) != want {
							t.Errorf("lost exact key %x", raw)
						}
						replacementWant := PromoExpired
						if string(raw) == "�" && i == 1 {
							replacementWant = Priced
						}
						if Coverage("�", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) != replacementWant {
							t.Error("lost legitimate UFFFD coverage")
						}
						wantCount := 2
						if string(raw) == "�" {
							wantCount = 1
						}
						if len(loaded) != wantCount {
							t.Errorf("unexpected keys: %v", loaded)
						}
					}
				}
			}
		})
	}
}

func TestCoverageExactUnicodeKeys(t *testing.T) {
	original := table
	t.Cleanup(func() { table = original })
	var canonical map[string]any
	if err := json.Unmarshal(canonicalBytes, &canonical); err != nil {
		t.Fatal(err)
	}
	row := canonical["models"].(map[string]any)["gpt-6-astra"].(map[string]any)
	rowBytes, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	row["promo_review_date"] = "2026-01-01"
	expiredBytes, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"version":"fixture","input_tier_threshold_tokens":1,"models":{"�":` + string(expiredBytes) +
		`,"𐀀":` + string(expiredBytes) + `,"\ud800\udc00":` + string(rowBytes) +
		`,"é":` + string(expiredBytes) + `,"e\u0301":` + string(rowBytes) +
		`,"\udbff\udfff":` + string(expiredBytes) + "}}"
	table, err = loadPricing([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model string
		want  Status
	}{
		{"�", PromoExpired}, {"𐀀", Priced}, {"é", PromoExpired}, {"e\\u0301", NoPrice},
		{"e\u0301", Priced}, {"\U0010ffff", PromoExpired}, {"unknown", NoPrice},
	} {
		if got := Coverage(tc.model, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); got != tc.want {
			t.Errorf("%q = %s, want %s", tc.model, got, tc.want)
		}
	}
}
