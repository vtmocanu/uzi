package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// sixMilestonesFrozen is a milestones_frozen jsonb of six milestones, parseable by
// DecodeMilestones. Six is above the clamp cases' inputs (0, 4, 99) so the [floor, 6]
// window is exercised at both ends and in the middle.
func sixMilestonesFrozen() []byte {
	return []byte(`[{"id":"m1","title":"One"},{"id":"m2","title":"Two"},{"id":"m3","title":"Three"},{"id":"m4","title":"Four"},{"id":"m5","title":"Five"},{"id":"m6","title":"Six"}]`)
}

// twoCompletedIDs is a milestones_completed jsonb of two ids, parseable by
// DecodeMilestoneIDs — the clamp FLOOR (len(completed)=2) for the scope window.
func twoCompletedIDs() []byte {
	return []byte(`["m1","m2"]`)
}

// scopeRunFixture builds a milestone-structured issue run (six frozen, two completed)
// owned by `user`, wired for the submitInput scope/stop path (GetRun -> GetRunByIDForUser
// -> runByID). It returns the fake, the service, and the (user, run) identity to submit
// under.
func scopeRunFixture(t *testing.T) (*fakeStore, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	user, runID := uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID: store.Run{
			ID:                  runID,
			UserID:              user,
			Kind:                runkind.Issue,
			Status:              "running",
			MilestonesFrozen:    sixMilestonesFrozen(),
			MilestonesCompleted: twoCompletedIDs(),
		},
	}
	return fs, New(fs, newBox(t), testParams()), user, runID
}

// TestSubmitInputScopeAcceptedOnMilestoneIssueRun: a `scope` on a milestone-structured
// issue run writes the ceiling via CreateScopeCeilingInput and reports it back. Body "4"
// sits strictly inside the [2,6] window, so the persisted and returned ceiling is 4 —
// not the floor, not the cap.
func TestSubmitInputScopeAcceptedOnMilestoneIssueRun(t *testing.T) {
	fs, svc, user, runID := scopeRunFixture(t)

	res, err := svc.SubmitInput(context.Background(), user, runID, "scope", "4", nil)
	if err != nil {
		t.Fatalf("SubmitInput scope: %v", err)
	}
	if fs.createdScopeCeiling == nil {
		t.Fatal("CreateScopeCeilingInput not called")
	}
	if fs.createdScopeCeiling.RunID != runID {
		t.Fatalf("scope write targeted run %v, want %v", fs.createdScopeCeiling.RunID, runID)
	}
	if !fs.createdScopeCeiling.ScopeCeiling.Valid || fs.createdScopeCeiling.ScopeCeiling.Int32 != 4 {
		t.Fatalf("persisted ceiling = %+v, want Valid Int32=4", fs.createdScopeCeiling.ScopeCeiling)
	}
	if res.ScopeCeiling == nil || *res.ScopeCeiling != 4 {
		t.Fatalf("result ScopeCeiling = %v, want 4", res.ScopeCeiling)
	}
	// A scope directive is never a stop verdict — no stop_kind is stamped.
	if fs.createdStopVerdict != nil {
		t.Fatalf("scope must not enqueue a stop verdict, got %+v", fs.createdStopVerdict)
	}
}

