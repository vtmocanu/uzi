package handler

// PRD #108 M4 — the 413 arm must feed the persistence-failure tracker.
//
// The 413 is answered by DecodeJSONLimited BEFORE AppendMessages runs, so the
// recorder inside AppendMessages never sees an oversize batch. That blind spot is
// the incident's own long tail: a pre-0.10.1 worker's retry batch GROWS past the
// 1 MiB cap, so the failure rotates 500 → 413 and then stays 413 forever — the
// exact steady state in which both M4's flag and M5's kill would otherwise go dark.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// oversizeTrackStore records the ownership lookup NoteOversizeBatch performs.
//
// That lookup is the observable proof the hook ran: on the 413 path the decode
// fails, so AppendMessages is never called and NOTHING ELSE in the request touches
// the store. A GetRunOwnedByWorker call here can only have come from the hook.
type oversizeTrackStore struct {
	workersvc.Store
	run     store.Run
	lookups []store.GetRunOwnedByWorkerParams
}

func (o *oversizeTrackStore) GetRunOwnedByWorker(_ context.Context, arg store.GetRunOwnedByWorkerParams) (store.Run, error) {
	o.lookups = append(o.lookups, arg)
	return o.run, nil
}

// oversizeBody builds a well-formed batch comfortably over the 1 MiB cap. Well
// formed on purpose: the route must reject it for its SIZE, not its shape, or this
// test would be exercising the 400 arm and proving nothing about 413.
func oversizeBody(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"messages":[`)
	for i := 1; i <= 300; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"seq":%d,"kind":"text","agent":"lead","payload":{"t":%q}}`, i, strings.Repeat("x", 4096))
	}
	b.WriteString(`]}`)
	if b.Len() <= 1<<20 {
		t.Fatalf("the oversized fixture is %d bytes, which is under the 1 MiB cap it exists to cross", b.Len())
	}
	return b.String()
}

func TestWorkerMessagesOversizeBatchIsCountedAgainstTheRun(t *testing.T) {
	runID := uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	fake := &oversizeTrackStore{run: store.Run{ID: runID, Status: "running", LastSeq: 42}}
	h := &Handler{wsvc: workersvc.New(fake, newHandlerTestBox(t), workersvc.Params{})}

	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/messages", strings.NewReader(oversizeBody(t)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	ctx := context.WithValue(mw.ContextWithWorker(req.Context(), wkr), chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	h.WorkerRunMessages(rec, req.WithContext(ctx))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 — this test measures the 413 arm, so a different code means it measured nothing", rec.Code)
	}
	if len(fake.lookups) != 1 {
		t.Fatalf("GetRunOwnedByWorker called %d times, want exactly 1: the 413 arm must call NoteOversizeBatch, "+
			"or a worker whose batch grew past the cap loops forever with neither a flag nor a stop", len(fake.lookups))
	}
	got := fake.lookups[0]
	if got.ID != runID {
		t.Errorf("ownership lookup ran against run %s, want the run id from the URL %s", got.ID, runID)
	}
	if uuid.UUID(got.WorkerID.Bytes) != wkr.ID || !got.WorkerID.Valid {
		t.Errorf("ownership lookup ran against worker %v, want the AUTHENTICATED worker %s — an unowned record is a cross-tenant kill primitive",
			got.WorkerID, wkr.ID)
	}
}

func TestWorkerMessagesMalformedBodyIsNotCountedAsOversize(t *testing.T) {
	// The other side of the split. A malformed (not oversized) body takes the 400
	// arm, which is NOT a persistence failure of the run's own making in the sense
	// this counter measures — and counting it would make any client bug drive a run
	// toward being killed.
	runID := uuid.New()
	fake := &oversizeTrackStore{run: store.Run{ID: runID, Status: "running"}}
	h := &Handler{wsvc: workersvc.New(fake, newHandlerTestBox(t), workersvc.Params{})}

	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/messages",
		strings.NewReader(`{"messages":[{"seq":1,"kind":"text","payload":"unclosed`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	ctx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()}), chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	h.WorkerRunMessages(rec, req.WithContext(ctx))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(fake.lookups) != 0 {
		t.Fatalf("GetRunOwnedByWorker called %d times on the malformed-body path, want 0: only the oversize arm records", len(fake.lookups))
	}
}
