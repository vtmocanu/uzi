package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// summariesFakeStore implements only the queries the summary service reaches; the
// embedded Store makes any other call panic (same pattern as findingsFakeStore).
type summariesFakeStore struct {
	Store
	run store.Run

	intentParams *store.SetRunIntentSummaryParams
	intentRows   int64

	planParams *store.SetRunPlanSummaryParams
	planRows   int64
}

func (f *summariesFakeStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == f.run.ID && arg.UserID == f.run.UserID {
		return f.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}

func (f *summariesFakeStore) SetRunIntentSummary(_ context.Context, arg store.SetRunIntentSummaryParams) (int64, error) {
	f.intentParams = &arg
	return f.intentRows, nil
}

func (f *summariesFakeStore) SetRunPlanSummary(_ context.Context, arg store.SetRunPlanSummaryParams) (int64, error) {
	f.planParams = &arg
	return f.planRows, nil
}

func baseSummaryRun() store.Run {
	return store.Run{
		ID:     uuid.New(),
		UserID: uuid.New(),
		RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Status: "running",
		Kind:   "issue",
		PlanMd: pgtype.Text{String: "the plan", Valid: true},
	}
}

// ── SetIntentSummary ────────────────────────────────────────────────────────────────

func TestSetIntentSummaryWritesAndBroadcasts(t *testing.T) {
	f := &summariesFakeStore{run: baseSummaryRun(), intentRows: 1}
	b := &fakeBroadcaster{}
	svc := New(f, nil, Params{})
	svc.SetBroadcaster(b)
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}

	_, written, err := svc.SetIntentSummary(context.Background(), wkr, f.run.ID, "does the thing")
	if err != nil {
		t.Fatalf("SetIntentSummary: %v", err)
	}
	if !written {
		t.Fatalf("written = false, want true for a fresh intent summary")
	}
	if f.intentParams == nil || f.intentParams.SummaryIntent.String != "does the thing" {
		t.Fatalf("SetRunIntentSummary params = %+v, want the summary text", f.intentParams)
	}
	if len(b.statuses) != 1 || b.statuses[0] != "running" {
		t.Fatalf("broadcast states = %v, want one 'running'", b.statuses)
	}
}

func TestSetIntentSummaryIdempotentOnSet(t *testing.T) {
	run := baseSummaryRun()
	run.SummaryIntent = pgtype.Text{String: "already here", Valid: true}
	f := &summariesFakeStore{run: run, intentRows: 1}
	b := &fakeBroadcaster{}
	svc := New(f, nil, Params{})
	svc.SetBroadcaster(b)
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}

	_, written, err := svc.SetIntentSummary(context.Background(), wkr, run.ID, "a newer one")
	if err != nil {
		t.Fatalf("SetIntentSummary: %v", err)
	}
	if written {
		t.Fatalf("written = true, want false — an already-set intent must be a no-op")
	}
	if f.intentParams != nil {
		t.Fatalf("SetRunIntentSummary was called (%+v), want skipped", f.intentParams)
	}
	if len(b.statuses) != 0 {
		t.Fatalf("broadcast states = %v, want none on a skipped write", b.statuses)
	}
}

func TestSetIntentSummaryForeignRunIs404(t *testing.T) {
	f := &summariesFakeStore{run: baseSummaryRun(), intentRows: 1}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}

	if _, _, err := svc.SetIntentSummary(context.Background(), wkr, uuid.New(), "x"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign run err = %v, want ErrRunNotFound", err)
	}
}

func TestSetIntentSummaryRepoRequired(t *testing.T) {
	run := baseSummaryRun()
	run.RepoID = pgtype.UUID{Valid: false}
	f := &summariesFakeStore{run: run, intentRows: 1}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}

	if _, _, err := svc.SetIntentSummary(context.Background(), wkr, run.ID, "x"); !errors.Is(err, ErrSummaryRepoRequired) {
		t.Fatalf("repo-less run err = %v, want ErrSummaryRepoRequired", err)
	}
}

func TestSetIntentSummaryTerminalRejected(t *testing.T) {
	run := baseSummaryRun()
	run.Status = "completed"
	f := &summariesFakeStore{run: run, intentRows: 1}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}

	if _, _, err := svc.SetIntentSummary(context.Background(), wkr, run.ID, "x"); !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("terminal run err = %v, want ErrRunTerminal", err)
	}
}

// ── SetPlanSummary ──────────────────────────────────────────────────────────────────

