package handler

import (
	"net/http"
	"strings"
	"testing"
)

func TestWorkerCodexSwitchPreflightRouteLiveDB(t *testing.T) {
	e := setupApplyRouteLiveDB(t)
	path := "/api/worker/runs/" + e.run.String() + "/credential-switch/codex/preflight"
	for _, body := range []string{
		"{}",
		"{",
		"{\"unknown\":true}",
		"{\"padding\":\"" + strings.Repeat("x", 40<<10) + "\"}",
	} {
		rec := e.do(e.worker, "POST", path, body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("strict bounded preflight status=%d", rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("preflight refusal is cacheable")
		}
	}
	body := strings.TrimSuffix(e.applyBody(e.op), "}") + ",\"target_secret_id\":\"" + e.secret.String() + "\"}"
	rec := e.do(e.worker, "POST", path, body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\"outcome\":\"fenced\"") {
		t.Fatalf("unnegotiated preflight status=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), e.plain) || strings.Contains(rec.Body.String(), "ciphertext") {
		t.Fatal("preflight disclosed material")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("preflight result is cacheable")
	}
}
