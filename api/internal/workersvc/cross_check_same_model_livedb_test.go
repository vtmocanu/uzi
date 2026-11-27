package workersvc

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// cross_check_same_model_livedb_test.go is PRD #4574 M2 against a real Postgres: the same-model guard
// inside the checker claim transaction, and the lead-first settlement of a provider-bound checker
// child (a refusal decides the check checker_unavailable and never parks the child). The provider
// checker child is stamped onto a vendor checker child by SQL, because creating one is a later
// milestone. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Every test name ends LiveDB so the store-IT sweep selects it.

// recordingTxBeginner wraps the real pool's Begin; every statement a transaction it opened runs is
// recorded by its sqlc "-- name:" header, which is how the vendor/vendor no-read property is proven.
type recordingTxBeginner struct {
	TxBeginner
	mu  sync.Mutex
	sql []string
}

func (b *recordingTxBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingTx{Tx: tx, b: b}, nil
}

func (b *recordingTxBeginner) record(sql string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sql = append(b.sql, sql)
}

// saw reports whether any recorded statement carries the sqlc header "-- name: <name> :".
func (b *recordingTxBeginner) saw(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sql {
		if strings.Contains(s, "-- name: "+name+" :") {
			return true
		}
	}
	return false
}

type recordingTx struct {
	pgx.Tx
	b *recordingTxBeginner
}

func (tx *recordingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.b.record(sql)
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *recordingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.b.record(sql)
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.b.record(sql)
	return tx.Tx.Exec(ctx, sql, args...)
}

const (
	smLiveLeadReads = "GetRunLeadConfigForGeneration"
	smLiveRevReads  = "GetModelProviderRevision"
	smLiveCodexMap  = `{"default":{"model":"gateway"},"gpt-6-sol":{"model":"gw-gpt-6-sol"}}`
)

type smLive struct {
	crossCheckContentionFixture
	t           *testing.T
	stage       string
	rec         *recordingTxBeginner
	child       uuid.UUID
	checkerProv uuid.UUID
	checkerKey  uuid.UUID
}

// newSMLive builds a plan or code checker child of a Claude lead, claimable by the lead's worker, with
// every Codex provider capability advertised. Both sides are vendor until stampChecker / stampLead.
func newSMLive(t *testing.T, stage string) *smLive {
	t.Helper()
	var f crossCheckContentionFixture
	if stage == "code" {
		f = codeFixture(t)
		cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
		if err != nil || !cc.CheckerRunID.Valid || cc.CheckerHarness.String != "codex" {
			t.Fatalf("submit code checker: %+v %v", cc, err)
		}
		f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	} else {
		f = laneFixture(t)
		f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.lead, f.workerID)
	}
	rec := &recordingTxBeginner{TxBeginner: f.env.pool}
	f.svc.SetTxBeginner(rec)
	f.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	f.env.exec(`UPDATE workers SET protocol_capabilities = array_cat(protocol_capabilities, $2) WHERE id = $1`, f.workerID,
		[]string{capability.CodexProviderV1, capability.CodexProviderCheckerV1})
	return &smLive{crossCheckContentionFixture: f, t: t, stage: stage, rec: rec, child: f.runID}
}

func (s *smLive) keyRev(key uuid.UUID) int64 {
	s.t.Helper()
	var rev int64
	if err := s.env.pool.QueryRow(s.env.ctx, `SELECT enablement_rev FROM user_secrets WHERE id = $1`, key).Scan(&rev); err != nil {
		s.t.Fatal(err)
	}
	return rev
}

func (s *smLive) stamp(runID, provider uuid.UUID, revision int32, key uuid.UUID) {
	s.env.exec(`UPDATE runs SET anthropic_secret_id=NULL, codex_secret_id=NULL, codex_auth_mode=NULL, credential_override_secret_id=NULL,
		model_provider_id=$2, model_provider_revision=$3, model_provider_key_mode='user_key',
		model_provider_key_secret_id=$4, model_provider_key_rev=$5 WHERE id=$1`, runID, provider, revision, key, s.keyRev(key))
}

