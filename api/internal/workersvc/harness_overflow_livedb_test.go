package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type overflowFix struct {
	*poolFix
	token uuid.UUID
	codex poolAccount
}

func newOverflowFix(t *testing.T) *overflowFix {
	t.Helper()
	fx := newPoolFix(t)
	fx.svc.SetForges(originDriftForges{f: &originDriftForge{}})
	fx.svc.p.Autoselect.MaxStaleness = 15 * time.Minute
	fx.svc.p.UsagePollInterval = 5 * time.Minute
	token := uuid.New()
	fx.env.exec(`INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with,is_default)
        VALUES ($1,$2,'anthropic_token','overflow-claude',$3,'master',true)`, token, fx.userID, []byte("ct"))
	c := fx.newAccount("overflow-codex")
	fx.makeDefault(c.alias)
	fx.pool(c.alias)
	fx.mode("auto")
	fx.env.exec(`UPDATE users SET harness_overflow_enabled=true,default_harness='claude' WHERE id=$1`, fx.userID)
	of := &overflowFix{poolFix: fx, token: token, codex: c}
	of.claudeReading(token, 90, "1 minute")
	fx.reading(c, 10)
	return of
}

func (fx *overflowFix) claudeReading(id uuid.UUID, pct int, ago string) {
	fx.t.Helper()
	fx.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id,user_id,five_hour_pct,seven_day_pct,
            seven_day_resets_at,source,synced_at,enablement_rev)
        SELECT id,user_id,$2,$2,now()+interval '2 days','usage_endpoint',now()-$3::interval,enablement_rev
        FROM user_secrets WHERE id=$1
        ON CONFLICT (user_secret_id) DO UPDATE SET five_hour_pct=EXCLUDED.five_hour_pct,
            seven_day_pct=EXCLUDED.seven_day_pct,seven_day_resets_at=EXCLUDED.seven_day_resets_at,
            synced_at=EXCLUDED.synced_at,enablement_rev=EXCLUDED.enablement_rev`, id, pct, ago)
}

func (fx *overflowFix) nextIssue() int64 {
	fx.t.Helper()
	fx.iid++
	seedEligibleIssue(fx.t, fx.env, fx.repoID, fx.iid)
	return fx.iid
}

func (fx *overflowFix) issue() (store.Run, error) {
	fx.t.Helper()
	return fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "overflow", nil, nil, false, nil, nil, nil, nil, nil, nil)
}

func TestHarnessOverflowIssueFreezeAndExplanationLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	run, err := fx.issue()
	if err != nil {
		t.Fatal(err)
	}
	got := mustRun(t, fx.env, run.ID)
	if got.Harness != "codex" || !got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != fx.codex.alias ||
		got.HarnessPreferred.String != "claude" || got.HarnessDecisionReason.String != "overflow" ||
		!got.HarnessPreferredScore.Valid || got.HarnessPreferredScore.Float64 != 10 ||
		!got.HarnessOtherScore.Valid || got.HarnessOtherScore.Float64 < 30 {
		t.Fatalf("unexpected decision/binding: harness=%s preferred=%v reason=%v scores=%v/%v alias=%v",
			got.Harness, got.HarnessPreferred, got.HarnessDecisionReason, got.HarnessPreferredScore, got.HarnessOtherScore, got.CodexSecretID)
	}
	// Moving both gauges and the preference leaves the committed decision and binding intact.
	fx.claudeReading(fx.token, 0, "1 minute")
	fx.reading(fx.codex, 99)
	fx.env.exec(`UPDATE users SET harness_overflow_enabled=false WHERE id=$1`, fx.userID)
	after := mustRun(t, fx.env, run.ID)
	if after.Harness != got.Harness || after.HarnessOtherScore != got.HarnessOtherScore ||
		after.HarnessDecisionReason != got.HarnessDecisionReason || after.CodexAccountKey != got.CodexAccountKey {
		t.Fatal("creation decision changed after gauge/preference update")
	}
}

func TestHarnessOverflowFreezeRollbackLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	failure := errors.New("overflow freeze failure")
	fx.svc.codexFreezeFn = func(context.Context, codexFreezeStore, uuid.UUID, uuid.UUID, uuid.UUID, string) error { return failure }
	if _, err := fx.issue(); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if fx.runs() != 0 {
		t.Fatal("failed freeze left a run")
	}
}

func TestHarnessOverflowCapturedCheckerFlagsLiveDB(t *testing.T) {
	for _, column := range []string{"plan_cross_check_enabled", "code_cross_check_enabled"} {
		t.Run(column, func(t *testing.T) {
			fx := newOverflowFix(t)
			// The hook runs after the policy SELECT, before the issue INSERT. The
			// concurrent connection's committed flip must not change captured flags.
			fx.svc.codexSelectLockedHook = func(context.Context) {
				if column == "plan_cross_check_enabled" {
					fx.env.exec(`UPDATE users SET plan_cross_check_enabled=true WHERE id=$1`, fx.userID)
				} else {
					fx.env.exec(`UPDATE users SET code_cross_check_enabled=true WHERE id=$1`, fx.userID)
				}
			}
			run, err := fx.svc.CreateAutopilotRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "overflow", nil)
			if err != nil {
				t.Fatal(err)
			}
			got := mustRun(t, fx.env, run.ID)
			if got.Harness != "codex" || got.HarnessDecisionReason.String != "overflow" ||
				got.PlanCrossCheckRequired || got.CodeCrossCheckRequired || !got.AutoApprove {
				t.Fatalf("capture raced: harness=%s reason=%v checker=%v/%v auto=%v",
					got.Harness, got.HarnessDecisionReason, got.PlanCrossCheckRequired, got.CodeCrossCheckRequired, got.AutoApprove)
			}
		})
	}
}

func TestHarnessOverflowRawCheckerExclusionsLiveDB(t *testing.T) {
	for _, plan := range []bool{true, false} {
		t.Run(map[bool]string{true: "plan", false: "code"}[plan], func(t *testing.T) {
			fx := newOverflowFix(t)
			fx.env.exec(`UPDATE users SET plan_cross_check_enabled=$2,code_cross_check_enabled=$3 WHERE id=$1`, fx.userID, plan, !plan)
			run, err := fx.issue() // manual never requires the plan checker, but RAW flag excludes it.
			if err != nil {
				t.Fatal(err)
			}
			if run.Harness != "claude" || run.HarnessDecisionReason.String != "excluded" || run.PlanCrossCheckRequired ||
				run.CodeCrossCheckRequired != !plan {
				t.Fatalf("raw checker exclusion: harness=%s reason=%v flags=%v/%v",
					run.Harness, run.HarnessDecisionReason, run.PlanCrossCheckRequired, run.CodeCrossCheckRequired)
			}
		})
	}
}

func TestHarnessOverflowFenceReplacementLiveDB(t *testing.T) {
	tests := []struct {
		name            string
		codexPreferred  bool
		replacementUtil float64
		wantHarness     string
		refuse          bool
	}{
		{"overflow qualifying", false, 20, "codex", false},
		{"overflow below floor", false, 75, "", true},
		{"overflow exhausted", false, 100, "", true},
		{"retained preferred", true, 20, "codex", false},
		{"retained preferred becomes overflow", true, 95, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOverflowFix(t)
			replacement := fx.newAccount("replacement")
			fx.pool(replacement.alias)
			fx.reading(replacement, tt.replacementUtil)
			if tt.codexPreferred {
				setDefaultHarness(t, fx.env, fx.userID, "codex")
				fx.claudeReading(fx.token, 10, "1 minute")
			}
			fx.markUnusableBeforeFence(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`)
			run, err := fx.issue()
			if tt.refuse {
				if !errors.Is(err, errHarnessOverflowFence) {
					t.Fatalf("error=%v want changed decision refusal", err)
				}
				if fx.runs() != 0 {
					t.Fatal("fence refusal left a run")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := mustRun(t, fx.env, run.ID)
			if got.Harness != tt.wantHarness || !got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != replacement.alias ||
				got.CodexAccountKey.String != accountKeyOf(fx.poolFix, replacement) {
				t.Fatalf("wrong frozen replacement: harness=%s alias=%v key=%v", got.Harness, got.CodexSecretID, got.CodexAccountKey)
			}
			score := got.HarnessOtherMeasured
			if tt.codexPreferred {
				score = got.HarnessPreferredMeasured
			}
			// Measure replacement through the shared scoring path; assert it differs
			// from the first pick without recomputing codexselect.Score's formula.
			if !score.Valid || score.Float64 >= 90 || score.Float64 < 30 {
				t.Fatalf("replacement evidence=%v", score)
			}
		})
	}
}

