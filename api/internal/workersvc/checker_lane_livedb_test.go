package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// checker_lane_livedb_test.go is PRD #4574 M2's creation cutover against a real Postgres: the checker
// lane is decided once, under the shared secret-mutation lock taken BEFORE the lead row lock
// (R5), Provider freezes the owner's Codex source (never a vendor child, never the vendor freeze), the
// code stage fails fast on a lead worker without the provider checker capability, and the vendor lanes
// are unchanged on a worker without provider capabilities. Skipped unless UZI_TEST_DATABASE_URL points
// at a throwaway Postgres; run via ./e2e/run-store-it.sh. Every test name ends LiveDB.

var (
	clBaseCaps = []string{"cross_check_v1", "cross_check_rounds_v1", "cross_check_codex_lead_v1", "cross_check_lane_v1",
		"cross_check_code_v1", "codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1"}
	clProviderCaps = []string{"codex_runtime_v3", "codex_provider_v1"}
	clCheckerCap   = "codex_provider_checker_v1"
)

func clCaps(extra ...string) []string { return append(append([]string{}, clBaseCaps...), extra...) }

// clFix is a vendor-complete owner (Anthropic token and a usable vendor Codex subscription): an
// unintended vendor fallback would succeed and fail the test.
type clFix struct {
	*provFix
	claudeProvider, claudeKey uuid.UUID
	codexProvider, codexKey   uuid.UUID
}

func newCLFix(t *testing.T) *clFix {
	t.Helper()
	fx := newProvFix(t)
	fx.anthropic()
	fx.codexDefault()
	return &clFix{provFix: fx}
}

// selectClaude seeds and selects a Claude source.
func (fx *clFix) selectClaude() {
	fx.t.Helper()
	fx.addProvider(pvDefaultMap, true)
	fx.claudeProvider, fx.claudeKey = fx.provider, fx.key
}

// selectCodex seeds and selects a Codex source (seedCodexSource also sets the Codex default harness).
func (fx *clFix) selectCodex() {
	fx.t.Helper()
	seedCodexSource(fx.t, fx.provFix)
	fx.codexProvider, fx.codexKey = fx.provider, fx.key
}

func (fx *clFix) clearSources() {
	fx.env.exec(`UPDATE users SET claude_provider_id=NULL,claude_provider_scope_id=NULL,codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1`, fx.userID)
}

// lead creates a Claude lead on the owner's CURRENT sources (vendor when none is selected, provider
// bound when a Claude source is) and enrolls it for the stage on the fixture worker.
func (fx *clFix) lead(stage string, wantBound bool) store.Run {
	fx.t.Helper()
	run, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, h(HarnessClaude), fx.insertAt(nil))
	if err != nil {
		fx.t.Fatalf("create lead: %v", err)
	}
	if providerBoundRun(run) != wantBound {
		fx.t.Fatalf("lead bound = %t, want %t", providerBoundRun(run), wantBound)
	}
	frozenLeadEnroll(fx.provFix, run.ID, stage)
	return run
}

// worker sets the fixture worker's capabilities and returns its row.
func (fx *clFix) worker(caps []string) store.Worker {
	fx.t.Helper()
	fx.env.exec(`UPDATE workers SET max_cross_check_slots=1,last_heartbeat_at=now(),protocol_capabilities=$2 WHERE id=$1`, fx.workerID, caps)
	return wkrRow(fx.t, fx.env, fx.workerID)
}

func (fx *clFix) submit(w store.Worker, id uuid.UUID, stage string) (store.CrossCheck, error) {
	return frozenLeadSubmit(fx.provFix, w, id, stage)
}

func (fx *clFix) codexBinding() bindingParams {
	return bindingParams{
		ProviderID: pgtype.UUID{Bytes: fx.codexProvider, Valid: true}, Revision: pgtype.Int4{Int32: 1, Valid: true},
		KeyMode: pgtype.Text{String: "user_key", Valid: true}, KeySecret: pgtype.UUID{Bytes: fx.codexKey, Valid: true},
		KeyRev: pgtype.Int8{Int64: fx.keyRevOf(fx.codexKey), Valid: true},
	}
}