// stampChecker makes the checker child a Codex provider-bound child on a freshly seeded provider.
func (s *smLive) stampChecker(t *testing.T, modelMap string) {
	t.Helper()
	p, err := s.env.q.InsertModelProvider(s.env.ctx, store.InsertModelProviderParams{OwnerUserID: s.userID, Name: "sm-codex-" + uuid.NewString()[:8], Engine: harnessCodex})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.env.q.InsertModelProviderRevision(s.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(modelMap)}); err != nil {
		t.Fatal(err)
	}
	sealed, err := s.env.box.Seal([]byte("gw-key-" + uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.env.q.InsertProviderKeySecret(s.env.ctx, store.InsertProviderKeySecretParams{
		UserID: s.userID, Label: p.ID.String(), Ciphertext: sealed, SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	s.checkerProv, s.checkerKey = p.ID, k.ID
	s.stamp(s.child, p.ID, 1, k.ID)
}

// stampLead makes the Claude lead provider-bound on a Claude provider revision (frozen at the given
// revision number, which need not exist) and returns the provider id.
func (s *smLive) stampLead(t *testing.T, revision int32) uuid.UUID {
	t.Helper()
	provider, key := seedProviderFor(t, s.env, s.userID, pvDefaultMap)
	s.stamp(s.lead, provider, revision, key)
	return provider
}

// leadRow inserts the lead's reported config for the check's generation (1) and this stage's phase.
func (s *smLive) leadRow(generation int64, phase string, requested, observed *string) {
	s.t.Helper()
	var evidence *string
	if observed != nil {
		evidence = smStr("claude_init")
	}
	s.env.exec(`INSERT INTO run_lead_configs (run_id, claim_generation, phase, phase_entry, seq, harness,
		requested_model, requested_model_source, observed_model, observed_model_evidence, requested_effort_mode, requested_effort_source)
		VALUES ($1,$2,$3,1,1,'claude',$4,'builtin',$5,$6,'runtime_default','builtin')`,
		s.lead, generation, phase, requested, observed, evidence)
}

func (s *smLive) phase() string { return leadConfigPhase(s.stage) }

func (s *smLive) claim(t *testing.T) *ClaimPayload {
	return laneClaim(t, s.crossCheckContentionFixture, "cross_check", nil)
}

// wantDelivered asserts the claim delivered this checker with the expected credential family.
func (s *smLive) wantDelivered(t *testing.T, p *ClaimPayload, provider bool) {
	t.Helper()
	if p == nil || p.RunID != s.child.String() || p.CrossCheck == nil || p.CrossCheck.Stage != s.stage {
		t.Fatalf("checker %s not delivered: %+v", s.child, p)
	}
	if provider && (p.Secrets.Provider == nil || p.Secrets.Codex != nil || p.Secrets.AnthropicOAuthToken != "") {
		t.Fatalf("provider checker secrets = %+v, want only a provider binding", p.Secrets)
	}
	if !provider && (p.Secrets.Provider != nil || p.Secrets.Codex == nil) {
		t.Fatalf("vendor checker secrets = %+v, want the vendor Codex credential", p.Secrets)
	}
	if got := mustRun(t, s.env, s.child); got.Status != "claimed" {
		t.Fatalf("child status = %s, want claimed", got.Status)
	}
}

// wantDecidedUnavailable asserts the lead-first settlement: nothing delivered, the check decided
// failed/checker_unavailable, the child failed as guardrail_blocked and never paused.
func (s *smLive) wantDecidedUnavailable(t *testing.T, p *ClaimPayload) {
	t.Helper()
	if p != nil {
		t.Fatalf("a refused provider checker delivered a payload: %+v", p)
	}
	child := mustRun(t, s.env, s.child)
	if child.Status != "failed" || child.FailOrigin.String != "guardrail_blocked" || child.HoldReason.Valid {
		t.Fatalf("child status=%s origin=%q hold=%v, want failed/guardrail_blocked with no hold (never paused)", child.Status, child.FailOrigin.String, child.HoldReason)
	}
	var verdict, outcome, reason *string
	if err := s.env.pool.QueryRow(s.env.ctx, `SELECT verdict, outcome, reason_class FROM cross_checks WHERE checker_run_id = $1`, s.child).Scan(&verdict, &outcome, &reason); err != nil {
		t.Fatal(err)
	}
	decided := verdict
	if s.stage == "code" {
		decided = outcome
	}
	if decided == nil || *decided != "failed" || reason == nil || *reason != "checker_unavailable" {
		t.Fatalf("%s check decided verdict=%v outcome=%v reason=%v, want failed/checker_unavailable", s.stage, verdict, outcome, reason)
	}
}

// Vendor/vendor: the claim succeeds with its vendor credential and the guard reads neither lead
// configs nor provider revisions, at both stages. The positive control is the provider pair below.
func TestSameModelGuardVendorVendorReadsNothingLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			s := newSMLive(t, stage)
			p := s.claim(t)
			s.wantDelivered(t, p, false)
			if s.rec.saw(smLiveLeadReads) || s.rec.saw(smLiveRevReads) {
				t.Fatalf("vendor/vendor claim read lead config=%v or provider revision=%v", s.rec.saw(smLiveLeadReads), s.rec.saw(smLiveRevReads))
			}
			if !s.rec.saw("RecordPlanCrossCheckClaim") && !s.rec.saw("RecordCodeCrossCheckClaim") {
				t.Fatal("recorder saw no claim write: the wrapper is not on the claim path")
			}
		})
	}
}

