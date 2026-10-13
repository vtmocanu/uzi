package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestControllerStatusPacingPersistenceLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared) VALUES($1,$2,'pacing-handler',$3,'offline','hosted','1.0.0','m','base')`, id, user, []byte(id.String()))
	receipt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h.now = func() time.Time { return receipt }
	post := func(kind string, pacing any) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"kind": kind, "reported_at": receipt.Add(365 * 24 * time.Hour), "workers": []any{map[string]any{"id": id.String(), "phase": "settled", "pacing": pacing}}})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
		if rec.Code != 204 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	hold := map[string]any{"state": "held", "target_image": "repo:v2", "target_version": "2.0.0"}
	post("fleet", hold)
	original := receipt
	receipt = receipt.Add(time.Hour)
	post("readiness", nil)
	post("", nil)
	post("fleet", map[string]any{"state": "completed"})
	var state string
	var pacingAt, ordinary time.Time
	if err := pool.QueryRow(context.Background(), `SELECT pacing->>'state',pacing_observed_at,observed_at FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&state, &pacingAt, &ordinary); err != nil {
		t.Fatal(err)
	}
	if state != "held" || !pacingAt.Equal(original) || !ordinary.Equal(receipt) {
		t.Fatalf("state=%s pacingAt=%v ordinary=%v", state, pacingAt, ordinary)
	}
	post("fleet", nil)
	var cleared bool
	if err := pool.QueryRow(context.Background(), `SELECT pacing IS NULL AND pacing_observed_at IS NULL FROM worker_upgrade_reports WHERE worker_id=$1`, id).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("clear=%v err=%v", cleared, err)
	}
}
