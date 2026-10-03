package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
)

func TestRepoSyncInvalidationWriterLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reg := healthsvc.NewSyncRegistry(func() time.Time { return now })
	calls := []uuid.UUID{}
	f.h.SetRepoSyncInvalidator(func(id uuid.UUID) { calls = append(calls, id); reg.Invalidate(id) })
	id := f.seedRepo(ctx, t, 71, true, protClean)
	svc := healthsvc.New(healthsvc.Config{Store: f.h.q, Now: func() time.Time { return now }, ForgeSyncRegistry: reg, ForgeSyncInterval: time.Minute})
	severity := func(want string) {
		t.Helper()
		doc, err := svc.Evaluate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Checks {
			if c.ID == "forge.sync" {
				if c.Severity != want {
					t.Fatalf("forge.sync=%+v, want %s", c, want)
				}
				return
			}
		}
		t.Fatal("missing forge.sync")
	}
	ids, err := f.h.q.ListEnabledRepoIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, enabledID := range ids {
		reg.Begin(enabledID)(true, forge.ErrorClassOther)
	}
	reg.Begin(id)(false, forge.ErrorClassTimeout)
	now = now.Add(10 * time.Minute)
	severity("danger")
	staleFailure := reg.Begin(id)
	staleSuccess := reg.Begin(id)
	// Both writes finish between evaluations. The enabled-ID query never sees disabled.
	for _, enabled := range []bool{false, true} {
		rec := f.setEnabled(t, id, enabled)
		if rec.Code != http.StatusOK {
			t.Fatalf("toggle %v: %d %s", enabled, rec.Code, rec.Body.String())
		}
	}
	staleFailure(false, forge.ErrorClassAuth)
	staleSuccess(true, forge.ErrorClassOther)
	severity("unknown")
	if len(calls) != 2 || calls[0] != id || calls[1] != id {
		t.Fatalf("hooks=%v", calls)
	}
	reg.Begin(id)(true, forge.ErrorClassOther)
	severity("ok")
	// Idempotent enable is approved to invalidate unconditionally.
	if rec := f.setEnabled(t, id, true); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	severity("unknown")
	reg.Begin(id)(true, forge.ErrorClassOther)
	if rec := f.setEnabled(t, id, false); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := f.setEnabled(t, id, false); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if len(calls) != 5 {
		t.Fatalf("idempotent writes did not each invalidate: %v", calls)
	}

	blocked := f.seedRepo(ctx, t, 72, false, protCanPush)
	if rec := f.setEnabled(t, blocked, true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("guard refusal: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.setEnabled(t, uuid.New(), false); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	other := newEnableGuardFixture(ctx, t)
	foreign := other.seedRepo(ctx, t, 73, true, protClean)
	if rec := f.setEnabled(t, foreign, false); rec.Code != http.StatusNotFound {
		t.Fatalf("nonowner: %d", rec.Code)
	}
	if len(calls) != 5 {
		t.Fatalf("refusal invalidated: %v", calls)
	}

	// Closing this fixture's pool makes the actual disable writer fail.
	f.pool.Close()
	if rec := f.setEnabled(t, id, false); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed write: %d %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 5 {
		t.Fatalf("failed write invalidated: %v", calls)
	}
}

func TestRepoSyncInvalidatorNilSafeLiveDB(t *testing.T) {
	f := newEnableGuardFixture(context.Background(), t)
	id := f.seedRepo(context.Background(), t, 74, true, protClean)
	if rec := f.setEnabled(t, id, false); rec.Code != http.StatusOK {
		t.Fatalf("unwired callback: %d %s", rec.Code, rec.Body.String())
	}
}