// TestSubmitInputScopeClampsToWindow: the desired ceiling is clamped into
// [len(completed), len(frozen)] = [2,6] and the CLAMPED value is what is persisted and
// returned. Below the floor -> 2, above the cap -> 6, inside -> unchanged. Each row uses
// a distinct expected value so a stuck/hardcoded clamp cannot pass all three.
func TestSubmitInputScopeClampsToWindow(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int32
	}{
		{"below floor clamps up to len(completed)", "0", 2},
		{"above cap clamps down to len(frozen)", "99", 6},
		{"inside window is unchanged", "4", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, svc, user, runID := scopeRunFixture(t)
			res, err := svc.SubmitInput(context.Background(), user, runID, "scope", tc.body, nil)
			if err != nil {
				t.Fatalf("SubmitInput scope %q: %v", tc.body, err)
			}
			if fs.createdScopeCeiling == nil {
				t.Fatal("CreateScopeCeilingInput not called")
			}
			if !fs.createdScopeCeiling.ScopeCeiling.Valid || fs.createdScopeCeiling.ScopeCeiling.Int32 != tc.want {
				t.Fatalf("persisted ceiling = %+v, want %d", fs.createdScopeCeiling.ScopeCeiling, tc.want)
			}
			if res.ScopeCeiling == nil || int32(*res.ScopeCeiling) != tc.want { //nolint:gosec // G115: *res.ScopeCeiling is a small bounded scope-ceiling test fixture well within int32 range.
				t.Fatalf("result ScopeCeiling = %v, want %d", res.ScopeCeiling, tc.want)
			}
		})
	}
}

// TestSubmitInputScopeInvalidBody: a non-integer body is rejected with
// ErrInvalidScopeCeiling BEFORE any write, so the audit row is never created.
func TestSubmitInputScopeInvalidBody(t *testing.T) {
	fs, svc, user, runID := scopeRunFixture(t)

	res, err := svc.SubmitInput(context.Background(), user, runID, "scope", "abc", nil)
	if !errors.Is(err, ErrInvalidScopeCeiling) {
		t.Fatalf("err = %v, want ErrInvalidScopeCeiling", err)
	}
	if fs.createdScopeCeiling != nil {
		t.Fatalf("an invalid body must not write a ceiling, got %+v", fs.createdScopeCeiling)
	}
	if res.ScopeCeiling != nil {
		t.Fatalf("rejected scope must return no ceiling, got %v", res.ScopeCeiling)
	}
}

// TestSubmitInputScopeRejectedShapes: a `scope` is meaningful ONLY on a milestone-
// structured issue run. A chat run (wrong kind) and an issue run with no frozen list
// (len 0) both return ErrScopeNotMilestoneRun and write nothing.
func TestSubmitInputScopeRejectedShapes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *store.Run)
	}{
		{"chat run is not a milestone issue run", func(r *store.Run) {
			r.Kind = runkind.Chat
		}},
		{"issue run with empty frozen list", func(r *store.Run) {
			r.MilestonesFrozen = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, svc, user, runID := scopeRunFixture(t)
			tc.mutate(&fs.runByID)
			_, err := svc.SubmitInput(context.Background(), user, runID, "scope", "4", nil)
			if !errors.Is(err, ErrScopeNotMilestoneRun) {
				t.Fatalf("err = %v, want ErrScopeNotMilestoneRun", err)
			}
			if fs.createdScopeCeiling != nil {
				t.Fatalf("a rejected scope shape must not write, got %+v", fs.createdScopeCeiling)
			}
		})
	}
}

// TestSubmitInputStopRemapsOnMilestoneIssueRun: a `stop` on a milestone-structured issue
// run is remapped to a scope write with ceiling = len(completed) (= 2), finalizing what
// is already done and starting nothing further. It does NOT go through the stop-verdict
// CTE (no stop_kind='stopped').
func TestSubmitInputStopRemapsOnMilestoneIssueRun(t *testing.T) {
	fs, svc, user, runID := scopeRunFixture(t)

	res, err := svc.SubmitInput(context.Background(), user, runID, "stop", "", nil)
	if err != nil {
		t.Fatalf("SubmitInput stop: %v", err)
	}
	if fs.createdScopeCeiling == nil {
		t.Fatal("a stop on a milestone issue run must write a scope ceiling")
	}
	if !fs.createdScopeCeiling.ScopeCeiling.Valid || fs.createdScopeCeiling.ScopeCeiling.Int32 != 2 {
		t.Fatalf("stop-remap ceiling = %+v, want len(completed)=2", fs.createdScopeCeiling.ScopeCeiling)
	}
	if res.ScopeCeiling == nil || *res.ScopeCeiling != 2 {
		t.Fatalf("result ScopeCeiling = %v, want 2", res.ScopeCeiling)
	}
	// The remap must NOT stamp a stop verdict — the disposition is settled at finalize.
	if fs.createdStopVerdict != nil {
		t.Fatalf("a milestone-run stop must not enqueue a stop verdict, got %+v", fs.createdStopVerdict)
	}
}

