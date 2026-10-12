package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2760 M1: an awaiting_approval run whose latest plan-ish message is a
// plan_revising is re-planning, so GetRun, ListRuns and AdminListRuns must all report
// is_revising=true and progress.state="planning" instead of "waiting". Skipped unless
// UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

func insertPlanMsg(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, seq int, kind string) {
	t.Helper()
	cliMustExec(t, pool, `INSERT INTO run_messages (run_id, seq, kind, agent, payload) VALUES ($1, $2, $3, 'lead', '{}'::jsonb)`,
		runID, seq, kind)
}

func TestRunRevisingDrivesProgressLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	t.Cleanup(func() {
		cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner)
		cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, admin)
	})
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), time.Now().UnixNano()&0x3fffffff, true)
	runID := rmSeedRun(t, pool, owner, repo, "awaiting_approval")
	ownerUser := store.User{ID: owner}

	type view struct {
		revising bool
		state    string
	}
	findRun := func(t *testing.T, body []byte) view {
		t.Helper()
		var resp struct {
			Runs []apitypes.RunListItemDTO `json:"runs"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode list: %v; body=%s", err, body)
		}
		for _, it := range resp.Runs {
			if it.ID == runID.String() {
				if it.Progress == nil {
					t.Fatal("list row has no progress block")
				}
				return view{it.IsRevising, it.Progress.State}
			}
		}
		t.Fatalf("run %s not in list: %s", runID, body)
		return view{}
	}
	surfaces := map[string]func(t *testing.T) view{
		"GetRun": func(t *testing.T) view {
			dto := getRunDTO(t, h, ownerUser, runID)
			if dto.Progress == nil {
				t.Fatal("run has no progress block")
			}
			return view{dto.IsRevising, dto.Progress.State}
		},
		"ListRuns": func(t *testing.T) view {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
			h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), ownerUser)))
			if rec.Code != http.StatusOK {
				t.Fatalf("ListRuns = %d %s", rec.Code, rec.Body.String())
			}
			return findRun(t, rec.Body.Bytes())
		},
		"AdminListRuns": func(t *testing.T) view {
			rec := httptest.NewRecorder()
			h.AdminListRuns(rec, httptest.NewRequest(http.MethodGet, "/api/admin/runs", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("AdminListRuns = %d %s", rec.Code, rec.Body.String())
			}
			return findRun(t, rec.Body.Bytes())
		},
	}
	check := func(label string, want view) {
		for name, fn := range surfaces {
			if got := fn(t); got != want {
				t.Errorf("%s: %s = %+v, want %+v", label, name, got, want)
			}
		}
	}

	insertPlanMsg(t, pool, runID, 1, "plan")
	check("plan awaiting approval", view{false, "waiting"})
	insertPlanMsg(t, pool, runID, 2, "plan_revising")
	check("revise in flight", view{true, "planning"})
	insertPlanMsg(t, pool, runID, 3, "plan")
	check("revised plan landed", view{false, "waiting"})
}
