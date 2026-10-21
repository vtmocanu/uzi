package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
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

// chatClaimStore records that the chat lane reached ClaimChatRun and that ClaimRun did not.
type chatClaimStore struct {
	*memClaimStore
	chatCalled bool
}

func (c *chatClaimStore) ClaimChatRun(context.Context, store.ClaimChatRunParams) (store.Run, error) {
	c.chatCalled = true
	return store.Run{}, pgx.ErrNoRows
}

// The chat lane is a different queue: a worker that has lost memory authority (explicitly, or
// capable with the member missing) still polls it, and ClaimRun's gate is never involved.
func TestWorkerClaimChatLaneIgnoresMemoryAuthority(t *testing.T) {
	capable := []string{capability.WorkerMemoryPressureV1, capability.WorkerMemoryAuthorityClaimV1}
	for _, body := range []string{``, `{"memory_authority":"lost"}`} {
		st := &chatClaimStore{memClaimStore: &memClaimStore{protocolStore: &protocolStore{}}}
		h := newProtocolHandler(t, st)
		rec := httptest.NewRecorder()
		h.WorkerClaim(rec, claimReqAs("/api/worker/claim?lane=chat", body, capable))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("body %q: status = %d, want 204: %s", body, rec.Code, rec.Body.String())
		}
		if !st.chatCalled {
			t.Errorf("body %q: chat lane did not reach ClaimChatRun", body)
		}
		if st.got != nil {
			t.Errorf("body %q: chat lane reached ClaimRun: %+v", body, st.got)
		}
	}
}

// memAuthListStore is custodyListStore plus a ClaimRun that finds nothing, so a gated
// claim can be recorded in the workersvc tracker through its public Claim path.
type memAuthListStore struct{ *custodyListStore }

func (memAuthListStore) ClaimRun(context.Context, store.ClaimRunParams) (store.Run, error) {
	return store.Run{}, pgx.ErrNoRows
}

// Issue #3142: memory_authority_lost_at rides the user list and the admin list, present for
// a gated worker and null for the other.
func TestWorkerSurfacesOverlayMemoryAuthority(t *testing.T) {
	id, other := uuid.New(), uuid.New()
	gate := func(t *testing.T, h *Handler) {
		t.Helper()
		lost := "lost"
		_, err := h.wsvc.ClaimReported(context.Background(), store.Worker{ID: id}, nil, workersvc.MemoryAuthorityReport{Value: &lost})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := h.wsvc.MemoryAuthorityLostFor(id); !ok {
			t.Fatal("setup: worker not recorded as gated")
		}
	}
	check := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		var body struct {
			Workers []map[string]json.RawMessage `json:"workers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Workers) != 2 {
			t.Fatalf("rows = %s", rec.Body.String())
		}
		gated, ok := body.Workers[0]["memory_authority_lost_at"]
		if !ok || string(gated) == "null" || len(gated) < 3 {
			t.Fatalf("gated row memory_authority_lost_at = %q (present=%t), want a timestamp", gated, ok)
		}
		if got, ok := body.Workers[1]["memory_authority_lost_at"]; !ok || string(got) != "null" {
			t.Fatalf("clean row memory_authority_lost_at = %q (present=%t), want null", got, ok)
		}
	}
	withUser := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/api/workers", nil).WithContext(mw.ContextWithUser(context.Background(), store.User{ID: uuid.New()}))
	}
	t.Run("user list", func(t *testing.T) {
		st := memAuthListStore{&custodyListStore{ownerRows: []store.ListWorkersByUserRow{{ID: id}, {ID: other}}}}
		h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}
		gate(t, h)
		rec := httptest.NewRecorder()
		h.ListWorkers(rec, withUser())
		check(t, rec)
	})
	t.Run("admin list", func(t *testing.T) {
		st := memAuthListStore{&custodyListStore{adminRows: []store.ListAllWorkersRow{{Worker: store.Worker{ID: id}}, {Worker: store.Worker{ID: other}}}}}
		h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}
		gate(t, h)
		rec := httptest.NewRecorder()
		h.AdminListWorkers(rec, withUser())
		check(t, rec)
	})
}
