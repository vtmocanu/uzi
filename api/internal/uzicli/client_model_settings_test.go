package uzicli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func putModelSettingsAgainst(t *testing.T, status int, contentType, body string) (ModelSettingsResult, string, error) {
	t.Helper()
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/me/model-settings" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	v := "m"
	res, err := newTestClient(srv).PutModelSettings(context.Background(), map[string]*string{"judge_model": &v, "summary_model": nil})
	return res, gotBody, err
}

func exitOf(t *testing.T, err error) *ExitError {
	t.Helper()
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	return ee
}

func TestPutModelSettingsSendsPatchWithNullAndKeepsEnvelope(t *testing.T) {
	envelope := `{"settings":{"judge_model":"m","summary_model":null},"extra_future_key":1}`
	res, sent, err := putModelSettingsAgainst(t, http.StatusOK, "application/json", envelope)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]*string
	if e := json.Unmarshal([]byte(sent), &got); e != nil || len(got) != 2 || got["judge_model"] == nil || *got["judge_model"] != "m" {
		t.Fatalf("sent body = %s", sent)
	}
	if v, ok := got["summary_model"]; !ok || v != nil {
		t.Fatalf("summary_model must be sent as null, body = %s", sent)
	}
	if string(res.Raw) != envelope {
		t.Fatalf("Raw = %s, want the server envelope unchanged", res.Raw)
	}
	if res.Settings.JudgeModel == nil || *res.Settings.JudgeModel != "m" {
		t.Fatalf("Settings.JudgeModel = %v", res.Settings.JudgeModel)
	}
}

func TestPutModelSettingsMalformed(t *testing.T) {
	for _, body := range []string{`{"settings":`, `{}`, `{"settings":null}`} {
		_, _, err := putModelSettingsAgainst(t, http.StatusOK, "application/json", body)
		if ee := exitOf(t, err); ee.Code != ExitGeneric {
			t.Fatalf("%s: code %d", body, ee.Code)
		}
	}
}

func TestPutModelSettingsTypedFiveHundreds(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"settings_commit_unknown", "the save may or may not have been applied; check with `uzi settings get`"},
		{"settings_saved_readback_failed", "settings saved; reading them back failed"},
	} {
		// The server text is hostile; the client words the message itself.
		body := `{"error":"\u001b[2Jrolled back","reason":"` + tc.reason + `"}`
		_, _, err := putModelSettingsAgainst(t, http.StatusInternalServerError, "application/json", body)
		ee := exitOf(t, err)
		if ee.Code != ExitUnreachable || ee.Error() != tc.want || ee.Reason != tc.reason {
			t.Fatalf("%s: code=%d msg=%q reason=%q", tc.reason, ee.Code, ee.Error(), ee.Reason)
		}
	}
	// A plain 500 keeps the server message and exit 6.
	_, _, err := putModelSettingsAgainst(t, http.StatusInternalServerError, "application/json", `{"error":"internal error"}`)
	if ee := exitOf(t, err); ee.Code != ExitUnreachable || ee.Error() != "internal error" {
		t.Fatalf("plain 500: %d %q", ee.Code, ee.Error())
	}
}

func TestPutModelSettingsFieldError(t *testing.T) {
	_, _, err := putModelSettingsAgainst(t, http.StatusBadRequest, "application/json", `{"error":"unknown field x","field":"x"}`)
	if ee := exitOf(t, err); ee.Code != ExitUsage || ee.Error() != "unknown field x" {
		t.Fatalf("%d %q", ee.Code, ee.Error())
	}
}

func TestPutModelSettingsOlderServer(t *testing.T) {
	const want = "this server does not support CLI model-setting writes; upgrade the server"
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{"404 text/plain", http.StatusNotFound, "text/plain; charset=utf-8", "404 page not found\n"},
		{"405 empty", http.StatusMethodNotAllowed, "", ""},
		{"404 html", http.StatusNotFound, "text/html", "<html>nope</html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := putModelSettingsAgainst(t, tc.status, tc.contentType, tc.body)
			if ee := exitOf(t, err); ee.Code != ExitGeneric || ee.Error() != want {
				t.Fatalf("code=%d msg=%q", ee.Code, ee.Error())
			}
		})
	}
	// A JSON error body means the route exists: not an older server.
	_, _, err := putModelSettingsAgainst(t, http.StatusNotFound, "application/json", `{"error":"user not found"}`)
	if ee := exitOf(t, err); ee.Code != ExitNotFound || ee.Error() != "user not found" {
		t.Fatalf("JSON 404: %d %q", ee.Code, ee.Error())
	}
	_, _, err = putModelSettingsAgainst(t, http.StatusMethodNotAllowed, "application/json", `{"error":"method not allowed"}`)
	if ee := exitOf(t, err); ee.Error() == want {
		t.Fatalf("JSON 405 was mapped to the upgrade message")
	}
}

func TestUnsupportedRouteErrorIgnoresOtherStatuses(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError} {
		if unsupportedRouteError(status, nil, "x") != nil {
			t.Fatalf("status %d mapped", status)
		}
	}
}