func TestHarnessOverflowCodexDefaultAndHoldsLiveDB(t *testing.T) {
	for _, scenario := range []string{"fresh", "stale", "cooldown", "spread", "limit", "unknown", "pool empty", "reserve unknown"} {
		t.Run(scenario, func(t *testing.T) {
			fx := newOverflowFix(t)
			fx.mode("default")
			switch scenario {
			case "stale":
				fx.readingRaw(fx.codex, poolBuckets(true, false, poolWindow(fiveHourSecs, 10, 100000), poolWindow(weekSecs, 10, 100000)), "2 hours")
			case "cooldown":
				fx.cooldown(fx.codex, time.Hour)
			case "spread":
				fx.cooldown(fx.codex, -30*time.Second)
				fx.env.exec(`UPDATE codex_account_admission_throttle SET spread_width_ms=60000 WHERE provider_account_id=$1`, fx.codex.account)
			case "limit":
				fx.limitReached(fx.codex, 1000)
			case "unknown":
				fx.env.exec(`DELETE FROM codex_account_rate_limits WHERE provider_account_id=$1`, fx.codex.account)
			case "pool empty":
				fx.mode("auto")
				fx.env.exec(`UPDATE user_secrets SET auto_eligible=false WHERE id=$1`, fx.codex.alias)
			case "reserve unknown":
				fx.reserve(20)
				fx.env.exec(`DELETE FROM codex_account_rate_limits WHERE provider_account_id=$1`, fx.codex.account)
			}
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			run = mustRun(t, fx.env, run.ID)
			if scenario == "fresh" {
				if run.Harness != "codex" || run.HarnessDecisionReason.String != "overflow" {
					t.Fatalf("fresh default: %s/%v", run.Harness, run.HarnessDecisionReason)
				}
			} else if run.Harness != "claude" || run.HarnessDecisionReason.String != "unknown_usage" {
				t.Fatalf("%s admitted destination or wrong reason: %s/%v", scenario, run.Harness, run.HarnessDecisionReason)
			}
			measured := pgtype.Float8{Float64: 90, Valid: true}
			held := scenario == "cooldown" || scenario == "spread" || scenario == "limit" || scenario == "reserve unknown"
			switch scenario {
			case "stale", "unknown", "reserve unknown":
				measured = pgtype.Float8{}
			case "limit":
				measured.Float64 = 0
			}
			comparison := pgtype.Float8{}
			if scenario == "fresh" {
				comparison = measured
			}
			if run.HarnessOtherScore != comparison || run.HarnessOtherMeasured != measured ||
				!run.HarnessOtherHeld.Valid || run.HarnessOtherHeld.Bool != held {
				t.Fatalf("%s destination evidence: comparison=%v want=%v measured=%v want=%v held=%v want=%v",
					scenario, run.HarnessOtherScore, comparison, run.HarnessOtherMeasured, measured, run.HarnessOtherHeld, held)
			}
		})
	}
}