// Positive control: a provider lead (CP) with a provider checker (XP) does read both, and an
// independent pair is delivered with its provider binding.
func TestSameModelGuardProviderPairReadsLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			s := newSMLive(t, stage)
			s.stampChecker(t, smLiveCodexMap)
			s.stampLead(t, 1)
			s.leadRow(1, s.phase(), smStr("opus"), nil) // corp-opus against gateway: independent
			p := s.claim(t)
			s.wantDelivered(t, p, true)
			if !s.rec.saw(smLiveLeadReads) || !s.rec.saw(smLiveRevReads) {
				t.Fatalf("provider pair did not read lead config=%v revision=%v", s.rec.saw(smLiveLeadReads), s.rec.saw(smLiveRevReads))
			}
		})
	}
}

// The guard's outcomes settle through the actual claim: same model and every Unresolved shape decide
// the check unavailable; the lead-first path leaves the child failed, never paused.
func TestSameModelGuardSettlesLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, tc := range []struct {
			name  string
			setup func(s *smLive, t *testing.T)
		}{
			{"same model (CP lead against XP checker)", func(s *smLive, t *testing.T) {
				s.stampChecker(t, `{"default":{"model":"corp-sonnet"}}`)
				s.stampLead(t, 1)
				s.leadRow(1, s.phase(), smStr("sonnet"), nil)
			}},
			{"same model (CV lead observed id against XP checker)", func(s *smLive, t *testing.T) {
				s.stampChecker(t, `{"default":{"model":"claude-opus-5-5"}}`)
				s.leadRow(1, s.phase(), smStr("opus"), smStr("claude-opus-5-5"))
			}},
			{"missing lead row", func(s *smLive, t *testing.T) {
				s.stampChecker(t, smLiveCodexMap)
				s.stampLead(t, 1)
			}},
			{"lead row of another generation", func(s *smLive, t *testing.T) {
				s.stampChecker(t, smLiveCodexMap)
				s.stampLead(t, 1)
				s.leadRow(2, s.phase(), smStr("opus"), nil)
			}},
			{"lead row of another phase", func(s *smLive, t *testing.T) {
				s.stampChecker(t, smLiveCodexMap)
				s.stampLead(t, 1)
				other := "implement"
				if s.stage == "code" {
					other = "plan"
				}
				s.leadRow(1, other, smStr("opus"), nil)
			}},
			{"missing frozen lead revision", func(s *smLive, t *testing.T) {
				s.stampChecker(t, smLiveCodexMap)
				s.stampLead(t, 99)
				s.leadRow(1, s.phase(), smStr("opus"), nil)
			}},
			{"request-only provider lead row with an unmapped value", func(s *smLive, t *testing.T) {
				s.stampChecker(t, smLiveCodexMap)
				s.stampLead(t, 1)
				s.leadRow(1, s.phase(), smStr("not-in-the-map"), nil)
			}},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				s := newSMLive(t, stage)
				tc.setup(s, t)
				s.wantDecidedUnavailable(t, s.claim(t))
			})
		}
	}
}

