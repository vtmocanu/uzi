package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func TestHarnessOverflowScheduledCreationLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	run, err := fx.svc.CreateScheduledRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(),
		"scheduled overflow", nil, nil, nil, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := mustRun(t, fx.env, run.ID)
	if got.TriggerSource != "schedule" || got.Harness != "codex" ||
		got.HarnessPreferred.String != "claude" || got.HarnessDecisionReason.String != "overflow" ||
		!got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != fx.codex.alias ||
		!got.CodexAccountKey.Valid || got.CodexAccountKey.String != accountKeyOf(fx.poolFix, fx.codex) {
		t.Fatalf("scheduled decision/binding: %+v", got)
	}
}

func TestHarnessOverflowPublicCreationExclusionsLiveDB(t *testing.T) {
	for _, scenario := range []string{"manual explicit", "manual credential", "schedule explicit", "schedule model", "schedule credential"} {
		t.Run(scenario, func(t *testing.T) {
			fx := newOverflowFix(t)
			claude := HarnessClaude
			model := "claude-sonnet-4-6"
			var run store.Run
			var err error
			switch scenario {
			case "manual explicit":
				run, err = fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(),
					"explicit", nil, nil, false, nil, nil, &claude, nil, nil, nil)
			case "manual credential":
				run, err = fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(),
					"credential", nil, nil, false, nil, nil, nil,
					&RawCredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &fx.token}, nil, nil)
			default:
				var explicit *Harness
				var pinnedModel *string
				var override *CredentialOverride
				switch scenario {
				case "schedule explicit":
					explicit = &claude
				case "schedule model":
					pinnedModel = &model
				case "schedule credential":
					override = &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &fx.token}
				}
				run, err = fx.svc.CreateScheduledRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(),
					"excluded schedule", nil, nil, pinnedModel, false, nil, override, explicit)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := mustRun(t, fx.env, run.ID)
			if got.Harness != "claude" || got.HarnessPreferred.String != "claude" ||
				got.HarnessDecisionReason.String != "excluded" || got.CodexSecretID.Valid {
				t.Fatalf("excluded creation: harness=%s preferred=%v reason=%v codex=%v",
					got.Harness, got.HarnessPreferred, got.HarnessDecisionReason, got.CodexSecretID)
			}
			if scenario == "schedule model" && (!got.Model.Valid || got.Model.String != model) {
				t.Fatalf("model pin lost: %v", got.Model)
			}
			if scenario == "manual credential" || scenario == "schedule credential" {
				if got.CredentialOverrideMode.String != CredentialOverrideModePinned ||
					!got.CredentialOverrideSecretID.Valid || uuid.UUID(got.CredentialOverrideSecretID.Bytes) != fx.token {
					t.Fatalf("credential pin lost: %v/%v", got.CredentialOverrideMode, got.CredentialOverrideSecretID)
				}
			}
		})
	}
}

func TestHarnessOverflowClaudeLoadIndependentLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	fx.mode("default") // Keep Codex account ranking load out of this comparison.
	second := uuid.New()
	fx.env.exec("INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,'anthropic_token','lower-headroom',$3,'master')",
		second, fx.userID, []byte("ct"))
	fx.claudeReading(second, 95, "1 minute")
	check := func(wantLoad int64) store.Run {
		t.Helper()
		rows, err := fx.env.q.ListAutoSelectCandidates(fx.env.ctx, fx.userID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("Claude query rows=%d want 2", len(rows))
		}
		for _, row := range rows {
			wantPct, wantInFlight := int16(95), int64(0)
			switch row.UserSecretID {
			case fx.token:
				wantPct, wantInFlight = 90, wantLoad
			case second:
			default:
				t.Fatalf("unexpected Claude token: %s", row.UserSecretID)
			}
			if row.InFlightRuns != wantInFlight || !row.FiveHourPct.Valid ||
				row.FiveHourPct.Int16 != wantPct || !row.SevenDayPct.Valid || row.SevenDayPct.Int16 != wantPct {
				t.Fatalf("Claude query evidence at load %d: %+v", wantLoad, row)
			}
		}
		run, err := fx.issue()
		if err != nil {
			t.Fatal(err)
		}
		got := mustRun(t, fx.env, run.ID)
		if got.Harness != "codex" || got.HarnessDecisionReason.String != "overflow" ||
			!got.HarnessPreferredScore.Valid || got.HarnessPreferredScore.Float64 != 10 ||
			!got.HarnessPreferredMeasured.Valid || got.HarnessPreferredMeasured.Float64 != 10 ||
			!got.HarnessPreferredHeld.Valid || got.HarnessPreferredHeld.Bool {
			t.Fatalf("Claude evidence at load %d: %s/%v comparison=%v measured=%v held=%v",
				wantLoad, got.Harness, got.HarnessDecisionReason, got.HarnessPreferredScore,
				got.HarnessPreferredMeasured, got.HarnessPreferredHeld)
		}
		return got
	}
	before := check(0)
	// Two active Claude sessions spend this exact token; neither changes its gauges.
	fx.env.exec(`INSERT INTO runs (user_id,kind,issue_title,issue_description,harness,status,anthropic_secret_id)
		SELECT $1,'chat','active Claude','load','claude','running',$2 FROM generate_series(1,2)`,
		fx.userID, fx.token)
	after := check(2)
	if before.HarnessPreferredScore != after.HarnessPreferredScore ||
		before.HarnessPreferredMeasured != after.HarnessPreferredMeasured {
		t.Fatal("Claude load changed stored headroom")
	}
}

