package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// memClaimStore records the ClaimRun params the handler's claim reached.
type memClaimStore struct {
	*protocolStore
	got *store.ClaimRunParams
}

func (m *memClaimStore) ClaimRun(_ context.Context, arg store.ClaimRunParams) (store.Run, error) {
	m.got = &arg
	return store.Run{}, pgx.ErrNoRows
}

func claimReqAs(path, body string, caps []string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx := mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: caps})
	return req.WithContext(ctx)
}

// Issue #3142: the run-lane claim body carries memory_authority. It is accepted whether or
// not the active-snapshot kill switch is on, and the handler hands the report to the service.
func TestWorkerClaimMemoryAuthorityBody(t *testing.T) {
	capable := []string{capability.WorkerMemoryPressureV1, capability.WorkerMemoryAuthorityClaimV1}
	for _, disabled := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			body string
			caps []string
			want bool
		}{
			{"held", `{"memory_authority":"held"}`, capable, false},
			{"lost", `{"memory_authority":"lost"}`, capable, true},
			{"lost without capability", `{"memory_authority":"lost"}`, nil, true},
			{"capable, member missing", ``, capable, true},
			{"old worker, nothing sent", ``, nil, false},
		} {
			st := &memClaimStore{protocolStore: &protocolStore{}}
			h := newProtocolHandler(t, st)
			h.cfg.ActiveSnapshotDisabled = disabled
			rec := httptest.NewRecorder()
			h.WorkerClaim(rec, claimReqAs("/api/worker/claim", tc.body, tc.caps))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("disabled=%t %s: status = %d, want 204: %s", disabled, tc.name, rec.Code, rec.Body.String())
			}
			if st.got == nil || st.got.MemoryAuthorityLost != tc.want {
				t.Errorf("disabled=%t %s: ClaimRun saw %+v, want MemoryAuthorityLost=%t", disabled, tc.name, st.got, tc.want)
			}
		}
	}
}

// A disabled api still rejects an active_snapshot, memory_authority alongside it or not.
func TestWorkerClaimMemoryAuthorityDoesNotRelaxSnapshotKillSwitch(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{claimErr: pgx.ErrNoRows})
	h.cfg.ActiveSnapshotDisabled = true
	rec := httptest.NewRecorder()
	h.WorkerClaim(rec, claimReqAs("/api/worker/claim", `{"memory_authority":"held","active_snapshot":{"snapshot_epoch":1,"active":[]}}`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