func (fx *clFix) keyRevOf(key uuid.UUID) int64 {
	fx.t.Helper()
	var rev int64
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT enablement_rev FROM user_secrets WHERE id=$1`, key).Scan(&rev); err != nil {
		fx.t.Fatalf("read key rev: %v", err)
	}
	return rev
}

// wantProviderChild asserts a checker child bound to the owner's Codex source with no vendor credential.
func (fx *clFix) wantProviderChild(cc store.CrossCheck, err error) store.Run {
	fx.t.Helper()
	if err != nil || !cc.CheckerRunID.Valid {
		fx.t.Fatalf("expected a provider checker child: checker=%v err=%v", cc.CheckerRunID.Valid, err)
	}
	child := mustRun(fx.t, fx.env, uuid.UUID(cc.CheckerRunID.Bytes))
	if child.Kind != "cross_check" || child.Harness != "codex" || bindingOfRun(child) != fx.codexBinding() ||
		child.CodexSecretID.Valid || child.CodexAuthMode.Valid || child.AnthropicSecretID.Valid || child.CredentialOverrideSecretID.Valid {
		fx.t.Fatalf("child = kind %s harness %s binding %+v codex=%v mode=%v, want a codex provider child bound to %+v with no vendor credential",
			child.Kind, child.Harness, bindingOfRun(child), child.CodexSecretID, child.CodexAuthMode, fx.codexBinding())
	}
	return child
}

// wantVendorChild asserts a vendor Codex checker child: all five binding columns NULL.
func (fx *clFix) wantVendorChild(cc store.CrossCheck, err error) {
	fx.t.Helper()
	if err != nil || !cc.CheckerRunID.Valid {
		fx.t.Fatalf("expected a vendor checker child: checker=%v err=%v", cc.CheckerRunID.Valid, err)
	}
	child := mustRun(fx.t, fx.env, uuid.UUID(cc.CheckerRunID.Bytes))
	if child.Harness != "codex" || bindingOfRun(child) != (bindingParams{}) {
		fx.t.Fatalf("child harness %s binding %+v, want a vendor codex child", child.Harness, bindingOfRun(child))
	}
}

// wantUnavailable asserts the stage's unavailable sink and that no checker child was created.
func (fx *clFix) wantUnavailable(stage string, cc store.CrossCheck, err error, runsBefore int) {
	fx.t.Helper()
	if stage == "plan" {
		if !errors.Is(err, ErrCrossCheckUnavailable) {
			fx.t.Fatalf("plan err = %v, want ErrCrossCheckUnavailable", err)
		}
	} else if err != nil || cc.Outcome.String != "failed" || cc.ReasonClass.String != "checker_unavailable" || cc.CheckerRunID.Valid {
		fx.t.Fatalf("code sink: outcome=%q reason=%q checker=%v err=%v, want failed/checker_unavailable with no child", cc.Outcome.String, cc.ReasonClass.String, cc.CheckerRunID.Valid, err)
	}
	if got := fx.runCount(); got != runsBefore {
		fx.t.Fatalf("run count = %d, want %d (a refusal left a run)", got, runsBefore)
	}
}

// countingFreeze fails the test when the vendor Codex binding freeze runs (the Provider arm must never
// reach it) and counts it, so a control can prove the counter works.
func (fx *clFix) countFreeze() *int {
	n := new(int)
	fx.svc.codexFreezeFn = func(ctx context.Context, q codexFreezeStore, userID, runID, secretID uuid.UUID, authMode string) error {
		*n++
		return fx.svc.freezeCodexBinding(ctx, q, userID, runID, secretID, authMode)
	}
	fx.t.Cleanup(func() { fx.svc.codexFreezeFn = nil })
	return n
}

// TestCheckerLaneProviderFreezesTheCodexSourceLiveDB: a Codex source selects the Provider lane for both
// stages and both lead kinds (CV and CP). The child carries the five binding columns of the source and
// no vendor credential, and createRunAtomic never reaches the vendor Codex freeze; the same owner with no
// Codex source is the Vendor control (the counter moves, the binding is empty). The plan stage creates
// the child on a lead worker WITHOUT the provider checker capability (it has no fast-fail: any of the
// owner's workers may claim after the affinity cutoff).
func TestCheckerLaneProviderFreezesTheCodexSourceLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, leadBound := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/leadBound=%t", stage, leadBound), func(t *testing.T) {
				fx := newCLFix(t)
				if leadBound {
					fx.selectClaude()
				}
				lead := fx.lead(stage, leadBound)
				fx.selectCodex()
				w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
				freezes := fx.countFreeze()
				cc, err := fx.submit(w, lead.ID, stage)
				child := fx.wantProviderChild(cc, err)
				if *freezes != 0 {
					t.Fatalf("a Provider checker reached the vendor Codex freeze %d times", *freezes)
				}
				if child.Status != "queued" {
					t.Fatalf("child status = %s", child.Status)
				}
				if leadBound {
					return // the vendor control below needs a vendor lead
				}
				// Vendor control: the same owner with no Codex source gets a vendor child and the freeze runs.
				fx.clearSources()
				lead2 := fx.lead(stage, false)
				cc, err = fx.submit(w, lead2.ID, stage)
				fx.wantVendorChild(cc, err)
				if *freezes != 1 {
					t.Fatalf("vendor control: freeze count = %d, want 1 (the counter must be live)", *freezes)
				}
			})
		}
	}
	t.Run("plan stage creates on a worker without the checker capability", func(t *testing.T) {
		fx := newCLFix(t)
		lead := fx.lead("plan", false)
		fx.selectCodex()
		w := fx.worker(clCaps(clProviderCaps...))
		cc, err := fx.submit(w, lead.ID, "plan")
		fx.wantProviderChild(cc, err)
	})
}

// TestCheckerLaneRefusedAndUnusableLiveDB: Refused rows and an unusable Codex source land on the
// stage's unavailable sink with no child, and an unusable source never falls back to the vendor Codex
// credential even though it is usable.
func TestCheckerLaneRefusedAndUnusableLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, tc := range []struct {
			name  string
			setup func(fx *clFix, stage string) store.Run
		}{
			{"claude source selected after a vendor lead (CV/XV held)", func(fx *clFix, stage string) store.Run {
				lead := fx.lead(stage, false)
				fx.selectClaude()
				return lead
			}},
			{"bound lead with no codex source (CP/XV held)", func(fx *clFix, stage string) store.Run {
				fx.selectClaude()
				return fx.lead(stage, true)
			}},
			{"unusable codex source with a usable vendor codex credential", func(fx *clFix, stage string) store.Run {
				lead := fx.lead(stage, false)
				fx.selectCodex()
				fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.codexKey)
				return lead
			}},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				fx := newCLFix(t)
				lead := tc.setup(fx, stage)
				w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
				before := fx.runCount()
				freezes := fx.countFreeze()
				cc, err := fx.submit(w, lead.ID, stage)
				fx.wantUnavailable(stage, cc, err, before)
				if *freezes != 0 {
					t.Fatalf("a refused checker reached the vendor freeze")
				}
			})
		}
	}
}

// TestCheckerLaneProviderNeverResolvesToVendorLiveDB: a Provider decision whose resolution carries no
// source returns ProviderSourceUnusableError(provider_missing), never a vendor child. The source is
// cleared by a lock-bypassing commit in the decision hook (every real writer holds the exclusive lock),
// so the resolver sees none and its vendor ladder would happily resolve the usable vendor Codex
// credential; the insert closure must not run.
func TestCheckerLaneProviderNeverResolvesToVendorLiveDB(t *testing.T) {
	fx := newCLFix(t)
	fx.selectCodex()
	fx.svc.checkerDecidedHook = func(context.Context) {
		fx.env.exec(`UPDATE users SET codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1`, fx.userID)
	}
	t.Cleanup(func() { fx.svc.checkerDecidedHook = nil })
	before := fx.runCount()
	inserted := 0
	freezes := fx.countFreeze()
	_, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, h(HarnessCodex), func(q *store.Queries, r resolvedHarness) (store.Run, error) {
		inserted++
		return fx.insertAt(nil)(q, r)
	}, harnessCreationOrigin{CheckerLane: true})
	var unusable *ProviderSourceUnusableError
	if !errors.As(err, &unusable) || unusable.Reason != SourceUnusableProviderMissing || !errors.Is(err, ErrProviderSourceUnusable) {
		t.Fatalf("err = %v, want provider_source_unusable (provider_missing)", err)
	}
	if inserted != 0 || *freezes != 0 || fx.runCount() != before {
		t.Fatalf("inserted=%d freezes=%d runs=%d (before %d): a Provider decision produced a vendor child", inserted, *freezes, fx.runCount(), before)
	}
}

// TestCheckerLaneAttestationLiveDB: the checker_unavailable attestation uses the same decision over a
// fresh read. It accepts Refused, accepts Provider only when its source is unusable, and keeps the
// vendor-resolution check for Vendor. The negative cases (a usable Codex source for a CV lead and for a
// CP lead, and a usable vendor Codex credential) are refused with ErrInvalidState.
func TestCheckerLaneAttestationLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(fx *clFix) store.Run
		accept bool
	}{
		{"refused: bound lead, no codex source", func(fx *clFix) store.Run { fx.selectClaude(); return fx.lead("plan", true) }, true},
		{"refused: claude source after a vendor lead", func(fx *clFix) store.Run {
			l := fx.lead("plan", false)
			fx.selectClaude()
			return l
		}, true},
		{"provider with an unusable source (CV)", func(fx *clFix) store.Run {
			l := fx.lead("plan", false)
			fx.selectCodex()
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.codexKey)
			return l
		}, true},
		{"provider with an unusable source (CP)", func(fx *clFix) store.Run {
			fx.selectClaude()
			l := fx.lead("plan", true)
			fx.selectCodex()
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.codexKey)
			return l
		}, true},
		{"negative: provider with a usable source (CV)", func(fx *clFix) store.Run {
			l := fx.lead("plan", false)
			fx.selectCodex()
			return l
		}, false},
		{"negative: provider with a usable source (CP)", func(fx *clFix) store.Run {
			fx.selectClaude()
			l := fx.lead("plan", true)
			fx.selectCodex()
			return l
		}, false},
		{"negative: vendor with a usable vendor codex credential", func(fx *clFix) store.Run { return fx.lead("plan", false) }, false},
		{"vendor with the vendor codex credential disabled", func(fx *clFix) store.Run {
			l := fx.lead("plan", false)
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE user_id = $1 AND kind IN ('codex_auth','openai_api_key')`, fx.userID)
			return l
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCLFix(t)
			lead := tc.setup(fx)
			fx.env.exec(`UPDATE runs SET status='running', worker_id=$2, auto_approve=true, plan_source='agent',
				plan_cross_check_required=true, claim_generation=1 WHERE id=$1`, lead.ID, fx.workerID)
			gen, reason := int64(1), "checker_unavailable"
			plan, size := "A plan that needed a checker", "s"
			caps, tools, ms := []string{}, []string{}, []Milestone{}
			_, applied, err := fx.svc.SetState(fx.env.ctx, store.Worker{ID: fx.workerID, UserID: fx.userID}, lead.ID, StateRequest{
				State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
				RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PlanCrossCheckGateReason: &reason})
			got := mustRun(t, fx.env, lead.ID)
			if tc.accept && (err != nil || !applied || got.Status != "awaiting_approval" || got.AutoApprove) {
				t.Fatalf("applied=%v err=%v status=%s auto_approve=%v, want the human gate", applied, err, got.Status, got.AutoApprove)
			}
			if !tc.accept && (applied || !errors.Is(err, ErrInvalidState) || got.Status != "running") {
				t.Fatalf("applied=%v err=%v status=%s, want ErrInvalidState and no change", applied, err, got.Status)
			}
		})
	}
}

