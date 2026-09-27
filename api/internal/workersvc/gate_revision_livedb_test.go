package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1795 M1 live-DB coverage of plan-gate revision allocation, against the real migrated
// schema: allocation, current-id retry, id-less publication, adoption, conflict and historical
// refusals, same-gate reclaim (including an approval's capability override surviving a reclaim
// built from the REAL claim's resume_gate_presented), snapshot initialization, the refusal cap,
// refusal fencing, declined and stale-claim reports, chat, and the run_user_inputs binding CHECK.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

type gateFix struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	svc    *Service
	wkr    store.Worker
	userID uuid.UUID
	repoID uuid.UUID
}

const gatePlanA = "# Plan A\n\nDo the first thing."

// gateIssueSeq gives each fixture run its own issue number: one active run per (repo, issue).
var gateIssueSeq int64 = 1795000

func newGateFix(t *testing.T, maxRefusals int) *gateFix {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	box := newBox(t)
	q := store.New(pool)
	params := testParams()
	params.RunGateRefusalMax = maxRefusals
	svc := New(q, box, params)
	svc.SetTxBeginner(pool)
	f := &gateFix{t: t, ctx: ctx, pool: pool, q: q, svc: svc, userID: uuid.New(), repoID: uuid.New()}

	connID := uuid.New()
	sealedPAT, err := box.Seal([]byte("bot-pat-gate1795-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedAnthropic, err := box.Seal([]byte("anthropic-gate1795-token-abcdef12345"))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	f.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.userID, fmt.Sprintf("gate1795-%s@e2e", f.userID))
	f.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, f.userID, sealedPAT)
	f.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/gate1795', 'https://forge.e2e/g/gate1795', 'main', true)`, f.repoID, connID)
	f.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), f.userID, "anthropic-"+uuid.NewString(), sealedAnthropic)

	wkrID := uuid.New()
	f.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-gate1795', $3, 'offline')`,
		wkrID, f.userID, wkrID[:])
	// Docker-capable so a run requiring docker stays claimable; receipt-capable like a current
	// worker. Not credential_switch_v1, so a generation-less (legacy) report is still honoured.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID:                   wkrID,
		Capabilities:         []string{capability.Docker},
		ProtocolCapabilities: []string{capability.InputReceiptsV1, capability.GateRevisionV1},
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	f.wkr, err = q.GetWorkerByID(ctx, wkrID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	return f
}

func (f *gateFix) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

// newRun inserts a run of kind claimed by the fixture worker at generation 1, running and not
// yet gated, with a session so an unapproved plan resumes at the gate.
func (f *gateFix) newRun(kind string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	var repo any = f.repoID
	gateIssueSeq++
	var issue any = gateIssueSeq
	if kind == "chat" {
		repo, issue = nil, nil
	}
	f.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
	        claim_generation, auto_approve, plan_source, session_id)
	      VALUES ($1, $2, $3, $4, $5, 't', 'd', 'running', $6, 1, false, 'agent', 'sess-gate1795')`,
		id, f.userID, repo, kind, issue, f.wkr.ID)
	return id
}

// gateReq is the default awaiting_approval report: plan A, one milestone, docker required.
func gateReq(gen *int64, pid *uuid.UUID) StateRequest {
	plan := gatePlanA
	sess := "sess-gate1795"
	return StateRequest{
		State:                "awaiting_approval",
		PlanMd:               &plan,
		SessionID:            &sess,
		ClaimGeneration:      gen,
		PresentationID:       pid,
		Milestones:           &[]Milestone{{ID: "M1", Title: "First"}},
		RequiredCapabilities: &[]string{capability.Docker},
		PlanChangedFiles:     &[]string{" M a.go"},
	}
}

func i64(v int64) *int64 { return &v }

func uid() *uuid.UUID { id := uuid.New(); return &id }

func (f *gateFix) report(runID uuid.UUID, req StateRequest) (store.Run, bool, int64, error) {
	f.t.Helper()
	return f.svc.SetStateReport(f.ctx, f.wkr, runID, req)
}

// mustPublish reports and requires an applied report answered with wantRev.
func (f *gateFix) mustPublish(runID uuid.UUID, req StateRequest, wantRev int64) {
	f.t.Helper()
	_, applied, rev, err := f.report(runID, req)
	if err != nil || !applied || rev != wantRev {
		f.t.Fatalf("report = (applied %v, rev %d, err %v), want applied at revision %d", applied, rev, err, wantRev)
	}
}

type gateRow struct {
	status        string
	revision      int64
	presentation  *uuid.UUID
	refusals      int32
	refusalGen    *int64
	payload       []byte
	digest        []byte
	planMd        *string
	caps          []string
	tools         []string
	sizeClass     string
	candidate     []byte
	failOrigin    *string
	failureReason *string
	generation    int64
}

func (f *gateFix) row(runID uuid.UUID) gateRow {
	f.t.Helper()
	var r gateRow
	if err := f.pool.QueryRow(f.ctx, `SELECT status, gate_revision, gate_presentation_id, gate_refusal_count, gate_refusal_generation,
	        gate_presented_payload, gate_payload_digest, plan_md, required_capabilities, required_tools, size_class,
	        milestones_candidate, fail_origin, failure_reason, claim_generation
	      FROM runs WHERE id = $1`, runID).Scan(&r.status, &r.revision, &r.presentation, &r.refusals, &r.refusalGen,
		&r.payload, &r.digest, &r.planMd, &r.caps, &r.tools, &r.sizeClass, &r.candidate, &r.failOrigin, &r.failureReason, &r.generation); err != nil {
		f.t.Fatalf("read run: %v", err)
	}
	return r
}

func (f *gateFix) presentations(runID uuid.UUID) map[uuid.UUID]int64 {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT presentation_id, revision FROM run_gate_presentations WHERE run_id = $1`, runID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var id uuid.UUID
		var rev int64
		if err := rows.Scan(&id, &rev); err != nil {
			f.t.Fatal(err)
		}
		out[id] = rev
	}
	return out
}

