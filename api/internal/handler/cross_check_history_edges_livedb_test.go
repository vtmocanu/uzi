package handler

import (
	"net/url"
	"testing"
	"time"
)

func TestCrossCheckHistoryRetainedCreationOrderLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	olderParent := f.parent("completed", "older parent newest check")
	newerParent := f.parent("completed", "newer parent older check")
	cliMustExec(t, f.pool, "UPDATE runs SET created_at=$2 WHERE id=$1", olderParent, base.Add(-2*time.Hour))
	cliMustExec(t, f.pool, "UPDATE runs SET created_at=$2 WHERE id=$1", newerParent, base.Add(-time.Hour))
	f.plan(olderParent, nil, 1, "approve", "approve", base)
	f.plan(newerParent, nil, 1, "approve", "approve", base.Add(-time.Minute))
	for _, cli := range []bool{false, true} {
		first := f.list("lifecycle=past&limit=1", cli)
		if len(first.Groups) != 1 || first.Groups[0].ParentID != olderParent.String() ||
			!first.Groups[0].LatestAt.Equal(base) || first.NextCursor == "" {
			t.Fatal("groups must sort by newest retained check, independently of parent creation")
		}
		second := f.list("lifecycle=past&limit=1&cursor="+url.QueryEscape(first.NextCursor), cli)
		if len(second.Groups) != 1 || second.Groups[0].ParentID != newerParent.String() ||
			!second.Groups[0].LatestAt.Equal(base.Add(-time.Minute)) || second.NextCursor != "" {
			t.Fatal("retained creation order continuation lost the older check")
		}
	}
}