func TestHarnessOverflowCodexInvalidReadingLiveDB(t *testing.T) {
	for _, mode := range []string{"default", "auto"} {
		for _, scenario := range []string{"aged", "polling disabled", "enablement revision", "generation write fence", "credential write fence", "pre reset", "vault locked"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				fx := newOverflowFix(t)
				fx.mode(mode)
				switch scenario {
				case "aged":
					fx.readingRaw(fx.codex, poolBuckets(true, false,
						poolWindow(fiveHourSecs, 10, 100000), poolWindow(weekSecs, 10, 100000)), "2 hours")
				case "polling disabled":
					fx.svc.p.CodexUsagePollInterval = 0
				case "enablement revision":
					fx.env.exec("UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1", fx.codex.alias)
				case "generation write fence", "credential write fence":
					// A rejected poll must not supply destination evidence.
					fx.env.exec("DELETE FROM codex_account_rate_limits WHERE provider_account_id=$1", fx.codex.account)
					generation, revision := int64(-1), int64(0)
					if scenario == "credential write fence" {
						generation, revision = 0, -1
					}
					n, err := fx.env.q.UpsertCodexAccountRateLimits(fx.env.ctx, store.UpsertCodexAccountRateLimitsParams{
						UserID: fx.userID, ProviderAccountID: fx.codex.account,
						Buckets:            []byte(poolBuckets(true, false, poolWindow(fiveHourSecs, 10, 100000), poolWindow(weekSecs, 10, 100000))),
						ObservedGeneration: generation, ObservedCredentialRevision: revision,
						AttemptStatus: "ok", EnablementSig: fx.codex.alias.String() + ":0",
					})
					if err != nil || n != 0 {
						t.Fatalf("stale poll accepted: rows=%d err=%v", n, err)
					}
				case "pre reset":
					// Bind the reset to a fixed selection clock, with the successful
					// reading before reset and selection after it; no sleeps.
					now := time.Now()
					fx.svc.now = func() time.Time { return now }
					fx.readingRaw(fx.codex, poolBuckets(true, false,
						poolWindow(fiveHourSecs, 20, now.Unix()-poolEpoch-30),
						poolWindow(weekSecs, 10, 100000)), "1 minute")
				case "vault locked":
					// Enrollment converts all existing secrets, including Claude's
					// gauge-only fixture, so give that row real master ciphertext.
					sealed, err := fx.env.box.Seal([]byte("overflow-claude-fixture"))
					if err != nil {
						t.Fatal(err)
					}
					fx.env.exec("UPDATE user_secrets SET ciphertext=$1 WHERE id=$2", sealed, fx.token)
					vlt := vault.NewPostgres(fx.env.box, fx.env.pool)
					fx.svc.SetVault(vlt)
					enrollCodexProtectedVault(t, fx.env, vlt, fx.userID, codexVaultLockedTestPassword)
					vlt.Lock(fx.userID)
					if vlt.Unlocked(fx.userID) {
						t.Fatal("vault still unlocked")
					}
				}
				run, err := fx.issue()
				if err != nil {
					t.Fatal(err)
				}
				got := mustRun(t, fx.env, run.ID)
				if got.Harness != "claude" || got.HarnessDecisionReason.String != "unknown_usage" ||
					got.HarnessOtherScore.Valid || got.HarnessOtherMeasured.Valid ||
					!got.HarnessOtherHeld.Valid || got.HarnessOtherHeld.Bool || got.CodexSecretID.Valid ||
					!got.HarnessPreferredScore.Valid || got.HarnessPreferredScore.Float64 != 10 {
					t.Fatalf("invalid Codex receiver: harness=%s reason=%v comparison=%v measured=%v held=%v",
						got.Harness, got.HarnessDecisionReason, got.HarnessOtherScore, got.HarnessOtherMeasured, got.HarnessOtherHeld)
				}
			})
		}
	}
}
