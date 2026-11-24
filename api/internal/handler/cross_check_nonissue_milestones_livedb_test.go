package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4555 over the real worker router: a non-issue lead's plan cross-check candidate is
// admitted whatever its milestones, an issue lead keeps its candidate_invalid rejection, and a
// failure to resolve the lead kind answers the cross-check refusal rather than a 400.

type ccNIRouter struct {
	h      *Handler
	lead   uuid.UUID
	submit func(milestones string) *httptest.ResponseRecorder
	cc     func() (rows, children int, milestones string)
}

func ccNIRouterFixture(t *testing.T, kind string) ccNIRouter {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	h.wsvc = workersvc.New(h.q, h.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), PlanCrossCheckMaxRevisions: 2})
	h.wsvc.SetVault(h.vault)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 45550000+int64(uuid.New().ID()%1000000), true)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	switch kind {
	case "mr_rework":
		cliMustExec(t, pool, `UPDATE runs SET kind='mr_rework', pipeline_ref='agent/issue-7', mr_iid=7, target_run_id=id WHERE id=$1`, lead)
	case "task":
		cliMustExec(t, pool, `UPDATE runs SET kind='task', issue_iid=NULL, branch='uzi/task/ni' WHERE id=$1`, lead)
	case "issue":
	default:
		t.Fatalf("unknown kind %s", kind)
	}
	token := "nonissue-milestones-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	worker := uuid.New()
	caps := []string{"cross_check_v1", "cross_check_rounds_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "completion_interlock_v1", "codex_completion_interlock_v1", capability.CrossCheckOwnerSteeringV1}
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities,max_concurrent_runs) VALUES($1,$2,'nonissue',$3,'online',$4,8)`, worker, owner, hash[:], caps)
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2,harness='claude',auto_approve=true,plan_cross_check_required=true,claim_generation=1 WHERE id=$1`, lead, worker)
	sealed := mustSealT(ctx, t, h.box, []byte("nonissue-milestones-fixture-"+uuid.NewString()))
	secret := uuid.New()
	cliMustExec(t, pool, `UPDATE forge_connections SET token_ciphertext=$2 WHERE id=$1`, conn, sealed)
	cliMustExec(t, pool, `INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'openai_api_key','nonissue',true,$3,'master')`, secret, owner, sealed)
	if _, err := h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret, UserID: owner, Status: "static"}); err != nil {
		t.Fatal(err)
	}
	return ccNIRouter{
		h: h, lead: lead,
		submit: func(milestones string) *httptest.ResponseRecorder {
			t.Helper()
			candidate := workersvc.PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage(milestones), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
			round, gen := int32(1), int64(1)
			body, err := json.Marshal(planCrossCheckRequest{Stage: "plan", Round: &round, ClaimGeneration: &gen, PlanCrossCheckCandidate: candidate})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+lead.String()+"/cross-checks", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			return rec
		},
		cc: func() (rows, children int, ms string) {
			t.Helper()
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
				(SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),
				coalesce((SELECT milestones::text FROM cross_checks WHERE lead_run_id=$1 AND round=1), '')`, lead).Scan(&rows, &children, &ms); err != nil {
				t.Fatal(err)
			}
			return rows, children, ms
		},
	}
}

func ccNIReason(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return body.Error, body.Reason
}

var ccNIMalformed = []string{`[{"id":"","title":""}]`, `[{"id":"m1","title":"t","extra":1}]`, `[{"id":"m1"}]`}

// H1: a non-issue lead with malformed milestones is admitted and stores canonical [].
func TestCrossCheckNonIssueMilestonesNonIssueAdmittedLiveDB(t *testing.T) {
	for _, kind := range []string{"mr_rework", "task"} {
		for _, bad := range ccNIMalformed {
			t.Run(kind+" "+bad, func(t *testing.T) {
				f := ccNIRouterFixture(t, kind)
				rec := f.submit(bad)
				if rec.Code != http.StatusOK {
					t.Fatalf("non-issue malformed: %d %s", rec.Code, rec.Body.String())
				}
				rows, children, ms := f.cc()
				if rows != 1 || children != 1 || ms != "[]" {
					t.Fatalf("rows=%d children=%d cc.milestones=%s, want 1/1/[]", rows, children, ms)
				}
			})
		}
	}
}

// H2: the issue control keeps the candidate_invalid rejection and creates nothing.
func TestCrossCheckNonIssueMilestonesIssueRejectedLiveDB(t *testing.T) {
	for _, bad := range ccNIMalformed {
		t.Run(bad, func(t *testing.T) {
			f := ccNIRouterFixture(t, "issue")
			rec := f.submit(bad)
			if _, reason := ccNIReason(t, rec); rec.Code != http.StatusBadRequest || reason != "candidate_invalid" {
				t.Fatalf("issue malformed: %d %s", rec.Code, rec.Body.String())
			}
			if rows, children, _ := f.cc(); rows != 0 || children != 0 {
				t.Fatalf("rejected submit created rows=%d children=%d", rows, children)
			}
		})
	}
}

// Only the lead-kind read fails; submission and worker authentication keep healthy stores.
type ccNILeadKindFaultDB struct {
	store.DBTX
	reads atomic.Int32
}

func (s *ccNILeadKindFaultDB) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.HasPrefix(query, "-- name: GetRunOwnedByWorker :one\n") && s.reads.Add(1) == 1 {
		return ccNILeadKindFaultRow{}
	}
	return s.DBTX.QueryRow(ctx, query, args...)
}

type ccNILeadKindFaultRow struct{}

func (ccNILeadKindFaultRow) Scan(...any) error {
	return errors.New("injected lead-kind lookup failure")
}

// H3: a lead-kind lookup fault must stop before SubmitPlanCrossCheck's owned-run read.
func TestCrossCheckNonIssueMilestonesKindLookupFaultLiveDB(t *testing.T) {
	f := ccNIRouterFixture(t, "mr_rework")
	fault := &ccNILeadKindFaultDB{DBTX: f.h.pool}
	svc := workersvc.New(store.New(fault), f.h.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), PlanCrossCheckMaxRevisions: 2})
	svc.SetVault(f.h.vault)
	svc.SetTxBeginner(f.h.pool)
	f.h.wsvc = svc
	rec := f.submit(`[{"id":"","title":""}]`)
	msg, reason := ccNIReason(t, rec)
	if rec.Code != http.StatusConflict || msg != "cross-check refused" {
		t.Fatalf("kind lookup fault: %d %s, want 409 cross-check refused", rec.Code, rec.Body.String())
	}
	if reads := fault.reads.Load(); reads != 1 {
		t.Fatalf("owned-run reads=%d, want only the failed lead-kind read (no submit)", reads)
	}
	t.Logf("kind lookup fault answered %d %q reason=%q", rec.Code, msg, reason)
	if rows, children, _ := f.cc(); rows != 0 || children != 0 {
		t.Fatalf("faulted submit created rows=%d children=%d", rows, children)
	}
	// A second request proves submission was not disabled by the injected lookup failure.
	if rec := f.submit(`[{"id":"","title":""}]`); rec.Code != http.StatusOK {
		t.Fatalf("healthy submit after lookup fault: %d %s", rec.Code, rec.Body.String())
	}
	if rows, children, ms := f.cc(); rows != 1 || children != 1 || ms != "[]" {
		t.Fatalf("healthy submit: rows=%d children=%d milestones=%s, want 1/1/[]", rows, children, ms)
	}
}

func TestCrossCheckNonIssueMilestonesMalformedRetryDispositionLiveDB(t *testing.T) {
	const legacy = `[{"id":"m1","title":"Legacy plan"}]`
	for _, kind := range []string{"mr_rework", "task"} {
		t.Run(kind, func(t *testing.T) {
			f := ccNIRouterFixture(t, kind)
			if rec := f.submit(legacy); rec.Code != http.StatusOK {
				t.Fatalf("seed attempt: %d %s", rec.Code, rec.Body.String())
			}
			// Model the immutable attempt stored before non-issue candidates dropped milestones.
			candidate := workersvc.PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage(legacy),
				RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
			for range 2 {
				var err error
				candidate, err = workersvc.NormalizePlanCrossCheckCandidate(candidate)
				if err != nil {
					t.Fatal(err)
				}
			}
			digest, err := candidate.Digest()
			if err != nil {
				t.Fatal(err)
			}
			cliMustExec(t, f.h.pool, `UPDATE cross_checks SET milestones=$2::jsonb, candidate_digest=$3 WHERE lead_run_id=$1 AND round=1`, f.lead, string(candidate.Milestones), digest)
			before, err := f.h.q.GetExactPlanCrossCheck(context.Background(), store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
			if err != nil {
				t.Fatal(err)
			}
			rec := f.submit(`[{"id":"m1"}]`)
			if msg, reason := ccNIReason(t, rec); rec.Code != http.StatusBadRequest || msg != "invalid cross-check candidate" || reason != "candidate_invalid" {
				t.Fatalf("malformed mismatching retry: %d %s, want 400 candidate_invalid", rec.Code, rec.Body.String())
			}
			if rec := f.submit(legacy); rec.Code != http.StatusOK {
				t.Fatalf("matching legacy retry: %d %s", rec.Code, rec.Body.String())
			}
			after, err := f.h.q.GetExactPlanCrossCheck(context.Background(), store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.Milestones, after.Milestones) || !bytes.Equal(before.CandidateDigest, after.CandidateDigest) {
				t.Fatal("retry changed the immutable attempt identity")
			}
			if rows, children, _ := f.cc(); rows != 1 || children != 1 {
				t.Fatalf("retries created rows=%d children=%d, want 1/1", rows, children)
			}
		})
	}
}

func TestCrossCheckNonIssueMilestonesMalformedCanonicalRetryLiveDB(t *testing.T) {
	f := ccNIRouterFixture(t, "mr_rework")
	for range 2 {
		if rec := f.submit(`[{"id":"m1"}]`); rec.Code != http.StatusOK {
			t.Fatalf("canonical retry: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rows, children, ms := f.cc(); rows != 1 || children != 1 || ms != "[]" {
		t.Fatalf("canonical retries: rows=%d children=%d milestones=%s, want 1/1/[]", rows, children, ms)
	}
}