// TestSubmitInputStopStillRejectedOnNonInteractive: a `stop` on a run that is neither a
// milestone issue run nor an interactive task still returns ErrStopNotInteractive and
// writes no scope ceiling. A chat run is the shape that is neither.
func TestSubmitInputStopStillRejectedOnNonInteractive(t *testing.T) {
	fs, svc, user, runID := scopeRunFixture(t)
	fs.runByID.Kind = runkind.Chat // not a milestone issue run, not an interactive task

	_, err := svc.SubmitInput(context.Background(), user, runID, "stop", "", nil)
	if !errors.Is(err, ErrStopNotInteractive) {
		t.Fatalf("err = %v, want ErrStopNotInteractive", err)
	}
	if fs.createdScopeCeiling != nil {
		t.Fatalf("a rejected stop must not write a scope ceiling, got %+v", fs.createdScopeCeiling)
	}
	if fs.createdStopVerdict != nil {
		t.Fatalf("a rejected stop must not enqueue a stop verdict, got %+v", fs.createdStopVerdict)
	}
}

// TestSubmitInputScopeOwnerScoped: a `scope` against a run the caller does not own is a
// not-found (GetRunByIDForUser is owner-scoped; a foreign run reads ErrRunNotFound) and
// writes nothing — no row leaks to another tenant's run.
func TestSubmitInputScopeOwnerScoped(t *testing.T) {
	fs, svc, user, runID := scopeRunFixture(t)
	fs.runByIDErr = pgx.ErrNoRows // GetRun maps this to ErrRunNotFound

	_, err := svc.SubmitInput(context.Background(), user, runID, "scope", "4", nil)
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
	if fs.createdScopeCeiling != nil {
		t.Fatalf("a foreign run must not write a scope ceiling, got %+v", fs.createdScopeCeiling)
	}
}

// TestSubmitInputScopeLastWriterWins: two successive scope directives both write, and the
// SECOND recorded ceiling is the one that stands (last-writer-wins on the column). The
// fixture's window is [2,6], so 4 then 5 are both persisted verbatim and distinct.
func TestSubmitInputScopeLastWriterWins(t *testing.T) {
	fs, svc, user, runID := scopeRunFixture(t)

	if _, err := svc.SubmitInput(context.Background(), user, runID, "scope", "4", nil); err != nil {
		t.Fatalf("first scope: %v", err)
	}
	if _, err := svc.SubmitInput(context.Background(), user, runID, "scope", "5", nil); err != nil {
		t.Fatalf("second scope: %v", err)
	}
	if len(fs.createdScopeCeilings) != 2 {
		t.Fatalf("expected 2 scope writes, got %d", len(fs.createdScopeCeilings))
	}
	if got := fs.createdScopeCeilings[0].ScopeCeiling; !got.Valid || got.Int32 != 4 {
		t.Fatalf("first write ceiling = %+v, want 4", got)
	}
	last := fs.createdScopeCeilings[1].ScopeCeiling
	if !last.Valid || last.Int32 != 5 {
		t.Fatalf("last write ceiling = %+v, want 5", last)
	}
	// createdScopeCeiling tracks the last call, which must agree with the slice tail.
	if fs.createdScopeCeiling == nil || fs.createdScopeCeiling.ScopeCeiling.Int32 != 5 {
		t.Fatalf("createdScopeCeiling last = %+v, want 5", fs.createdScopeCeiling)
	}
}

