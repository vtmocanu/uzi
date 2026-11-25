package uzicli

import (
	"net/http"
	"strings"
	"testing"
)

// Issue #3263 module A widened the PUT /api/me/model-settings body: a 200 also carries the
// effective settings (or null plus effective_error) and a 400 also carries an errors array. The
// CLI keeps reading only settings on success and the flat error on failure, and prints the
// envelope it was given byte for byte.

func TestPutModelSettingsToleratesEffectiveInTheEnvelope(t *testing.T) {
	for name, envelope := range map[string]string{
		"effective object":          `{"settings":{"judge_model":"m"},"effective":{"version":1,"models":[],"future":{"a":1}}}`,
		"effective null with error": `{"settings":{"judge_model":"m"},"effective":null,"effective_error":"settings_effective_readback_failed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			res, _, err := putModelSettingsAgainst(t, http.StatusOK, "application/json", envelope)
			if err != nil {
				t.Fatalf("a 200 with the new keys must still succeed: %v", err)
			}
			if string(res.Raw) != envelope {
				t.Fatalf("Raw = %s, want the server envelope unchanged", res.Raw)
			}
			if res.Settings.JudgeModel == nil || *res.Settings.JudgeModel != "m" {
				t.Fatalf("Settings.JudgeModel = %v", res.Settings.JudgeModel)
			}
		})
	}
}

// A 400 carrying the errors array still surfaces the flat error text as a usage error; the list
// is not required and not parsed.
func TestPutModelSettingsFieldErrorWithErrorsArray(t *testing.T) {
	body := `{"error":"unknown field aaa","field":"aaa","errors":[` +
		`{"field":"aaa","message":"unknown field aaa"},` +
		`{"field":"cross_check_pins","message":"cross_check_pins plan/claude effort: bad","stage":"plan","harness":"claude","member":"effort"}]}`
	_, _, err := putModelSettingsAgainst(t, http.StatusBadRequest, "application/json", body)
	ee := exitOf(t, err)
	if ee.Code != ExitUsage || ee.Error() != "unknown field aaa" {
		t.Fatalf("code=%d msg=%q, want ExitUsage with the flat error", ee.Code, ee.Error())
	}
	if strings.Contains(ee.Error(), "cross_check_pins") {
		t.Fatalf("only the first error is surfaced: %q", ee.Error())
	}
}
