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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// PRD #4213 M1 (U2) over a real database: the size table feeding the controller poll and
// the provision gate, and the controller's admin-size capability. Run via
// ./e2e/run-store-it.sh; skipped without UZI_TEST_DATABASE_URL.

func reconcileSizesLiveDB(t *testing.T, h *Handler) {
	t.Helper()
	if err := store.ReconcileBuiltinWorkerSizes(context.Background(), h.pool); err != nil {
		t.Fatalf("reconcile builtin sizes: %v", err)
	}
}

// A seeded worker of each builtin size polls with Resources equal to the builtin values;
// a worker whose hosted_size has no row polls with nil Resources, not zeroes.
func TestHostedPollCarriesSizeResourcesLiveDB(t *testing.T) {
	h, pool, q, box, userID := hostedLiveDB(t, "20")
	ctx := context.Background()
	reconcileSizesLiveDB(t, h)

	ids := map[string]uuid.UUID{}
	for _, s := range append(builtin.Sizes(), builtin.Size{Name: "ghost"}) {
		id := uuid.New()
		ids[s.Name] = id
		if _, err := pool.Exec(ctx, `INSERT INTO workers (id, user_id, name, token_hash, kind, hosted_size, template_declared, docker_enabled)
			VALUES ($1, $2, $3, $4, 'hosted', $5, 'base', false)`, id, userID, "sz-"+s.Name+"-"+id.String(), id[:], s.Name); err != nil {
			t.Fatalf("seed worker %s: %v", s.Name, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = pool.Exec(context.Background(), `DELETE FROM workers WHERE id = $1`, id)
		}
	})

	resp, err := hostedsvc.New(q, box, time.Now, time.Hour).Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	byID := map[string]hostedsvc.DesiredWorker{}
	for _, w := range resp.Workers {
		byID[w.ID] = w
	}
	for _, s := range builtin.Sizes() {
		w, ok := byID[ids[s.Name].String()]
		if !ok {
			t.Fatalf("worker of size %q absent from the poll", s.Name)
		}
		want := hostedsvc.SizeResources{
			CPURequestMillicores: s.CPURequestMillicores, CPULimitMillicores: s.CPULimitMillicores,
			MemoryRequestMiB: s.MemoryRequestMiB, MemoryLimitMiB: s.MemoryLimitMiB,
			DataGiB: s.DataGiB, DinDDataGiB: s.DinDDataGiB,
		}
		if w.Resources == nil || *w.Resources != want {
			t.Errorf("size %q: Resources = %+v, want %+v", s.Name, w.Resources, want)
		}
	}
	ghost, ok := byID[ids["ghost"].String()]
	if !ok {
		t.Fatal("ghost-size worker absent from the poll")
	}
	if ghost.Resources != nil {
		t.Errorf("a size with no row sent Resources %+v, want nil", ghost.Resources)
	}
}

// A well-formed size that names no row is refused at provision with 400, after the policy
// gates; a builtin size (reconciled in) is accepted.
func TestProvisionHostedWorkerUnknownSizeLiveDB(t *testing.T) {
	h, pool, _, _, userID := hostedLiveDB(t, "20")
	reconcileSizesLiveDB(t, h)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/workers/hosted", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: userID, Email: "u@uzi.local", IsActive: true}))
		rec := httptest.NewRecorder()
		h.ProvisionHostedWorker(rec, req)
		return rec
	}
	if rec := post(`{"template":"base","size":"xl"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "unknown worker size") {
		t.Fatalf("unknown size: %d %s, want 400 unknown worker size", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM workers WHERE user_id = $1`, userID) })
	if rec := post(`{"template":"base","size":"m"}`); rec.Code >= 400 {
		t.Fatalf("builtin size m: %d %s, want success", rec.Code, rec.Body.String())
	}
}

// The controller's admin-size version is recorded by a FLEET status report (0 included) with
// the api's receipt clock, and a readiness report leaves it untouched.
func TestControllerStatusAdminSizesCapabilityLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	ctx := context.Background()
	receipt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	h.now = func() time.Time { return receipt }

	post := func(body map[string]any) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(string(data))))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	read := func() (version pgtype.Int4, at pgtype.Timestamptz) {
		t.Helper()
		got, err := store.New(pool).GetControllerAdminSizesAdvertisement(ctx)
		if err != nil {
			t.Fatalf("read advertisement: %v", err)
		}
		return got.AdminSizesVersion, got.AdminSizesAdvertisedAt
	}

	post(map[string]any{"kind": "fleet", "admin_sizes": 1, "workers": []any{}})
	if v, at := read(); !v.Valid || v.Int32 != 1 || !at.Valid || !at.Time.Equal(receipt) {
		t.Fatalf("after v1 fleet report: version %+v advertised_at %+v, want 1 at %v", v, at, receipt)
	}

	// A readiness report (which carries no capability by construction) must not overwrite it.
	later := receipt.Add(10 * time.Minute)
	h.now = func() time.Time { return later }
	post(map[string]any{"kind": "readiness", "admin_sizes": 0, "workers": []any{}})
	if v, at := read(); !v.Valid || v.Int32 != 1 || !at.Time.Equal(receipt) {
		t.Fatalf("a readiness report changed the advertisement: version %+v advertised_at %+v", v, at)
	}

	// A fleet report from a controller without the capability records an honest 0.
	post(map[string]any{"kind": "fleet", "workers": []any{}})
	if v, at := read(); !v.Valid || v.Int32 != 0 || !at.Time.Equal(later) {
		t.Fatalf("after v0 fleet report: version %+v advertised_at %+v, want 0 at %v", v, at, later)
	}
}
