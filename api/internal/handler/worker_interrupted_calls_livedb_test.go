package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestWorkerInterruptedCallsRouteLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	wid := fx.onlineWorker("interrupted calls", false)
	id := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE workers SET snapshot_register_nonce='nonce' WHERE id=$1", wid); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET worker_id=$2,status='running',claim_generation=2 WHERE id=$1", id, wid); err != nil {
		t.Fatal(err)
	}
	// Use the production authenticated worker router and a real bearer credential.
	e := setupJobRouteLiveDB(t)
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET worker_id=$2,user_id=$3 WHERE id=$1", id, e.worker, e.user); err != nil {
		t.Fatal(err)
	}
	e.exec(t, "UPDATE workers SET snapshot_register_nonce='nonce' WHERE id=$1", e.worker)
	e.exec(t, `INSERT INTO run_messages(run_id,seq,kind,agent,payload,claim_generation) VALUES($1,1,'tool_use','lead','{"id":"call","name":"Bash","input":{"command":"echo ok"}}',1)`, id)
	path := "/api/worker/runs/" + id.String() + "/interrupted-calls"
	send := func(query, nonce, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Worker-Register-Nonce", nonce)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("status=%d want=%d: %s", rec.Code, want, rec.Body.String())
		}
		return rec
	}
	token := e.tokens[e.worker]
	rec := send("?claim_generation=2", "nonce", token, 200)
	var out apitypes.InterruptedCallsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Format != 1 || out.RegisterNonce != "nonce" || len(out.Entries) != 1 || out.Entries[0].Command != "echo ok" || out.Entries[0].AgentInstance != nil || out.Entries[0].AgentLabel != nil {
		t.Fatalf("%s", rec.Body.String())
	}
	var entry map[string]json.RawMessage
	raw, _ := json.Marshal(out.Entries[0])
	_ = json.Unmarshal(raw, &entry)
	if len(entry) != 8 {
		t.Fatalf("wire entry changed: %s", raw)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rec.Header())
	}
	send("?claim_generation=2", "nonce", "bad", 401)
	for _, query := range []string{"", "?claim_generation=1", "?claim_generation=0", "?claim_generation=no", "?claim_generation=9223372036854775808"} {
		send(query, "nonce", token, 400)
	}
	send("?claim_generation=2", "", token, 400)
	send("?claim_generation=2", "stale", token, 404)
	send("?claim_generation=3", "nonce", token, 404)
	e.exec(t, "UPDATE workers SET snapshot_register_nonce='rotated' WHERE id=$1", e.worker)
	rec = send("?claim_generation=2", "nonce", token, 404)
	if strings.Contains(rec.Body.String(), "echo") || strings.Contains(rec.Body.String(), "nonce") {
		t.Fatalf("content leaked: %s", rec.Body.String())
	}
}
