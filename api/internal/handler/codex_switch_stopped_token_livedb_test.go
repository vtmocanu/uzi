package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A stopped settlement never reads or returns credential material: the run's
// bound secret appears in no acknowledgement, run message, user input or server
// log line, on the first stop or its replay.
func TestWorkerCodexStoppedTokenAbsenceRouteLiveDB(t *testing.T) {
	e, w := setupStoppedGuardsRoute(t)
	ctx := context.Background()
	var sealed []byte
	if err := e.pool.QueryRow(ctx, `SELECT s.ciphertext FROM user_secrets s JOIN runs r ON r.codex_secret_id=s.id WHERE r.id=$1`, e.run).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	plain, err := e.handler.box.Open(sealed)
	if err != nil || len(plain) < 16 {
		t.Fatal("bound fixture secret is not readable", err)
	}
	secret := string(plain)
	logs := captureSlog(t)
	withdrawStoppedRoute(t, e, w)
	body, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range []string{"first", "replay"} {
		rec := e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state", string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s stop status=%d", report, rec.Code)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("%s stop acknowledgement carries the bound secret", report)
		}
	}
	var leaked int
	if err := e.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM run_messages WHERE run_id=$1 AND payload::text LIKE '%' || $2 || '%') +
 (SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND body LIKE '%' || $2 || '%')`, e.run, secret).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("stopped settlement wrote the bound secret into run messages or inputs")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("stopped settlement logged the bound secret")
	}
}
