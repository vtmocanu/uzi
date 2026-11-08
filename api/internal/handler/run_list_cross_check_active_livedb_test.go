package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4082: cross_check_active on the personal and admin run lists. Every deadline here is
// relative to the database's own now() (live = +10 minutes, past = -10 minutes), never the
// Go clock, so the list's SQL now() and the parity probes agree on every case.
const (
	ccLive = "10 minutes"
	ccPast = "-10 minutes"
)

type ccListFixture struct {
	pool        *pgxpool.Pool
	q           *store.Queries
	owner       uuid.UUID
	conn        uuid.UUID
	worker      uuid.UUID
	otherWorker uuid.UUID
	project     int64
}

func newCCListFixture(t *testing.T, pool *pgxpool.Pool) *ccListFixture {
	t.Helper()
	f := &ccListFixture{pool: pool, q: store.New(pool), owner: cliSeedUser(t, pool, false), project: 41000}
	// Deleting the owner cascades to exactly the connection, repos, workers and runs seeded here.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, f.owner); err != nil {
			t.Logf("cleanup owner: %v", err)
		}
	})
	f.conn = rmSeedConn(t, pool, f.owner)
	mkWorker := func(name string) uuid.UUID {
		id := uuid.New()
		hash := sha256.Sum256([]byte(name + id.String()))
		cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`, id, f.owner, name+"-"+id.String()[:8], hash[:])
		return id
	}
	f.worker, f.otherWorker = mkWorker("ccw"), mkWorker("ccx")
	return f
}

// lead seeds a fresh repo and a run of the given kind and status, bound to the fixture worker
// with claim generation 1 and the claude harness.
func (f *ccListFixture) lead(t *testing.T, kind, status string) uuid.UUID {
	t.Helper()
	f.project++
	repo := rmSeedRepo(t, f.pool, f.conn, f.project, true)
	id := rmSeedRun(t, f.pool, f.owner, repo, status)
	// runs_kind_shape ties each kind to the columns it must (not) carry.
	shape := map[string]string{
		"issue":  ``,
		"prompt": `,issue_iid=NULL`,
		"task":   `,issue_iid=NULL,branch='cc-list-task'`,
		"ci_fix": `,pipeline_id=1,pipeline_ref='main'`,
	}[kind]
	cliMustExec(t, f.pool, `UPDATE runs SET kind=$2,worker_id=$3,harness='claude',claim_generation=1`+shape+` WHERE id=$1`, id, kind, f.worker)
	return id
}

type ccPlanRound struct {
	verdict  string
	deadline string
}

// planRounds inserts plan cross-check rounds 1..n for lead at the given lead claim generation.
func (f *ccListFixture) planRounds(t *testing.T, lead uuid.UUID, gen int64, rounds ...ccPlanRound) {
	t.Helper()
	for i, r := range rounds {
		var reason, findings any
		if r.verdict != "pending" {
			reason, findings = r.verdict, `{"summary":"s","items":[]}`
		}
		cliMustExec(t, f.pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,planning_diff,candidate_digest,verdict,reason_class,findings,decided_at,automatic_rounds_enabled,automatic_revision_limit,created_at,deadline_at)
VALUES($1,'plan',$2,$3,'plan','[]','s',repeat('a',40),'',$4,$5,$6,$7::jsonb,CASE WHEN $5='pending' THEN NULL ELSE now() END,true,3,now()-interval '30 minutes',now()+$8::interval)`,
			lead, i+1, gen, make([]byte, 32), r.verdict, reason, findings, r.deadline)
	}
}

type ccCode struct {
	outcome       string
	deadline      string
	interrupted   bool
	gen           int64
	childWorker   uuid.UUID
	childHarness  string
	checkerHarnes string
	childStatus   string
}

func (f *ccListFixture) codeDefaults() ccCode {
	return ccCode{outcome: "pending", deadline: ccLive, gen: 1, childWorker: f.worker, childHarness: "codex", checkerHarnes: "codex", childStatus: "running"}
}

