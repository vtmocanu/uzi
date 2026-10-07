package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

const branchMovedCanonical = "branch_moved: remote_branch_advanced; superseding_tip=0123456789abcdef0123456789abcdef01234567"
const branchMovedReason = "superseded by a concurrent branch advance; further publication stopped."
const branchMovedDetailedReason = "superseded by a concurrent branch advance; further publication stopped. cause=remote_branch_advanced; superseding_tip=0123456789abcdef0123456789abcdef01234567"

func TestSetStateFailedBranchMovedKindRegistry(t *testing.T) {
	for _, kind := range append(runkind.All(), "unknown") {
		t.Run(kind, func(t *testing.T) {
			run := runningRun(false)
			run.Kind = kind
			fs, svc, wkr := limitParkFixture(t, run)
			moved := true
			reason := branchMovedCanonical
			_, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
				State: "failed", FailureReason: &reason, BranchMoved: &moved,
			})
			if kind == runkind.Job {
				if !errors.Is(err, ErrMissingClaimGeneration) || fs.supersededByWorker != nil {
					t.Fatalf("job protocol guard: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			allowed := kind == runkind.MRRework || kind == runkind.CIFix
			if (fs.supersededByWorker != nil) != allowed {
				t.Fatalf("supersede called=%v, want %v", fs.supersededByWorker != nil, allowed)
			}
			if allowed {
				if fs.setFailed != nil {
					t.Fatal("supersession called SetRunFailed")
				}
			} else if fs.setFailed == nil || fs.setFailed.FailOrigin.String != "agent_failure" {
				t.Fatalf("ordinary failure missing: %+v", fs.setFailed)
			}
		})
	}
}

func TestSetStateFailedBranchMovedDiagnostics(t *testing.T) {
	cases := []struct {
		name   string
		reason *string
		want   string
	}{}
	add := func(name, reason, want string) {
		cases = append(cases, struct {
			name   string
			reason *string
			want   string
		}{name, &reason, want})
	}
	cases = append(cases, struct {
		name   string
		reason *string
		want   string
	}{"absent", nil, branchMovedReason})
	add("canonical", branchMovedCanonical, branchMovedDetailedReason)
	for name, reason := range map[string]string{
		"legacy":        "finalize push rejected non-fast-forward",
		"partial":       "branch_moved: remote_branch_advanced",
		"short":         strings.TrimSuffix(branchMovedCanonical, "7"),
		"long":          branchMovedCanonical + "0",
		"uppercase":     strings.Replace(branchMovedCanonical, "abcdef", "ABCDEF", 1),
		"nonhex":        strings.TrimSuffix(branchMovedCanonical, "7") + "g",
		"cause":         strings.Replace(branchMovedCanonical, "remote_branch_advanced", "attacker_cause", 1),
		"prefix":        "hostile prose " + branchMovedCanonical,
		"suffix":        branchMovedCanonical + "; hostile prose",
		"newline":       branchMovedCanonical + "\n",
		"leading_space": " " + branchMovedCanonical,
		"hostile":       "<script>hostile</script>",
	} {
		add(name, reason, branchMovedReason)
	}
	for _, kind := range []string{runkind.MRRework, runkind.CIFix} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				run := runningRun(false)
				run.Kind = kind
				fs, svc, wkr := limitParkFixture(t, run)
				moved := true
				if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "failed", BranchMoved: &moved, FailureReason: tc.reason}); err != nil {
					t.Fatal(err)
				}
				if fs.supersededByWorker == nil {
					t.Fatal("supersession missing")
				}
				if got := fs.supersededByWorker.StopReason; got != tc.want {
					t.Fatalf("stop reason=%q, want %q", got, tc.want)
				}
			})
		}
	}
}

func TestSetStateFailedBranchMovedAbsentOrFalse(t *testing.T) {
	no := false
	for _, kind := range []string{runkind.MRRework, runkind.CIFix} {
		for _, moved := range []*bool{nil, &no} {
			run := runningRun(false)
			run.Kind = kind
			fs, svc, wkr := limitParkFixture(t, run)
			reason := branchMovedCanonical
			if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "failed", BranchMoved: moved, FailureReason: &reason}); err != nil {
				t.Fatal(err)
			}
			if fs.supersededByWorker != nil || fs.setFailed == nil || fs.setFailed.FailOrigin.String != "agent_failure" {
				t.Fatalf("diagnostics alone changed failure: %+v", fs.setFailed)
			}
		}
	}
}

func TestSetStateFailedBranchMovedStopPrecedence(t *testing.T) {
	for _, kind := range []string{runkind.MRRework, runkind.CIFix} {
		for _, stop := range []string{"cancelled", "stopped", "plan_rejected"} {
			t.Run(kind+"/"+stop, func(t *testing.T) {
				run := runningRun(false)
				run.Kind = kind
				run.StopKind = pgtype.Text{String: stop, Valid: true}
				fs, svc, wkr := limitParkFixture(t, run)
				moved := true
				reason := branchMovedCanonical
				if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "failed", BranchMoved: &moved, FailureReason: &reason}); err != nil {
					t.Fatal(err)
				}
				if fs.supersededByWorker != nil {
					t.Fatal("branch moved overrode operator stop")
				}
				if stop != "plan_rejected" && fs.cancelledByWorker == nil {
					t.Fatal("operator cancellation missing")
				}
				if stop == "plan_rejected" && fs.setFailedPlanRejected == nil {
					t.Fatal("plan rejection missing")
				}
			})
		}
	}
}