// scopeCappedCompletedFixture builds a milestone issue run OWNED-BY-WORKER (SetState's
// runOwnedByWorker -> GetRunOwnedByWorker -> runOwned path) that already carries a
// scope_ceiling, with the completed transition wired to apply (setCompletedRows=1).
func scopeCappedCompletedFixture(t *testing.T, scopeCeilingValid bool) (*fakeStore, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	fs.runOwned.MilestonesFrozen = sixMilestonesFrozen()
	if scopeCeilingValid {
		fs.runOwned.ScopeCeiling = pgInt4(2)
	}
	fs.setCompletedRows = 1
	return fs, svc, wkr, runID
}

// TestSetStateCompletedSettlesAppliedOnScopeCappedCompletion: a `completed` report with
// ScopeCapped=true on a run that actually carries a scope_ceiling settles the pending
// scope audit row with disposition 'applied'.
func TestSetStateCompletedSettlesAppliedOnScopeCappedCompletion(t *testing.T) {
	fs, svc, wkr, runID := scopeCappedCompletedFixture(t, true)
	capped := true

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:       "completed",
		Branch:      strp("agent/issue-7"),
		ScopeCapped: &capped,
	})
	if err != nil || !applied {
		t.Fatalf("SetState completed: applied=%v err=%v", applied, err)
	}
	if fs.settledScope == nil {
		t.Fatal("SettleScopeInputDisposition not called on a scope-capped completion")
	}
	if fs.settledScope.RunID != runID {
		t.Fatalf("settle targeted run %v, want %v", fs.settledScope.RunID, runID)
	}
	if !fs.settledScope.Disposition.Valid || fs.settledScope.Disposition.String != "applied" {
		t.Fatalf("disposition = %+v, want 'applied'", fs.settledScope.Disposition)
	}
	// A genuine scope-capped completion also stamps stop_kind='scope_capped'.
	if fs.setCompleted == nil || !fs.setCompleted.StopKind.Valid || fs.setCompleted.StopKind.String != "scope_capped" {
		t.Fatalf("stop_kind = %+v, want 'scope_capped'", fs.setCompleted.StopKind)
	}
}

// TestSetStateCompletedSettlesDeclinedOnNormalCompletion: a `completed` report with no
// ScopeCapped declaration settles the pending scope row 'declined' — the directive did
// not change behaviour (e.g. the lead completed everything anyway). No scope_capped
// stop_kind is stamped.
func TestSetStateCompletedSettlesDeclinedOnNormalCompletion(t *testing.T) {
	fs, svc, wkr, runID := scopeCappedCompletedFixture(t, true)

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:  "completed",
		Branch: strp("agent/issue-7"),
		// ScopeCapped left nil — a normal completion despite the directive.
	})
	if err != nil || !applied {
		t.Fatalf("SetState completed: applied=%v err=%v", applied, err)
	}
	if fs.settledScope == nil {
		t.Fatal("SettleScopeInputDisposition not called on completion")
	}
	if !fs.settledScope.Disposition.Valid || fs.settledScope.Disposition.String != "declined" {
		t.Fatalf("disposition = %+v, want 'declined'", fs.settledScope.Disposition)
	}
	// A normal completion leaves stop_kind untouched (NULL), so nothing was minted.
	if fs.setCompleted != nil && fs.setCompleted.StopKind.Valid {
		t.Fatalf("normal completion must not stamp a stop_kind, got %+v", fs.setCompleted.StopKind)
	}
}

