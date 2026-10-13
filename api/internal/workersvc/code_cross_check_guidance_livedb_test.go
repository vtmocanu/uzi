package workersvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeCrossCheckGuidanceFrozenLiveDB(t *testing.T) {
	f := codeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	lead := mustRun(t, f.env, f.lead)
	repo := uuid.UUID(lead.RepoID.Bytes)
	guidance, instructions := "Verify the public contract.", "Use the repository conventions."
	enabled := true
	input := CodeCrossCheckInstructions{RepoInstructionsEnabled: &enabled, RepoInstructionsText: instructions}
	f.env.exec("UPDATE repos SET code_cross_check_guidance=$2,cross_check_repo_instructions_enabled=true WHERE id=$1", repo, guidance)
	consent, err := f.svc.CodeCrossCheckRepoInstructionsEnabled(f.env.ctx, w, f.lead, 1)
	if err != nil || !consent {
		t.Fatalf("fresh consent: %t %v", consent, err)
	}
	for _, worker := range []store.Worker{{ID: uuid.New(), UserID: w.UserID}, {ID: w.ID, UserID: uuid.New()}} {
		if _, err := f.svc.CodeCrossCheckRepoInstructionsEnabled(f.env.ctx, worker, f.lead, 1); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("foreign settings read: %v", err)
		}
	}
	if _, err := f.svc.CodeCrossCheckRepoInstructionsEnabled(f.env.ctx, w, f.lead, 2); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("stale settings read")
	}
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head, input)
	if err != nil {
		t.Fatal(err)
	}
	guidanceHash, instructionsHash := sha256.Sum256([]byte(guidance)), sha256.Sum256([]byte(instructions))
	if cc.GuidanceText.String != guidance || cc.GuidanceSnapshot.String != guidance || !cc.RepoInstructionsEnabled || cc.RepoInstructionsText.String != instructions || !bytes.Equal(cc.GuidanceDigest, guidanceHash[:]) || !bytes.Equal(cc.RepoInstructionsDigest, instructionsHash[:]) {
		t.Fatalf("frozen row: %+v", cc)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	claim := laneClaim(t, f, "cross_check", nil)
	if claim == nil || claim.CrossCheck == nil || claim.CrossCheck.GuidanceSnapshot != guidance || claim.CrossCheck.RepoInstructionsEnabled == nil || !*claim.CrossCheck.RepoInstructionsEnabled || claim.CrossCheck.RepoInstructionsText != instructions || claim.CrossCheck.GuidanceDigest != hex.EncodeToString(guidanceHash[:]) || claim.CrossCheck.RepoInstructionsDigest != hex.EncodeToString(instructionsHash[:]) {
		t.Fatalf("frozen claim: %+v", claim)
	}
	f.env.exec("UPDATE repos SET code_cross_check_guidance='Changed',cross_check_repo_instructions_enabled=false WHERE id=$1", repo)
	f.env.exec("UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1 WHERE user_id=$1 AND kind IN ('openai_api_key','codex_auth')", w.UserID)
	if _, err := f.svc.CodeCrossCheckRepoInstructionsEnabled(f.env.ctx, w, f.lead, 1); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("existing row authorized fresh read")
	}
	retry, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head, input)
	if err != nil || retry.ID != cc.ID || !bytes.Equal(retry.CandidateDigest, cc.CandidateDigest) {
		t.Fatalf("retry rebound: %v", err)
	}
	changed := input
	changed.RepoInstructionsText = "changed file"
	if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head, changed); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatalf("changed retry text: %v", err)
	}
	if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatalf("changed retry toggle: %v", err)
	}
	verdict, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, f.runID, claim.ClaimGeneration, "completed", "", nil)
	if err != nil || verdict.GuidanceText.String != guidance || verdict.RepoInstructionsText.String != instructions || !bytes.Equal(verdict.CandidateDigest, cc.CandidateDigest) {
		t.Fatalf("verdict rebound: %+v %v", verdict, err)
	}
	// Trigger rejects each independent rebind, including a consistent new text/hash.
	for _, sql := range []string{
		"UPDATE cross_checks SET guidance_snapshot='changed' WHERE id=$1",
		"UPDATE cross_checks SET guidance_text='changed',guidance_digest=sha256(convert_to('changed','UTF8')) WHERE id=$1",
		"UPDATE cross_checks SET repo_instructions_enabled=false,repo_instructions_text='',repo_instructions_digest=sha256(convert_to('','UTF8')) WHERE id=$1",
		"UPDATE cross_checks SET repo_instructions_text='changed',repo_instructions_digest=sha256(convert_to('changed','UTF8')) WHERE id=$1",
		"UPDATE cross_checks SET guidance_digest=sha256(convert_to('changed','UTF8')) WHERE id=$1",
		"UPDATE cross_checks SET repo_instructions_digest=sha256(convert_to('changed','UTF8')) WHERE id=$1",
	} {
		_, err := f.env.pool.Exec(f.env.ctx, sql, cc.ID)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "P0001" || !strings.Contains(pe.Message, "immutable code cross-check snapshot") {
			t.Fatalf("snapshot UPDATE: %s: %v", sql, err)
		}
	}
}