func TestHarnessOverflowClaudeReadingsLiveDB(t *testing.T) {
	tests := []struct {
		name        string
		wantReason  string
		wantHarness string
	}{
		{"unpooled", "overflow", "claude"},
		{"stale without reserve", "unknown_usage", "codex"},
		{"missing", "unknown_usage", "codex"},
		{"rejected", "unknown_usage", "codex"},
		{"disabled", "excluded", "codex"},
		{"revision", "unknown_usage", "codex"},
		{"all reserve held", "preferred", "codex"},
		{"mixed held unknown", "unknown_usage", "codex"},
		{"destination minimum", "preferred", "codex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOverflowFix(t)
			setDefaultHarness(t, fx.env, fx.userID, "codex")
			fx.reading(fx.codex, 95)
			fx.claudeReading(fx.token, 10, "1 minute")
			switch tt.name {
			case "stale without reserve":
				fx.claudeReading(fx.token, 10, "2 hours")
			case "missing":
				fx.env.exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id=$1`, fx.token)
			case "rejected":
				fx.env.exec(`UPDATE user_secrets SET anthropic_rejected_at=now() WHERE id=$1`, fx.token)
			case "disabled":
				fx.env.exec(`UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, fx.token)
			case "revision":
				fx.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, fx.token)
			case "all reserve held":
				fx.reserve(20)
				fx.claudeReading(fx.token, 90, "1 minute")
			case "mixed held unknown", "destination minimum":
				second := uuid.New()
				fx.env.exec(`INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with)
                VALUES ($1,$2,'anthropic_token','second',$3,'master')`, second, fx.userID, []byte("ct"))
				fx.claudeReading(fx.token, 90, "1 minute")
				if tt.name == "mixed held unknown" {
					fx.reserve(20)
					fx.svc.p.Autoselect.MaxStaleness = 5 * time.Minute
					fx.claudeReading(second, 10, "10 minutes")
				} else {
					fx.claudeReading(second, 10, "1 minute")
				}
			}
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			if run.Harness != tt.wantHarness || run.HarnessDecisionReason.String != tt.wantReason {
				t.Fatalf("%s: harness=%s reason=%v", tt.name, run.Harness, run.HarnessDecisionReason)
			}
		})
	}
}

