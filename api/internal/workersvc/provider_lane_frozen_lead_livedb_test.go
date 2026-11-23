package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Keep only generated SQL names and counts: never SQL arguments, credentials or DSNs.
// The dedicated pool traces both Service.q and every store.New(tx) query.
type frozenLeadVendorReads struct {
	mu    sync.Mutex
	names map[string]int
}

func (r *frozenLeadVendorReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	fields := strings.Fields(strings.SplitN(d.SQL, "\n", 2)[0])
	if len(fields) >= 3 && fields[0] == "--" && fields[1] == "name:" {
		r.mu.Lock()
		r.names[fields[2]]++
		r.mu.Unlock()
	}
	return ctx
}

func (*frozenLeadVendorReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *frozenLeadVendorReads) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = make(map[string]int)
}

func (r *frozenLeadVendorReads) vendor() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int)
	for name, n := range r.names {
		// Include metadata and availability, not just ciphertext. Credential-input
		// queries are included even if they do not mention a particular vendor.
		if strings.Contains(name, "Secret") || strings.Contains(name, "Credential") ||
			strings.Contains(name, "AnthropicToken") || name == "GetUserCodexAccountMode" ||
			name == "ListCodexPoolAliases" || name == "GetCodexProviderAccountSnapshot" || strings.Contains(name, "CodexAccount") {
			out[name] = n
		}
	}
	return out
}

func frozenLeadFixture(t *testing.T, stage string, engine Harness) (*provFix, store.Run, store.Worker, *frozenLeadVendorReads) {
	t.Helper()
	fx := newProvFix(t)
	fx.anthropic()
	fx.codexDefault()
	if engine == HarnessCodex {
		seedCodexSource(t, fx)
	} else {
		fx.addProvider(pvDefaultMap, true)
	}
	lead, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, h(engine), fx.insertAt(nil))
	if err != nil {
		t.Fatalf("freeze provider lead: %v", err)
	}
	if engine == HarnessCodex {
		wantCodexBinding(t, fx, lead, 1, fx.keyRev())
	} else {
		fx.wantBound(lead, 1, fx.keyRev())
	}
	// No Claim or provider capability advertisement: this is an unclaimed legal
	// frozen profile, put into the same storage custody used by the existing tests.
	fx.env.exec(`UPDATE users SET claude_provider_id=NULL,claude_provider_scope_id=NULL,codex_provider_id=NULL WHERE id=$1`, fx.userID)
	sources, err := fx.env.q.GetUserProviderLaneSources(fx.env.ctx, fx.userID)
	if err != nil || sources.ClaudeProviderID.Valid || sources.CodexProviderID.Valid {
		t.Fatalf("both current provider sources must be NULL: %+v err=%v", sources, err)
	}
	fx.env.exec(`UPDATE workers SET max_cross_check_slots=1,last_heartbeat_at=now(),
		protocol_capabilities=ARRAY['cross_check_v1','cross_check_rounds_v1','cross_check_codex_lead_v1',
		'cross_check_lane_v1','cross_check_code_v1','codex_harness_v1','codex_runtime_v2','codex_custom_model_v1']
		WHERE id=$1`, fx.workerID)
	reads := &frozenLeadVendorReads{}
	reads.reset()
	pool, err := store.OpenPool(fx.env.ctx, fx.env.pool.Config().ConnString(), store.WithQueryTracer(reads))
	if err != nil {
		t.Fatal("open traced pool")
	}
	t.Cleanup(pool.Close)
	fx.svc.q = store.New(pool)
	fx.svc.SetTxBeginner(pool)
	frozenLeadEnroll(fx, lead.ID, stage)
	return fx, lead, wkrRow(t, fx.env, fx.workerID), reads
}

func frozenLeadEnroll(fx *provFix, id uuid.UUID, stage string) {
	fx.env.exec(`UPDATE runs SET status='running',worker_id=$2,claim_generation=1,
		auto_approve=$3,plan_cross_check_required=$3,code_cross_check_required=$4
		WHERE id=$1`, id, fx.workerID, stage == "plan", stage == "code")
}