// TestCheckerLaneCodeStageFastFailLiveDB: the code checker runs on the lead's worker, so a Provider
// decision on a worker without codex_provider_checker_v1 fails fast (checker_unavailable, no child),
// in the Go check and, independently, in the guarded SQL write; the positive control on a capable worker
// creates the child. A vendor checker is unaffected on a worker WITHOUT any provider capability.
func TestCheckerLaneCodeStageFastFailLiveDB(t *testing.T) {
	t.Run("go check", func(t *testing.T) {
		fx := newCLFix(t)
		lead := fx.lead("code", false)
		fx.selectCodex()
		// Positive control first: the capable worker creates the Provider child.
		w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
		before := fx.runCount()
		cc, err := fx.submit(w, lead.ID, "code")
		fx.wantProviderChild(cc, err)
		if fx.runCount() != before+1 {
			t.Fatalf("positive control created %d runs, want 1", fx.runCount()-before)
		}
		// Missing only the checker capability, on a fresh lead. The Go check refuses BEFORE the guarded
		// write is attempted (the SQL guard alone would give the same sink), which a traced pool proves.
		reads := &frozenLeadVendorReads{}
		reads.reset()
		pool, perr := store.OpenPool(fx.env.ctx, fx.env.pool.Config().ConnString(), store.WithQueryTracer(reads))
		if perr != nil {
			t.Fatal("open traced pool")
		}
		t.Cleanup(pool.Close)
		fx.svc.q = store.New(pool)
		fx.svc.SetTxBeginner(pool)
		t.Cleanup(func() { fx.svc.q = fx.env.q; fx.svc.SetTxBeginner(fx.env.pool) })
		lead2 := fx.lead("code", false)
		w = fx.worker(clCaps(clProviderCaps...))
		before = fx.runCount()
		reads.reset()
		cc, err = fx.submit(w, lead2.ID, "code")
		fx.wantUnavailable("code", cc, err, before)
		reads.mu.Lock()
		attempted := reads.names["CreateCodeCrossCheckChild"]
		reads.mu.Unlock()
		if attempted != 0 {
			t.Fatalf("the guarded write was attempted %d times: the Go capability check did not refuse first", attempted)
		}
	})
	t.Run("guarded SQL write", func(t *testing.T) {
		fx := newCLFix(t)
		fx.selectCodex()
		bind := fx.codexBinding()
		call := func(leadID uuid.UUID, b bindingParams) error {
			_, err := fx.env.q.CreateCodeCrossCheckChild(fx.env.ctx, store.CreateCodeCrossCheckChildParams{
				ChildID: uuid.New(), ChildHarness: "codex", LeadRunID: leadID, UserID: fx.userID, WorkerID: pgtype.UUID{Bytes: fx.workerID, Valid: true},
				ClaimGeneration: 1, BudgetWallSeconds: 300,
				ModelProviderID: b.ProviderID, ModelProviderRevision: b.Revision, ModelProviderKeyMode: b.KeyMode, ModelProviderKeySecretID: b.KeySecret, ModelProviderKeyRev: b.KeyRev})
			return err
		}
		// Without the checker capability: a provider-bound stamp finds no row; a vendor stamp (all NULL)
		// is unaffected on a worker with no provider capability at all.
		for _, caps := range [][]string{clCaps(clProviderCaps...), clCaps()} {
			fx.worker(caps)
			lead := fx.lead("code", false)
			before := fx.runCount()
			if err := call(lead.ID, bind); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("caps %v: provider stamp err = %v, want pgx.ErrNoRows", caps, err)
			}
			if fx.runCount() != before {
				t.Fatalf("caps %v: the guarded write inserted a run", caps)
			}
			if err := call(lead.ID, bindingParams{}); err != nil {
				t.Fatalf("caps %v: vendor stamp refused: %v", caps, err)
			}
			if fx.runCount() != before+1 {
				t.Fatalf("caps %v: vendor stamp created %d runs", caps, fx.runCount()-before)
			}
		}
		// Each provider capability is required: dropping any one refuses the provider stamp.
		full := clCaps(append(clProviderCaps, clCheckerCap)...)
		for _, drop := range append([]string{"codex_harness_v1", "codex_runtime_v2"}, append(clProviderCaps, clCheckerCap)...) {
			var caps []string
			for _, c := range full {
				if c != drop {
					caps = append(caps, c)
				}
			}
			fx.worker(caps)
			lead := fx.lead("code", false)
			if err := call(lead.ID, bind); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("without %s: provider stamp err = %v, want pgx.ErrNoRows", drop, err)
			}
		}
		// All capabilities present: the stamp is written.
		fx.worker(full)
		lead := fx.lead("code", false)
		if err := call(lead.ID, bind); err != nil {
			t.Fatalf("capable worker: %v", err)
		}
	})
	t.Run("vendor unaffected on a worker without provider capabilities", func(t *testing.T) {
		for _, stage := range []string{"plan", "code"} {
			fx := newCLFix(t)
			lead := fx.lead(stage, false)
			w := fx.worker(clCaps())
			cc, err := fx.submit(w, lead.ID, stage)
			fx.wantVendorChild(cc, err)
		}
	})
}

