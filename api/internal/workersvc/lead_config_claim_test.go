package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/leadcfg/leadcfgtest"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// leadClaimFake builds a fakeStore whose claimed run is a Claude run of the given kind, claimed
// by worker() through the real Claim path (the same shape as ciFixClaimPayload).
func leadClaimFake(t *testing.T, kind string, mutate func(*store.Run)) *fakeStore {
	t.Helper()
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("FORGE-PAT-PLACEHOLDER"))
	sealedTok, _ := box.Seal([]byte("ANTHROPIC-OAUTH-PLACEHOLDER"))
	run := store.Run{
		ID:               uuid.New(),
		RepoID:           pgconv.UUID(uuid.New()),
		Kind:             kind,
		Harness:          "claude",
		IssueTitle:       "t",
		IssueDescription: "d",
		Status:           "claimed",
		PlanSource:       planSourceAgent,
		IssueIid:         pgtype.Int8{Int64: 7, Valid: true},
	}
	if kind == "ci_fix" {
		run.IssueIid = pgtype.Int8{}
		run.PipelineRef = pgconv.TextOrNull("main")
		run.PipelineID = pgtype.Int8{Int64: 4200, Valid: true}
		run.FailureSnapshot, _ = json.Marshal(sampleSnapshot())
	}
	if mutate != nil {
		mutate(&run)
	}
	return &fakeStore{
		claimRun:           run,
		attributionEnabled: true,
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			DefaultBranch: pgconv.TextOrNull("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic: sealedTok,
	}
}

func leadClaim(t *testing.T, fs *fakeStore) ClaimPayload {
	t.Helper()
	svc := newOffModeService(fs, newBox(t), testParams())
	payload, err := svc.Claim(context.Background(), worker(), nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim: %v (payload=%v)", err, payload)
	}
	return *payload
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func TestClaimCarriesLeadProvenance(t *testing.T) {
	cases := []struct {
		name                          string
		lane, effort                  string
		runModel                      string
		wantModel, wantModelSrc       string
		wantEffort, wantEffortSrc     string
		wantFallbackNotesAtLeastOne   bool
		wantNoModelButSourceForEffort bool
	}{
		{name: "lane and builtin effort", lane: "opus", wantModel: "opus", wantModelSrc: "user_default", wantEffort: "medium", wantEffortSrc: "builtin"},
		{name: "chosen effort", lane: "opus", effort: "high", wantModel: "opus", wantModelSrc: "user_default", wantEffort: "high", wantEffortSrc: "user_default"},
		{name: "whitespace effort is builtin", lane: "opus", effort: "  ", wantModel: "opus", wantModelSrc: "user_default", wantEffort: "medium", wantEffortSrc: "builtin"},
		{name: "schedule pin", lane: "opus", runModel: "sonnet", wantModel: "sonnet", wantModelSrc: "schedule_pin", wantEffort: "medium", wantEffortSrc: "builtin"},
		{name: "no lane ships no model and no model source", wantEffort: "medium", wantEffortSrc: "builtin"},
		{name: "codex role in the lane is dropped", lane: "gpt-6-luna", wantEffort: "medium", wantEffortSrc: "builtin", wantFallbackNotesAtLeastOne: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := leadClaimFake(t, "issue", func(r *store.Run) {
				if c.runModel != "" {
					r.Model = pgconv.TextOrNull(c.runModel)
				}
			})
			fs.defaultModel = pgconv.TextOrNull(c.lane)
			fs.defaultEffort = pgconv.TextOrNull(c.effort)
			p := leadClaim(t, fs)
			if got := derefOr(p.Config.DefaultModel, ""); got != c.wantModel {
				t.Errorf("default_model = %q, want %q", got, c.wantModel)
			}
			if got := derefOr(p.Config.DefaultModelSource, ""); got != c.wantModelSrc {
				t.Errorf("default_model_source = %q, want %q", got, c.wantModelSrc)
			}
			if (p.Config.DefaultModel == nil) != (p.Config.DefaultModelSource == nil) {
				t.Errorf("default_model_source must be present exactly when default_model is shipped")
			}
			if got := derefOr(p.Config.DefaultEffort, ""); got != c.wantEffort {
				t.Errorf("default_effort = %q, want %q", got, c.wantEffort)
			}
			if got := derefOr(p.Config.DefaultEffortSource, ""); got != c.wantEffortSrc {
				t.Errorf("default_effort_source = %q, want %q", got, c.wantEffortSrc)
			}
			notes := 0
			for _, m := range fs.insertedMessages {
				if m.Kind == harnessModelFallbackKind {
					notes++
				}
			}
			if (notes > 0) != c.wantFallbackNotesAtLeastOne {
				t.Errorf("fallback notes = %d, want any = %v", notes, c.wantFallbackNotesAtLeastOne)
			}
		})
	}
}

