package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// A Codex apply logs its result through the production JSON handler without
// B's access token or the newly minted capability; only the response carries
// them.
func TestWorkerCodexApplyLogRedactionRouteLiveDB(t *testing.T) {
	e, w := setupStoppedGuardsRoute(t)
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	rec := e.do(e.worker, http.MethodPost, e.applyPath(),
		fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`, w.ClaimGeneration, w.RegisterNonce, e.op))
	var applied struct {
		Outcome string `json:"outcome"`
		Codex   struct {
			AccessToken string `json:"access_token"`
			Capability  string `json:"capability"`
		} `json:"codex"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &applied) != nil || applied.Outcome != "applied" ||
		len(applied.Codex.AccessToken) < 16 || len(applied.Codex.Capability) < 16 {
		t.Fatalf("Codex apply status=%d; want applied with B material", rec.Code)
	}
	got := buf.String()
	if !strings.Contains(got, "worker credential switch apply") {
		t.Fatal("apply log line was not captured")
	}
	for _, secret := range []string{applied.Codex.AccessToken, applied.Codex.Capability} {
		if strings.Contains(got, secret) {
			t.Fatal("Codex apply logged B material")
		}
	}
}