// TestCheckerLaneVendorVendorUnsetModelsLiveDB: vendor/vendor with both models unset keeps creating its
// checker in both stages on a worker without provider capabilities, with an empty binding (the same
// control the code-stage fast-fail must not touch).
func TestCheckerLaneVendorVendorUnsetModelsLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			fx := newCLFix(t)
			lead := fx.lead(stage, false)
			if lead.Model.Valid {
				t.Fatalf("lead model = %q, want unset", lead.Model.String)
			}
			cc, err := fx.submit(fx.worker(clCaps()), lead.ID, stage)
			fx.wantVendorChild(cc, err)
			child := mustRun(t, fx.env, uuid.UUID(cc.CheckerRunID.Bytes))
			if child.Model.Valid {
				t.Fatalf("child model = %q, want unset", child.Model.String)
			}
		})
	}
}

// secretLockState counts the granted and waiting requests on this owner's secret-mutation advisory lock
// key, by mode (pg_locks), from a connection of its own.
func (fx *clFix) secretLockState() (sharedGranted, exclusiveGranted, waiting int) {
	fx.t.Helper()
	rows, err := fx.env.pool.Query(fx.env.ctx, `SELECT mode, granted FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 2
		AND classid::bigint = ($1::bigint & 4294967295) AND objid::bigint = ($2::bigint & 4294967295)`,
		int64(store.SecretMutationLockClass), int64(store.SecretMutationLockObjID(fx.userID)))
	if err != nil {
		fx.t.Fatalf("read pg_locks: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mode string
		var granted bool
		if err := rows.Scan(&mode, &granted); err != nil {
			fx.t.Fatal(err)
		}
		switch {
		case !granted:
			waiting++
		case mode == "ShareLock":
			sharedGranted++
		default:
			exclusiveGranted++
		}
	}
	return sharedGranted, exclusiveGranted, waiting
}

// exclusiveSourceWriter is what every source writer does: the exclusive secret-mutation lock, then the
// write. It returns when committed.
func (fx *clFix) exclusiveSourceWriter(write string, args ...any) error {
	ctx := fx.env.ctx
	tx, err := fx.env.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '30s'`); err != nil {
		return err
	}
	if err := store.New(tx).LockSecretMutation(ctx, fx.userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, write, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// waitFor polls cond up to 15s.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestCheckerLaneLockOrderLiveDB (R5): the checker lane takes the shared secret-mutation lock BEFORE the
// lead row lock, on every checker creation (Provider, Vendor and Refused). In the decision hook the
// create transaction holds the shared lock (pg_locks) and the lead row is still free (a NOWAIT
// FOR UPDATE succeeds); the probe then HOLDS the lead row, so the creation closure blocks on it, and
// the blocked backend is observed holding the granted shared lock while an exclusive source writer
// queues behind it.
func TestCheckerLaneLockOrderLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, tc := range []struct {
			name         string
			setup        func(fx *clFix) store.Run
			reachesClose bool
		}{
			{"provider", func(fx *clFix) store.Run { l := fx.lead(stage, false); fx.selectCodex(); return l }, true},
			{"vendor", func(fx *clFix) store.Run { return fx.lead(stage, false) }, true},
			{"refused", func(fx *clFix) store.Run { fx.selectClaude(); return fx.lead(stage, true) }, false},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				fx := newCLFix(t)
				lead := tc.setup(fx)
				w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
				var (
					hookErr    error
					evidence   = make(chan error, 1)
					writerDone = make(chan error, 1)
					hookRan    bool
				)
				fx.svc.checkerDecidedHook = func(ctx context.Context) {
					hookRan = true
					if shared, excl, _ := fx.secretLockState(); shared != 1 || excl != 0 {
						hookErr = fmt.Errorf("in the decision hook the shared secret-mutation lock is held %d times (exclusive %d), want exactly 1 shared", shared, excl)
						return
					}
					if !tc.reachesClose {
						return
					}
					// The lead row must still be free: NOWAIT FOR UPDATE succeeds, and we keep holding it.
					tx, err := fx.env.pool.Begin(ctx)
					if err != nil {
						hookErr = err
						return
					}
					var bpid int
					if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&bpid); err != nil {
						_ = tx.Rollback(ctx)
						hookErr = err
						return
					}
					if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE NOWAIT`, lead.ID); err != nil {
						_ = tx.Rollback(ctx)
						hookErr = fmt.Errorf("the lead row was already locked when the lane was decided: %w", err)
						return
					}
					go func() {
						defer func() { _ = tx.Rollback(context.Background()) }()
						var blocked int
						ok := waitFor(func() bool {
							if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT pid FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, bpid).Scan(&blocked); err != nil {
								return false
							}
							return true
						})
						if !ok {
							evidence <- errors.New("the creation closure never blocked on the lead row")
							return
						}
						var n int
						if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM pg_locks WHERE pid = $1 AND locktype = 'advisory' AND granted AND mode = 'ShareLock'
							AND classid::bigint = ($2::bigint & 4294967295) AND objid::bigint = ($3::bigint & 4294967295)`, blocked,
							int64(store.SecretMutationLockClass), int64(store.SecretMutationLockObjID(fx.userID))).Scan(&n); err != nil || n != 1 {
							evidence <- fmt.Errorf("the backend blocked on the lead row holds %d shared secret-mutation locks (err %v), want 1", n, err)
							return
						}
						// A source writer queues behind the held shared lock, still waiting while the lead is blocked.
						go func() {
							writerDone <- fx.exclusiveSourceWriter(`UPDATE users SET default_harness = default_harness WHERE id = $1`, fx.userID)
						}()
						if !waitFor(func() bool { _, _, waiting := fx.secretLockState(); return waiting >= 1 }) {
							evidence <- errors.New("the exclusive writer never queued behind the create's shared lock")
							return
						}
						evidence <- tx.Commit(fx.env.ctx)
					}()
				}
				t.Cleanup(func() { fx.svc.checkerDecidedHook = nil })
				cc, err := fx.submit(w, lead.ID, stage)
				if !hookRan {
					t.Fatal("the lane hook never ran")
				}
				if hookErr != nil {
					t.Fatal(hookErr)
				}
				if tc.reachesClose {
					select {
					case e := <-evidence:
						if e != nil {
							t.Fatal(e)
						}
					case <-time.After(30 * time.Second):
						t.Fatal("no lock-order evidence")
					}
					select {
					case e := <-writerDone:
						if e != nil {
							t.Fatalf("writer: %v", e)
						}
					case <-time.After(30 * time.Second):
						t.Fatal("the writer never finished after the create committed")
					}
				}
				switch tc.name {
				case "provider":
					fx.wantProviderChild(cc, err)
				case "vendor":
					fx.wantVendorChild(cc, err)
				default:
					fx.wantUnavailable(stage, cc, err, fx.runCount())
				}
			})
		}
	}
}

// TestCheckerLaneSourceRaceLiveDB: the decision is final under the lock. A source writer started after
// the decision read queues behind the create's shared lock (pg_locks) and commits only after it, so the
// create keeps the decision it read: Refused stays refused (its writer then makes the owner eligible for
// the NEXT creation), and a Provider decision commits a child frozen to the source it read even though
// the writer clears that source.
func TestCheckerLaneSourceRaceLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage+"/refused to eligible", func(t *testing.T) {
			fx := newCLFix(t)
			fx.selectClaude()
			lead := fx.lead(stage, true) // a bound lead with no Codex source: Refused
			w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
			codexProvider, codexKey := seedCodexProviderKeyFor(t, fx.env, fx.userID)
			fx.codexProvider, fx.codexKey = codexProvider, codexKey
			t.Cleanup(func() {
				_, _ = fx.env.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1`, fx.userID)
			})
			var hookErr error
			writerDone := make(chan error, 1)
			fx.svc.checkerDecidedHook = func(context.Context) {
				go func() {
					writerDone <- fx.exclusiveSourceWriter(`UPDATE users SET codex_provider_id=$2, codex_provider_scope_id=$1 WHERE id=$1`, fx.userID, codexProvider)
				}()
				if !waitFor(func() bool { _, _, waiting := fx.secretLockState(); return waiting >= 1 }) {
					hookErr = errors.New("the writer was never seen queued behind the create's shared lock")
					return
				}
				select {
				case e := <-writerDone:
					hookErr = fmt.Errorf("the writer finished (%v) while the create held the shared lock", e)
				default:
				}
			}
			t.Cleanup(func() { fx.svc.checkerDecidedHook = nil })
			before := fx.runCount()
			cc, err := fx.submit(w, lead.ID, stage)
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			fx.wantUnavailable(stage, cc, err, before)
			select {
			case e := <-writerDone:
				if e != nil {
					t.Fatalf("writer: %v", e)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the writer never finished")
			}
			// The writer committed after the refusal: the plan stage's NEXT creation is eligible (Provider),
			// the code stage's recorded failure is immutable.
			fx.svc.checkerDecidedHook = nil
			cc, err = fx.submit(w, lead.ID, stage)
			if stage == "plan" {
				fx.wantProviderChild(cc, err)
			} else if err != nil || cc.Outcome.String != "failed" || cc.CheckerRunID.Valid {
				t.Fatalf("code retry: outcome=%q checker=%v err=%v, want the recorded failure", cc.Outcome.String, cc.CheckerRunID.Valid, err)
			}
		})
		t.Run(stage+"/eligible to refused", func(t *testing.T) {
			fx := newCLFix(t)
			lead := fx.lead(stage, false)
			fx.selectCodex()
			w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
			var hookErr error
			writerDone := make(chan error, 1)
			fx.svc.checkerDecidedHook = func(context.Context) {
				go func() {
					writerDone <- fx.exclusiveSourceWriter(`UPDATE users SET codex_provider_id=NULL, codex_provider_scope_id=NULL WHERE id=$1`, fx.userID)
				}()
				if !waitFor(func() bool { _, _, waiting := fx.secretLockState(); return waiting >= 1 }) {
					hookErr = errors.New("the writer was never seen queued behind the create's shared lock")
				}
			}
			t.Cleanup(func() { fx.svc.checkerDecidedHook = nil })
			cc, err := fx.submit(w, lead.ID, stage)
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			// The child is frozen to the source the decision read, though the writer then cleared it.
			bound := fx.wantProviderChild(cc, err)
			if bound.CodexSecretID.Valid {
				t.Fatal("vendor credential on a provider child")
			}
			select {
			case e := <-writerDone:
				if e != nil {
					t.Fatalf("writer: %v", e)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the writer never finished")
			}
		})
	}
}

