package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type summaryHandlerStore struct {
	*runsStore
	row   store.GetLatestPlanCrossCheckSummaryRow
	err   error
	calls int
	arg   store.GetLatestPlanCrossCheckSummaryParams
}

func (q *summaryHandlerStore) GetLatestPlanCrossCheckSummary(_ context.Context, arg store.GetLatestPlanCrossCheckSummaryParams) (store.GetLatestPlanCrossCheckSummaryRow, error) {
	q.calls++
	q.arg = arg
	return q.row, q.err
}

func TestPlanCrossCheckSummaryOverlayOwnerOnly(t *testing.T) {
	owner, other, runID := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name   string
		viewer uuid.UUID
		err    error
		want   bool
		calls  int
	}{
		{"owner", owner, nil, true, 1},
		{"otherOwner", other, nil, false, 0},
		{"queryFailure", owner, errors.New("private query error"), false, 1},
		{"noCheck", owner, pgx.ErrNoRows, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := store.Run{ID: runID, UserID: owner, Status: "completed"}
			q := &summaryHandlerStore{runsStore: &runsStore{ownerID: owner, run: run}, row: store.GetLatestPlanCrossCheckSummaryRow{Round: 1, Verdict: "approve"}, err: tc.err}
			h := newRunsHandler(t, q)
			var dto apitypes.RunDTO
			h.overlayPlanCrossCheckSummary(t.Context(), tc.viewer, run, &dto)
			if (dto.PlanCrossCheckSummary != nil) != tc.want || q.calls != tc.calls {
				t.Fatalf("owner overlay: %+v calls=%d", dto.PlanCrossCheckSummary, q.calls)
			}
			if q.calls > 0 && (q.arg.UserID != owner || q.arg.LeadRunID != runID) {
				t.Fatal("incorrect query scope")
			}
		})
	}
}

func TestPlanCrossCheckSummaryGetRunSeam(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	for _, tc := range []struct {
		name        string
		viewer      store.User
		wantStatus  int
		wantSummary bool
	}{
		{"owner", owner, http.StatusOK, true},
		{"adminOwner", store.User{ID: owner.ID, IsAdmin: true}, http.StatusOK, true},
		{"otherOwner", store.User{ID: uuid.New()}, http.StatusNotFound, false},
		{"adminOtherOwner", store.User{ID: uuid.New(), IsAdmin: true}, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &summaryHandlerStore{
				runsStore: &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "completed"}},
				row:       store.GetLatestPlanCrossCheckSummaryRow{Round: 1, Verdict: "approve"},
			}
			h := newRunsHandler(t, q)
			rec := httptest.NewRecorder()
			h.GetRun(rec, runReq(tc.viewer, runID))
			if rec.Code != tc.wantStatus {
				t.Fatalf("GetRun status=%d body=%s", rec.Code, rec.Body.String())
			}
			var response struct {
				Run apitypes.RunDTO `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if (response.Run.PlanCrossCheckSummary != nil) != tc.wantSummary {
				t.Fatalf("GetRun summary present=%v want=%v; main must call overlayPlanCrossCheckSummary after runToDTO", response.Run.PlanCrossCheckSummary != nil, tc.wantSummary)
			}
			if tc.wantSummary && (q.calls != 1 || q.arg.UserID != owner.ID || q.arg.LeadRunID != runID) {
				t.Fatalf("GetRun query scope: calls=%d arg=%+v", q.calls, q.arg)
			}
			if !tc.wantSummary && q.calls != 0 {
				t.Fatal("nonowner queried private metadata")
			}
		})
	}
}

// Each fixture has its own owner/repo and removes only those users at cleanup.
func summaryLiveFixture(t *testing.T) (*Handler, http.Handler, *pgxpool.Pool, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner) })
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), 2149, true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	child := uuid.New()
	cliMustExec(t, pool, `UPDATE runs SET harness='claude',claim_generation=1 WHERE id=$1`, lead)
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','private issue context')`, child, owner, repo, lead)
	cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,checker_harness,checker_model,checker_effort,verdict,reason_class,findings,deadline_at)