// codeCheck inserts the checker child run and the code cross-check row for lead.
func (f *ccListFixture) codeCheck(t *testing.T, lead uuid.UUID, c ccCode) {
	t.Helper()
	var repo uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT repo_id FROM runs WHERE id=$1`, lead).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	child := uuid.New()
	cliMustExec(t, f.pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,claim_generation,report_only,budget_wall_seconds,issue_title,issue_description)
VALUES($1,$2,$3,$4,'cross_check',$5,$6,$7,1,true,1800,'checker','candidate')`, child, f.owner, repo, c.childWorker, lead, c.childHarness, c.childStatus)
	cliMustExec(t, f.pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,base_commit,head_commit,candidate_digest,verdict,outcome,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,created_at,deadline_at,interrupted_at)
VALUES($1,$2,$3,'code',1,$4,repeat('a',40),repeat('b',40),$5,'failed',$6,'{}','','',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),now()-interval '30 minutes',now()+$7::interval,CASE WHEN $8::boolean THEN now() ELSE NULL END)`,
		lead, child, c.checkerHarnes, c.gen, make([]byte, 32), c.outcome, c.deadline, c.interrupted)
}

// codeLead makes lead eligible for a code cross-check (claude harness, running, required).
func (f *ccListFixture) codeEligible(t *testing.T, lead uuid.UUID) {
	t.Helper()
	cliMustExec(t, f.pool, `UPDATE runs SET code_cross_check_required=true WHERE id=$1`, lead)
}

func (f *ccListFixture) planEligible(t *testing.T, lead uuid.UUID) {
	t.Helper()
	cliMustExec(t, f.pool, `UPDATE runs SET plan_cross_check_required=true WHERE id=$1`, lead)
}

// parity reads the two health probes for the same run, with the database's own now() as the
// code probe's ServerNow. The plan probe has no kind guard (health.go applies
// runkind.PlanCrossCheckable in Go), so the same guard is applied here.
func (f *ccListFixture) parity(t *testing.T, lead uuid.UUID, kind string) (plan, code bool) {
	t.Helper()
	var now time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	live, err := f.q.HasLivePlanCrossCheck(t.Context(), lead)
	if err != nil {
		t.Fatalf("HasLivePlanCrossCheck: %v", err)
	}
	_, err = f.q.GetLiveCodeCrossCheckDeadline(t.Context(), store.GetLiveCodeCrossCheckDeadlineParams{
		LeadRunID: lead, ServerNow: pgtype.Timestamptz{Time: now, Valid: true}})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetLiveCodeCrossCheckDeadline: %v", err)
	}
	return live && runkind.PlanCrossCheckable(kind), err == nil
}

// ccActiveByID decodes cross_check_active per row id; the key must be present (null or string)
// on EVERY row, since the field is always sent.
func ccActiveByID(t *testing.T, label string, b liveListBody) map[string]*string {
	t.Helper()
	out := make(map[string]*string, len(b.Runs))
	for i, row := range b.Runs {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatalf("%s row %d id: %v", label, i, err)
		}
		raw, ok := row["cross_check_active"]
		if !ok {
			t.Fatalf("%s row %d (%s) lacks the always-sent cross_check_active key", label, i, id)
		}
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s row %d cross_check_active %s: %v", label, i, raw, err)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("%s lists run %s more than once", label, id)
		}
		out[id] = v
	}
	return out
}

func ccStr(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

func TestRunListCrossCheckActiveStatesLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	f := newCCListFixture(t, pool)
	adminID := cliSeedUser(t, pool, true)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, adminID) })

	type tc struct {
		name     string
		kind     string
		status   string
		want     string // "" is JSON null
		terminal bool
		seed     func(lead uuid.UUID)
	}
	plan := func(lead uuid.UUID, gen int64, rounds ...ccPlanRound) {
		f.planEligible(t, lead)
		f.planRounds(t, lead, gen, rounds...)
	}
	livePending := ccPlanRound{"pending", ccLive}
	code := func(lead uuid.UUID, mut func(*ccCode)) {
		f.codeEligible(t, lead)
		c := f.codeDefaults()
		if mut != nil {
			mut(&c)
		}
		f.codeCheck(t, lead, c)
	}
	cases := []tc{
		{name: "plan live", kind: "issue", status: "running", want: "plan", seed: func(l uuid.UUID) { plan(l, 1, livePending) }},
		{name: "plan live claimed", kind: "prompt", status: "claimed", want: "plan", seed: func(l uuid.UUID) { plan(l, 1, livePending) }},
		{name: "plan live gated only", kind: "issue", status: "running", want: "plan", seed: func(l uuid.UUID) {
			cliMustExec(t, pool, `UPDATE runs SET plan_cross_check_gated=true WHERE id=$1`, l)
			f.planRounds(t, l, 1, livePending)
		}},
		{name: "plan several rounds, latest live", kind: "ci_fix", status: "running", want: "plan", seed: func(l uuid.UUID) {
			plan(l, 1, ccPlanRound{"revise", ccPast}, ccPlanRound{"revise", ccPast}, livePending)
		}},
		{name: "plan several rounds, latest decided", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			plan(l, 1, ccPlanRound{"revise", ccPast}, ccPlanRound{"approve", ccLive})
		}},
		{name: "plan verdict recorded", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) { plan(l, 1, ccPlanRound{"approve", ccLive}) }},
		{name: "plan past deadline", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) { plan(l, 1, ccPlanRound{"pending", ccPast}) }},
		{name: "plan terminal lead", kind: "issue", status: "completed", want: "", terminal: true, seed: func(l uuid.UUID) { plan(l, 1, livePending) }},
		{name: "plan awaiting approval lead", kind: "issue", status: "awaiting_approval", want: "", seed: func(l uuid.UUID) { plan(l, 1, livePending) }},
		{name: "plan stale claim generation", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			plan(l, 1, livePending)
			cliMustExec(t, pool, `UPDATE runs SET claim_generation=2 WHERE id=$1`, l)
		}},
		{name: "plan claim released", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			plan(l, 1, livePending)
			cliMustExec(t, pool, `UPDATE runs SET claim_released_at=now() WHERE id=$1`, l)
		}},
		{name: "plan not required", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) { f.planRounds(t, l, 1, livePending) }},
		{name: "plan on a kind without a plan gate", kind: "task", status: "running", want: "", seed: func(l uuid.UUID) { plan(l, 1, livePending) }},

		{name: "code live", kind: "issue", status: "running", want: "code", seed: func(l uuid.UUID) { code(l, nil) }},
		{name: "code live on a task", kind: "task", status: "running", want: "code", seed: func(l uuid.UUID) { code(l, nil) }},
		{name: "code verdict recorded", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) { code(l, func(c *ccCode) { c.outcome = "completed" }) }},
		{name: "code past deadline", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) { code(l, func(c *ccCode) { c.deadline = ccPast }) }},
		{name: "code interrupted", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) { code(l, func(c *ccCode) { c.interrupted = true }) }},
		{name: "code terminal lead", kind: "issue", status: "completed", want: "", terminal: true, seed: func(l uuid.UUID) { code(l, nil) }},
		{name: "code stale claim generation", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			code(l, nil)
			cliMustExec(t, pool, `UPDATE runs SET claim_generation=2 WHERE id=$1`, l)
		}},
		{name: "code not required", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			code(l, nil)
			cliMustExec(t, pool, `UPDATE runs SET code_cross_check_required=false WHERE id=$1`, l)
		}},
		{name: "code report only lead", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			code(l, nil)
			cliMustExec(t, pool, `UPDATE runs SET report_only=true WHERE id=$1`, l)
		}},
		{name: "code lead judged not code", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			code(l, nil)
			cliMustExec(t, pool, `UPDATE runs SET fix_verdict='not_code' WHERE id=$1`, l)
		}},
		{name: "code child on the wrong worker", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			code(l, func(c *ccCode) { c.childWorker = f.otherWorker })
		}},
		{name: "code child on the same harness", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			code(l, func(c *ccCode) { c.childHarness, c.checkerHarnes = "claude", "claude" })
		}},
		{name: "code terminal child", kind: "issue", status: "running", want: "", seed: func(l uuid.UUID) {
			code(l, func(c *ccCode) { c.childStatus = "completed" })
		}},
		{name: "plan and code both live", kind: "issue", status: "running", want: "plan", seed: func(l uuid.UUID) {
			plan(l, 1, livePending)
			code(l, nil)
		}},
		{name: "plan decided and code live", kind: "issue", status: "running", want: "code", seed: func(l uuid.UUID) {
			plan(l, 1, ccPlanRound{"approve", ccLive})
			code(l, nil)
		}},
	}

	ids := make([]uuid.UUID, len(cases))
	for i, c := range cases {
		ids[i] = f.lead(t, c.kind, c.status)
		c.seed(ids[i])
	}

	// Parity with the health probes for the same run, and the expected value itself.
	for i, c := range cases {
		plan, code := f.parity(t, ids[i], c.kind)
		switch c.want {
		case "plan":
			if !plan {
				t.Errorf("%s: HasLivePlanCrossCheck says not live but the case expects plan", c.name)
			}
		case "code":
			if plan || !code {
				t.Errorf("%s: probes plan=%v code=%v, want code only", c.name, plan, code)
			}
		default:
			if plan || code {
				t.Errorf("%s: probes plan=%v code=%v, want neither", c.name, plan, code)
			}
		}
	}

	ownerTok := cliMintToken(t, pool, f.owner, clitoken.ScopeUser)
	adminTok := cliMintToken(t, pool, adminID, clitoken.ScopeAdminRO)
	for _, view := range []string{"", "?view=summary"} {
		for _, surface := range []struct{ name, path, tok string }{
			{"personal", "/api/runs", ownerTok},
			{"admin", "/api/admin/runs", adminTok},
		} {
			label := surface.name + view
			got := ccActiveByID(t, label, liveList(t, router, surface.path+view, surface.tok))
			for i, c := range cases {
				v, present := got[ids[i].String()]
				if surface.name == "admin" && c.terminal {
					if present {
						t.Errorf("%s %q: terminal run unexpectedly in the active list", label, c.name)
					}
					continue
				}
				if !present {
					t.Errorf("%s %q: run missing from the list", label, c.name)
					continue
				}
				if ccStr(v) != map[bool]string{true: c.want, false: "null"}[c.want != ""] {
					t.Errorf("%s %q: cross_check_active = %s, want %q", label, c.name, ccStr(v), c.want)
				}
			}
		}
	}
}

// ccBoundaryRuns bulk-inserts queued issue runs for owner under repo with created_at
// base + n seconds (n = 1..len(ids)) and returns nothing; ids[n-1] is run n.
func ccBoundaryRuns(t *testing.T, pool *pgxpool.Pool, owner, repo uuid.UUID, ids []uuid.UUID, createdAt string) {
	t.Helper()
	cliMustExec(t, pool, fmt.Sprintf(`INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind,created_at)