func TestHarnessOverflowReadersUsageIndependentLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	check := func() {
		t.Helper()
		h, e := fx.svc.ResolveHarnessForUser(fx.env.ctx, fx.userID, nil)
		if e != nil || h != HarnessClaude {
			t.Fatalf("user resolver=%s/%v", h, e)
		}
		h, e = fx.svc.ResolveHarnessForUserOn(fx.env.ctx, fx.env.q, fx.userID, nil)
		if e != nil || h != HarnessClaude {
			t.Fatalf("on resolver=%s/%v", h, e)
		}
		h, e = fx.svc.ResolveSettingsHarness(fx.env.ctx, fx.userID)
		if e != nil || h != HarnessClaude {
			t.Fatalf("settings resolver=%s/%v", h, e)
		}
		preferred := HarnessCodex
		h, e = fx.svc.ResolveSettingsHarnessWithDefault(fx.env.ctx, fx.userID, &preferred)
		if e != nil || h != HarnessCodex {
			t.Fatalf("proposed resolver=%s/%v", h, e)
		}
		both, e := fx.svc.BothHarnessesUsable(fx.env.ctx, fx.userID)
		if e != nil || !both {
			t.Fatalf("both=%v/%v", both, e)
		}
	}
	check()
	fx.claudeReading(fx.token, 0, "1 minute")
	fx.reading(fx.codex, 99)
	check()
}

func TestHarnessOverflowMissingTransactionLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	fx.svc.txBeginner = nil
	if _, err := fx.issue(); !errors.Is(err, errHarnessOverflowRequiresTx) {
		t.Fatalf("error=%v", err)
	}
	if fx.runs() != 0 {
		t.Fatal("nontransactional create left a run")
	}
}

