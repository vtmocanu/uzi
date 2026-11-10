package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestEffectiveSettingsSelfImproveCredentialMatchesClaimLiveDB pins PredictSelfImproveCredential
// (PRD #3254) to the real claim ladder: a self_improve run resolves its credential through
// claimSecretID, which follows the owner's judge binding. For each judge bind mode the prediction's
// source is compared with the literal expectation AND with the choice claimSecretID returns for a
// real self_improve run. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run
// via ./e2e/run-store-it.sh.
func TestEffectiveSettingsSelfImproveCredentialMatchesClaimLiveDB(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(fx *effFix, other uuid.UUID)
		wantSource string
		// what the real claim chose: a pinned token id, any pooled token, or the owner default (nil id)
		wantPinned, wantSome bool
		wantReason           string
		// whether the binding is auto, which the preview uses to decide token_fixed
		wantAuto bool
	}{
		{
			name: "judge pinned to a token", wantSource: "token_override", wantPinned: true, wantReason: "judge",
			setup: func(fx *effFix, other uuid.UUID) {
				fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = $2 WHERE id = $1`, fx.userID, other)
			},
		},
		{
			name: "judge pinned with the token since deleted", wantSource: "owner_default", wantReason: "default",
			setup: func(fx *effFix, _ uuid.UUID) {
				fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'pinned', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
			},
		},
		{
			name: "judge auto with a pooled token", wantSource: "auto_selection", wantSome: true, wantReason: "auto", wantAuto: true,
			setup: func(fx *effFix, _ uuid.UUID) {
				fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'auto', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
				fx.env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, fx.token)
				fx.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id,user_id,five_hour_pct,seven_day_pct,
					seven_day_resets_at,source,synced_at,enablement_rev)
					SELECT id,user_id,10,10,now()+interval '2 days','usage_endpoint',now()-interval '1 minute',enablement_rev
					FROM user_secrets WHERE id=$1`, fx.token)
			},
		},
		{
			name: "judge auto with pooled tokens and no reading takes the floor", wantSource: "auto_selection", wantSome: true, wantReason: "pool_stale", wantAuto: true,
			setup: func(fx *effFix, _ uuid.UUID) {
				fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'auto', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
				fx.env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, fx.token)
			},
		},
		{
			name: "judge auto with an empty pool falls back to the default", wantSource: "owner_default", wantReason: "pool_empty", wantAuto: true,
			setup: func(fx *effFix, _ uuid.UUID) {
				fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'auto', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
				fx.env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE user_id = $1`, fx.userID)
			},
		},
		{
			name: "judge default", wantSource: "owner_default", wantReason: "default",
			setup: func(fx *effFix, _ uuid.UUID) {
				fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'default', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newEffFix(t)
			fx.anthropic()
			other := uuid.New()
			sealed, err := fx.env.box.Seal([]byte(codexToken("anthropic-second")))
			if err != nil {
				t.Fatal(err)
			}
			fx.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
				VALUES ($1, $2, 'anthropic_token', $3, false, $4, 'master')`, other, fx.userID, "eff-second-"+other.String()[:8], sealed)
			c.setup(fx, other)

			pred, auto, err := fx.svc.PredictSelfImproveCredential(fx.env.ctx, fx.userID)
			if err != nil {
				t.Fatal(err)
			}
			if auto != c.wantAuto {
				t.Errorf("auto binding = %v, want %v", auto, c.wantAuto)
			}
			if pred.Kind != "anthropic" || pred.Source != c.wantSource || pred.FixedAt != "claim" {
				t.Fatalf("prediction = %+v, want anthropic %s fixed at claim", pred, c.wantSource)
			}

			runID := uuid.New()
			fx.iid++
			fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
				VALUES ($1, $2, $3, 'self_improve', $4, 't', 'd', 'queued')`, runID, fx.userID, fx.repoID, fx.iid)
			run := mustRun(t, fx.env, runID)
			if run.Kind != runkind.SelfImprove {
				t.Fatalf("run kind = %q", run.Kind)
			}
			choice, err := fx.svc.claimSecretID(fx.env.ctx, store.Worker{ID: fx.workerID, UserID: fx.userID}, run)
			if err != nil {
				t.Fatalf("claimSecretID: %v", err)
			}
			if choice.reason != c.wantReason {
				t.Errorf("claim reason = %q, want %q", choice.reason, c.wantReason)
			}
			switch {
			case c.wantPinned:
				if choice.secretID == nil || *choice.secretID != other {
					t.Errorf("claim spent %v, want the pinned judge token", choice.secretID)
				}
			case c.wantSome:
				if choice.secretID == nil {
					t.Errorf("claim chose the owner default, want a pooled token")
				}
			default:
				if choice.secretID != nil {
					t.Errorf("claim spent %v, want the owner default (no id)", choice.secretID)
				}
			}
		})
	}
}