VALUES($1,$2,'plan',1,1,'private candidate','[{"id":"m1","title":"one"}]',ARRAY['a','b'],ARRAY['git','go'],'s',repeat('a',40),'private diff',$3,'codex','recorded-model','high','approve','approve','{"summary":"ok","items":[]}',now()+interval '30 minutes')`, lead, child, []byte("digest"))
	return h, router, pool, owner, lead, child
}

func TestPlanCrossCheckSummaryQueryLiveDB(t *testing.T) {
	t.Run("humanRevisionWithUnchangedCandidate", func(t *testing.T) {
		h, _, pool, owner, lead, _ := summaryLiveFixture(t)
		cliMustExec(t, pool, `UPDATE runs SET status='awaiting_approval',gate_revision=1,plan_md='private candidate',milestones_candidate='[{"id":"m1","title":"one"}]',milestones_frozen='[{"id":"m1","title":"one"}]',required_capabilities=ARRAY['a','b'],required_tools=ARRAY['git','go'],size_class='s' WHERE id=$1`, lead)
		read := func() *apitypes.PlanCrossCheckSummaryDTO {
			t.Helper()
			got, err := h.wsvc.PlanCrossCheckSummary(t.Context(), owner, lead)
			if err != nil || got == nil {
				t.Fatalf("actual query: %+v %v", got, err)
			}
			return got
		}
		snapshot := func() (string, int32, int64) {
			t.Helper()
			var candidate string
			var revisions int32
			var gateRevision int64
			err := pool.QueryRow(t.Context(), `SELECT jsonb_build_array(plan_md,milestones_candidate,milestones_frozen,required_capabilities,required_tools,size_class,claim_generation)::text,revise_count,gate_revision FROM runs WHERE id=$1`, lead).Scan(&candidate, &revisions, &gateRevision)
			if err != nil {
				t.Fatal(err)
			}
			return candidate, revisions, gateRevision
		}
		before, revisionsBefore, gateRevision := snapshot()
		if revisionsBefore != 0 || read().Historical {
			t.Fatal("current gate without a human revision must not be historical")
		}
		input, err := store.New(pool).CreateRunReviseInputIfUnderCap(t.Context(), store.CreateRunReviseInputIfUnderCapParams{
			RunID:                lead,
			Body:                 pgtype.Text{String: "Reconsider this plan", Valid: true},
			MaxRevisions:         1,
			ExpectedGateRevision: pgtype.Int8{Int64: gateRevision, Valid: true},
		})
		if err != nil {
			t.Fatalf("enqueue human revision: %v", err)
		}
		if input.RunID != lead || input.Kind != "revise_plan" || !input.GateRevision.Valid || input.GateRevision.Int64 != gateRevision {
			t.Fatalf("revision must target the current gate: %+v", input)
		}
		after, revisionsAfter, gateRevisionAfter := snapshot()
		if after != before || revisionsAfter != revisionsBefore+1 || gateRevisionAfter != gateRevision {
			t.Fatalf("revision changed candidate or gate: before=%s after=%s revisions=%d->%d gate=%d->%d", before, after, revisionsBefore, revisionsAfter, gateRevision, gateRevisionAfter)
		}
		if !read().Historical {
			t.Fatal("earlier checker findings must be historical after a human revision with unchanged candidate and claim generation")
		}
	})
	h, _, pool, owner, lead, child := summaryLiveFixture(t)
	read := func() *apitypes.PlanCrossCheckSummaryDTO {
		t.Helper()
		got, err := h.wsvc.PlanCrossCheckSummary(t.Context(), owner, lead)
		if err != nil || got == nil {
			t.Fatalf("actual query: %+v %v", got, err)
		}
		return got
	}
	if got := read(); got.Historical || got.CheckerRunID == nil || *got.CheckerRunID != child.String() || got.Usage != nil {
		t.Fatalf("upcoming candidate: %+v", got)
	}
	if got, err := h.wsvc.PlanCrossCheckSummary(t.Context(), uuid.New(), lead); err != nil || got != nil {
		t.Fatalf("foreign query: %+v %v", got, err)
	}
	cliMustExec(t, pool, `UPDATE runs SET plan_md='private candidate',milestones_frozen='[{"title":"one","id":"m1"}]',required_capabilities=ARRAY['b','a','a'],required_tools=ARRAY['go','git'],size_class='s' WHERE id=$1`, lead)
	if read().Historical {
		t.Fatal("semantic JSONB/set equality marked historical")
	}
	for _, change := range []struct{ name, sql, restore string }{
		{"plan", `UPDATE runs SET plan_md='human edit' WHERE id=$1`, `UPDATE runs SET plan_md='private candidate' WHERE id=$1`},
		{"milestones", `UPDATE runs SET milestones_frozen='[{"id":"m2","title":"two"}]' WHERE id=$1`, `UPDATE runs SET milestones_frozen='[{"title":"one","id":"m1"}]' WHERE id=$1`},
		{"capabilities", `UPDATE runs SET required_capabilities=ARRAY['c'] WHERE id=$1`, `UPDATE runs SET required_capabilities=ARRAY['b','a'] WHERE id=$1`},
		{"tools", `UPDATE runs SET required_tools=ARRAY['other'] WHERE id=$1`, `UPDATE runs SET required_tools=ARRAY['go','git'] WHERE id=$1`},
		{"size", `UPDATE runs SET size_class='l' WHERE id=$1`, `UPDATE runs SET size_class='s' WHERE id=$1`},
		{"generation", `UPDATE runs SET claim_generation=2 WHERE id=$1`, `UPDATE runs SET claim_generation=1 WHERE id=$1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			cliMustExec(t, pool, change.sql, lead)
			if !read().Historical {
				t.Fatal("changed candidate not historical")
			}
			cliMustExec(t, pool, change.restore, lead)
			if read().Historical {
				t.Fatal("unchanged candidate historical")
			}
		})
	}
	cliMustExec(t, pool, `UPDATE runs SET status='awaiting_approval',milestones_candidate='[{"title":"one","id":"m1"}]',milestones_frozen='[]' WHERE id=$1`, lead)
	if read().Historical {
		t.Fatal("awaiting approval must use candidate milestones")
	}
	for _, pair := range [][2]string{{"approve", "approve"}, {"revise", "revise"}, {"block", "block"}, {"failed", "malformed"}, {"failed", "model_error"}, {"failed", "model_timeout"}, {"failed", "checker_unavailable"}, {"failed", "confinement_failed"}, {"pending", ""}, {"failed", "timed_out"}, {"failed", "superseded"}} {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			var reason any
			var findings any
			if pair[1] != "" {
				reason = pair[1]
			}
			if pair[1] != "" && pair[1] != "timed_out" && pair[1] != "superseded" {
				findings = `{"summary":"actual result","items":[]}`
			}
			cliMustExec(t, pool, `UPDATE cross_checks SET verdict=$2,reason_class=$3,findings=$4 WHERE lead_run_id=$1`, lead, pair[0], reason, findings)
			got := read()
			if got.Verdict != pair[0] || (got.Findings != nil) != (findings != nil) {
				t.Fatalf("persisted outcome: %+v", got)
			}
			if reason != nil && (got.ReasonClass == nil || *got.ReasonClass != pair[1]) {
				t.Fatal("actual reason lost")
			}
		})
	}
	cliMustExec(t, pool, `UPDATE cross_checks SET verdict='approve',reason_class='approve',findings='{"summary":"retained","items":[]}' WHERE lead_run_id=$1`, lead)
	// Metered values across distinct models must come from the rollup view.
	for _, model := range []string{"model-a", "model-b"} {
		cliMustExec(t, pool, `INSERT INTO run_usage(run_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens,cost_usd,harness,cost_status) VALUES($1,$2,10,20,30,40,1.25,'codex','metered')`, child, model)
	}
	if got := read().Usage; got == nil || got.InputTokens != 20 || got.CacheReadTokens != 40 || got.CacheCreationTokens != 60 || got.OutputTokens != 80 || got.CostUSD != 2.5 || got.CostStatus != "metered" {
		t.Fatalf("rollup: %+v", got)
	}
	for _, status := range []string{"subscription", "unreported"} {
		cliMustExec(t, pool, `UPDATE run_usage SET cost_status=$2,cost_usd=0 WHERE run_id=$1`, child, status)
		if got := read().Usage; got == nil || got.CostStatus != status || got.CostUSD != 0 {
			t.Fatalf("cost marker: %+v", got)
		}
	}
	// Ordinary, foreign, and wrong-harness links cannot expose child usage.
	var repo uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT repo_id FROM runs WHERE id=$1`, lead).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	ordinary := rmSeedRun(t, pool, owner, repo, "completed")
	cliMustExec(t, pool, `UPDATE cross_checks SET checker_run_id=$2 WHERE lead_run_id=$1`, lead, ordinary)
	if got := read(); got.CheckerRunID != nil || got.Usage != nil {
		t.Fatal("ordinary link exposed")
	}
	cliMustExec(t, pool, `UPDATE cross_checks SET checker_run_id=$2,checker_harness='claude' WHERE lead_run_id=$1`, lead, child)
	if got := read(); got.CheckerRunID != nil || got.Usage != nil {
		t.Fatal("wrong harness exposed")
	}
	cliMustExec(t, pool, `UPDATE cross_checks SET checker_harness='codex' WHERE lead_run_id=$1`, lead)
	foreign := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, foreign) })
	cliMustExec(t, pool, `UPDATE runs SET user_id=$2 WHERE id=$1`, child, foreign)
	if got := read(); got.CheckerRunID != nil || got.Usage != nil {
		t.Fatal("foreign child exposed")
	}
	cliMustExec(t, pool, `UPDATE runs SET user_id=$2,target_run_id=$3 WHERE id=$1`, child, owner, ordinary)
	if got := read(); got.CheckerRunID != nil || got.Usage != nil {
		t.Fatal("wrong target child exposed")
	}
	cliMustExec(t, pool, `UPDATE runs SET target_run_id=$2 WHERE id=$1`, child, lead)
	cliMustExec(t, pool, `DELETE FROM runs WHERE id=$1`, child)
	got := read()
	if got.CheckerRunID != nil || got.Usage != nil || got.Findings == nil || *got.CheckerModel != "recorded-model" || *got.CheckerEffort != "high" {
		t.Fatalf("deleted checker: %+v", got)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private candidate", "private diff", "private issue context", "candidate_digest"} {
		if strings.Contains(string(out), private) {
			t.Fatalf("leak: %s", out)
		}
	}
	for _, n := range []int{512, 513} {
		cliMustExec(t, pool, `UPDATE cross_checks SET checker_model=$2 WHERE lead_run_id=$1`, lead, strings.Repeat("m", n))
		result, err := h.wsvc.PlanCrossCheckSummary(t.Context(), owner, lead)
		if err != nil || (result != nil) != (n == 512) {
			t.Fatalf("SQL metadata bound %d: %+v %v", n, result, err)
		}
	}
	cliMustExec(t, pool, `UPDATE cross_checks SET checker_model='recorded-model',findings=jsonb_build_object('summary',repeat('x',65536),'items','[]'::jsonb) WHERE lead_run_id=$1`, lead)
	if got, err := h.wsvc.PlanCrossCheckSummary(t.Context(), owner, lead); got != nil || err != nil {
		t.Fatalf("SQL findings transfer bound: %+v %v", got, err)
	}
	// Reading expiry is observational; settlement remains the lifecycle's job.
	cliMustExec(t, pool, `UPDATE cross_checks SET verdict='pending',reason_class=NULL,findings=NULL,deadline_at=now()-interval '1 minute' WHERE lead_run_id=$1`, lead)
	if got := read(); got.Verdict != "pending" || got.ReasonClass != nil || got.Findings != nil {
		t.Fatal("read invented expiry result")
	}
	cliMustExec(t, pool, `UPDATE runs SET status='running' WHERE id=$1`, lead)
	cliMustExec(t, pool, `UPDATE runs SET claim_generation=2 WHERE id=$1`, lead)
	if got := read(); got.Verdict != "failed" || got.ReasonClass == nil || *got.ReasonClass != "superseded" || !got.Historical {
		t.Fatalf("actual lifecycle supersession: %+v", got)
	}
}

func TestPlanCrossCheckSummaryOwnerGetRunLiveDB(t *testing.T) {
	_, router, pool, owner, lead, _ := summaryLiveFixture(t)
	jwt := cliMintJWT(t, pool, owner)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	for _, mode := range []string{"session", "cli"} {
		t.Run(mode, func(t *testing.T) {
			rec := cookieReq(t, router, http.MethodGet, "/api/runs/"+lead.String(), jwt, "")
			if mode == "cli" {
				rec = bearerReq(router, http.MethodGet, "/api/runs/"+lead.String(), token)
			}
			if rec.Code != 200 {
				t.Fatalf("owner route: %d %s", rec.Code, rec.Body.String())
			}
			var response struct {
				Run apitypes.RunDTO `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Run.ID != lead.String() {
				t.Fatal("incorrect run envelope")
			}
			if response.Run.PlanCrossCheckSummary == nil || response.Run.PlanCrossCheckSummary.Verdict != "approve" {
				t.Fatal("actual owner GetRun missing summary (main must bind overlay)")
			}
		})
	}
}