// Bare store callers retain the legacy flag subqueries unless captured flags are
// supplied. The production issue path supplies BOTH from GetHarnessCreationPolicy.
func TestHarnessOverflowClaudeAllUnknownReserveHeldLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	fx.reserve(20)
	fx.env.exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id=$1`, fx.token)
	run, err := fx.issue()
	if err != nil {
		t.Fatal(err)
	}
	if run.Harness != "codex" || run.HarnessDecisionReason.String != "overflow" ||
		!run.HarnessPreferredHeld.Bool || !run.HarnessPreferredScore.Valid ||
		run.HarnessPreferredScore.Float64 != 0 || run.HarnessPreferredMeasured.Valid {
		t.Fatalf("reserve unknown must compare as zero separately from measurement: %s/%v/%v/%v/%v",
			run.Harness, run.HarnessDecisionReason, run.HarnessPreferredHeld, run.HarnessPreferredScore, run.HarnessPreferredMeasured)
	}
}

func TestHarnessOverflowCodexPreferredDefaultLiveDB(t *testing.T) {
	for _, scenario := range []string{"fresh low", "stale", "cooldown", "limit", "reserve unknown", "API key"} {
		t.Run(scenario, func(t *testing.T) {
			fx := newOverflowFix(t)
			fx.mode("default")
			setDefaultHarness(t, fx.env, fx.userID, "codex")
			fx.claudeReading(fx.token, 10, "1 minute")
			fx.reading(fx.codex, 95)
			want := "claude"
			switch scenario {
			case "stale":
				fx.readingRaw(fx.codex, poolBuckets(true, false, poolWindow(fiveHourSecs, 95, 100000), poolWindow(weekSecs, 95, 100000)), "2 hours")
				want = "codex"
			case "cooldown":
				fx.reading(fx.codex, 10)
				fx.cooldown(fx.codex, time.Hour)
			case "limit":
				fx.limitReached(fx.codex, 1000)
			case "reserve unknown":
				fx.reserve(20)
				fx.env.exec(`DELETE FROM codex_account_rate_limits WHERE provider_account_id=$1`, fx.codex.account)
			case "API key":
				fx.env.exec(`UPDATE user_secrets SET is_default=false WHERE id=$1`, fx.codex.alias)
				key := fx.env.seedStaticAPIKey(t, fx.userID, "unmetered", codexToken("sk"))
				fx.makeDefault(key)
				want = "codex"
			}
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			run = mustRun(t, fx.env, run.ID)
			if run.Harness != want {
				t.Fatalf("%s harness=%s want %s", scenario, run.Harness, want)
			}
			if scenario == "stale" || scenario == "API key" {
				if run.HarnessDecisionReason.String != "unknown_usage" {
					t.Fatalf("unknown reason=%v", run.HarnessDecisionReason)
				}
			} else if run.HarnessDecisionReason.String != "overflow" {
				t.Fatalf("expected overflow: %v", run.HarnessDecisionReason)
			}
			if scenario == "cooldown" || scenario == "limit" || scenario == "reserve unknown" {
				measured := pgtype.Float8{Float64: 90, Valid: true}
				switch scenario {
				case "limit":
					measured.Float64 = 0
				case "reserve unknown":
					measured = pgtype.Float8{}
				}
				if !run.HarnessPreferredHeld.Valid || !run.HarnessPreferredHeld.Bool ||
					run.HarnessPreferredScore != (pgtype.Float8{Float64: 0, Valid: true}) ||
					run.HarnessPreferredMeasured != measured {
					t.Fatalf("%s preferred evidence: comparison=%v measured=%v want=%v held=%v",
						scenario, run.HarnessPreferredScore, run.HarnessPreferredMeasured, measured, run.HarnessPreferredHeld)
				}
			}
		})
	}
}

func TestHarnessOverflowCreationOverrideInheritanceLiveDB(t *testing.T) {
	for _, mode := range []string{CredentialOverrideModeInherit, CredentialOverrideModeAuto, CredentialOverrideModeDefault} {
		t.Run(mode, func(t *testing.T) {
			fx := newOverflowFix(t)
			run, err := fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "override", nil, nil, false, nil, nil, nil, &RawCredentialOverride{Mode: mode}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := "excluded"
			if mode == CredentialOverrideModeInherit {
				want = "overflow"
			}
			if run.HarnessDecisionReason.String != want {
				t.Fatalf("mode=%s reason=%v", mode, run.HarnessDecisionReason)
			}
		})
	}
}

func TestHarnessOverflowInheritRefusalCompatibilityLiveDB(t *testing.T) {
	for _, scenario := range []string{"off", "explicit Codex", "preferred Codex"} {
		t.Run(scenario, func(t *testing.T) {
			fx := newOverflowFix(t)
			var explicit *Harness
			switch scenario {
			case "off":
				fx.env.exec("UPDATE users SET harness_overflow_enabled=false,default_harness='codex' WHERE id=$1", fx.userID)
			case "explicit Codex":
				h := HarnessCodex
				explicit = &h
			case "preferred Codex":
				setDefaultHarness(t, fx.env, fx.userID, "codex")
			}
			_, err := fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(),
				"inherit", nil, nil, false, nil, nil, explicit, &RawCredentialOverride{Mode: CredentialOverrideModeInherit}, nil, nil)
			if !errors.Is(err, ErrCredentialOverrideHarnessUnsupported) {
				t.Fatalf("legacy refusal changed: %v", err)
			}
			if fx.runs() != 0 {
				t.Fatal("refused override left a run")
			}
		})
	}
}

func TestHarnessOverflowUnavailableDefaultPreservesPreferredLiveDB(t *testing.T) {
	for _, preference := range []string{"claude", "codex"} {
		for _, state := range []string{"quarantined", "reauth_required"} {
			t.Run(preference+"/"+state, func(t *testing.T) {
				fx := newOverflowFix(t)
				fx.mode("default")
				setDefaultHarness(t, fx.env, fx.userID, preference)
				if state == "quarantined" {
					fx.env.exec("UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1", fx.codex.account)
				} else {
					fx.env.exec("UPDATE codex_provider_account SET reauth_required=true,reauth_generation=1,reauth_credential_revision=1 WHERE id=$1", fx.codex.account)
				}
				run, err := fx.issue()
				if err != nil {
					t.Fatal(err)
				}
				wantReason := "excluded"
				if preference == "codex" {
					wantReason = "preferred"
					if state == "reauth_required" {
						wantReason = "unknown_usage"
					}
				}
				if run.Harness != preference || run.HarnessDecisionReason.String != wantReason {
					t.Fatalf("unavailable default changed preferred choice: harness=%s reason=%v", run.Harness, run.HarnessDecisionReason)
				}
			})
		}
	}
}

func TestHarnessOverflowQuarantinedPreferredUsesMeasuredScoreLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	fx.mode("default")
	setDefaultHarness(t, fx.env, fx.userID, "codex")
	fx.reading(fx.codex, 95)
	fx.claudeReading(fx.token, 10, "1 minute")
	fx.env.exec("UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1", fx.codex.account)
	run, err := fx.issue()
	if err != nil {
		t.Fatal(err)
	}
	if run.Harness != "claude" || run.HarnessDecisionReason.String != "overflow" ||
		!run.HarnessPreferredScore.Valid || run.HarnessPreferredScore.Float64 >= 20 ||
		run.HarnessPreferredHeld.Bool {
		t.Fatalf("preferred score was discarded: harness=%s reason=%v score=%v held=%v",
			run.Harness, run.HarnessDecisionReason, run.HarnessPreferredScore, run.HarnessPreferredHeld)
	}
}

func TestHarnessOverflowMigrationDefaultsAndConstraintsLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	policy, err := fx.env.q.GetHarnessCreationPolicy(fx.env.ctx, fx.userID)
	if err != nil || policy.HarnessOverflowEnabled {
		t.Fatalf("migration must default off: policy=%+v err=%v", policy, err)
	}
	account := fx.newAccount("migration-check")
	fx.makeDefault(account.alias)
	run := fx.mustCreate()
	for _, query := range []string{
		"UPDATE runs SET harness_decision_reason='other' WHERE id=$1",
		"UPDATE runs SET harness_preferred_score=101 WHERE id=$1",
		"UPDATE runs SET harness_other_measured=-1 WHERE id=$1",
		"UPDATE runs SET harness_preferred=NULL WHERE id=$1",
	} {
		if _, err := fx.env.pool.Exec(fx.env.ctx, query, run.ID); err == nil {
			t.Fatalf("invalid decision accepted: %s", query)
		}
	}
}

func TestHarnessOverflowCodexSelectorOutcomesLiveDB(t *testing.T) {
	for _, scenario := range []string{"auto", "best_of_pool", "pool_stale", "pool_exhausted", "not_allowed"} {
		t.Run(scenario, func(t *testing.T) {
			fx := newOverflowFix(t)
			switch scenario {
			case "best_of_pool":
				fx.svc.p.Autoselect.MinHeadroom = 95
			case "pool_stale":
				fx.env.exec("DELETE FROM codex_account_rate_limits WHERE provider_account_id=$1", fx.codex.account)
			case "pool_exhausted":
				setDefaultHarness(t, fx.env, fx.userID, "codex")
				fx.claudeReading(fx.token, 10, "1 minute")
				// Measured 90 points, but the selected preferred account is held.
				fx.cooldown(fx.codex, time.Hour)
			case "not_allowed":
				fx.readingRaw(fx.codex, poolBuckets(false, false,
					poolWindow(fiveHourSecs, 10, 100000), poolWindow(weekSecs, 10, 100000)), "1 minute")
			}
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			run = mustRun(t, fx.env, run.ID)
			switch scenario {
			case "auto", "best_of_pool":
				if run.Harness != "codex" || run.HarnessDecisionReason.String != "overflow" ||
					run.CodexSelectReason.String != scenario {
					t.Fatalf("qualifying destination: harness=%s reason=%v alias reason=%v",
						run.Harness, run.HarnessDecisionReason, run.CodexSelectReason)
				}
			case "pool_exhausted":
				if run.Harness != "claude" || run.HarnessDecisionReason.String != "overflow" ||
					!run.HarnessPreferredHeld.Bool || !run.HarnessPreferredScore.Valid ||
					run.HarnessPreferredScore.Float64 != 0 || !run.HarnessPreferredMeasured.Valid ||
					run.HarnessPreferredMeasured.Float64 < 20 {
					t.Fatalf("held preferred must overflow with measured evidence: harness=%s reason=%v comparison=%v measured=%v",
						run.Harness, run.HarnessDecisionReason, run.HarnessPreferredScore, run.HarnessPreferredMeasured)
				}
			default:
				measured := pgtype.Float8{}
				held := scenario == "not_allowed"
				if held {
					measured = pgtype.Float8{Float64: 90, Valid: true}
				}
				if run.Harness != "claude" || run.HarnessDecisionReason.String != "unknown_usage" ||
					run.HarnessOtherScore.Valid || run.HarnessOtherMeasured != measured ||
					!run.HarnessOtherHeld.Valid || run.HarnessOtherHeld.Bool != held {
					t.Fatalf("disqualified destination: harness=%s reason=%v comparison=%v measured=%v want=%v held=%v",
						run.Harness, run.HarnessDecisionReason, run.HarnessOtherScore, run.HarnessOtherMeasured, measured, run.HarnessOtherHeld)
				}
			}
		})
	}
}

func TestHarnessOverflowClaudeReserveDepletionLiveDB(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "held measured", true: "held and unknown"}[unknown], func(t *testing.T) {
			fx := newOverflowFix(t)
			fx.reserve(20)
			fx.claudeReading(fx.token, 50, "1 minute")
			if unknown {
				second := uuid.New()
				fx.env.exec("INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,'anthropic_token','unknown',$3,'master')",
					second, fx.userID, []byte("ct"))
			}
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			if run.Harness != "codex" || run.HarnessDecisionReason.String != "overflow" ||
				!run.HarnessPreferredHeld.Bool || !run.HarnessPreferredScore.Valid ||
				run.HarnessPreferredScore.Float64 != 0 {
				t.Fatalf("reserve depletion: harness=%s reason=%v held=%v comparison=%v",
					run.Harness, run.HarnessDecisionReason, run.HarnessPreferredHeld, run.HarnessPreferredScore)
			}
			if unknown {
				if run.HarnessPreferredMeasured.Valid {
					t.Fatal("missing measurement invented")
				}
			} else if !run.HarnessPreferredMeasured.Valid || run.HarnessPreferredMeasured.Float64 != 50 {
				t.Fatalf("held measurement lost: %v", run.HarnessPreferredMeasured)
			}
		})
	}
}

func TestHarnessOverflowBareCreateCompatibilityLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	fx.env.exec(`UPDATE users SET plan_cross_check_enabled=true,code_cross_check_enabled=true WHERE id=$1`, fx.userID)
	for _, seeded := range []bool{false, true} {
		source := planSourceAgent
		var md pgtype.Text
		if seeded {
			source = planSourceSeeded
			md = pgtype.Text{String: "plan", Valid: true}
		}
		fx.iid++
		run, err := fx.env.q.CreateRun(fx.env.ctx, store.CreateRunParams{
			UserID: fx.userID, RepoID: fx.repoID, IssueIid: pgtype.Int8{Int64: fx.iid, Valid: true},
			IssueTitle: "bare", IssueDescription: "bare", PlanSource: source, PlanMd: md,
			TriggerSource: "manual", Harness: "claude", AutoApprove: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if run.PlanCrossCheckRequired == seeded || !run.CodeCrossCheckRequired || run.HarnessDecisionReason.Valid {
			t.Fatalf("bare compatibility: seeded=%v checker=%v/%v decision=%v", seeded, run.PlanCrossCheckRequired, run.CodeCrossCheckRequired, run.HarnessDecisionReason)
		}
	}
}
