package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

// A label-selected Codex run must pass through the actual atomic insert/freeze seam.
func TestIssueLabelCodexStampingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	seedAnthropicToken(t, env, userID)
	setDefaultHarness(t, env, userID, string(HarnessClaude))
	svc := atomicSvc(env)
	for n, origin := range []string{"scheduled", "scheduled_auto", "poller"} {
		t.Run(origin, func(t *testing.T) {
			iid := int64(94000 + n)
			seedEligibleIssue(t, env, repoID, iid)
			// The cached harness disagrees with the fresh capture in every automatic origin.
			env.exec(`UPDATE issues SET labels='["uzi","harness::claude"]' WHERE repo_id=$1 AND forge_issue_iid=$2`, repoID, iid)
			var projectID int64
			if err := env.pool.QueryRow(env.ctx, "SELECT forge_project_id FROM repos WHERE id=$1", repoID).Scan(&projectID); err != nil {
				t.Fatal(err)
			}
			capture := &issueinput.Capture{ProjectID: projectID, Issue: forge.Issue{IID: iid, Labels: []string{"uzi", "harness::codex"}, Description: "fresh"}, Digest: issueinput.Digest("", "fresh"), Thread: &issueinput.Thread{Version: issueinput.SnapshotVersion, Comments: []issueinput.Comment{}}}
			selection, err := ResolveIssueHarness(capture, projectID, iid, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := issueinput.WithCapture(env.ctx, capture)
			var runID uuid.UUID
			switch origin {
			case "scheduled":
				run, err := svc.CreateScheduledRun(ctx, userID, repoID, iid, "fresh", nil, nil, nil, false, nil, nil, selection.Explicit)
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
			case "scheduled_auto":
				run, err := svc.CreateScheduledAutopilotRun(ctx, userID, repoID, iid, "fresh", nil, nil, nil, false, nil, selection.Explicit)
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
			case "poller":
				// A Codex-only owner must also reach the authoritative create seam.
				env.exec("DELETE FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", userID)
				if _, err := svc.ResolveHarnessForUser(ctx, userID, selection.Explicit); err != nil {
					t.Fatal(err)
				}
				run, err := svc.CreateAutopilotRun(ctx, userID, repoID, iid, "fresh", selection.Explicit)
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
			}
			assertRunHarness(t, env, runID, HarnessCodex)
		})
	}
}

func TestScheduledStoredOverrideCodexGuardLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	seedAnthropicToken(t, env, userID)
	var alias uuid.UUID
	if err := env.pool.QueryRow(env.ctx, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", userID).Scan(&alias); err != nil {
		t.Fatal(err)
	}
	svc := atomicSvc(env)
	codex := HarnessCodex
	for _, disabled := range []bool{false, true} {
		for _, auto := range []bool{false, true} {
			iid := int64(95000)
			if disabled {
				iid += 10
			}
			if auto {
				iid++
			}
			seedEligibleIssue(t, env, repoID, iid)
			if disabled {
				disableSecret(env, alias, true)
			} else {
				env.exec("UPDATE user_secrets SET disabled_at=NULL WHERE id=$1", alias)
			}
			override := &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &alias}
			var err error
			if auto {
				_, err = svc.CreateScheduledAutopilotRun(env.ctx, userID, repoID, iid, "fresh", nil, nil, nil, false, override, &codex)
			} else {
				_, err = svc.CreateScheduledRun(env.ctx, userID, repoID, iid, "fresh", nil, nil, nil, false, nil, override, &codex)
			}
			want := ErrCredentialOverrideHarnessUnsupported
			if disabled {
				want = ErrCredentialDisabled
			}
			if !errors.Is(err, want) {
				t.Fatalf("disabled=%v auto=%v err=%v want=%v", disabled, auto, err, want)
			}
		}
	}
	if n := env.countRunsForUser(t, userID); n != 0 {
		t.Fatalf("refused creates left %d runs", n)
	}
}
