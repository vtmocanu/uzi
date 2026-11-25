package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #3263 module A: PUT /api/me/model-settings now also answers "effective" (or null plus
// "effective_error"). The one-field `uzi settings set` path, run through the real HTTP client,
// behaves exactly as with the old envelope: exit 0, the same human output, and --json printing the
// server's body untouched.

const settingsSetSettingsPart = `"settings":{"judge_model":"m","judge_models":[` +
	`{"harness":"claude","model":"m","default_model":"","default_source":"","resolved_model":"m","source":"user_default"}]}`

func settingsSetAgainst(t *testing.T, putBody string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/me/model-settings" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(putBody))
			return
		}
		// The effective-settings read the CLI makes afterwards: an older-server 404.
		http.NotFound(w, r)
	}))
	defer srv.Close()
	return runCLI(t, httpEnv(srv), append([]string{"settings"}, args...)...)
}

func TestSettingsSetAcceptsEffectiveInTheSaveEnvelope(t *testing.T) {
	oldOut, oldErr, oldCode := settingsSetAgainst(t, `{`+settingsSetSettingsPart+`}`,
		"set", "judge-model", "--harness", "claude", "m")
	if oldCode != 0 || !strings.Contains(oldOut, "judge") {
		t.Fatalf("baseline code=%d out=%q err=%q", oldCode, oldOut, oldErr)
	}
	for name, body := range map[string]string{
		"effective object":     `{` + settingsSetSettingsPart + `,"effective":{"version":1,"models":[],"efforts":[]}}`,
		"effective null+error": `{` + settingsSetSettingsPart + `,"effective":null,"effective_error":"settings_effective_readback_failed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, stderr, code := settingsSetAgainst(t, body, "set", "judge-model", "--harness", "claude", "m")
			if code != 0 {
				t.Fatalf("code=%d stderr=%q, want exit 0", code, stderr)
			}
			if out != oldOut || stderr != oldErr {
				t.Fatalf("human output changed with the new envelope keys:\n old out %q err %q\n new out %q err %q", oldOut, oldErr, out, stderr)
			}
		})
	}
}

func TestSettingsSetJSONPassesTheWholeEnvelopeThrough(t *testing.T) {
	for name, body := range map[string]string{
		"effective object":     `{` + settingsSetSettingsPart + `,"effective":{"version":1,"models":[]},"future_key":[1,2]}`,
		"effective null+error": `{` + settingsSetSettingsPart + `,"effective":null,"effective_error":"settings_effective_readback_failed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, stderr, code := settingsSetAgainst(t, body, "set", "judge-model", "--harness", "claude", "m", "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			// The CLI re-indents the JSON it prints, so compare structure, not bytes.
			var got, want any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout is not JSON: %q", out)
			}
			_ = json.Unmarshal([]byte(body), &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("--json = %s, want the server body %s", out, body)
			}
		})
	}
}

// A 400 with the errors array: exit usage, the first error's text on stderr, nothing on stdout.
func TestSettingsSetRejectionWithErrorsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"judge_model: model is a Codex model id","field":"judge_model","errors":[` +
			`{"field":"judge_model","message":"judge_model: model is a Codex model id"},` +
			`{"field":"default_harness","message":"invalid default_harness"}]}`))
	}))
	defer srv.Close()
	out, stderr, code := runCLI(t, httpEnv(srv), "settings", "set", "judge-model", "--harness", "claude", "m")
	if code != uzicli.ExitUsage || out != "" || !strings.Contains(stderr, "judge_model: model is a Codex model id") {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, stderr)
	}
}