// reclaim simulates a committed reclaim by the same worker: the generation moves on and the run
// is gated again (the reclaimed worker re-presents).
func (f *gateFix) reclaim(runID uuid.UUID) int64 {
	f.t.Helper()
	var gen int64
	if err := f.pool.QueryRow(f.ctx, `UPDATE runs SET claim_generation = claim_generation + 1, claim_released_at = NULL
	      WHERE id = $1 RETURNING claim_generation`, runID).Scan(&gen); err != nil {
		f.t.Fatal(err)
	}
	return gen
}

func TestGateRevisionAllocationLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("new id allocates and current-id retry returns the same revision", func(t *testing.T) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		r := f.row(run)
		if r.revision != 1 || r.presentation == nil || *r.presentation != *a || r.status != "awaiting_approval" {
			t.Fatalf("after publication: %+v", r)
		}
		if got := f.presentations(run); len(got) != 1 || got[*a] != 1 {
			t.Fatalf("presentations = %v, want {%s:1}", got, a)
		}
		// A lost ACK: the same report retried is answered with the same revision, no new row.
		f.mustPublish(run, gateReq(i64(1), a), 1)
		if r2 := f.row(run); r2.revision != 1 || string(r2.digest) != string(r.digest) {
			t.Fatalf("retry moved the gate: %+v", r2)
		}
		if got := f.presentations(run); len(got) != 1 {
			t.Fatalf("retry inserted a presentation: %v", got)
		}
		// A different, unseen id (an identical-text stub gate) allocates N+1.
		f.mustPublish(run, gateReq(i64(1), uid()), 2)
		if got := f.presentations(run); len(got) != 2 {
			t.Fatalf("presentations = %v, want two", got)
		}
	})

	t.Run("id-less report allocates each time and records no presentation", func(t *testing.T) {
		run := f.newRun("issue")
		f.mustPublish(run, gateReq(nil, nil), 1)
		f.mustPublish(run, gateReq(nil, nil), 2)
		var n int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_gate_presentations WHERE run_id = $1`, run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		r := f.row(run)
		if n != 0 || r.revision != 2 || r.presentation != nil || len(r.payload) == 0 || len(r.digest) != sha256.Size {
			t.Fatalf("id-less: presentation rows %d, row %+v", n, r)
		}
	})

	t.Run("historical id refused", func(t *testing.T) {
		run := f.newRun("issue")
		a, b := uid(), uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		f.mustPublish(run, gateReq(i64(1), b), 2)
		_, applied, rev, err := f.report(run, gateReq(i64(1), a))
		if !errors.Is(err, ErrGatePresentationHistorical) || applied || rev != 0 {
			t.Fatalf("historical report = (applied %v, rev %d, err %v), want ErrGatePresentationHistorical", applied, rev, err)
		}
		if r := f.row(run); r.revision != 2 || *r.presentation != *b {
			t.Fatalf("historical report moved the gate: %+v", r)
		}
	})

	t.Run("declined report allocates nothing", func(t *testing.T) {
		run := f.newRun("issue")
		f.exec(`UPDATE runs SET status = 'limit_wait' WHERE id = $1`, run)
		_, applied, rev, err := f.report(run, gateReq(i64(1), uid()))
		if err != nil || applied || rev != 0 {
			t.Fatalf("declined report = (applied %v, rev %d, err %v)", applied, rev, err)
		}
		if r := f.row(run); r.revision != 0 || len(f.presentations(run)) != 0 || r.refusals != 0 {
			t.Fatalf("declined report allocated: %+v", r)
		}
	})

	t.Run("fenced stale claim allocates nothing", func(t *testing.T) {
		run := f.newRun("issue")
		f.reclaim(run) // generation 2 now; the report still stamps 1
		_, applied, rev, err := f.report(run, gateReq(i64(1), uid()))
		if !errors.Is(err, ErrStaleClaim) || applied || rev != 0 {
			t.Fatalf("stale report = (applied %v, rev %d, err %v), want ErrStaleClaim", applied, rev, err)
		}
		if r := f.row(run); r.revision != 0 || r.status != "running" || len(f.presentations(run)) != 0 {
			t.Fatalf("stale report allocated: %+v", r)
		}
	})

	t.Run("chat never allocates", func(t *testing.T) {
		run := f.newRun("chat")
		_, _, rev, err := f.report(run, gateReq(nil, nil))
		if err != nil || rev != 0 {
			t.Fatalf("chat report = (rev %d, err %v)", rev, err)
		}
		if r := f.row(run); r.revision != 0 || len(r.payload) != 0 {
			t.Fatalf("chat allocated: %+v", r)
		}
		// An id-bearing chat report is not classified either (no 400, no refusal).
		if _, _, rev, err := f.report(run, gateReq(nil, uid())); err != nil || rev != 0 {
			t.Fatalf("id-bearing chat report = (rev %d, err %v)", rev, err)
		}
	})

	t.Run("id-bearing report without a claim generation is refused 400", func(t *testing.T) {
		run := f.newRun("issue")
		if _, _, _, err := f.report(run, gateReq(nil, uid())); !errors.Is(err, ErrClaimGenerationRequired) {
			t.Fatalf("err = %v, want ErrClaimGenerationRequired", err)
		}
		req := gateReq(nil, uid())
		req.AdoptGateRevision = i64(1)
		if _, _, _, err := f.report(run, req); !errors.Is(err, ErrClaimGenerationRequired) {
			t.Fatalf("adoption err = %v, want ErrClaimGenerationRequired", err)
		}
		if r := f.row(run); r.revision != 0 || r.status != "running" {
			t.Fatalf("refused report wrote: %+v", r)
		}
	})
}

func TestGateRevisionConflictLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	for _, c := range []struct {
		name   string
		mutate func(*StateRequest)
	}{
		{"changed plan", func(r *StateRequest) { p := "# Plan B"; r.PlanMd = &p }},
		{"changed milestone list", func(r *StateRequest) {
			r.Milestones = &[]Milestone{{ID: "M1", Title: "First"}, {ID: "M2", Title: "Second"}}
		}},
		{"added required capability", func(r *StateRequest) { r.RequiredCapabilities = &[]string{capability.Docker, capability.JVM} }},
		{"changed required tools", func(r *StateRequest) { r.RequiredTools = &[]string{capability.ToolGo} }},
		{"changed size class", func(r *StateRequest) { s := "l"; r.SizeClass = &s }},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := f.newRun("issue")
			a := uid()
			f.mustPublish(run, gateReq(i64(1), a), 1)
			before := f.row(run)
			req := gateReq(i64(1), a)
			c.mutate(&req)
			_, applied, rev, err := f.report(run, req)
			if !errors.Is(err, ErrGatePresentationConflict) || applied || rev != 0 {
				t.Fatalf("conflict report = (applied %v, rev %d, err %v), want ErrGatePresentationConflict", applied, rev, err)
			}
			after := f.row(run)
			// No write: the presented plan, candidate and requirements are untouched.
			if *after.planMd != *before.planMd || string(after.candidate) != string(before.candidate) ||
				!slices.Equal(after.caps, before.caps) || !slices.Equal(after.tools, before.tools) || after.sizeClass != before.sizeClass ||
				string(after.digest) != string(before.digest) || after.revision != 1 {
				t.Fatalf("conflict wrote the report:\n before %+v\n after  %+v", before, after)
			}
			// Counted once for this claim generation.
			if after.refusals != 1 || after.refusalGen == nil || *after.refusalGen != 1 || after.status != "awaiting_approval" {
				t.Fatalf("refusal accounting = count %d gen %v status %s, want 1/1/awaiting_approval", after.refusals, after.refusalGen, after.status)
			}
		})
	}
}

func TestGateRevisionAdoptionLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("accepted, then retried after a lost ACK", func(t *testing.T) {
		run := f.newRun("issue")
		f.mustPublish(run, gateReq(nil, nil), 1) // an old worker's id-less gate
		gen := f.reclaim(run)
		b := uid()
		req := gateReq(&gen, b)
		req.AdoptGateRevision = i64(1)
		f.mustPublish(run, req, 1)
		r := f.row(run)
		if r.revision != 1 || r.presentation == nil || *r.presentation != *b {
			t.Fatalf("adoption row = %+v", r)
		}
		if got := f.presentations(run); len(got) != 1 || got[*b] != 1 {
			t.Fatalf("presentations = %v, want {%s:1}", got, b)
		}
		// The retry after a lost adoption ACK is answered as a current-id retry.
		f.mustPublish(run, req, 1)
		if got := f.presentations(run); len(got) != 1 {
			t.Fatalf("retry inserted a presentation: %v", got)
		}
	})

	t.Run("refused as stale", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			setup func(run uuid.UUID)
			adopt int64
			req   func(*StateRequest)
		}{
			{"revision moved on", func(run uuid.UUID) {
				f.mustPublish(run, gateReq(nil, nil), 1)
				f.mustPublish(run, gateReq(nil, nil), 2)
			}, 1, nil},
			{"gate already carries an id", func(run uuid.UUID) { f.mustPublish(run, gateReq(i64(1), uid()), 1) }, 1, nil},
			{"payload changed", func(run uuid.UUID) { f.mustPublish(run, gateReq(nil, nil), 1) }, 1,
				func(r *StateRequest) { p := "# Different plan"; r.PlanMd = &p }},
			{"no gate yet", func(uuid.UUID) {}, 1, nil},
		} {
			t.Run(c.name, func(t *testing.T) {
				run := f.newRun("issue")
				c.setup(run)
				before := f.row(run)
				req := gateReq(i64(1), uid())
				req.AdoptGateRevision = i64(c.adopt)
				if c.req != nil {
					c.req(&req)
				}
				_, applied, rev, err := f.report(run, req)
				if !errors.Is(err, ErrGateAdoptionStale) || applied || rev != 0 {
					t.Fatalf("stale adoption = (applied %v, rev %d, err %v), want ErrGateAdoptionStale", applied, rev, err)
				}
				after := f.row(run)
				if after.revision != before.revision || fmt.Sprint(after.presentation) != fmt.Sprint(before.presentation) ||
					len(f.presentations(run)) != len(presentationsOf(before)) {
					t.Fatalf("stale adoption moved the gate: before %+v after %+v", before, after)
				}
			})
		}
	})
}

func presentationsOf(r gateRow) map[uuid.UUID]int64 {
	if r.presentation == nil {
		return nil
	}
	return map[uuid.UUID]int64{*r.presentation: r.revision}
}

func TestGateRevisionSameGateReclaimLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("milestone-less SDK re-presentation keeps N and the candidate", func(t *testing.T) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		before := f.row(run)
		gen := f.reclaim(run)
		req := gateReq(&gen, a)
		req.Milestones = nil
		f.mustPublish(run, req, 1)
		after := f.row(run)
		if after.revision != 1 || string(after.candidate) != string(before.candidate) || len(after.candidate) == 0 {
			t.Fatalf("milestone-less re-presentation: before %+v after %+v", before, after)
		}
	})

	t.Run("cold SDK reclaim whose plan_changed_files changed keeps N", func(t *testing.T) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		gen := f.reclaim(run)
		req := gateReq(&gen, a)
		req.PlanChangedFiles = &[]string{" M b.go", "?? c.go"}
		f.mustPublish(run, req, 1)
	})

	t.Run("an approval's capability override survives a reclaim built from the real claim", func(t *testing.T) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		if r := f.row(run); !slices.Equal(r.caps, []string{capability.Docker}) {
			t.Fatalf("published caps = %v, want [docker]", r.caps)
		}
		// The owner approves "without the capability": the override clears the live set.
		if _, err := f.svc.SubmitInputWithCapabilityOverride(f.ctx, f.userID, run, "approve_plan", "", nil); err != nil {
			t.Fatalf("override approve: %v", err)
		}
		if r := f.row(run); len(r.caps) != 0 {
			t.Fatalf("override left caps %v", r.caps)
		}
		// The worker dies before consuming the approve; the run is requeued and the SAME worker
		// reclaims it through the real ClaimRun + claim assembly.
		f.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, run)
		claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
		if err != nil {
			t.Fatalf("svc.Claim: %v", err)
		}
		if claim == nil || claim.RunID != run.String() {
			t.Fatalf("svc.Claim = %+v, want run %s", claim, run)
		}
		if claim.ResumePhase != "awaiting_approval" || claim.ResumeGateRevision != 1 ||
			claim.ResumeGatePresentationID == nil || *claim.ResumeGatePresentationID != *a {
			t.Fatalf("claim resume gate = phase %q rev %d id %v, want awaiting_approval/1/%s",
				claim.ResumePhase, claim.ResumeGateRevision, claim.ResumeGatePresentationID, a)
		}
		p := claim.ResumeGatePresented
		if p == nil || !slices.Equal(p.RequiredCapabilities, []string{capability.Docker}) {
			t.Fatalf("resume_gate_presented = %+v, want the presented [docker], not the cleared live set", p)
		}
		// Wire shape: the three keys ride the claim.
		wire, err := json.Marshal(claim)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(wire, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"resume_gate_revision", "resume_gate_presentation_id", "resume_gate_presented"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("claim wire lacks %q: %s", k, wire)
			}
		}
		// The reclaimed worker re-presents from the claim's presented copy.
		gen := claim.ClaimGeneration
		req := gateReq(&gen, claim.ResumeGatePresentationID)
		req.RequiredCapabilities = &p.RequiredCapabilities
		if len(p.RequiredTools) > 0 {
			req.RequiredTools = &p.RequiredTools
		}
		req.SizeClass = p.SizeClass
		f.mustPublish(run, req, 1)
		after := f.row(run)
		if after.revision != 1 || len(after.caps) != 0 {
			t.Fatalf("after re-presentation: revision %d caps %v, want 1 and the override-cleared []", after.revision, after.caps)
		}
	})

	t.Run("claim of an id-less gate carries the revision and no id", func(t *testing.T) {
		run := f.newRun("issue")
		f.mustPublish(run, gateReq(nil, nil), 1)
		f.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, run)
		claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
		if err != nil || claim == nil || claim.RunID != run.String() {
			t.Fatalf("svc.Claim = %+v, %v", claim, err)
		}
		if claim.ResumeGateRevision != 1 || claim.ResumeGatePresentationID != nil || claim.ResumeGatePresented == nil {
			t.Fatalf("claim = rev %d id %v presented %v, want 1/nil/set", claim.ResumeGateRevision, claim.ResumeGatePresentationID, claim.ResumeGatePresented)
		}
	})
}

func TestGateRevisionSnapshotInitializationLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	t.Run("first publication initializes the snapshot from the effective persisted payload", func(t *testing.T) {
		run := f.newRun("issue")
		// A repo hint and earlier inference already on the row.
		f.exec(`UPDATE runs SET required_capabilities = '{jvm}', required_tools = '{go}', size_class = 'm' WHERE id = $1`, run)
		f.mustPublish(run, gateReq(i64(1), uid()), 1)
		r := f.row(run)
		snap, ok, err := decodeGatePresentedPayload(r.payload)
		if err != nil || !ok {
			t.Fatalf("snapshot = %v %v", ok, err)
		}
		if snap.PlanMd == nil || *snap.PlanMd != gatePlanA ||
			!slices.Equal(sortedSet(snap.RequiredCapabilities), []string{capability.Docker, capability.JVM}) ||
			!slices.Equal(snap.RequiredTools, []string{capability.ToolGo}) || snap.SizeClass == nil || *snap.SizeClass != "m" {
			t.Fatalf("snapshot = %+v", snap)
		}
		canonical, sum, err := snap.digest()
		if err != nil {
			t.Fatal(err)
		}
		if string(sum) != string(r.digest) {
			t.Fatalf("stored digest does not match the snapshot's canonical form %s", canonical)
		}
		if _, err := canonicalRawJSON(snap.Milestones); err != nil || string(snap.Milestones) == "null" {
			t.Fatalf("snapshot milestones = %s (%v)", snap.Milestones, err)
		}
	})

	t.Run("migration-era adoption initializes the snapshot under the lock", func(t *testing.T) {
		run := f.newRun("issue")
		// A gate at revision 1 with no id and no snapshot, the persisted row carrying plan A.
		f.exec(`UPDATE runs SET status = 'awaiting_approval', plan_md = $2, required_capabilities = '{docker}',
		        milestones_candidate = '[{"id":"M1","title":"First"}]', gate_revision = 1 WHERE id = $1`, run, gatePlanA)
		b := uid()
		req := gateReq(i64(1), b)
		req.AdoptGateRevision = i64(1)
		f.mustPublish(run, req, 1)
		r := f.row(run)
		if len(r.payload) == 0 || len(r.digest) != sha256.Size || r.presentation == nil || *r.presentation != *b {
			t.Fatalf("adoption did not initialize the snapshot: %+v", r)
		}
		snap, _, err := decodeGatePresentedPayload(r.payload)
		if err != nil || snap.PlanMd == nil || *snap.PlanMd != gatePlanA || !slices.Equal(snap.RequiredCapabilities, []string{capability.Docker}) {
			t.Fatalf("initialized snapshot = %+v (%v)", snap, err)
		}
	})
}

func TestGateRevisionRefusalCapLiveDB(t *testing.T) {
	f := newGateFix(t, 3)

	// historicalRun publishes A then B, so A is historical, all under generation 1.
	historicalRun := func() (uuid.UUID, *uuid.UUID) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		f.mustPublish(run, gateReq(i64(1), uid()), 2)
		return run, a
	}

	t.Run("below the cap: once per claim generation, reset by a publication", func(t *testing.T) {
		run, a := historicalRun()
		for want := int32(1); want <= 2; want++ {
			gen := f.reclaim(run)
			if _, _, _, err := f.report(run, gateReq(&gen, a)); !errors.Is(err, ErrGatePresentationHistorical) {
				t.Fatalf("refusal %d err = %v", want, err)
			}
			if r := f.row(run); r.refusals != want || *r.refusalGen != gen || r.status != "awaiting_approval" {
				t.Fatalf("after refusal %d: count %d gen %v status %s", want, r.refusals, r.refusalGen, r.status)
			}
		}
		gen := f.row(run).generation
		f.mustPublish(run, gateReq(&gen, uid()), 3)
		if r := f.row(run); r.refusals != 0 || r.refusalGen != nil {
			t.Fatalf("publication did not reset the refusal accounting: count %d gen %v", r.refusals, r.refusalGen)
		}
	})

	t.Run("a current-id retry also resets the accounting", func(t *testing.T) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		changed := gateReq(i64(1), a)
		p := "# Plan B"
		changed.PlanMd = &p
		if _, _, _, err := f.report(run, changed); !errors.Is(err, ErrGatePresentationConflict) {
			t.Fatal(err)
		}
		f.mustPublish(run, gateReq(i64(1), a), 1)
		if r := f.row(run); r.refusals != 0 || r.refusalGen != nil {
			t.Fatalf("retained publication kept count %d gen %v", r.refusals, r.refusalGen)
		}
	})

	t.Run("the (cap+1)th counted refusal fails the run", func(t *testing.T) {
		run, a := historicalRun()
		for i := 1; i <= 3; i++ {
			gen := f.reclaim(run)
			if _, _, _, err := f.report(run, gateReq(&gen, a)); !errors.Is(err, ErrGatePresentationHistorical) {
				t.Fatalf("refusal %d err = %v", i, err)
			}
		}
		if r := f.row(run); r.status != "awaiting_approval" || r.refusals != 3 {
			t.Fatalf("at the cap: %+v", r)
		}
		gen := f.reclaim(run)
		got, applied, _, err := f.report(run, gateReq(&gen, a))
		if !errors.Is(err, ErrGatePresentationHistorical) || applied {
			t.Fatalf("cap refusal = (applied %v, err %v)", applied, err)
		}
		if got.Status != "failed" {
			t.Fatalf("returned run status %q, want failed", got.Status)
		}
		r := f.row(run)
		const wantReason = "the plan gate could not be re-presented: a historical presentation refused across 4 claims"
		if r.status != "failed" || r.failOrigin == nil || *r.failOrigin != "gate_presentation_refused" ||
			r.failureReason == nil || *r.failureReason != wantReason || r.refusals != 4 {
			t.Fatalf("cap failure row = status %s origin %v reason %v count %d", r.status, r.failOrigin, r.failureReason, r.refusals)
		}
	})

	t.Run("unlimited when the cap is 0", func(t *testing.T) {
		g := newGateFix(t, 0)
		run := g.newRun("issue")
		a := uid()
		g.mustPublish(run, gateReq(i64(1), a), 1)
		g.mustPublish(run, gateReq(i64(1), uid()), 2)
		for i := 0; i < 5; i++ {
			gen := g.reclaim(run)
			if _, _, _, err := g.report(run, gateReq(&gen, a)); !errors.Is(err, ErrGatePresentationHistorical) {
				t.Fatal(err)
			}
		}
		if r := g.row(run); r.status != "awaiting_approval" || r.refusals != 5 {
			t.Fatalf("cap 0 = %+v, want never failed", r)
		}
	})
}

func TestGateRevisionRefusalFencingLiveDB(t *testing.T) {
	f := newGateFix(t, 1)

	historicalRun := func() (uuid.UUID, *uuid.UUID) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		f.mustPublish(run, gateReq(i64(1), uid()), 2)
		return run, a
	}

	t.Run("a reclaim that commits before the refused report takes the lock fences it", func(t *testing.T) {
		run, a := historicalRun()
		// Hold the run row lock, start the generation-1 report (it blocks on its FOR UPDATE),
		// then commit a reclaim to generation 2 before releasing the lock.
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		if _, err := tx.Exec(f.ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, run); err != nil {
			t.Fatal(err)
		}
		type result struct {
			applied bool
			err     error
		}
		done := make(chan result, 1)
		go func() {
			_, applied, _, err := f.svc.SetStateReport(f.ctx, f.wkr, run, gateReq(i64(1), a))
			done <- result{applied, err}
		}()
		waitForLockWaiter(t, f)
		if _, err := tx.Exec(f.ctx, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, run); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		res := <-done
		if !errors.Is(res.err, ErrStaleClaim) || res.applied {
			t.Fatalf("raced report = (applied %v, err %v), want ErrStaleClaim", res.applied, res.err)
		}
		if r := f.row(run); r.refusals != 0 || r.refusalGen != nil || r.status != "awaiting_approval" {
			t.Fatalf("a fenced report counted or failed: count %d gen %v status %s", r.refusals, r.refusalGen, r.status)
		}
	})

	// The two cases below are deliberately separate (operator clarification, 2026-09-27): a
	// FENCED stale claim (superseded generation, released claim, or not the owning worker) is
	// answered stale and neither counts nor fails; a historical id sent by the STILL-CURRENT
	// claim is a legitimate refusal under the normal counting policy (once per claim
	// generation, cap failure past RUN_GATE_REFUSAL_MAX).
	t.Run("a FENCED stale claim's report (superseded generation, released claim, foreign worker) neither counts nor fails", func(t *testing.T) {
		run := f.newRun("issue")
		a := uid()
		f.mustPublish(run, gateReq(i64(1), a), 1)
		gen := f.reclaim(run)
		f.mustPublish(run, gateReq(&gen, uid()), 2) // the current claim publishes; a is historical
		// Superseded generation, released claim, and a foreign worker: all fenced.
		if _, _, _, err := f.report(run, gateReq(i64(1), a)); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("superseded-generation report err = %v, want ErrStaleClaim", err)
		}
		f.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)
		if _, _, _, err := f.report(run, gateReq(&gen, a)); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("released-claim report err = %v, want ErrStaleClaim", err)
		}
		f.exec(`UPDATE runs SET claim_released_at = NULL WHERE id = $1`, run)
		other := uuid.New()
		f.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-gate1795-other', $3, 'online')`, other, f.userID, other[:])
		otherWkr, err := f.q.GetWorkerByID(f.ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := f.svc.SetStateReport(f.ctx, otherWkr, run, gateReq(&gen, a)); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("foreign-worker report err = %v, want ErrRunNotOwned", err)
		}
		if r := f.row(run); r.refusals != 0 || r.refusalGen != nil || r.failOrigin != nil || r.status != "awaiting_approval" || r.revision != 2 {
			t.Fatalf("fenced stale reports changed the run (want count 0, gate_refusal_generation and fail_origin NULL): %+v", r)
		}
	})

	t.Run("a historical id from the STILL-CURRENT claim is a counted refusal under the normal policy", func(t *testing.T) {
		run, a := historicalRun()
		gen := f.row(run).generation
		if _, _, _, err := f.report(run, gateReq(&gen, a)); !errors.Is(err, ErrGatePresentationHistorical) {
			t.Fatalf("err = %v", err)
		}
		if r := f.row(run); r.refusals != 1 || *r.refusalGen != gen || r.status != "awaiting_approval" {
			t.Fatalf("current-claim refusal = count %d gen %v status %s, want counted once, not failed (cap 1)", r.refusals, r.refusalGen, r.status)
		}
		// With cap 1, the next COUNTED refusal (a new claim generation) fails the run.
		gen = f.reclaim(run)
		if _, _, _, err := f.report(run, gateReq(&gen, a)); !errors.Is(err, ErrGatePresentationHistorical) {
			t.Fatal(err)
		}
		if r := f.row(run); r.status != "failed" || r.refusals != 2 {
			t.Fatalf("second counted refusal: %+v, want failed", r)
		}
	})

	t.Run("a lost refusal response retried answers the same 409, counted once", func(t *testing.T) {
		run, a := historicalRun()
		for i := 0; i < 3; i++ {
			if _, _, _, err := f.report(run, gateReq(i64(1), a)); !errors.Is(err, ErrGatePresentationHistorical) {
				t.Fatalf("retry %d err = %v", i, err)
			}
		}
		if r := f.row(run); r.refusals != 1 || r.status != "awaiting_approval" {
			t.Fatalf("retried refusal = count %d status %s, want 1 and not failed", r.refusals, r.status)
		}
	})

	t.Run("a second refusal within the same claim generation is not counted twice", func(t *testing.T) {
		run, a := historicalRun()
		if _, _, _, err := f.report(run, gateReq(i64(1), a)); !errors.Is(err, ErrGatePresentationHistorical) {
			t.Fatal(err)
		}
		stale := gateReq(i64(1), uid())
		stale.AdoptGateRevision = i64(1)
		if _, _, _, err := f.report(run, stale); !errors.Is(err, ErrGateAdoptionStale) {
			t.Fatalf("second refusal err = %v", err)
		}
		if r := f.row(run); r.refusals != 1 || r.status != "awaiting_approval" {
			t.Fatalf("two refusals in one claim = count %d status %s, want 1", r.refusals, r.status)
		}
	})
}

