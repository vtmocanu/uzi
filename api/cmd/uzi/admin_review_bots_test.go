package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func reviewBotsFake(v string) *uzicli.FakeClient {
	return &uzicli.FakeClient{AdminSettingsV: uzicli.AdminSettingsView{
		Settings: map[string]string{"mr_review_trusted_bots": v},
		Sources:  map[string]string{"mr_review_trusted_bots": "db"},
	}}
}

func TestAdminReviewBotsEmpty(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(reviewBotsFake("")), "admin", "review-bots")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "no trusted review bots: third-party review bots' comments are withheld") {
		t.Errorf("missing empty note:\n%s", out)
	}
}

func TestAdminReviewBotsTwoEntries(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(reviewBotsFake("https://github.com#136622811, https://gitlab.example.com#42")), "admin", "review-bots")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"BASE URL", "https://github.com", "136622811", "https://gitlab.example.com", "42"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "malformed") || strings.Contains(out, "no trusted") {
		t.Errorf("unexpected flag/note:\n%s", out)
	}
}

func TestAdminReviewBotsMalformedShownVerbatim(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(reviewBotsFake("garbage-entry,https://github.com#1")), "admin", "review-bots")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "garbage-entry") || !strings.Contains(out, "malformed") {
		t.Errorf("malformed entry not shown/flagged:\n%s", out)
	}
	if !strings.Contains(out, "https://github.com") {
		t.Errorf("valid entry lost:\n%s", out)
	}
}

func TestAdminReviewBotsJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(reviewBotsFake("https://github.com#7,bad")), "admin", "review-bots", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got struct {
		Source  string           `json:"source"`
		Entries []reviewBotEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.Source != "db" || len(got.Entries) != 2 || got.Entries[0].ForgeUserID != "7" || !got.Entries[1].Malformed || got.Entries[1].Raw != "bad" {
		t.Errorf("json = %+v", got)
	}
}

func TestAdminReviewBotsNonAdminErrorPassthrough(t *testing.T) {
	fc := &uzicli.FakeClient{Err: &uzicli.ExitError{Code: uzicli.ExitAuth, Err: errors.New("admin access required")}}
	_, errOut, code := runCLI(t, fakeEnv(fc), "admin", "review-bots")
	if code != uzicli.ExitAuth {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitAuth)
	}
	if !strings.Contains(errOut, "admin access required") {
		t.Errorf("stderr = %q", errOut)
	}
}