// TestSetStateCompletedScopeCappedWithoutCeilingIsNoOp: an untrusted worker declaring
// ScopeCapped=true on a run that carries NO scope_ceiling cannot mint 'applied' — the
// guard (owned.ScopeCeiling.Valid) fails. PRD #634 follow-up (P4): because the run never
// carried a directive there is no pending audit row to settle, so settleScopeDisposition
// stays "" and NO SettleScopeInputDisposition UPDATE is issued at all (previously it fired
// a 0-row 'declined'). No scope_capped stop_kind is stamped either.
func TestSetStateCompletedScopeCappedWithoutCeilingIsNoOp(t *testing.T) {
	fs, svc, wkr, runID := scopeCappedCompletedFixture(t, false) // no scope_ceiling on the run
	capped := true

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:       "completed",
		Branch:      strp("agent/issue-7"),
		ScopeCapped: &capped,
	})
	if err != nil || !applied {
		t.Fatalf("SetState completed: applied=%v err=%v", applied, err)
	}
	if fs.settledScope != nil {
		t.Fatalf("a run with no scope directive must issue no settle UPDATE, got %+v", fs.settledScope)
	}
	if fs.setCompleted != nil && fs.setCompleted.StopKind.Valid {
		t.Fatalf("no ceiling means no scope_capped stop_kind, got %+v", fs.setCompleted.StopKind)
	}
}

// TestSetStateCompletedNoDirectiveIssuesNoSettle: PRD #634 follow-up (P4). A normal
// `completed` report (no ScopeCapped) on a run that never carried a scope directive issues
// NO SettleScopeInputDisposition UPDATE — the old unconditional 'declined' fired a 0-row
// UPDATE on every completion, which this drops.
func TestSetStateCompletedNoDirectiveIssuesNoSettle(t *testing.T) {
	fs, svc, wkr, runID := scopeCappedCompletedFixture(t, false) // no scope_ceiling on the run

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:  "completed",
		Branch: strp("agent/issue-7"),
		// ScopeCapped left nil — an ordinary completion of a run with no directive.
	})
	if err != nil || !applied {
		t.Fatalf("SetState completed: applied=%v err=%v", applied, err)
	}
	if fs.settledScope != nil {
		t.Fatalf("a completion of a directive-free run must issue no settle UPDATE, got %+v", fs.settledScope)
	}
}

// scopeFailedFixture builds a milestone issue run OWNED-BY-WORKER wired for a `failed`
// terminal transition (SetRunFailed returns rows=1). When scopeCeilingValid is true the
// run carries a scope_ceiling — the P2a discriminator for "this run carried a directive".
func scopeFailedFixture(t *testing.T, scopeCeilingValid bool) (*fakeStore, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	fs.runOwned.MilestonesFrozen = sixMilestonesFrozen()
	if scopeCeilingValid {
		fs.runOwned.ScopeCeiling = pgInt4(2)
	}
	return fs, svc, wkr, runID
}

// TestSetStateFailedSettlesDeclinedOnScopeDirectedRun: PRD #634 follow-up (P2a). A
// scope-directed run that terminates as `failed` (agent failure — no stop_kind) never
// applied the cap, so its pending scope audit row is settled 'declined' rather than left
// NULL (which `run inputs`/the web card would render "active" for a terminal run).
func TestSetStateFailedSettlesDeclinedOnScopeDirectedRun(t *testing.T) {
	fs, svc, wkr, runID := scopeFailedFixture(t, true)

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:         "failed",
		FailureReason: strp("agent crashed"),
	})
	if err != nil || !applied {
		t.Fatalf("SetState failed: applied=%v err=%v", applied, err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed not called on an agent-failure terminal transition")
	}
	if fs.settledScope == nil {
		t.Fatal("SettleScopeInputDisposition not called on a scope-directed failed transition")
	}
	if fs.settledScope.RunID != runID {
		t.Fatalf("settle targeted run %v, want %v", fs.settledScope.RunID, runID)
	}
	if !fs.settledScope.Disposition.Valid || fs.settledScope.Disposition.String != "declined" {
		t.Fatalf("disposition = %+v, want 'declined'", fs.settledScope.Disposition)
	}
}