func TestCrossCheckHistoryPastOutcomeWindowsLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	interrupted := base.Add(30*time.Minute + 123456*time.Microsecond)
	type attempt struct {
		stage, result, reason, review, execution, category string
		round                                              int
		historical, prestart, interrupted, noChild         bool
		findings                                           int32
		// decided moves decided_at relative to the interruption: "after" or "equal".
		decided string
	}
	// Literal expectations are independent of the classifier under test.
	plans := []attempt{
		{stage: "plan", round: 1, result: "approve", reason: "approve", review: "approved", execution: "finished", category: "passed", historical: true},
		{stage: "plan", round: 2, result: "revise", reason: "revise", review: "changes_requested", execution: "finished", category: "raised_issues", historical: true, findings: 1},
		{stage: "plan", round: 3, result: "block", reason: "block", review: "blocked", execution: "finished", category: "raised_issues", historical: true},
		{stage: "plan", round: 4, result: "failed", review: "none", execution: "didnt_finish", category: "didnt_finish", historical: true},
		{stage: "plan", round: 5, result: "approve", review: "none", execution: "unknown", historical: true, findings: -1},
		{stage: "plan", round: 6, result: "revise", review: "none", execution: "unknown", historical: true},
		{stage: "plan", round: 7, result: "block", review: "none", execution: "unknown", historical: true},
		{stage: "plan", round: 8, result: "failed", reason: "malformed", review: "none", execution: "didnt_finish", category: "didnt_finish", historical: true},
		{stage: "plan", round: 9, result: "failed", reason: "superseded", review: "none", execution: "replaced", category: "replaced", historical: true},
		{stage: "plan", round: 10, result: "failed", reason: "approved_not_stored", review: "approved", execution: "replaced", category: "replaced", historical: true},
		// A Plan verdict recorded before custody loss keeps its verdict and stays historical (#4037).
		{stage: "plan", round: 11, result: "approve", reason: "approve", review: "approved", execution: "finished", category: "passed", historical: true, interrupted: true},
		{stage: "plan", round: 12, result: "revise", reason: "revise", review: "changes_requested", execution: "finished", category: "raised_issues", historical: true, interrupted: true},
		{stage: "plan", round: 13, result: "block", reason: "block", review: "blocked", execution: "finished", category: "raised_issues", historical: true, interrupted: true},
		{stage: "plan", round: 14, result: "approve", reason: "approve", review: "approved", execution: "finished", category: "passed", historical: true, interrupted: true, decided: "equal"},
		// Controls that stay replaced: decided after interruption, missing reason,
		// pending, settled supersession and an unstored approval.
		{stage: "plan", round: 15, result: "approve", reason: "approve", review: "approved", execution: "interrupted", category: "replaced", historical: true, interrupted: true, decided: "after"},
		{stage: "plan", round: 16, result: "approve", review: "none", execution: "interrupted", category: "replaced", historical: true, interrupted: true},
		{stage: "plan", round: 17, result: "pending", review: "none", execution: "interrupted", category: "replaced", historical: true, interrupted: true, noChild: true},
		{stage: "plan", round: 18, result: "failed", reason: "superseded", review: "none", execution: "interrupted", category: "replaced", historical: true, interrupted: true, noChild: true, decided: "equal"},
		{stage: "plan", round: 19, result: "failed", reason: "approved_not_stored", review: "approved", execution: "interrupted", category: "replaced", historical: true, interrupted: true},
		{stage: "plan", round: 20, result: "approve", reason: "approve", review: "approved", execution: "finished", category: "passed"},
		{stage: "code", round: 1, result: "completed", review: "changes_requested", execution: "finished", category: "raised_issues", findings: 1},
	}
	parent := f.parent("completed", "mixed retained outcomes")
	expected := map[string][]attempt{parent.String(): plans}
	for _, a := range plans {
		created := base.Add(time.Duration(a.round) * time.Second)
		var child any
		if (a.execution == "finished" || a.interrupted || a.reason == "approved_not_stored") && !a.noChild {
			child = f.child(parent, "completed")
		}
		if a.stage == "plan" {
			f.plan(parent, child, a.round, a.result, a.reason, created)
			if a.findings < 0 {
				cliMustExec(t, f.pool, `UPDATE cross_checks SET findings='"malformed optional evidence"' WHERE lead_run_id=$1 AND stage='plan' AND round=$2`, parent, a.round)
			}
			if a.findings > 0 {
				cliMustExec(t, f.pool, `UPDATE cross_checks SET findings='{"items":[{"message":"private raw findings"}]}' WHERE lead_run_id=$1 AND stage='plan' AND round=$2`, parent, a.round)
			}
		} else {
			// The helper deliberately stores legacy verdict=failed. Completed
			// outcome and findings must still produce a successful review.
			f.code(parent, child, a.result, a.reason, `[{"message":"private raw findings"}]`, created)
		}
		if a.interrupted {
			cliMustExec(t, f.pool, "UPDATE cross_checks SET interrupted_at=$3 WHERE lead_run_id=$1 AND stage='plan' AND round=$2", parent, a.round, interrupted)
		}
		switch a.decided {
		case "after":
			cliMustExec(t, f.pool, "UPDATE cross_checks SET decided_at=$3 WHERE lead_run_id=$1 AND stage='plan' AND round=$2", parent, a.round, interrupted.Add(time.Second))
		case "equal":
			cliMustExec(t, f.pool, "UPDATE cross_checks SET decided_at=$3 WHERE lead_run_id=$1 AND stage='plan' AND round=$2", parent, a.round, interrupted)
		}
	}
	// Code has one retained attempt per parent. Keep failure, missing-child,
	// interruption and replacement distinct from successful findings review.
	for _, a := range []attempt{
		{stage: "code", round: 1, result: "completed", review: "approved", execution: "unknown", category: "passed"},
		{stage: "code", round: 1, result: "failed", reason: "model_error", review: "none", execution: "didnt_finish", category: "didnt_finish"},
		{stage: "code", round: 1, result: "failed", reason: "worker_unsupported", review: "none", execution: "couldnt_start", category: "didnt_finish", prestart: true},
		{stage: "code", round: 1, result: "failed", reason: "snapshot_failed", review: "none", execution: "couldnt_start", category: "didnt_finish", prestart: true},
		{stage: "code", round: 1, result: "failed", reason: "superseded", review: "none", execution: "replaced", category: "replaced", historical: true},
		{stage: "code", round: 1, result: "completed", review: "approved", execution: "interrupted", category: "replaced", historical: true, interrupted: true},
	} {
		p := f.parent("completed", "code retained outcome")
		expected[p.String()] = []attempt{a}
		var child any
		if a.interrupted {
			child = f.child(p, "completed")
		}
		if a.prestart {
			// Safe pre-start shape has no commits and empty frozen guidance.
			// Do not clear the helper's immutable private snapshot after insert.
			cliMustExec(t, f.pool, `INSERT INTO cross_checks
 (lead_run_id,stage,round,lead_claim_generation,checker_harness,verdict,outcome,reason_class,
 code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,findings,created_at,deadline_at)
 VALUES($1,'code',1,1,'codex','failed','failed',$2,'{}','','',sha256(convert_to('','UTF8')),
 '',sha256(convert_to('','UTF8')),'null',$3,$3::timestamptz+interval '30 minutes')`, p, a.reason, base)
		} else {
			f.code(p, child, a.result, a.reason, "[]", base)
		}
		if a.interrupted {
			cliMustExec(t, f.pool, "UPDATE cross_checks SET interrupted_at=$2 WHERE lead_run_id=$1 AND stage='code'", p, interrupted)
		}
	}
	reasons := map[string]string{
		"": "Cause not recorded for this attempt", "approve": "Review approved",
		"revise": "Changes requested", "block": "Review blocked", "malformed": "Checker response could not be read",
		"approved_not_stored": "Approval was not stored", "superseded": "Attempt replaced",
		"model_error": "Checker model failed", "worker_unsupported": "Worker does not support this check",
		"snapshot_failed": "Snapshot could not be created",
	}
	for _, stage := range []string{"all", "plan", "code"} {
		for _, outcome := range []string{"any", "passed", "raised_issues", "didnt_finish", "replaced"} {
			t.Run(stage+"/"+outcome, func(t *testing.T) {
				query := "lifecycle=past&stage=" + stage + "&outcome=" + outcome + "&attempt_limit=2"
				matching := map[string][]attempt{}
				for id, attempts := range expected {
					for _, a := range attempts {
						if (stage == "all" || a.stage == stage) && (outcome == "any" || a.category == outcome) {
							matching[id] = append(matching[id], a)
						}
					}
				}
				for _, cli := range []bool{false, true} {
					first := f.list(query, cli)
					if first.NextCursor != "" || len(first.Groups) != len(matching) {
						t.Fatalf("cli=%t: got %d groups want %d, cursor=%q", cli, len(first.Groups), len(matching), first.NextCursor)
					}
					seen := map[string]bool{}
					for _, initial := range first.Groups {
						want, ok := matching[initial.ParentID]
						if !ok || seen[initial.ParentID] {
							t.Fatalf("unexpected or duplicate parent %s", initial.ParentID)
						}
						seen[initial.ParentID] = true
						g := initial
						end := len(want)
						for page := 0; ; page++ {
							start := max(0, end-2)
							if g.RefreshNeeded || g.Active || g.TotalAttempts != int64(len(expected[g.ParentID])) ||
								g.MatchingAttempts != int64(len(want)) || len(g.Attempts) != end-start {
								t.Fatalf("parent %s page %d: invalid filtered window metadata", g.ParentID, page)
							}
							for i, got := range g.Attempts {
								a := want[start+i]
								if got.Stage != a.stage || int(got.Round) != a.round {
									t.Fatalf("parent %s page %d index %d: got %s/%d want %s/%d", g.ParentID, page, i, got.Stage, got.Round, a.stage, a.round)
								}
								if got.Result != a.result || got.Review != a.review || got.Execution != a.execution ||
									got.Historical != a.historical || got.Active || got.Reason != reasons[a.reason] ||
									got.Effect != "Effect not recorded" || got.NextAction != "No action recorded" {
									t.Fatalf("%s %s/%d: unexpected review/execution/effect: %+v", g.ParentID, a.stage, a.round, got)
								}
								wantChild := (a.execution == "finished" || a.interrupted || a.reason == "approved_not_stored") && !a.noChild
								if wantChild {
									if got.ChildID == nil || got.ChildStatus == nil || *got.ChildStatus != "completed" || got.ChildContext != "Checker run available" {
										t.Fatalf("%s/%d: completed child evidence missing: %+v", a.stage, a.round, got)
									}
								} else if got.ChildID != nil || got.ChildStatus != nil || got.ChildContext != "No checker run available" {
									t.Fatalf("%s/%d: invented child evidence: %+v", a.stage, a.round, got)
								}
								if a.prestart || a.findings < 0 {
									if got.FindingsCount != nil || got.ChildID != nil || got.ChildStatus != nil || got.ChildContext != "No checker run available" {
										t.Fatalf("missing findings or child evidence was invented: %+v", got)
									}
								} else if got.FindingsCount == nil || *got.FindingsCount != a.findings {
									t.Fatalf("%s/%d: findings count got %v want %d", a.stage, a.round, got.FindingsCount, a.findings)
								}
								if a.interrupted {
									if got.InterruptedAt == nil || !got.InterruptedAt.Equal(interrupted) {
										t.Fatalf("%s/%d: interruption timestamp did not roundtrip", a.stage, a.round)
									}
								} else if got.InterruptedAt != nil {
									t.Fatalf("%s/%d: invented interruption timestamp", a.stage, a.round)
								}
							}
							if (g.OlderCursor != "") != (start > 0) {
								t.Fatalf("parent %s page %d: continuation boundary end=%d start=%d cursor=%q", g.ParentID, page, end, start, g.OlderCursor)
							}
							if start == 0 {
								break
							}
							next := f.list(query+"&attempt_cursor="+url.QueryEscape(g.OlderCursor), cli)
							if len(next.Groups) != 1 || next.Groups[0].ParentID != initial.ParentID || next.NextCursor != "" {
								t.Fatal("older continuation changed parent or fetched fresh groups")
							}
							g, end = next.Groups[0], start
						}
					}
				}
			})
		}
	}
}