SELECT s.id,$1,$2,s.n,'Boundary','d','queued','issue',%s
FROM unnest($3::uuid[]) WITH ORDINALITY AS s(id,n)`, createdAt), owner, repo, ids)
}

func ccNewIDs(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}
	return ids
}

// ccAssertRowOrder compares the listed ids with the expected ordered ids and reports only the
// first mismatch.
func ccAssertRowOrder(t *testing.T, label string, b liveListBody, want []uuid.UUID) {
	t.Helper()
	if len(b.Runs) != len(want) {
		t.Errorf("%s: %d rows, want exactly %d", label, len(b.Runs), len(want))
	}
	for i, row := range b.Runs {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatalf("%s row %d id: %v", label, i, err)
		}
		if i >= len(want) || id != want[i].String() {
			t.Errorf("%s: first order mismatch at row %d: got %s", label, i, id)
			return
		}
	}
}

func TestRunListCrossCheckActiveLimitBoundaryLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	t.Run("personal list keeps the newest 200", func(t *testing.T) {
		f := newCCListFixture(t, pool)
		f.project++
		repo := rmSeedRepo(t, pool, f.conn, f.project, true)
		// n = 1 is the oldest (now - 201s), n = 201 the newest (now - 1s).
		ids := ccNewIDs(201)
		ccBoundaryRuns(t, pool, f.owner, repo, ids, `now() - (202 - s.n) * interval '1 second'`)
		multi, codeLead := ids[190], ids[195]
		cliMustExec(t, pool, `UPDATE runs SET status='running',kind='issue',worker_id=$2,harness='claude',claim_generation=1 WHERE id = ANY($1)`, []uuid.UUID{multi, codeLead}, f.worker)
		f.planEligible(t, multi)
		f.planRounds(t, multi, 1, ccPlanRound{"revise", ccPast}, ccPlanRound{"revise", ccPast}, ccPlanRound{"pending", ccLive})
		f.codeEligible(t, codeLead)
		f.codeCheck(t, codeLead, f.codeDefaults())

		want := make([]uuid.UUID, 0, 200)
		for i := len(ids) - 1; i >= 1; i-- { // newest first; ids[0], the oldest, is the excluded 201st
			want = append(want, ids[i])
		}
		tok := cliMintToken(t, pool, f.owner, clitoken.ScopeUser)
		for _, view := range []string{"", "?view=summary"} {
			b := liveList(t, router, "/api/runs"+view, tok)
			ccAssertRowOrder(t, "personal"+view, b, want)
			got := ccActiveByID(t, "personal"+view, b)
			if _, in := got[ids[0].String()]; in {
				t.Errorf("personal%s: the oldest (201st) run must be excluded", view)
			}
			if v := got[multi.String()]; ccStr(v) != "plan" {
				t.Errorf("personal%s: several-round plan run = %s, want plan exactly once", view, ccStr(v))
			}
			if v := got[codeLead.String()]; ccStr(v) != "code" {
				t.Errorf("personal%s: code run = %s, want code", view, ccStr(v))
			}
		}
	})

	t.Run("admin list keeps the newest 500", func(t *testing.T) {
		f := newCCListFixture(t, pool)
		adminID := cliSeedUser(t, pool, true)
		t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, adminID) })
		f.project++
		repo := rmSeedRepo(t, pool, f.conn, f.project, true)
		// Clearly in the future so no other test's non-terminal run can be newer.
		// n = 1 is the oldest of the seeded rows, n = 501 the newest.
		ids := ccNewIDs(501)
		ccBoundaryRuns(t, pool, f.owner, repo, ids, `now() + interval '1 day' + s.n * interval '1 second'`)
		multi, codeLead := ids[300], ids[400]
		cliMustExec(t, pool, `UPDATE runs SET status='running',kind='issue',worker_id=$2,harness='claude',claim_generation=1 WHERE id = ANY($1)`, []uuid.UUID{multi, codeLead}, f.worker)
		f.planEligible(t, multi)
		f.planRounds(t, multi, 1, ccPlanRound{"revise", ccPast}, ccPlanRound{"revise", ccPast}, ccPlanRound{"pending", ccLive})
		f.codeEligible(t, codeLead)
		f.codeCheck(t, codeLead, f.codeDefaults())

		want := make([]uuid.UUID, 0, 500)
		for i := len(ids) - 1; i >= 1; i-- {
			want = append(want, ids[i])
		}
		tok := cliMintToken(t, pool, adminID, clitoken.ScopeAdminRO)
		for _, view := range []string{"", "?view=summary"} {
			b := liveList(t, router, "/api/admin/runs"+view, tok)
			ccAssertRowOrder(t, "admin"+view, b, want)
			got := ccActiveByID(t, "admin"+view, b)
			if _, in := got[ids[0].String()]; in {
				t.Errorf("admin%s: the 501st run must be excluded", view)
			}
			if v := got[multi.String()]; ccStr(v) != "plan" {
				t.Errorf("admin%s: several-round plan run = %s, want plan exactly once", view, ccStr(v))
			}
			if v := got[codeLead.String()]; ccStr(v) != "code" {
				t.Errorf("admin%s: code run = %s, want code", view, ccStr(v))
			}
		}
	})
}
