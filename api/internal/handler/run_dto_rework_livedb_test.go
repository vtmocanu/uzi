package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// runReworkDTO is the slice of GET /api/runs/{id}'s {"run":…} envelope this test reads: the
// two owner-only PRD #1202 loop-guard fields.
type runReworkDTO struct {
	Run struct {
		MrReworkAutoCycles *int `json:"mr_rework_auto_cycles"`
		MrReworkAutoCap    *int `json:"mr_rework_auto_cap"`
	} `json:"run"`
}

func decodeRunReworkDTO(t *testing.T, rec *httptest.ResponseRecorder) runReworkDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var dto runReworkDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode run dto: %v\nbody: %s", err, rec.Body.String())
	}
	return dto
}

// TestRunReworkAutoCyclesDTOOwnerOnlyLiveDB pins the PRD #1202 D11 owner-only enrichment on
// GetRun: the automatic-rework cycles/cap pair is populated ONLY for the run's owner on a
// completed issue/prompt/self_improve run whose MR is open, and is NULL for a non-owner
// (admin) viewer and for a run whose MR has left the opened state — so the pair never leaks
// another user's MR state. It is a live-DB test because the enrichment reads the real ledger
// + the settings cap and is gated by GetRunForViewer's owner-vs-admin visibility.
//
// The admin viewer uses the COOKIE session path, not a uzc_ Bearer, on purpose: a user-scope
// CLI token resolves IsAdmin=false in the request (admin visibility rides the session or a
// uza_ token), so only the cookie path exercises "an ADMIN sees the run but the owner-only
// field is withheld".
func TestRunReworkAutoCyclesDTOOwnerOnlyLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	ownerUzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	adminJWT := cliMintJWT(t, pool, admin)
	repoID := cliSeedOwnedRepo(t, pool, owner)

	// A completed issue run owned by `owner` with an OPEN MR on agent/issue-<n>, plus a
	// ledger row at the cap (5 automatic cycles spent) — the "watcher halted" state the owner
	// reworks from.
	openRun := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		 VALUES ($1, $2, $3, 'issue', 5501, 't', 'd', 'agent/issue-5501', 5501, 'opened', 'completed')`,
		openRun, owner, repoID)
	cliMustExec(t, pool,
		`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water) VALUES ($1, 'agent/issue-5501', 5, 200)`,
		repoID)

	// Owner view: both fields populated — 5 cycles spent, cap 5 (the settings default in this
	// harness).
	got := decodeRunReworkDTO(t, bearerReq(router, http.MethodGet, "/api/runs/"+openRun.String(), ownerUzc))
	if got.Run.MrReworkAutoCycles == nil || *got.Run.MrReworkAutoCycles != 5 {
		t.Fatalf("owner mr_rework_auto_cycles = %v, want 5", got.Run.MrReworkAutoCycles)
	}
	if got.Run.MrReworkAutoCap == nil || *got.Run.MrReworkAutoCap != 5 {
		t.Fatalf("owner mr_rework_auto_cap = %v, want 5 (settings default)", got.Run.MrReworkAutoCap)
	}

	// Admin (non-owner) view of the SAME run over a COOKIE session: GetRunForViewer lets an
	// admin see it (200), but the pair is owner-only and must be NULL — the leak this gate
	// exists to prevent.
	adminView := decodeRunReworkDTO(t, cookieReq(t, router, http.MethodGet, "/api/runs/"+openRun.String(), adminJWT, ""))
	if adminView.Run.MrReworkAutoCycles != nil || adminView.Run.MrReworkAutoCap != nil {
		t.Fatalf("admin (non-owner) must see NULL rework cycles/cap, got %v / %v",
			adminView.Run.MrReworkAutoCycles, adminView.Run.MrReworkAutoCap)
	}

	// A completed issue run owned by `owner` whose MR is MERGED: the pair is NULL even for the
	// owner (the watcher no longer applies), so the run page shows no stale loop-guard line.
	mergedRun := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		 VALUES ($1, $2, $3, 'issue', 5502, 't', 'd', 'agent/issue-5502', 5502, 'merged', 'completed')`,
		mergedRun, owner, repoID)
	mergedView := decodeRunReworkDTO(t, bearerReq(router, http.MethodGet, "/api/runs/"+mergedRun.String(), ownerUzc))
	if mergedView.Run.MrReworkAutoCycles != nil || mergedView.Run.MrReworkAutoCap != nil {
		t.Fatalf("owner of a MERGED-MR run must see NULL rework cycles/cap, got %v / %v",
			mergedView.Run.MrReworkAutoCycles, mergedView.Run.MrReworkAutoCap)
	}
}