func TestPlanCrossCheckSummaryViewerGetRunLiveDB(t *testing.T) {
	h, router, pool, owner, lead, _ := summaryLiveFixture(t)
	for _, admin := range []bool{false, true} {
		viewer := cliSeedUser(t, pool, admin)
		t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, viewer) })
		rec := cookieReq(t, router, http.MethodGet, "/api/runs/"+lead.String(), cliMintJWT(t, pool, viewer), "")
		want := http.StatusNotFound
		if admin {
			want = http.StatusOK
		}
		if rec.Code != want || strings.Contains(rec.Body.String(), "plan_cross_check_summary") {
			t.Fatalf("nonowner admin=%v: %d %s", admin, rec.Code, rec.Body.String())
		}
		run, err := h.wsvc.GetRunForViewer(t.Context(), owner, false, lead)
		if err != nil {
			t.Fatal(err)
		}
		var dto apitypes.RunDTO
		h.overlayPlanCrossCheckSummary(t.Context(), viewer, run, &dto)
		if dto.PlanCrossCheckSummary != nil {
			t.Fatal("helper leaked summary to nonowner")
		}
	}
	var repo uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT repo_id FROM runs WHERE id=$1`, lead).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	ordinary := rmSeedRun(t, pool, owner, repo, "completed")
	rec := cookieReq(t, router, http.MethodGet, "/api/runs/"+ordinary.String(), cliMintJWT(t, pool, owner), "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "plan_cross_check_summary") {
		t.Fatalf("ordinary detail changed: %d %s", rec.Code, rec.Body.String())
	}
}
