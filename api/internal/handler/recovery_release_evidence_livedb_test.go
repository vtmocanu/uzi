package handler

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/recovery"
)

// TestRecoveryReleaseAndDiscardEvidenceLiveDB is the PRD #1392 M1 (D3) endpoint-level gate for
// release-evidence stamping on the recovery.Service worker Release and owner DiscardHold paths,
// plus the v2 exact-release generation echo, against a REAL Postgres. recovery.Service holds a
// live pgx pool (store.New(s.pool)), so this exercises the real store round-trip a fake store
// cannot exhibit: each release path stores its allowlisted/derived class, an unknown
// worker-supplied evidence is a bad request that settles nothing, and the exact path echoes the
// released generation.
//
// It lives in the handler package DELIBERATELY: e2e/run-store-it.sh and CI's test-api-store-it
// job run `-run 'LiveDB$'` over ./internal/store/... and ./internal/handler/... only — the
// internal/recovery package is swept by NEITHER, so an equivalent *LiveDB test there would run
// nowhere in CI (self-skips locally, prints `ok` under `go test ./...`).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints `ok`
// with PASS=0 is INVALID, not green.
func TestRecoveryReleaseAndDiscardEvidenceLiveDB(t *testing.T) {
	e := newRecoveryEnv(t) // self-skips unless UZI_TEST_DATABASE_URL is set
	svc := e.service(recoveryTestLimits())

	// A v2 worker names its generation on release and gets the exact-release generation echo.
	v2 := e.worker
	v2.ProtocolCapabilities = []string{capability.RecoveryArchiveV1, capability.RecoveryArchiveV2}
	gen := func(g int64) *int64 { return &g }
	str := func(s string) *string { return &s }

	// The env consumed issue_iid 1 on e.repo; fresh holds take distinct iids so the
	// one-active-run-per-issue unique index never trips.
	iid := int64(1)
	// openHold seeds a fresh run + a fresh OPEN gen-1 hold held live by the env worker, so each
	// release/discard path settles an independent hold.
	openHold := func() (runID, holdID uuid.UUID) {
		t.Helper()
		iid++
		runID, holdID = uuid.New(), uuid.New()
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running')`, runID, e.user, e.repo, iid); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO recovery_custody_holds
		      (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, 1, 'open', $5, $6, $5, $4)`,
			holdID, e.user, e.repo, runID, e.worker.ID, e.worker.Name); err != nil {
			t.Fatalf("insert hold: %v", err)
		}
		return runID, holdID
	}
	holdEvidence := func(id uuid.UUID) (state string, evidence *string) {
		t.Helper()
		var ev *string
		if err := e.pool.QueryRow(e.ctx, `SELECT state, release_evidence FROM recovery_custody_holds WHERE id = $1`, id).Scan(&state, &ev); err != nil {
			t.Fatalf("read hold: %v", err)
		}
		return state, ev
	}

	// ── (a) v2 release with evidence 'publication' echoes the released generation and stamps
	// 'publication'. Uses the env's base hold (gen 1). ──
	res, err := svc.Release(e.ctx, v2, e.run, apitypes.RecoveryReleaseRequest{Generation: gen(1), ReleaseEvidence: str("publication")})
	if err != nil {
		t.Fatalf("v2 Release(publication): %v", err)
	}
	if !res.Released || res.HoldsReleased != 1 {
		t.Fatalf("Release result = %+v, want Released/HoldsReleased=1", res)
	}
	if res.Generation == nil || *res.Generation != 1 {
		t.Fatalf("Release did not echo the released generation: Generation=%v, want 1", res.Generation)
	}
	if state, ev := holdEvidence(e.holdID); state != "released" || ev == nil || *ev != "publication" {
		t.Fatalf("hold state=%q evidence=%v, want released/publication", state, ev)
	}

	// ── (b) v2 release with evidence 'forge_no_output' stamps that class. ──
	r2, h2 := openHold()
	if _, err := svc.Release(e.ctx, v2, r2, apitypes.RecoveryReleaseRequest{Generation: gen(1), ReleaseEvidence: str("forge_no_output")}); err != nil {
		t.Fatalf("v2 Release(forge_no_output): %v", err)
	}
	if state, ev := holdEvidence(h2); state != "released" || ev == nil || *ev != "forge_no_output" {
		t.Fatalf("hold state=%q evidence=%v, want released/forge_no_output", state, ev)
	}

	// ── (c) an UNKNOWN release evidence is a bad request; nothing is released. ──
	r3, h3 := openHold()
	if _, err := svc.Release(e.ctx, v2, r3, apitypes.RecoveryReleaseRequest{Generation: gen(1), ReleaseEvidence: str("publication'; DROP")}); !errors.Is(err, recovery.ErrBadRequest) {
		t.Fatalf("v2 Release(unknown evidence) = %v, want ErrBadRequest", err)
	}
	if state, _ := holdEvidence(h3); state != "open" {
		t.Fatalf("hold state = %q after a rejected release, want open (nothing settled)", state)
	}

	// ── (d) owner Discard stamps 'owner_discard'. ──
	r4, h4 := openHold()
	discarded, err := svc.DiscardHold(e.ctx, e.user, r4, h4)
	if err != nil || !discarded {
		t.Fatalf("DiscardHold: discarded=%v err=%v", discarded, err)
	}
	if state, ev := holdEvidence(h4); state != "discarded" || ev == nil || *ev != "owner_discard" {
		t.Fatalf("hold state=%q evidence=%v, want discarded/owner_discard", state, ev)
	}
}