func TestCrossCheckHistoryNullCurrentPlanLiveDB(t *testing.T) {
	for _, change := range []string{"gate_revision", "claim_generation"} {
		t.Run(change, func(t *testing.T) {
			f := newHistoryLiveFixture(t)
			parent := f.parent("completed", "not yet stored plan")
			// Terminal parent avoids lifecycle settlement while testing only the
			// current-candidate evidence, with schema-valid same-generation rows.
			cliMustExec(t, f.pool, "UPDATE runs SET plan_md=NULL,gate_revision=0 WHERE id=$1", parent)
			f.plan(parent, f.child(parent, "completed"), 1, "approve", "approve", time.Now().UTC().Truncate(time.Microsecond))
			check := func(historical bool) {
				t.Helper()
				for _, cli := range []bool{false, true} {
					got := f.list("lifecycle=past&stage=plan&outcome=passed", cli)
					if len(got.Groups) != 1 || got.Groups[0].ParentID != parent.String() || len(got.Groups[0].Attempts) != 1 {
						t.Fatal("NULL current plan lost retained approval")
					}
					a := got.Groups[0].Attempts[0]
					if a.Stage != "plan" || a.Round != 1 || a.Historical != historical || a.InterruptedAt != nil ||
						a.Result != "approve" || a.Review != "approved" || a.Execution != "finished" {
						t.Fatalf("historical=%t: unexpected current-candidate evidence: %+v", historical, a)
					}
				}
			}
			check(false)
			switch change {
			case "gate_revision":
				cliMustExec(t, f.pool, "UPDATE runs SET gate_revision=1 WHERE id=$1", parent)
			case "claim_generation":
				cliMustExec(t, f.pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", parent)
			}
			check(true)
		})
	}
}
