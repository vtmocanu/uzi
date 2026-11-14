package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// workersvc cannot import hostedsvc (cycle); the handler converts, so pin the mirror here without a database.
func TestHibernationCleanupTransitionConstantsMatchWire(t *testing.T) {
	for local, wire := range map[string]string{
		workersvc.TransitionRefused:        hostedsvc.TransitionRefused,
		workersvc.TransitionScalingDown:    hostedsvc.TransitionScalingDown,
		workersvc.TransitionScaledDown:     hostedsvc.TransitionScaledDown,
		workersvc.TransitionReady:          hostedsvc.TransitionReady,
		workersvc.TransitionCleanupStopped: hostedsvc.TransitionCleanupStopped,
		workersvc.CleanupPhaseRequested:    "requested",
		workersvc.CleanupPhaseStarted:      "started",
		workersvc.CleanupPhaseUnknown:      "unknown",
		workersvc.CleanupPhaseSettled:      "settled",
	} {
		if local != wire {
			t.Fatalf("workersvc %q != wire %q", local, wire)
		}
	}
}

// The agent-facing wire names are a contract with agent/src/hibernation-cache-prune.ts.
func TestHibernationCachePruneWireShapes(t *testing.T) {
	proj, err := json.Marshal(workersvc.HibernationCachePrune{OpID: "o", RegisterNonce: "n", Phase: "started", CancelRequested: true, ReconcileSeq: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(proj), `{"op_id":"o","register_nonce":"n","phase":"started","cancel_requested":true,"reconcile_seq":3}`; got != want {
		t.Fatalf("projection = %s, want %s", got, want)
	}
	var rep workersvc.HibernationCachePruneReport
	dec := json.NewDecoder(strings.NewReader(`{"op_id":"o","register_nonce":"n","kind":"result","steps":[{"command":"builder_prune","state":"exited_ok"}],"gate_exclusive_throughout":true}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil || rep.Kind != "result" || len(rep.Steps) != 1 || !rep.GateExclusiveThroughout {
		t.Fatalf("result report decode: %v %+v", err, rep)
	}
	dec = json.NewDecoder(strings.NewReader(`{"op_id":"o","register_nonce":"n","kind":"ready","local_claims":0,"local_executions":0,"custody_clear":true,"custody_checked_at":"2026-01-02T03:04:05Z","gate_exclusive":true}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil || rep.LocalClaims == nil || rep.CustodyCheckedAt == nil || !rep.GateExclusive {
		t.Fatalf("ready report decode: %v %+v", err, rep)
	}
}

func TestWorkerHeartbeatHibernationCachePruneReportWireAndFailClosed(t *testing.T) {
	const token = "test-hibernation-worker-credential"
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		// The fake row carries no protocol capability, so any report is refused: a worker that
		// never negotiated hibernation_cache_prune_v1 cannot reach the attempt.
		{"unsupported worker", `{"hibernation_cache_prune_report":{"op_id":"` + uuid.NewString() + `","register_nonce":"n","kind":"ready"}}`, http.StatusConflict},
		{"report omitted", `{"version":"1"}`, http.StatusOK},
		{"unknown report field", `{"hibernation_cache_prune_report":{"surprise":1}}`, http.StatusBadRequest},
		{"wrong count type", `{"hibernation_cache_prune_report":{"local_claims":"0"}}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &dindHandlerStore{worker: store.Worker{
				ID: uuid.New(), UserID: uuid.New(), TokenHash: jointoken.Hash(token), Kind: "hosted",
				DockerEnabled: pgtype.Bool{Bool: true, Valid: true},
			}}
			h := newProtocolHandler(t, st)
			endpoint := mw.RequireWorker(st)(http.HandlerFunc(h.WorkerHeartbeat))
			req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			endpoint.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK && strings.Contains(rec.Body.String(), "hibernation_cache_prune") {
				t.Fatalf("a worker without the capability was sent the field: %s", rec.Body.String())
			}
		})
	}
}