// TestCheckerLaneNoTransactionArmsLiveDB: createRunAtomic with no transaction beginner over the real
// store (the nil-txBeginner arm), asserting the exact error AND the insert count per decision.
func TestCheckerLaneNoTransactionArmsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(fx *clFix)
		explicit    Harness
		leadBound   bool
		wantErr     error
		wantInserts int
	}{
		{"provider decision", func(fx *clFix) { fx.selectCodex() }, HarnessCodex, false, errProviderCreateRequiresTx, 0},
		{"provider decision with an unusable source", func(fx *clFix) {
			fx.selectCodex()
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.codexKey)
		}, HarnessCodex, false, errProviderCreateRequiresTx, 0},
		{"refused: claude source", func(fx *clFix) { fx.selectClaude() }, HarnessCodex, false, ErrProviderLaneUnsupported, 0},
		{"refused: bound lead", func(fx *clFix) {}, HarnessCodex, true, ErrProviderLaneUnsupported, 0},
		{"vendor claude checker", func(fx *clFix) {}, HarnessClaude, false, nil, 1},
		{"vendor claude checker with only a codex source", func(fx *clFix) { fx.selectCodex() }, HarnessClaude, false, nil, 1},
		{"vendor codex checker", func(fx *clFix) {}, HarnessCodex, false, errCodexCreateRequiresTx, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCLFix(t)
			tc.setup(fx)
			fx.svc.txBeginner = nil
			t.Cleanup(func() { fx.svc.txBeginner = fx.env.pool })
			inserts := 0
			ins := fx.insertAt(nil)
			_, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, h(tc.explicit), func(q *store.Queries, r resolvedHarness) (store.Run, error) {
				inserts++
				return ins(q, r)
			}, harnessCreationOrigin{CheckerLane: true, LeadProviderBound: tc.leadBound})
			if (tc.wantErr == nil && err != nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || inserts != tc.wantInserts {
				t.Fatalf("err = %v inserts = %d, want %v and %d", err, inserts, tc.wantErr, tc.wantInserts)
			}
		})
	}
}

