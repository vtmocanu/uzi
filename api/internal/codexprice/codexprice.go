// Package codexprice reports coverage from the agent's canonical Codex pricing table.
package codexprice

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed codex-pricing.json
var canonicalBytes []byte

var table = mustLoadPricing(canonicalBytes)

// Status distinguishes unknown prices from expired promotional prices.
type Status string

const (
	Priced       Status = "priced"
	NoPrice      Status = "no_price"
	PromoExpired Status = "promo_expired"
)

// Coverage matches exact model identifiers and expires promotions at UTC midnight.
func Coverage(model string, now time.Time) Status {
	review, ok := table[model]
	if !ok {
		return NoPrice
	}
	if review != nil && !now.Before(*review) {
		return PromoExpired
	}
	return Priced
}

// PricedModels returns a sorted, nonnil list of currently covered identifiers.
func PricedModels(now time.Time) []string {
	models := make([]string, 0, len(table))
	for model := range table {
		if Coverage(model, now) == Priced {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

func mustLoadPricing(data []byte) map[string]*time.Time {
	models, err := loadPricing(data)
	if err != nil {
		panic(fmt.Sprintf("codexprice: invalid embedded codex-pricing.json: %v", err))
	}
	return models
}

// loadPricing validates final decoded values, matching JSON.parse's last-key-wins
// policy. UseNumber retains overflow numbers for validation, including when an
// invalid earlier value is overwritten during decoding.
func loadPricing(data []byte) (map[string]*time.Time, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("table: expected valid UTF-8 bytes")
	}
	if err := validateJSONStrings(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("table: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("table: expected exactly one JSON value")
	}
	root, err := object(value, "table", []string{"version", "input_tier_threshold_tokens", "models"})
	if err != nil {
		return nil, err
	}
	if version, ok := root["version"].(string); !ok || version == "" {
		return nil, fmt.Errorf("version: expected nonempty string")
	}
	threshold, err := number(root["input_tier_threshold_tokens"], "input_tier_threshold_tokens")
	if err != nil || threshold <= 0 || math.Trunc(threshold) != threshold {
		return nil, fmt.Errorf("input_tier_threshold_tokens: expected positive finite integer")
	}
	rows, ok := root["models"].(map[string]any)
	if !ok || len(rows) == 0 {
		return nil, fmt.Errorf("models: expected nonempty object")
	}
	models := make(map[string]*time.Time, len(rows))
	for model, value := range rows {
		if model == "" {
			return nil, fmt.Errorf("models: expected nonempty model identifier")
		}
		path := "models." + model
		row, err := object(value, path, []string{"verified_at", "sources", "low", "high"}, "promo_review_date")
		if err != nil {
			return nil, err
		}
		if _, err := date(row["verified_at"], path+".verified_at"); err != nil {
			return nil, err
		}
		var review *time.Time
		if value, present := row["promo_review_date"]; present {
			d, err := date(value, path+".promo_review_date")
			if err != nil {
				return nil, err
			}
			review = &d
		}
		sources, ok := row["sources"].([]any)
		if !ok || len(sources) == 0 {
			return nil, fmt.Errorf("%s.sources: expected nonempty array", path)
		}
		for i, value := range sources {
			if err := source(value, fmt.Sprintf("%s.sources[%d]", path, i)); err != nil {
				return nil, err
			}
		}
		for _, tier := range []string{"low", "high"} {
			buckets := []string{"uncached_input", "cached_input", "cache_write", "output"}
			rates, err := object(row[tier], path+"."+tier, buckets)
			if err != nil {
				return nil, err
			}
			for _, bucket := range buckets {
				ratePath := path + "." + tier + "." + bucket
				rate, err := number(rates[bucket], ratePath)
				if err != nil {
					return nil, err
				}
				if rate < 0 {
					return nil, fmt.Errorf("%s: expected nonnegative finite number", ratePath)
				}
			}
		}
		models[model] = review
	}
	return models, nil
}

// validateJSONStrings checks escape lexemes before encoding/json can replace them.
// This is a pricing-local scan; the native decoder still owns all JSON syntax.
func validateJSONStrings(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) || data[i] != 'u' || i+4 >= len(data) {
			continue
		}
		unit, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			continue // Native JSON decoding rejects malformed escapes.
		}
		i += 4
		if unit < 0xd800 || unit > 0xdfff {
			continue
		}
		if unit <= 0xdbff && i+6 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err == nil && low >= 0xdc00 && low <= 0xdfff {
				i += 6
				continue
			}
		}
		return fmt.Errorf("table: JSON strings must contain well-formed Unicode")
	}
	return nil
}

func object(value any, path string, required []string, optional ...string) (map[string]any, error) {
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected object", path)
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		if _, ok := result[key]; !ok {
			return nil, fmt.Errorf("%s.%s: missing required field", path, key)
		}
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range result {
		if !allowed[key] {
			return nil, fmt.Errorf("%s.%s: unknown field", path, key)
		}
	}
	return result, nil
}

func number(value any, path string) (float64, error) {
	n, ok := value.(json.Number)
	if ok {
		f, err := n.Float64()
		if err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, nil
		}
	}
	return 0, fmt.Errorf("%s: expected finite number", path)
}

var datePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func date(value any, path string) (time.Time, error) {
	s, ok := value.(string)
	if ok && datePattern.MatchString(s) && !strings.HasPrefix(s, "0000") {
		d, err := time.Parse("2006-01-02", s)
		if err == nil {
			return d, nil
		}
	}
	return time.Time{}, fmt.Errorf("%s: expected real Gregorian YYYY-MM-DD date in years 0001–9999", path)
}

var (
	sourcePattern = regexp.MustCompile(`^https://([^/:?#]+)(?::([0-9]+))?([/?#][A-Za-z0-9._~!$&'()*+,;=:@/?#%+-]*)?$`)
	labelPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	ipv4Pattern   = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){3}$`)
	escapePattern = regexp.MustCompile(`^[0-9A-Fa-f]{2}$`)
)

func source(value any, path string) error {
	fail := func() error { return fmt.Errorf("%s: expected portable ASCII HTTPS URL", path) }
	s, ok := value.(string)
	if !ok {
		return fail()
	}
	for _, c := range s {
		if c < 0x21 || c > 0x7e {
			return fail()
		}
	}
	match := sourcePattern.FindStringSubmatch(s)
	if match == nil || len(match[1]) > 253 {
		return fail()
	}
	labels := strings.Split(match[1], ".")
	for _, label := range labels {
		if len(label) > 63 || !labelPattern.MatchString(label) {
			return fail()
		}
	}
	if ipv4Pattern.MatchString(match[1]) {
		for _, label := range labels {
			n, err := strconv.ParseFloat(label, 64)
			if err != nil || n > 255 {
				return fail()
			}
		}
	}
	if match[2] != "" {
		n, err := strconv.ParseFloat(match[2], 64)
		if err != nil || n < 1 || n > 65535 {
			return fail()
		}
	}
	suffix := match[3]
	for i := 0; i < len(suffix); i++ {
		if suffix[i] == '%' {
			if i+2 >= len(suffix) || !escapePattern.MatchString(suffix[i+1:i+3]) {
				return fail()
			}
			i += 2
		}
	}
	return nil
}