// TestSetStateFailedNoDirectiveIssuesNoSettle: PRD #634 follow-up (P2a). An ordinary
// `failed` transition on a run that never carried a scope directive issues NO settle
// UPDATE — the P2a settle is guarded on owned.ScopeCeiling.Valid.
func TestSetStateFailedNoDirectiveIssuesNoSettle(t *testing.T) {
	fs, svc, wkr, runID := scopeFailedFixture(t, false) // no scope_ceiling on the run

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:         "failed",
		FailureReason: strp("agent crashed"),
	})
	if err != nil || !applied {
		t.Fatalf("SetState failed: applied=%v err=%v", applied, err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed not called on an agent-failure terminal transition")
	}
	if fs.settledScope != nil {
		t.Fatalf("a failed run with no scope directive must issue no settle UPDATE, got %+v", fs.settledScope)
	}
}

// TestSubmitInputServerSideCancelSettlesDeclinedOnScopeDirectedRun: PRD #634 follow-up
// (P2b). A server-side `cancel` (no live poller — the run is queued) on a scope-directed
// run commits terminal OUTSIDE SetState, so the settle is issued directly in SubmitInput;
// the pending scope audit row is settled 'declined' rather than left "active" forever.
func TestSubmitInputServerSideCancelSettlesDeclinedOnScopeDirectedRun(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	fs := &fakeStore{runByID: store.Run{
		ID: runID, UserID: user, Kind: runkind.Issue, Status: "queued", // queued -> no live poller
		MilestonesFrozen: sixMilestonesFrozen(),
		ScopeCeiling:     pgInt4(2), // the run carried a scope directive
	}}
	svc := New(fs, newBox(t), testParams())

	res, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil)
	if err != nil {
		t.Fatalf("SubmitInput cancel: %v", err)
	}
	if !res.ServerSide {
		t.Fatal("a cancel on a queued run (no poller) must be applied server-side")
	}
	if fs.cancelled == nil {
		t.Fatal("CancelRunServerSide not called")
	}
	if fs.settledScope == nil {
		t.Fatal("SettleScopeInputDisposition not called on a server-side cancel of a scope-directed run")
	}
	if fs.settledScope.RunID != runID {
		t.Fatalf("settle targeted run %v, want %v", fs.settledScope.RunID, runID)
	}
	if !fs.settledScope.Disposition.Valid || fs.settledScope.Disposition.String != "declined" {
		t.Fatalf("disposition = %+v, want 'declined'", fs.settledScope.Disposition)
	}
}

// TestSubmitInputServerSideCancelNoDirectiveIssuesNoSettle: PRD #634 follow-up (P2b). A
// server-side cancel on a run with NO scope directive issues no settle UPDATE — the P2b
// settle is guarded on run.ScopeCeiling.Valid.
func TestSubmitInputServerSideCancelNoDirectiveIssuesNoSettle(t *testing.T) {
	user, runID := uuid.New(), uuid.New()
	fs := &fakeStore{runByID: store.Run{
		ID: runID, UserID: user, Kind: runkind.Issue, Status: "queued",
		// no ScopeCeiling — the run never carried a directive
	}}
	svc := New(fs, newBox(t), testParams())

	res, err := svc.SubmitInput(context.Background(), user, runID, "cancel", "", nil)
	if err != nil {
		t.Fatalf("SubmitInput cancel: %v", err)
	}
	if !res.ServerSide {
		t.Fatal("a cancel on a queued run (no poller) must be applied server-side")
	}
	if fs.settledScope != nil {
		t.Fatalf("a server-side cancel of a directive-free run must issue no settle UPDATE, got %+v", fs.settledScope)
	}
}