// TestGateRevisionRefusalOnParkedOrTerminalRunLiveDB pins gateReportAdmissible: a report that
// WOULD be refused (a historical id, or a changed payload under the current id) onto a run that
// SetRunAwaitingApproval would decline anyway (parked in limit_wait or recovery_wait, or terminal)
// is declined before classification. It counts no refusal, cannot reach the cap failure, and
// leaves the status unchanged. The fixture's cap is 1 and each report comes from a fresh claim
// generation, so a counted refusal would also FAIL the run on the second report.
func TestGateRevisionRefusalOnParkedOrTerminalRunLiveDB(t *testing.T) {
	f := newGateFix(t, 1)

	for _, status := range []string{"limit_wait", "recovery_wait", "failed"} {
		for _, kind := range []string{"historical id", "current-id changed payload"} {
			t.Run(status+"/"+kind, func(t *testing.T) {
				run := f.newRun("issue")
				a := uid()
				f.mustPublish(run, gateReq(i64(1), a), 1)
				req := gateReq(nil, a)
				wantRev := int64(1)
				if kind == "historical id" {
					f.mustPublish(run, gateReq(i64(1), uid()), 2) // a is now historical
					wantRev = 2
				} else {
					p := "# Plan B\n\nSomething else."
					req.PlanMd = &p
				}
				before := f.row(run)
				f.exec(`UPDATE runs SET status = $2 WHERE id = $1`, run, status)
				for i := 0; i < 2; i++ {
					gen := f.reclaim(run)
					req.ClaimGeneration = &gen
					_, applied, rev, err := f.report(run, req)
					if err != nil || applied || rev != 0 {
						t.Fatalf("report %d onto a %s run = (applied %v, rev %d, err %v), want declined (nil error, not applied)", i, status, applied, rev, err)
					}
				}
				r := f.row(run)
				if r.status != status || r.refusals != 0 || r.refusalGen != nil || r.failOrigin != nil ||
					r.revision != wantRev || string(r.digest) != string(before.digest) {
					t.Fatalf("declined refusal-class report changed the run: status %s (want %s) refusals %d gen %v origin %v revision %d",
						r.status, status, r.refusals, r.refusalGen, r.failOrigin, r.revision)
				}
			})
		}
	}
}