func TestClaimKeepsBothFallbackNotesInOrder(t *testing.T) {
	fs := leadClaimFake(t, "issue", func(r *store.Run) { r.Model = pgconv.TextOrNull(" gpt-6-sol ") })
	fs.defaultModel = pgconv.TextOrNull("  gpt-6-luna ")
	leadClaim(t, fs)
	var dropped []string
	for _, m := range fs.insertedMessages {
		if m.Kind != harnessModelFallbackKind {
			continue
		}
		var pl harnessModelFallbackPayload
		if err := json.Unmarshal(m.Payload, &pl); err != nil {
			t.Fatal(err)
		}
		dropped = append(dropped, pl.DroppedModel)
	}
	// The lane note carries the trimmed token, the run-model note the stored value untrimmed.
	if fmt.Sprint(dropped) != "[gpt-6-luna  gpt-6-sol ]" {
		t.Fatalf("dropped models = %q, want the lane note then the untrimmed run-model note", dropped)
	}
}

// TestClaimLeadSourceKeysAgreeWithLeadLane runs every claimable lead kind crossed with review
// target and egress binding through the real claim path and asserts the two source keys are
// present exactly when leadcfg.LeadLane holds, against the independent oracle of
// leadcfgtest.LaneMatrix. Kinds with their own claim assembly (cross_check, judge, chat, job)
// are covered by their dedicated absence tests; here they must not be lead-lane.
func TestClaimLeadSourceKeysAgreeWithLeadLane(t *testing.T) {
	assembled := map[string]bool{"issue": true, "ci_fix": true, "prompt": true, "task": true, "mr_rework": true, "self_improve": true}
	visited := 0
	for _, lc := range leadcfgtest.LaneMatrix(t) {
		if leadcfg.LeadLane(lc.Kind, lc.ReviewTarget, lc.EgressBound) != lc.Want {
			t.Fatalf("LeadLane(%+v) disagrees with the contract", lc)
		}
		if !assembled[lc.Kind] {
			if lc.Want {
				t.Fatalf("lead-lane kind %q has no claim-assembly case in this test", lc.Kind)
			}
			continue
		}
		if lc.EgressBound {
			// An isolated-lane claim assembles against the database (egress snapshot, credential
			// mint); TestIsolatedClaimCarriesNoLeadSourcesLiveDB covers it on the real path.
			continue
		}
		name := fmt.Sprintf("%s/review=%v/egress=%v", lc.Kind, lc.ReviewTarget, lc.EgressBound)
		t.Run(name, func(t *testing.T) {
			fs := leadClaimFake(t, lc.Kind, func(r *store.Run) {
				if lc.ReviewTarget {
					r.ReviewTargetRunID = pgconv.UUID(uuid.New())
				}
				if lc.EgressBound {
					r.EgressProfileID = pgconv.UUID(uuid.New())
				}
			})
			fs.defaultModel = pgconv.TextOrNull("opus")
			p := leadClaim(t, fs)
			if got := p.Config.DefaultEffortSource != nil; got != lc.Want {
				t.Errorf("default_effort_source present = %v, want %v", got, lc.Want)
			}
			if got := p.Config.DefaultModelSource != nil; got != lc.Want {
				t.Errorf("default_model_source present = %v, want %v", got, lc.Want)
			}
		})
		visited++
	}
	if visited == 0 {
		t.Fatal("no claim case ran")
	}
}
