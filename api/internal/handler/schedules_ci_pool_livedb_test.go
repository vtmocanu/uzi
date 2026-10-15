package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// One connection makes a nested pool acquisition deterministic: the config transaction
// owns the only connection, so every read before its commit must use its queries.
// A request deadline bounds the broken case without timing-dependent concurrency.
func TestCIWatchConfigEditSingleConnectionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, harness    string
		catalog, repoint bool
	}{
		{name: "user repoint and pinned credential", harness: "claude", repoint: true},
		{name: "user implicit harness and pinned credential"},
		{name: "default pinned harness and pinned credential", harness: "claude", catalog: true},
		{name: "default implicit harness and pinned credential", catalog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newScheduleFixture(ctx, t)
			secret := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "schedule-pool-regression")
			var schedule apitypes.ScheduleDTO
			if tc.catalog {
				var status int
				schedule, status = f.enableCatalog(t, f.owner.ID, f.repoID, "ci-repair")
				if status != http.StatusCreated {
					t.Fatalf("enable catalog: status %d", status)
				}
			} else {
				schedule = f.createCIWatch(t, "")
			}
			wantRepo := f.repoID
			body := map[string]any{"credential_override": map[string]any{"mode": "pinned", "secret_id": secret.String()}}
			if tc.harness != "" {
				body["harness"] = tc.harness
			}
			if tc.repoint {
				wantRepo = f.insertRepo(ctx, t, f.owner, 9876, "acme/pool-repoint")
				body["repo_id"] = wantRepo.String()
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := pgxpool.ParseConfig(os.Getenv("UZI_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxConns, cfg.MinConns = 1, 0
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			q := store.New(pool)
			f.h.pool, f.h.q = pool, q
			f.h.wsvc = workersvc.New(q, f.h.box, workersvc.Params{})
			req := userReq(http.MethodPatch, "/api/schedules/"+schedule.ID, string(raw), f.owner.ID, map[string]string{"id": schedule.ID})
			requestCtx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
			defer cancel()
			req = req.WithContext(requestCtx)
			rec := httptest.NewRecorder()
			f.h.PatchSchedule(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("PATCH with one connection: status %d, context %v, body %s", rec.Code, requestCtx.Err(), rec.Body.String())
			}
			if err := requestCtx.Err(); err != nil {
				t.Fatalf("PATCH exhausted its deadline: %v", err)
			}
			var repo, mode, pinned string
			if err := pool.QueryRow(ctx, `SELECT repo_id::text, credential_override_mode,
				credential_override_secret_id::text FROM run_schedules WHERE id = $1`, schedule.ID).Scan(&repo, &mode, &pinned); err != nil {
				t.Fatal(err)
			}
			if repo != wantRepo.String() || mode != "pinned" || pinned != secret.String() {
				t.Fatalf("persisted repo/override = %s/%s/%s", repo, mode, pinned)
			}
		})
	}
}
