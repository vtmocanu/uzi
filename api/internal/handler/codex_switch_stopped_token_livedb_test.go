package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A stopped settlement never returns or logs credential material: neither the
// run's bound secret nor the withdrawn target's appears in any acknowledgement,
// run message, user input or server log line, on the first stop or its replay.
func TestWorkerCodexStoppedTokenAbsenceRouteLiveDB(t *testing.T) {
	e, w := setupStoppedGuardsRoute(t)
	ctx := context.Background()
	var secrets []string
	rows, err := e.pool.Query(ctx, `SELECT ciphertext FROM user_secrets WHERE user_id=$1 AND kind='openai_api_key'`, e.user)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sealed []byte
		if err := rows.Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		plain, err := e.handler.box.Open(sealed)
		if err != nil || len(plain) < 16 {
			t.Fatal("fixture secret is not readable", err)
		}
		secrets = append(secrets, string(plain))
	}
	rows.Close()
	if len(secrets) != 2 {
		t.Fatalf("want the bound and the withdrawn target secret, got %d", len(secrets))
	}
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
		for _, secret := range secrets {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("%s stop acknowledgement carries the bound secret", report)
			}
		}
	}
	for _, secret := range secrets {
		var leaked int
		if err := e.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM run_messages WHERE run_id=$1 AND strpos(payload::text, $2) > 0) +
 (SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND strpos(body, $2) > 0)`, e.run, secret).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Fatal("stopped settlement wrote the bound secret into run messages or inputs")
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatal("stopped settlement logged the bound secret")
		}
	}
}