func TestCodeCrossCheckInstructionAdmissionLiveDB(t *testing.T) {
	for _, scenario := range []string{"off text", "settings race", "exact boundary", "over boundary", "invalid UTF8", "checker unavailable", "credential unavailable", "no snapshot authority"} {
		t.Run(scenario, func(t *testing.T) {
			f := codeFixture(t)
			w := wkrRow(t, f.env, f.workerID)
			lead := mustRun(t, f.env, f.lead)
			repo := uuid.UUID(lead.RepoID.Bytes)
			enabled := true
			input := CodeCrossCheckInstructions{RepoInstructionsEnabled: &enabled, RepoInstructionsText: strings.Repeat("é", 32768)}
			f.env.exec("UPDATE repos SET code_cross_check_guidance='Frozen checks',cross_check_repo_instructions_enabled=true WHERE id=$1", repo)
			switch scenario {
			case "off text":
				enabled = false
				f.env.exec("UPDATE repos SET cross_check_repo_instructions_enabled=false WHERE id=$1", repo)
			case "settings race":
				if consent, err := f.svc.CodeCrossCheckRepoInstructionsEnabled(f.env.ctx, w, f.lead, 1); err != nil || !consent {
					t.Fatalf("snapshot authorization: %v", err)
				}
				f.env.exec("UPDATE repos SET cross_check_repo_instructions_enabled=false WHERE id=$1", repo)
			case "over boundary":
				input.RepoInstructionsText += "a"
			case "invalid UTF8":
				input.RepoInstructionsText = string([]byte{255})
			case "checker unavailable":
				f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_code_v1') WHERE id=$1", w.ID)
				w = wkrRow(t, f.env, w.ID)
			case "credential unavailable":
				f.env.exec("UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1 WHERE user_id=$1 AND kind IN ('openai_api_key','codex_auth')", w.UserID)
			}
			var cc store.CrossCheck
			var err error
			if scenario == "no snapshot authority" {
				cc, err = f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, "", "", "snapshot_failed", input)
			} else {
				cc, err = f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40), input)
			}
			if scenario == "exact boundary" || scenario == "checker unavailable" || scenario == "credential unavailable" {
				if err != nil || cc.RepoInstructionsText.String != input.RepoInstructionsText || !cc.RepoInstructionsEnabled || cc.GuidanceText.String != "Frozen checks" {
					t.Fatalf("freeze: %+v %v", cc, err)
				}
				if scenario != "exact boundary" && (cc.CheckerRunID.Valid || cc.Outcome.String != "failed") {
					t.Fatal("unavailable child created")
				}
			} else {
				if !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatalf("admission: %v", err)
				}
				var count int
				if err := f.env.pool.QueryRow(f.env.ctx, "SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'", f.lead).Scan(&count); err != nil || count != 0 {
					t.Fatalf("refusal created child: %d %v", count, err)
				}
			}
		})
	}
}

func TestCodeCrossCheckLegacyCandidateBytesLiveDB(t *testing.T) {
	f := codeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	f.env.exec("UPDATE runs SET issue_title='Legacy title',issue_description='Legacy body',plan_md=NULL,milestones_frozen=NULL,required_capabilities='{}',required_tools='{}',size_class='m' WHERE id=$1", f.lead)
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	// Golden from the pre-U4 canonical candidate, independent of new input binding.
	const legacyDigest = "562f0bae72b173177bdb55a6317c1cdc2896d70f6e9d2d200b774adfc5e664f1"
	if hex.EncodeToString(cc.CandidateDigest) != legacyDigest {
		t.Fatalf("legacy candidate bytes drifted: %x", cc.CandidateDigest)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	claim := laneClaim(t, f, "cross_check", nil)
	if claim == nil || claim.CrossCheck == nil || claim.CrossCheck.CandidateDigest != legacyDigest {
		t.Fatalf("legacy claim: %+v", claim)
	}
}