func frozenLeadSubmit(fx *provFix, w store.Worker, id uuid.UUID, stage string) (store.CrossCheck, error) {
	if stage == "code" {
		return fx.svc.SubmitCodeCrossCheck(fx.env.ctx, w, id, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
	}
	return fx.svc.SubmitPlanCrossCheck(fx.env.ctx, w, id, 1, PlanCrossCheckCandidate{
		PlanMd: "plan", Milestones: []byte("[]"), RequiredCapabilities: []string{},
		RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff",
	})
}

func frozenLeadNativeControl(t *testing.T, fx *provFix, w store.Worker, reads *frozenLeadVendorReads, stage string, engine Harness) (store.Run, store.CrossCheck) {
	t.Helper()
	native, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, h(engine), fx.insertAt(nil))
	if err != nil {
		t.Fatalf("create native control: %v", err)
	}
	fx.wantVendor(native)
	frozenLeadEnroll(fx, native.ID, stage)
	reads.reset()
	cc, err := frozenLeadSubmit(fx, w, native.ID, stage)
	if err != nil || !cc.CheckerRunID.Valid || (stage == "code" && cc.Outcome.String != "pending") {
		t.Fatalf("native %s control must create checker: checker=%v err=%v", stage, cc.CheckerRunID.Valid, err)
	}
	child := mustRun(t, fx.env, uuid.UUID(cc.CheckerRunID.Bytes))
	opposite := "codex"
	availability := "CountEnabledCodexSecrets"
	if engine == HarnessCodex {
		opposite = "claude"
		availability = "UserHasEnabledAnthropicToken"
	}
	counts := reads.vendor()
	if child.Harness != opposite || counts[availability] == 0 {
		t.Fatalf("native control: harness=%s want=%s; vendor query counts=%v (require %s)", child.Harness, opposite, counts, availability)
	}
	t.Logf("native %s/%s control vendor query counts=%v", stage, engine, counts)
	return native, cc
}

// A cleared current source must not reclassify a frozen provider lead or let
// checker credential usability run before the frozen-profile refusal.
func TestProviderCheckerFrozenLeadBeforeVendorLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, engine := range []Harness{HarnessClaude, HarnessCodex} {
			t.Run(stage+"/"+string(engine), func(t *testing.T) {
				fx, lead, w, reads := frozenLeadFixture(t, stage, engine)
				frozenLeadNativeControl(t, fx, w, reads, stage, engine)
				before := fx.runCount()
				reads.reset()
				cc, err := frozenLeadSubmit(fx, w, lead.ID, stage)
				if stage == "plan" {
					if !errors.Is(err, ErrCrossCheckUnavailable) {
						t.Errorf("plan submit err=%v, want ErrCrossCheckUnavailable", err)
					}
					// The schema uses verdict=failed as its no-checker sentinel; no checker decided it.
				} else if err != nil || cc.Outcome.String != "failed" || cc.ReasonClass.String != "checker_unavailable" ||
					cc.CheckerRunID.Valid || cc.Verdict != "failed" {
					t.Errorf("code refusal: outcome=%q reason=%q checker=%v verdict=%q err=%v", cc.Outcome.String, cc.ReasonClass.String, cc.CheckerRunID.Valid, cc.Verdict, err)
				}
				if counts := reads.vendor(); len(counts) != 0 {
					t.Errorf("frozen provider lead read vendor usability/credential inputs before refusal: %v", counts)
				}
				if fx.runCount() != before {
					t.Error("provider refusal created a checker run")
				}
				got := mustRun(t, fx.env, lead.ID)
				if bindingOfRun(got) != bindingOfRun(lead) || got.Harness != string(engine) ||
					got.AnthropicSecretID.Valid || got.CodexSecretID.Valid {
					t.Fatal("source clearing or refusal reclassified frozen provider lead")
				}
				if stage == "plan" {
					gen, reason, plan, size := int64(1), "checker_unavailable", "A plan that needed a checker", "s"
					caps, tools, ms := []string{}, []string{}, []Milestone{}
					_, applied, e := fx.svc.SetState(fx.env.ctx, w, lead.ID, StateRequest{
						State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
						RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PlanCrossCheckGateReason: &reason,
					})
					got = mustRun(t, fx.env, lead.ID)
					if e != nil || !applied || got.Status != "awaiting_approval" || got.AutoApprove ||
						got.PlanCrossCheckGateReason.String != reason {
						t.Fatalf("human gate: applied=%v status=%s auto=%v reason=%q err=%v", applied, got.Status, got.AutoApprove, got.PlanCrossCheckGateReason.String, e)
					}
				}
			})
		}
	}
}

// Retries recover real immutable attempts before reconsidering the owner's newly
// selected provider source. No fabricated checker row or provider claim is used.
func TestProviderCheckerImmutableRetryAfterSourceEditLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, engine := range []Harness{HarnessClaude, HarnessCodex} {
			t.Run(stage+"/"+string(engine), func(t *testing.T) {
				fx, _, w, reads := frozenLeadFixture(t, stage, engine)
				native, prior := frozenLeadNativeControl(t, fx, w, reads, stage, engine)
				// The legacy Claude selection blocks any new checker family.
				fx.addProvider(pvDefaultMap, true)
				reads.reset()
				retry, err := frozenLeadSubmit(fx, w, native.ID, stage)
				if err != nil || retry.ID != prior.ID || retry.CheckerRunID != prior.CheckerRunID {
					t.Fatalf("immutable retry changed attempt: id=%s checker=%v err=%v", retry.ID, retry.CheckerRunID, err)
				}
				if counts := reads.vendor(); len(counts) != 0 {
					t.Errorf("immutable retry read vendor inputs: %v", counts)
				}
				fx.wantVendor(mustRun(t, fx.env, native.ID))
			})
		}
	}
}