func TestSetPlanSummaryWritesAndBroadcasts(t *testing.T) {
	f := &summariesFakeStore{run: baseSummaryRun(), planRows: 1}
	b := &fakeBroadcaster{}
	svc := New(f, nil, Params{})
	svc.SetBroadcaster(b)
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}

	deltas := []apitypes.RunSummaryDelta{{Kind: "added", Text: "a test"}, {Kind: "dropped", Text: "the cache"}}
	_, err := svc.SetPlanSummary(context.Background(), wkr, f.run.ID, "plan summary", deltas, "the plan")
	if err != nil {
		t.Fatalf("SetPlanSummary: %v", err)
	}
	if f.planParams == nil {
		t.Fatalf("SetRunPlanSummary was not called")
	}
	if f.planParams.ExpectedPlanMd.String != "the plan" {
		t.Fatalf("expected_plan_md = %q, want the posted plan_md (the stale-write guard)", f.planParams.ExpectedPlanMd.String)
	}
	var stored []apitypes.RunSummaryDelta
	if err := json.Unmarshal(f.planParams.SummaryDeltas, &stored); err != nil {
		t.Fatalf("stored deltas not valid jsonb: %v", err)
	}
	if len(stored) != 2 || stored[0].Kind != "added" || stored[1].Kind != "dropped" {
		t.Fatalf("stored deltas = %+v, want the two validated deltas", stored)
	}
	if len(b.statuses) != 1 || b.statuses[0] != "running" {
		t.Fatalf("broadcast states = %v, want one 'running'", b.statuses)
	}
}

func TestSetPlanSummaryStaleWriteRejected(t *testing.T) {
	// 0 rows updated ⇒ the run's plan_md no longer matches the posted plan_md: a re-plan
	// landed first, so the write is rejected as a conflict rather than failing the run.
	f := &summariesFakeStore{run: baseSummaryRun(), planRows: 0}
	b := &fakeBroadcaster{}
	svc := New(f, nil, Params{})
	svc.SetBroadcaster(b)
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}

	_, err := svc.SetPlanSummary(context.Background(), wkr, f.run.ID, "plan summary", nil, "a stale plan")
	if !errors.Is(err, ErrSummaryPlanStale) {
		t.Fatalf("stale write err = %v, want ErrSummaryPlanStale", err)
	}
	if len(b.statuses) != 0 {
		t.Fatalf("broadcast states = %v, want none on a rejected stale write", b.statuses)
	}
}

func TestSetPlanSummaryInvalidDeltasRejected(t *testing.T) {
	cases := map[string][]apitypes.RunSummaryDelta{
		"unknown kind":  {{Kind: "removed", Text: "x"}},
		"empty text":    {{Kind: "added", Text: ""}},
		"oversize text": {{Kind: "changed", Text: strings.Repeat("x", MaxSummaryDeltaTextBytes+1)}},
	}
	for name, deltas := range cases {
		t.Run(name, func(t *testing.T) {
			f := &summariesFakeStore{run: baseSummaryRun(), planRows: 1}
			svc := New(f, nil, Params{})
			wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}
			_, err := svc.SetPlanSummary(context.Background(), wkr, f.run.ID, "s", deltas, "the plan")
			if !errors.Is(err, ErrSummaryDeltasInvalid) {
				t.Fatalf("err = %v, want ErrSummaryDeltasInvalid", err)
			}
			if f.planParams != nil {
				t.Fatalf("SetRunPlanSummary was called (%+v) on invalid deltas, want rejected before the write", f.planParams)
			}
		})
	}
}

func TestSetPlanSummaryOversizeDeltasListRejected(t *testing.T) {
	deltas := make([]apitypes.RunSummaryDelta, MaxSummaryDeltas+1)
	for i := range deltas {
		deltas[i] = apitypes.RunSummaryDelta{Kind: "added", Text: "x"}
	}
	f := &summariesFakeStore{run: baseSummaryRun(), planRows: 1}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}
	if _, err := svc.SetPlanSummary(context.Background(), wkr, f.run.ID, "s", deltas, "the plan"); !errors.Is(err, ErrSummaryDeltasInvalid) {
		t.Fatalf("oversize list err = %v, want ErrSummaryDeltasInvalid", err)
	}
}

// ── delta encode/decode helpers ──────────────────────────────────────────────────────

func TestValidateAndMarshalDeltasEmptyIsEmptyArray(t *testing.T) {
	raw, err := validateAndMarshalDeltas(nil)
	if err != nil {
		t.Fatalf("validateAndMarshalDeltas(nil): %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("marshalled = %q, want []", raw)
	}
}

func TestDecodeSummaryDeltas(t *testing.T) {
	t.Run("nil/empty → nil", func(t *testing.T) {
		got, err := DecodeSummaryDeltas(nil)
		if err != nil || got != nil {
			t.Fatalf("DecodeSummaryDeltas(nil) = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("valid round-trips", func(t *testing.T) {
		raw := []byte(`[{"kind":"added","text":"a test"}]`)
		got, err := DecodeSummaryDeltas(raw)
		if err != nil {
			t.Fatalf("DecodeSummaryDeltas: %v", err)
		}
		if len(got) != 1 || got[0].Kind != "added" || got[0].Text != "a test" {
			t.Fatalf("got %+v, want one added delta", got)
		}
	})
	t.Run("malformed → error (caller degrades to nil)", func(t *testing.T) {
		if _, err := DecodeSummaryDeltas([]byte(`{not json`)); err == nil {
			t.Fatalf("malformed jsonb decoded without error, want an error the caller degrades to nil")
		}
	})
}