// scopeLimitOptOutFixture builds a milestone issue run OWNED-BY-WORKER whose owner opted
// OUT of usage-limit parking (WaitOnLimit=false), the primary signal decideLimitPark reads
// via limitParkInput.WaitOnLimit. A limit_wait report on such a run is COERCED to a
// terminal `failed` (SetRunFailed, which the fake returns rows=1 for) rather than parked.
// When scopeCeilingValid is true the run carries a scope_ceiling — the "this run carried a
// directive" discriminator for the P2 settle.
func scopeLimitOptOutFixture(t *testing.T, scopeCeilingValid bool) (*fakeStore, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	fs.runOwned.MilestonesFrozen = sixMilestonesFrozen()
	fs.runOwned.WaitOnLimit = false // opt-out: decideLimitPark returns Park=false
	if scopeCeilingValid {
		fs.runOwned.ScopeCeiling = pgInt4(2)
	}
	return fs, svc, wkr, runID
}

// TestSetStateLimitWaitOptOutSettlesDeclinedOnScopeDirectedRun: PRD #634 follow-up (issue
// #640 P2). A scope-directed run whose owner opted out of parking hits the rate-limit
// opt-out in setLimitWait — a terminal SetRunFailed. That never sets the scope disposition,
// so its pending audit row is settled 'declined' here rather than left NULL (which `run
// inputs`/the web card would render "active (scope ceiling set)" for a terminal run).
func TestSetStateLimitWaitOptOutSettlesDeclinedOnScopeDirectedRun(t *testing.T) {
	fs, svc, wkr, runID := scopeLimitOptOutFixture(t, true)

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:         "limit_wait",
		RateLimitType: strp("five_hour"),
	})
	if err != nil || !applied {
		t.Fatalf("SetState limit_wait opt-out: applied=%v err=%v", applied, err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed not called on a coerced opt-out terminal transition")
	}
	if fs.setLimitWait != nil {
		t.Fatalf("an opt-out run must not park, got %+v", fs.setLimitWait)
	}
	if fs.settledScope == nil {
		t.Fatal("SettleScopeInputDisposition not called on a scope-directed opt-out failure")
	}
	if fs.settledScope.RunID != runID {
		t.Fatalf("settle targeted run %v, want %v", fs.settledScope.RunID, runID)
	}
	if !fs.settledScope.Disposition.Valid || fs.settledScope.Disposition.String != "declined" {
		t.Fatalf("disposition = %+v, want 'declined'", fs.settledScope.Disposition)
	}
}

// TestSetStateLimitWaitOptOutNoDirectiveIssuesNoSettle: PRD #634 follow-up (issue #640 P2).
// A rate-limit opt-out on a run that never carried a scope directive issues NO settle UPDATE
// — the settle is guarded on run.ScopeCeiling.Valid.
func TestSetStateLimitWaitOptOutNoDirectiveIssuesNoSettle(t *testing.T) {
	fs, svc, wkr, runID := scopeLimitOptOutFixture(t, false) // no scope_ceiling on the run

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:         "limit_wait",
		RateLimitType: strp("five_hour"),
	})
	if err != nil || !applied {
		t.Fatalf("SetState limit_wait opt-out: applied=%v err=%v", applied, err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed not called on a coerced opt-out terminal transition")
	}
	if fs.settledScope != nil {
		t.Fatalf("an opt-out failure of a directive-free run must issue no settle UPDATE, got %+v", fs.settledScope)
	}
}

// TestSetStateRunningNeverSettlesScope: a non-completed transition (a `running` report)
// must NEVER settle a scope disposition — settleScopeDisposition stays empty and the
// settle UPDATE is not issued, even on a run that carries a scope_ceiling.
func TestSetStateRunningNeverSettlesScope(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	fs.runOwned.MilestonesFrozen = sixMilestonesFrozen()
	fs.runOwned.ScopeCeiling = pgInt4(2)

	_, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:               "running",
		MilestonesCompleted: strptr("m1"),
	})
	if err != nil || !applied {
		t.Fatalf("SetState running: applied=%v err=%v", applied, err)
	}
	if fs.settledScope != nil {
		t.Fatalf("a running transition must not settle a scope disposition, got %+v", fs.settledScope)
	}
}
