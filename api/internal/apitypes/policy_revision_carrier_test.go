package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPolicyRevisionCarrier(t *testing.T) {
	const id = "3068abcd-0123-4567-89ab-0123456789ab"
	const digest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	const human = `{"source":"run_user_input","input_id":"9223372036854775807"}`
	const crossCheck = `{"source":"plan_cross_check","cross_check_id":"` + id + `"}`
	const gate = `{"revision":"9007199254740993","presentation_id":"` + id + `","payload_digest":"` + digest + `"}`
	wrap := func(g, r string) string { return `{"gate":` + g + `,"revision":` + r + `}` }
	for _, tc := range []struct {
		name, raw, source string
		hasGate           bool
	}{
		{"human max int64 known absence", wrap("null", human), "run_user_input", false},
		{"cross-check exact gate", wrap(gate, crossCheck), "plan_cross_check", true},
		{"human exact gate", wrap(gate, human), "run_user_input", true},
		{"cross-check known absence", wrap("null", crossCheck), "plan_cross_check", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePolicyRevisionCarrier([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got.Revision.Source != tc.source || (got.Gate != nil) != tc.hasGate {
				t.Fatalf("carrier = %+v", got)
			}
			if tc.source == "run_user_input" && (got.Revision.InputID == nil || *got.Revision.InputID != "9223372036854775807" || got.Revision.CrossCheckID != nil) {
				t.Fatalf("human reference = %+v", got.Revision)
			}
			if tc.source == "plan_cross_check" && (got.Revision.CrossCheckID == nil || *got.Revision.CrossCheckID != id || got.Revision.InputID != nil) {
				t.Fatalf("cross-check row reference = %+v", got.Revision)
			}
			if got.Gate != nil && (got.Gate.Revision != "9007199254740993" || got.Gate.PresentationID != id || got.Gate.PayloadDigest != digest) {
				t.Fatalf("gate = %+v", got.Gate)
			}
			raw, err := json.Marshal(got)
			if err != nil || string(raw) != tc.raw {
				t.Fatalf("round trip = %s, err = %v", raw, err)
			}
		})
	}

	invalid := map[string]string{
		"null carrier":          "null",
		"absent carrier":        "",
		"empty carrier":         "{}",
		"array carrier":         "[]",
		"missing gate":          `{"revision":` + human + `}`,
		"missing revision":      `{"gate":null}`,
		"null revision":         wrap("null", "null"),
		"carrier alias":         `{"Gate":null,"revision":` + human + `}`,
		"carrier duplicate":     `{"gate":null,"gate":null,"revision":` + human + `}`,
		"carrier alias extra":   `{"Gate":null,"gate":null,"revision":` + human + `}`,
		"trailing value":        wrap("null", human) + "{}",
		"unknown source":        wrap("null", `{"source":"latest","input_id":"1"}`),
		"missing union id":      wrap("null", `{"source":"run_user_input"}`),
		"union null opposite":   wrap("null", `{"source":"run_user_input","input_id":"1","cross_check_id":null}`),
		"union both arms":       wrap("null", `{"source":"run_user_input","input_id":"1","cross_check_id":"`+id+`"}`),
		"union wrong arm":       wrap("null", `{"source":"plan_cross_check","input_id":"1"}`),
		"union duplicate":       wrap("null", `{"source":"run_user_input","input_id":"1","input_id":"2"}`),
		"union alias":           wrap("null", `{"Source":"run_user_input","input_id":"1"}`),
		"union alias extra":     wrap("null", `{"source":"run_user_input","Source":"run_user_input","input_id":"1"}`),
		"gate missing digest":   wrap(`{"revision":"1","presentation_id":"`+id+`"}`, human),
		"gate alias":            wrap(strings.Replace(gate, "presentation_id", "Presentation_ID", 1), human),
		"gate duplicate":        wrap(strings.Replace(gate, `"revision":`, `"revision":"1","revision":`, 1), human),
		"gate null revision":    wrap(strings.Replace(gate, `"9007199254740993"`, "null", 1), human),
		"gate null id":          wrap(strings.Replace(gate, `"`+id+`"`, "null", 1), human),
		"gate uppercase uuid":   wrap(strings.Replace(gate, id, strings.ToUpper(id), 1), human),
		"gate uppercase digest": wrap(strings.Replace(gate, digest, strings.ToUpper(digest), 1), human),
		"gate short digest":     wrap(strings.Replace(gate, digest, "abcd", 1), human),
		"gate leading zero":     wrap(strings.Replace(gate, "9007199254740993", "01", 1), human),
	}
	for _, extra := range []string{"feedback", "worker_approval", "generation", "body"} {
		invalid["carrier extra "+extra] = `{"gate":null,"revision":` + human + `,"` + extra + `":null}`
		invalid["gate extra "+extra] = wrap(strings.TrimSuffix(gate, "}")+`,"`+extra+`":null}`, human)
		invalid["union extra "+extra] = wrap("null", strings.TrimSuffix(human, "}")+`,"`+extra+`":null}`)
	}
	for _, value := range []string{`"0"`, `"-1"`, `"+1"`, `"01"`, `" 1"`, `"1 "`, `"1.0"`, `"1e2"`, `"9223372036854775808"`, "1", "null"} {
		invalid["human decimal "+value] = wrap("null", `{"source":"run_user_input","input_id":`+value+`}`)
	}
	for _, value := range []string{"", strings.ToUpper(id), "{" + id + "}", strings.ReplaceAll(id, "-", ""), "not-a-uuid"} {
		invalid["row UUID "+value] = wrap("null", `{"source":"plan_cross_check","cross_check_id":"`+value+`"}`)
	}
	for name, raw := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePolicyRevisionCarrier([]byte(raw)); err == nil {
				t.Fatalf("accepted invalid carrier: %s", raw)
			}
		})
	}
}