// fakeArmQueries is a store that is NOT a *store.Queries, so createRunResolved takes its in-memory arm
// while every query still reaches the real database.
type fakeArmQueries struct{ *store.Queries }

// TestCheckerLaneFakeStoreSubmitSinksLiveDB: through the in-memory creation arm a Provider decision is
// errProviderCreateRequiresTx, and the caller still lands on the stage's unavailable sink (the plan
// human gate, the code checker_unavailable record), never a vendor checker.
func TestCheckerLaneFakeStoreSubmitSinksLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			fx := newCLFix(t)
			lead := fx.lead(stage, false)
			fx.selectCodex()
			w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
			fx.svc.q = fakeArmQueries{fx.env.q}
			t.Cleanup(func() { fx.svc.q = fx.env.q })
			before := fx.runCount()
			cc, err := fx.submit(w, lead.ID, stage)
			fx.wantUnavailable(stage, cc, err, before)
		})
	}
}

// TestCheckerLaneHeldRowsUnderConcurrentSourceWriteLiveDB: the rows PRD #4574 M2 leaves held (CV/XV and
// CP/XV) and the vendor row keep their outcomes when a source writer runs concurrently with the create,
// because the create read its decision under the shared secret-mutation lock the writer queues behind
// (pg_locks), and the writer commits only after the create does.
func TestCheckerLaneHeldRowsUnderConcurrentSourceWriteLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, tc := range []struct {
			name    string
			setup   func(fx *clFix) store.Run
			write   string
			refused bool
		}{
			{"CV/XV held, writer clears the claude source", func(fx *clFix) store.Run {
				l := fx.lead(stage, false)
				fx.selectClaude()
				return l
			}, `UPDATE users SET claude_provider_id=NULL, claude_provider_scope_id=NULL WHERE id=$1`, true},
			{"CP/XV held, writer clears the claude source", func(fx *clFix) store.Run {
				fx.selectClaude()
				return fx.lead(stage, true)
			}, `UPDATE users SET claude_provider_id=NULL, claude_provider_scope_id=NULL WHERE id=$1`, true},
			{"vendor/vendor, writer selects a claude source", func(fx *clFix) store.Run { return fx.lead(stage, false) }, "", false},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				fx := newCLFix(t)
				lead := tc.setup(fx)
				w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
				write, args := tc.write, []any{fx.userID}
				if write == "" {
					prov, _ := seedProviderFor(t, fx.env, fx.userID, pvDefaultMap)
					t.Cleanup(func() {
						_, _ = fx.env.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id=NULL,claude_provider_scope_id=NULL WHERE id=$1`, fx.userID)
					})
					write, args = `UPDATE users SET claude_provider_id=$2, claude_provider_scope_id=$1 WHERE id=$1`, []any{fx.userID, prov}
				}
				var hookErr error
				writerDone := make(chan error, 1)
				fx.svc.checkerDecidedHook = func(context.Context) {
					go func() { writerDone <- fx.exclusiveSourceWriter(write, args...) }()
					if !waitFor(func() bool { _, _, waiting := fx.secretLockState(); return waiting >= 1 }) {
						hookErr = errors.New("the writer was never seen queued behind the create's shared lock")
					}
				}
				t.Cleanup(func() { fx.svc.checkerDecidedHook = nil })
				before := fx.runCount()
				cc, err := fx.submit(w, lead.ID, stage)
				if hookErr != nil {
					t.Fatal(hookErr)
				}
				if tc.refused {
					fx.wantUnavailable(stage, cc, err, before)
				} else {
					fx.wantVendorChild(cc, err)
				}
				select {
				case e := <-writerDone:
					if e != nil {
						t.Fatalf("writer: %v", e)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("the writer never finished")
				}
			})
		}
	}
}

// TestCheckerLaneLeadBindingMismatchIsRefusedLiveDB: createRunAtomic has no lead row, so the create
// closure re-asserts LeadProviderBound against the locked lead. The decision (Vendor: no source, lead
// read as vendor) is taken before the lead becomes provider-bound (a lock-bypassing change in the
// decision hook), and the closure's equality refuses with no child instead of creating a vendor checker
// for a provider-bound lead.
func TestCheckerLaneLeadBindingMismatchIsRefusedLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			fx := newCLFix(t)
			lead := fx.lead(stage, false)
			w := fx.worker(clCaps(append(clProviderCaps, clCheckerCap)...))
			prov, key := seedProviderFor(t, fx.env, fx.userID, pvDefaultMap)
			fx.svc.checkerDecidedHook = func(context.Context) {
				fx.env.exec(`UPDATE runs SET model_provider_id=$2, model_provider_revision=1, model_provider_key_mode='user_key',
					model_provider_key_secret_id=$3, model_provider_key_rev=(SELECT enablement_rev FROM user_secrets WHERE id=$3) WHERE id=$1`, lead.ID, prov, key)
			}
			t.Cleanup(func() { fx.svc.checkerDecidedHook = nil })
			before := fx.runCount()
			cc, err := fx.submit(w, lead.ID, stage)
			fx.wantUnavailable(stage, cc, err, before)
			if !providerBoundRun(mustRun(t, fx.env, lead.ID)) {
				t.Fatal("the hook did not bind the lead: the mismatch was never exercised")
			}
		})
	}
}