// waitForLockWaiter blocks until some backend waits on a row lock (the report's FOR UPDATE). The
// report's query text is truncated in pg_stat_activity (the expanded runs column list exceeds
// track_activity_query_size), so it matches on the lock wait alone; see waitForLockWaiters for
// why that relies on the sweep's -p 1.
func waitForLockWaiter(t *testing.T, f *gateFix) {
	t.Helper()
	waitForLockWaiters(t, f, 1)
}

func TestRunUserInputsGateBindingCheckLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	run := f.newRun("issue")
	insert := func(binding any, revision any) error {
		_, err := f.pool.Exec(f.ctx, `INSERT INTO run_user_inputs (run_id, kind, body, gate_binding, gate_revision)
		      VALUES ($1, 'approve_plan', 'x', $2, $3)`, run, binding, revision)
		return err
	}
	for _, c := range []struct {
		name     string
		binding  any
		revision any
		ok       bool
	}{
		{"legacy", nil, nil, true},
		{"unbound", "unbound", nil, true},
		{"bound 1", "bound", int64(1), true},
		{"legacy with a revision", nil, int64(1), false},
		{"bound with NULL", "bound", nil, false},
		{"bound with 0", "bound", int64(0), false},
		{"bound with a negative revision", "bound", int64(-1), false},
		{"unbound with a revision", "unbound", int64(1), false},
		{"unknown binding", "maybe", nil, false},
		{"unknown binding with a revision", "maybe", int64(1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := insert(c.binding, c.revision)
			if c.ok && err != nil {
				t.Fatalf("valid shape rejected: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("invalid shape accepted")
			}
		})
	}
}