// An early refusal (the frozen key was revoked or rotated before the claim, or the destination left
// the allowlist) decides the check checker_unavailable lead-first and never parks the child.
func TestProviderCheckerEarlyRefusalLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for name, refuse := range map[string]func(s *smLive){
			"key disabled": func(s *smLive) {
				s.env.exec(`UPDATE user_secrets SET disabled_at=now(), enablement_rev=enablement_rev+1 WHERE id=$1`, s.checkerKey)
			},
			"key rotated": func(s *smLive) {
				s.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, s.checkerKey)
			},
			"destination left the allowlist": func(s *smLive) { s.svc.p.ProviderAllowedBaseURLs = nil },
		} {
			t.Run(stage+"/"+name, func(t *testing.T) {
				s := newSMLive(t, stage)
				s.stampChecker(t, smLiveCodexMap)
				refuse(s)
				s.wantDecidedUnavailable(t, s.claim(t))
			})
		}
	}
}

// A late refusal: assembly succeeded with a provider-only payload, then the binding changed before
// the finish transaction's locked re-check. The check is decided unavailable lead-first, the child is
// never paused, and no payload is delivered.
func TestProviderCheckerLateRefusalLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for name, revoke := range map[string]func(s *smLive){
			"key disabled": func(s *smLive) {
				s.env.exec(`UPDATE user_secrets SET disabled_at=now(), enablement_rev=enablement_rev+1 WHERE id=$1`, s.checkerKey)
			},
			"key rotated": func(s *smLive) {
				s.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, s.checkerKey)
			},
		} {
			t.Run(stage+"/"+name, func(t *testing.T) {
				s := newSMLive(t, stage)
				s.stampChecker(t, smLiveCodexMap)
				s.leadRow(1, s.phase(), smStr("opus"), smStr("claude-opus-5-5")) // independent of the gateway model
				hookRan := false
				s.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
					if run.ID != s.child {
						return
					}
					if err != nil || p == nil || p.Secrets.Provider == nil || p.Secrets.Codex != nil || p.Secrets.AnthropicOAuthToken != "" {
						t.Errorf("assembly must succeed with a provider-only payload: payload=%v err=%v", p != nil, err)
					}
					hookRan = true
					revoke(s)
				}}
				p := s.claim(t)
				if !hookRan {
					t.Fatal("the afterAssembly hook never ran for the checker")
				}
				s.wantDecidedUnavailable(t, p)
			})
		}
	}
}

// The capability backstop (errProviderLaneClaimRefused, found at assembly on a worker lacking the
// checker capability) settles lead-first through the production finalizer, like a pin failure.
func TestProviderCheckerCapabilityBackstopLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			s := newSMLive(t, stage)
			s.stampChecker(t, smLiveCodexMap)
			s.env.exec(`UPDATE runs SET status='claimed', claim_generation=1, worker_id=$2 WHERE id=$1`, s.child, s.workerID)
			run := mustRun(t, s.env, s.child)
			p, err := s.svc.finishRunClaim(s.env.ctx, run, nil, errProviderLaneClaimRefused, claimRecoveryIdentity{workerID: s.workerID})
			if err != nil {
				t.Fatal(err)
			}
			s.wantDecidedUnavailable(t, p)
		})
	}
}
