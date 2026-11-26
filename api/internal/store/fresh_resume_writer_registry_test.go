package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Fresh resume writer registry (issue #4628). While a pending or claimed run_fresh_resumes row exists,
// a0_guard_fresh_resume_trg (migration 00535) raises UZ416 on any runs status / worker_id /
// claim_generation write. That raise is only a backstop: a sweep that touches several runs in one
// statement would abort wholesale on one transfer run and stall every other user's run behind it. So
// every statement that assigns one of those columns (or checkpoint_tip) must be registered here with a
// disposition, and the mechanical rule below applies to all of them.
//
//   - exclude   carries the literal NOT EXISTS (... run_fresh_resumes ... 'pending', 'claimed'). A statement
//     that carries the literal is exclude automatically; it needs no entry below.
//   - refuse    keyed on one run id; the rule-1 raise is the guard, and the service wrapper maps UZ416 to 409
//     (or the agent to fresh_resume_pre_seed).
//   - allowlist the fresh-resume queries themselves, Cancel, auto-stop under its GUC, W2's running advance.
//
// The literal is mechanically mandatory, whatever disposition a statement is given, for every :many
// statement and every statement not keyed on a single run id, and for the Lock* selectors named below.
// ClaimRun is the one explicit exception: it is :many-shaped but the fresh arm targets exactly one
// pending transfer through only_run_id, so it carries only the post-seed literal.
//
// A statement that sets a watched column and is in neither place fails this test, so a new writer cannot
// land unregistered. Statements it does not see: Go code that builds runs UPDATEs by string (the one such
// writer, workersvc/milestone_reconciliation.go, is registered below) and migrations.

// freshResumeRefuse lists the single-run writers guarded by the rule-1 raise alone.
var freshResumeRefuse = map[string]string{ //nolint:gosec // G101: the keys are sqlc query names, not credentials.
	"SetRunBaseAttestationHold": "single run", "FailCodexAccountWaitRun": "single run", "SetCodexSwitchStopped": "single run",
	"ParkCredentialDisabledRun": "assembly arm: RefuseFreshResumeInTx in m2", "ParkRunOverrideUnavailableRun": "assembly arm",
	"ParkRunEnvHistoryUnavailableRun": "assembly arm: refuseFreshAtAssembly returns before the run-env park (PRD #4416)",
	"ParkCredentialDisabledChatRun":   "chat", "PromoteCredentialDisabledRun": "single run", "ReassignCredentialDisabledRun": "single run",
	"FailJobRunWithoutResult": "job", "ResumeMemoryEpisode": "single run", "SetRunPhaseSwitchHold": "single run",
	"ApplyPolicyHold": "single run", "ResumePolicyHold": "single run", "ParkRunPublicationForgeUnreachable": "single run",
	"ResumePublicationForgeHold": "single run", "SetRunRepoAgentSourceHold": "single run", "SetRunAwaitingApproval": "single run",
	"SetRunLimitWait": "single run", "PromoteLimitWaitRunNow": "single run", "ResumeProviderGatewayLimitWait": "single run",
	"ParkRunningCodexAccountUnavailable": "single run", "HoldRunForgeUnreachable": "single run", "ResumeForgeUnreachableHold": "single run",
	"HoldRunDataVolumeFull": "single run", "ResumeAnthropicSpendLimitHold": "single run", "ResumeDataVolumeFullHold": "single run",
	"SetRunProviderRecoveryWait": "single run", "SetRunProviderHold": "single run", "PromoteRecoveryWaitRunNow": "single run",
	"SetRunPaused": "single run", "SetRunCompletionHold": "single run", "ResumePausedRun": "single run", "SetRunIterationPark": "single run",
	"ReleaseCredentialSwitch": "single run", "SetRunCredentialSwitchUncertainHold": "single run", "SetRunAwaitingInput": "single run",
	"SettleRunClarificationToGate": "single run", "AdmitRunFollowupFinalize": "single run", "SetRunAwaitingFollowup": "single run",
	"SetRunCompleted": "single run", "SetRunFailedPlanRejected": "single run", "MarkRunFailedByID": "single run",
	"SupersedeRunByWorker": "single run", "RejectRunServerSide": "single run", "ParkRunCodexAccountUnavailable": "single run",
	"PromoteCodexAccountWaitRun": "single run", "FailClaimAssemblyExact": "assembly arm", "RequeueClaimAssemblyExact": "assembly arm",
	"PromotePoolWaitRun": "single run", "ExtendAndResumeWallPark": "single run", "StopWallPark": "single run",
	"ResumeWorkerRecoveryEpisode": "single run: owner Resume refuses a pending row first", "ResumeCodexIdleStall": "single run",
}

var freshResumeAllowlist = map[string]string{
	"ClaimRun":                              "fresh claim target via only_run_id; post-seed literal only",
	"PrepareFreshResumeClaim":               "fresh resume query",
	"RestoreFreshResumeTuple":               "fresh resume query",
	"FailRunAutoStop":                       "auto-stop under uzi.fresh_resume_terminal",
	"SetRunRunning":                         "W2 running advance",
	"CancelRunServerSide":                   "Cancel",
	"CancelRunServerSideWithPendingOutcome": "Cancel",
	"CancelRunByWorker":                     "Cancel",
	"CancelRunByWorkerSettlingCancel":       "Cancel",
	"CancelRunServerSideForTerminalTarget":  "Cancel",
}

// freshResumeExclude are the statements the plan names as exclude by name (multi-run writers, Lock*
// selectors and the named single-run writers); each must carry the literal.
var freshResumeExclude = []string{
	"LockFailRunsOfStaleWorkersOverCap", "RequeueRunsOfStaleWorkers", "ParkRunsAtWall", "SweepClaimedNeverStarted",
	"RequeueWorkerRuns", "RequeueRunsMissingFromSnapshot", "ReadoptRunsFromSnapshot", "RequeueClaimedRunToQueued",
	"RequeueAttestedFinalizeRuns", "failRunsOfStaleWorkersOverCapLocked", "failWorkerRunsOverCapLocked",
	"failAttestedFinalizeRunsOverCapLocked", "failRunsMissingFromSnapshotLocked", "ParkQueuedCodexAccountUnavailablePage",
	"chargeClaimedNeverStartedContinuationsLocked", "parkUnwitnessedContinuationsLocked",
	"FrozenRequeueRunsMissingFromSnapshot", "FrozenReadoptRunsFromSnapshot", "FrozenRequeueWorkerRuns",
	"frozenFailRunsMissingFromSnapshotLocked", "frozenFailWorkerRunsOverCapLocked", "frozenFailAttestedFinalizeRunsOverCapLocked",
	"FrozenRequeueAttestedFinalizeRuns", "ParkFrozenQueuedCustodyExhausted", "HoldPublicationRunsWithoutCustodian",
	"PromoteLimitWaitRuns", "PromoteRecoveryWaitRuns", "PromoteVaultLockedRecoveryWaitRuns", "RequestWallParks",
	"SweepTaskNeverDispatched", "ClaimChatRun", "SweepIdleChatRuns", "FailJobsPastWallDeadline", "ExpireCodeCrossCheck",
	"ExpirePlanCrossCheck", "SupersedeExitedPlanCrossChecks",
	"ParkWorkerMemoryPressure", "ParkRunDataVolumeFull", "ParkRunForgeUnreachable", "SetRunRecoveryWait", "SetRunFailed",
	"SetRunWallPark", "HoldCredentialSwitchUnsettled", "ParkRunRetainedRecovery",
	// Lock* selectors and the id-list lock
	"LockFailWorkerRunsOverCap", "LockFailAttestedFinalizeRunsOverCap", "LockFailRunsMissingFromSnapshot",
	"LockFrozenFailRunsMissingFromSnapshot", "LockFrozenFailWorkerRunsOverCap", "LockFrozenFailAttestedFinalizeRunsOverCap",
	"LockFrozenWorkerSnapshotRuns", "LockClaimedNeverStartedContinuations", "LockUnwitnessedContinuations", "LockOwnedRunsByIDs",
}

// freshResumeGoWriters are non-generated Go files that build a runs UPDATE by string.
var freshResumeGoWriters = map[string]string{
	"../workersvc/milestone_reconciliation.go": "refuse: backed by the rule-1 raise",
}

type freshStmt struct {
	file, name, kind, body string
}

func freshResumeStatements(t *testing.T) map[string]freshStmt {
	t.Helper()
	entries, err := os.ReadDir("queries")
	if err != nil {
		t.Fatal(err)
	}
	trailing := regexp.MustCompile(`\s--[^\n]*`)
	out := map[string]freshStmt{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("queries", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range strings.Split(string(raw), "-- name: ")[1:] {
			head, rest, _ := strings.Cut(block, "\n")
			name, kind, _ := strings.Cut(strings.TrimSpace(head), " ")
			var lines []string
			for _, l := range strings.Split(rest, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(l), "--") {
					lines = append(lines, l)
				}
			}
			out[name] = freshStmt{file: e.Name(), name: name, kind: strings.TrimSpace(kind), body: trailing.ReplaceAllString(strings.Join(lines, "\n"), "")}
		}
	}
	return out
}

var (
	freshRunsUpdate = regexp.MustCompile(`UPDATE\s+runs(?:\s+(?:AS\s+)?\w+)?\s+SET`)
	freshAssigns    = regexp.MustCompile(`(?:\bSET\b|,)\s*(?:\w+\.)?(status|worker_id|claim_generation|checkpoint_tip)\s*=`)
	freshLiteral    = regexp.MustCompile(`NOT EXISTS \(SELECT 1 FROM run_fresh_resumes \w+ WHERE \w+\.(?:run_id|capture_id) = [\w.]+ AND \w+\.state IN \('pending', 'claimed'\)\)`)
	// A statement is keyed on one run only when the runs row (the table name, its alias, or an unqualified
	// id / run_id) is compared to a parameter. Another table's id (w.id = @worker_id) does not count.
	freshRunsAlias = regexp.MustCompile(`UPDATE\s+runs(?:\s+(?:AS\s+)?(\w+))?\s+SET`)
)

// freshSingleRun reports whether the statement's WHERE keys the runs row on one parameter.
func freshSingleRun(body string) bool {
	quals := []string{"runs"}
	for _, m := range freshRunsAlias.FindAllStringSubmatch(body, -1) {
		if m[1] != "" && m[1] != "SET" {
			quals = append(quals, m[1])
		}
	}
	qualified := regexp.MustCompile(`\b(?:` + strings.Join(quals, "|") + `)\.(?:run_)?id\s*=\s*(?:@|sqlc\.arg)`)
	bare := regexp.MustCompile(`(?:^|[^.\w])(?:run_)?id\s*=\s*(?:@|sqlc\.arg)`)
	return qualified.MatchString(body) || bare.MatchString(body)
}

func TestFreshResumeWriterRegistry(t *testing.T) {
	stmts := freshResumeStatements(t)
	var unregistered, missingLiteral, missingStamp, missingFinal []string
	watched := 0
	for _, s := range stmts {
		if !freshRunsUpdate.MatchString(s.body) {
			continue
		}
		cols := map[string]bool{}
		for _, m := range freshAssigns.FindAllStringSubmatch(s.body, -1) {
			cols[m[1]] = true
		}
		if len(cols) == 0 {
			continue
		}
		watched++
		hasLiteral := freshLiteral.MatchString(s.body)
		_, refuse := freshResumeRefuse[s.name]
		_, allow := freshResumeAllowlist[s.name]
		if cols["checkpoint_tip"] {
			// Every checkpoint_tip writer stamps the tip's generation and the tip it binds to, in the same statement.
			if !strings.Contains(s.body, "checkpoint_tip_generation =") || !strings.Contains(s.body, "checkpoint_tip_generation_tip =") {
				missingStamp = append(missingStamp, s.name)
			}
		}
		onlyTip := len(cols) == 1 && cols["checkpoint_tip"]
		if onlyTip {
			continue
		}
		if !hasLiteral && !refuse && !allow {
			unregistered = append(unregistered, s.name)
			continue
		}
		if allow {
			continue
		}
		// A literal in a selector CTE does not guard the UPDATE that follows: each UPDATE runs that assigns a
		// watched column must carry one in its own text too (ParkRunsAtWall kept a CTE copy when the UPDATE's was folded away).
		if hasLiteral && freshWatchedUpdatesUnguarded(s.body) > 0 && !freshSelectorOnly[s.name] {
			missingFinal = append(missingFinal, s.name)
		}
		// The mechanical rule: :many and not-single-run-keyed statements carry the literal, whatever the disposition.
		if (s.kind == ":many" || !freshSingleRun(s.body)) && !hasLiteral {
			missingLiteral = append(missingLiteral, s.name)
		}
	}
	if watched < 60 {
		t.Fatalf("only %d watched writers found: the scan is not looking at the queries", watched)
	}
	for _, l := range [][]string{unregistered, missingLiteral, missingStamp, missingFinal} {
		sort.Strings(l)
	}
	if len(unregistered) > 0 {
		t.Errorf("runs status/worker_id/claim_generation/checkpoint_tip writers neither carrying the fresh resume literal nor registered: %v", unregistered)
	}
	if len(missingLiteral) > 0 {
		t.Errorf("multi-run (:many or not keyed on one run id) writers without the literal NOT EXISTS (... run_fresh_resumes ...): %v", missingLiteral)
	}
	if len(missingFinal) > 0 {
		t.Errorf("exclude writers with an UPDATE runs that assigns a watched column and lacks the literal NOT EXISTS (... run_fresh_resumes ...): %v", missingFinal)
	}
	if len(missingStamp) > 0 {
		t.Errorf("checkpoint_tip writers that do not stamp checkpoint_tip_generation and checkpoint_tip_generation_tip: %v", missingStamp)
	}
}

func TestFreshResumeNamedWritersCarryLiteral(t *testing.T) {
	stmts := freshResumeStatements(t)
	var missing, absent []string
	for _, n := range freshResumeExclude {
		s, ok := stmts[n]
		if !ok {
			absent = append(absent, n)
			continue
		}
		if !freshLiteral.MatchString(s.body) {
			missing = append(missing, n)
		}
	}
	if len(absent) > 0 {
		t.Errorf("named writers that no longer exist (update the registry): %v", absent)
	}
	if len(missing) > 0 {
		t.Errorf("named exclude writers without the literal: %v", missing)
	}
	// ClaimRun keeps its explicit exception: only the post-seed literal, plus the only_run_id selector.
	claim := stmts["ClaimRun"].body
	for _, want := range []string{"only_run_id", "t.state IN ('seeded', 'superseded')", "t.predecessor_worker_id"} {
		if !strings.Contains(claim, want) {
			t.Errorf("ClaimRun lost %q", want)
		}
	}
	if freshLiteral.MatchString(claim) {
		t.Error("ClaimRun must not exclude pending/claimed rows: the fresh arm claims exactly that run")
	}
	for _, n := range []string{"CountOnlineWorkersClaimableForRun", "WorkerHasPendingWakeDemand"} {
		if b := stmts[n].body; !strings.Contains(b, "seeded', 'superseded'") {
			t.Errorf("%s lost the post-seed predecessor exclusion mirror", n)
		}
	}
}

func TestFreshResumeGoWritersRegistered(t *testing.T) {
	var found []string
	err := filepath.Walk("..", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, ".sql.go") {
			return err
		}
		b, rerr := os.ReadFile(filepath.Clean(p)) //nolint:gosec // G304: p comes from walking the store package tree.
		if rerr != nil {
			return rerr
		}
		for _, loc := range regexp.MustCompile(`(?s)UPDATE\s+runs\s+SET.{0,1000}`).FindAllString(string(b), -1) {
			end := strings.Index(loc[1:], "`")
			if end > 0 {
				loc = loc[:end+1]
			}
			for _, m := range freshAssigns.FindAllStringSubmatch(loc, -1) {
				if m[1] != "checkpoint_tip" {
					found = append(found, p)
					return nil
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range found {
		if _, ok := freshResumeGoWriters[p]; !ok {
			t.Errorf("Go writer of runs status/worker_id/claim_generation not registered: %s", p)
		}
	}
	for p := range freshResumeGoWriters {
		seen := false
		for _, f := range found {
			seen = seen || f == p
		}
		if !seen {
			t.Errorf("registered Go writer no longer writes the watched columns: %s", p)
		}
	}
}

// freshSelectorOnly names exclude writers whose literal legitimately sits only in a selector CTE
// (none today). An entry owes a reason.
var freshSelectorOnly = map[string]bool{}

// freshWatchedUpdatesUnguarded returns the number of UPDATE runs ... SET blocks that assign a watched
// column (status, worker_id, claim_generation) but whose own text, from that UPDATE to the next UPDATE
// runs or the end, lacks the literal. A literal in a selector CTE above does not count for the block.
func freshWatchedUpdatesUnguarded(body string) int {
	locs := freshRunsUpdate.FindAllStringIndex(body, -1)
	n := 0
	for i, loc := range locs {
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		seg := body[loc[0]:end]
		watched := false
		for _, m := range freshAssigns.FindAllStringSubmatch(seg, -1) {
			watched = watched || m[1] != "checkpoint_tip"
		}
		if watched && !freshLiteral.MatchString(seg) {
			n++
		}
	}
	return n
}

// freshSetColumns returns the columns assigned by every UPDATE runs ... SET list in the statement.
// The list ends at the first FROM, WHERE or RETURNING outside parentheses and string literals.
func freshSetColumns(body string) map[string]bool {
	cols := map[string]bool{}
	for _, loc := range freshRunsUpdate.FindAllStringIndex(body, -1) {
		i, depth, inStr, itemStart := loc[1], 0, false, loc[1]
		var items []string
		for ; i < len(body); i++ {
			c := body[i]
			if inStr {
				inStr = c != '\''
				continue
			}
			switch c {
			case '\'':
				inStr = true
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					items = append(items, body[itemStart:i])
					itemStart = i + 1
				}
			}
			if depth == 0 && (c == ' ' || c == '\n' || c == '\t') {
				rest := body[i+1:]
				if strings.HasPrefix(rest, "FROM ") || strings.HasPrefix(rest, "WHERE ") || strings.HasPrefix(rest, "WHERE\n") || strings.HasPrefix(rest, "RETURNING ") {
					break
				}
			}
		}
		items = append(items, body[itemStart:min(i, len(body))])
		for _, it := range items {
			lhs, _, ok := strings.Cut(it, "=")
			if !ok {
				continue
			}
			f := strings.Fields(lhs)
			if len(f) == 0 {
				continue
			}
			name := f[len(f)-1]
			if _, after, found := strings.Cut(name, "."); found {
				name = after
			}
			cols[name] = true
		}
	}
	return cols
}

// freshTupleFenced are columns the claim writes that a refusal deliberately never restores: the
// monotonic fences (claim_generation stays at G'; codex_claim_epoch and codex_cap_hash only advance),
// the claim provenance, worker_id (restored from saved_worker_id, a real FK, not from the jsonb) and
// the bookkeeping columns every restore rewrites itself.
var freshTupleFenced = map[string]string{
	"claim_generation": "monotonic fence", "codex_claim_epoch": "monotonic fence", "codex_cap_hash": "monotonic fence",
	"worker_id": "saved_worker_id", "updated_at": "restore rewrites it", "fresh_hold_version": "restore bumps it",
}

// TestFreshResumeSavedTupleCoversClaimWriters asserts that every column the claim path
// (PrepareFreshResumeClaim, ClaimRun) or W2's running advance (SetRunRunning) can write is in the saved
// tuple and restored by RestoreFreshResumeTuple, minus the documented fences, so a refusal cannot leave a
// claim-written value behind.
func TestFreshResumeSavedTupleCoversClaimWriters(t *testing.T) {
	stmts := freshResumeStatements(t)
	prep := stmts["PrepareFreshResumeClaim"].body
	saved := map[string]bool{}
	for _, m := range regexp.MustCompile(`'(\w+)',\s*cur\.\w+`).FindAllStringSubmatch(prep, -1) {
		saved[m[1]] = true
	}
	restored := freshSetColumns(stmts["RestoreFreshResumeTuple"].body)
	if len(saved) < 20 || len(restored) < 20 {
		t.Fatalf("saved tuple keys %d, restored columns %d: the parse is not reading the queries", len(saved), len(restored))
	}
	var notSaved, notRestored []string
	for _, w := range []string{"PrepareFreshResumeClaim", "ClaimRun", "SetRunRunning"} {
		cols := freshSetColumns(stmts[w].body)
		if len(cols) < 8 {
			t.Fatalf("%s: parsed %d SET columns: the parse is not reading the statement", w, len(cols))
		}
		for c := range cols {
			if _, fenced := freshTupleFenced[c]; fenced || strings.HasPrefix(c, "claim_provenance") || strings.HasPrefix(c, "codex_claim_provenance") {
				continue
			}
			if !saved[c] {
				notSaved = append(notSaved, w+"."+c)
			}
			if !restored[c] {
				notRestored = append(notRestored, w+"."+c)
			}
		}
	}
	sort.Strings(notSaved)
	sort.Strings(notRestored)
	if len(notSaved) > 0 {
		t.Errorf("columns written on the claim path but missing from the saved tuple: %v", notSaved)
	}
	if len(notRestored) > 0 {
		t.Errorf("columns written on the claim path but not restored by RestoreFreshResumeTuple: %v", notRestored)
	}
}

func TestFreshResumeSingleRunClassifier(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       bool
	}{
		{"runs alias id", "UPDATE runs r SET status = 'x' WHERE r.id = @id", true},
		{"runs table id", "UPDATE runs SET status = 'x' WHERE runs.id = sqlc.arg('id')", true},
		{"bare id", "UPDATE runs SET status = 'x' WHERE id = @id", true},
		{"bare run_id", "UPDATE runs SET status = 'x' WHERE run_id = @run_id", true},
		{"worker id is not a run key", "UPDATE runs r SET status = 'x' FROM workers w WHERE r.worker_id = w.id AND w.id = @worker_id", false},
		{"worker id only", "UPDATE runs SET status = 'x' FROM workers w WHERE runs.worker_id = w.id AND w.id = @worker_id", false},
	} {
		if got := freshSingleRun(c.body); got != c.want {
			t.Errorf("%s: freshSingleRun = %v, want %v", c.name, got, c.want)
		}
	}
}
