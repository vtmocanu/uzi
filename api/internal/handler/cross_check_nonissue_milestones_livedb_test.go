package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
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
	caps := []string{"cross_check_v1", "cross_check_rounds_v1", "codex_harness_v1", "codex_runtime_v2", "completion_interlock_v1", "codex_completion_interlock_v1", capability.CrossCheckOwnerSteeringV1}
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

// H3: when the lead kind cannot be resolved (the workers service's database is gone) a
// malformed non-issue candidate answers the 409 cross-check refusal, not a 400 and not a 503.
// The worker-auth read goes through h.q on the healthy pool, so only the service's own pool is
// closed.
func TestCrossCheckNonIssueMilestonesKindLookupFaultLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	f := ccNIRouterFixture(t, "mr_rework")
	sep, err := store.OpenPool(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	svc := workersvc.New(store.New(sep), f.h.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), PlanCrossCheckMaxRevisions: 2})
	svc.SetVault(f.h.vault)
	svc.SetTxBeginner(sep)
	f.h.wsvc = svc
	sep.Close()
	rec := f.submit(`[{"id":"","title":""}]`)
	msg, reason := ccNIReason(t, rec)
	if rec.Code != http.StatusConflict || msg != "cross-check refused" {
		t.Fatalf("kind lookup fault: %d %s, want 409 cross-check refused", rec.Code, rec.Body.String())
	}
	t.Logf("kind lookup fault answered %d %q reason=%q", rec.Code, msg, reason)
	if rows, children, _ := f.cc(); rows != 0 || children != 0 {
		t.Fatalf("faulted submit created rows=%d children=%d", rows, children)
	}
}
