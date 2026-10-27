package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestAdminRateLimitsVaultMode(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, tc := range []struct {
			name, mode, want string
			locked           bool
		}{
			{"off stale lock", "unprotected", "off", true},
			{"off", "unprotected", "off", false},
			{"protected locked", "protected", "locked", true},
			{"protected unlocked", "protected", "unlocked", false},
			{"legacy locked", "", "locked", true},
			{"legacy unlocked", "", "unlocked", false},
			{"unknown locked", "future", "locked", true},
		} {
			for _, populated := range []bool{false, true} {
				name := provider + "/" + tc.name
				if populated {
					name += "/populated"
				} else {
					name += "/empty"
				}
				t.Run(name, func(t *testing.T) {
					claude := apitypes.AdminRateLimitRowDTO{ID: "u1", Email: "me@example.com", VaultMode: tc.mode, VaultLocked: tc.locked}
					codex := apitypes.CodexAdminRateLimitRowDTO{ID: "u1", Email: "me@example.com", VaultMode: tc.mode, VaultLocked: tc.locked}
					if populated {
						claude.Tokens = []apitypes.TokenRateLimitDTO{{Label: "personal", Limits: apitypes.RateLimitDTO{Status: "ok"}}}
						codex.Accounts = []apitypes.CodexAccountRateLimitDTO{{AccountID: "cx1", Status: "fresh", Buckets: []apitypes.CodexRateLimitBucketDTO{{ID: "5h"}, {ID: "7d"}}}}
					}
					fc := &uzicli.FakeClient{RateLimits: []apitypes.AdminRateLimitRowDTO{claude}, CodexRateLimits: []apitypes.CodexAdminRateLimitRowDTO{codex}}
					args := []string{"admin", "rate-limits", "--provider", provider}
					out, errOut, code := runCLI(t, fakeEnv(fc), args...)
					if code != uzicli.ExitOK {
						t.Fatalf("exit=%d: %s", code, errOut)
					}
					lines := strings.Split(strings.TrimSpace(out), "\n")
					wantRows := 1
					if provider == "codex" && populated {
						wantRows = 2
					}
					if len(lines) != wantRows+1 || strings.Fields(lines[0])[1] != "VAULT" {
						t.Fatalf("unexpected table: %s", out)
					}
					for _, line := range lines[1:] {
						cells := strings.Fields(line)
						if len(cells) < 2 || cells[0] != "me@example.com" || cells[1] != tc.want {
							t.Errorf("want EMAIL/VAULT me@example.com %s, got %q", tc.want, line)
						}
					}

					// Presentation must not rewrite the DTO's mode or stale locked bit.
					out, errOut, code = runCLI(t, fakeEnv(fc), append(args, "--json")...)
					if code != uzicli.ExitOK {
						t.Fatalf("JSON exit=%d: %s", code, errOut)
					}
					var want any = fc.RateLimits
					if provider == "codex" {
						want = fc.CodexRateLimits
					}
					wantJSON, err := json.Marshal(want)
					if err != nil {
						t.Fatal(err)
					}
					var gotValue, wantValue any
					if err := json.Unmarshal([]byte(out), &gotValue); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(gotValue, wantValue) {
						t.Errorf("JSON DTO changed: got %s, want %s", out, wantJSON)
					}
				})
			}
		}
	}
}

func TestVaultIndicatorOwnSessionMode(t *testing.T) {
	for _, tc := range []struct {
		name, vault string
		wantLock    bool
	}{
		{"off", `{"mode":"unprotected","unlocked":false}`, false},
		{"protected locked", `{"mode":"protected","unlocked":false}`, true},
		{"protected unlocked", `{"mode":"protected","unlocked":true}`, false},
		{"legacy locked", `{"unlocked":false}`, true},
		{"absent unlocked", `{"mode":"protected"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/auth/me" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"me@example.com"},"vault":` + tc.vault + `}`))
			}))
			defer srv.Close()
			// Start locked to prove an off session clears an existing indicator.
			m := vaultModel(t, true)
			m.client = uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL})
			msg := m.fetchVaultCmd()().(vaultStatusMsg)
			if msg.err != nil {
				t.Fatal(msg.err)
			}
			if msg.user.ID != "u1" {
				t.Fatalf("identity lost: %+v", msg.user)
			}
			next, _ := m.Update(msg)
			m = next.(tuiModel)
			out := stripANSI(m.View().Content)
			if strings.Contains(out, "vault locked") != tc.wantLock {
				t.Errorf("want lock=%v, board:\n%s", tc.wantLock, out)
			}
			if (m.vaultIndicatorLine() != "") != tc.wantLock {
				t.Errorf("want indicator=%v, got %q", tc.wantLock, m.vaultIndicatorLine())
			}
		})
	}
}
