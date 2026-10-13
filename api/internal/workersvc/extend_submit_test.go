package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// extendRunFixture builds a running, owner-owned issue run (started 1h ago so RunDeadline is
// non-nil) wired for the submitInput `extend` path (GetRun -> GetRunByIDForUser), plus a
// Service whose health-settings reader serves the given extension cap. It returns the fake,
// the service, and the (user, run) identity to submit under.
func extendRunFixture(t *testing.T, capSeconds int) (*fakeStore, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	user, runID := uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID: store.Run{
			ID:        runID,
			UserID:    user,
			Kind:      runkind.Issue,
			Status:    "running",
			StartedAt: pgconv.Time(time.Now().Add(-1 * time.Hour)),
		},
	}
	svc := New(fs, newBox(t), testParams())
	svc.SetHealthSettings(fakeHealthSettings{enabled: true, runExtensionCap: capSeconds})
	return fs, svc, user, runID
}

// TestSubmitInputExtendAcceptsMinimum: a body of exactly the 60s floor reaches
// CreateExtendInput (the boundary is >= 60, so 60 is valid). The write carries the parsed
// seconds and the served cap.
func TestSubmitInputExtendAcceptsMinimum(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)
	fs.extendReturn = 60

	res, err := svc.SubmitInput(context.Background(), user, runID, "extend", "60", nil)
	if err != nil {
		t.Fatalf("SubmitInput extend 60: %v", err)
	}
	if fs.createdExtend == nil {
		t.Fatal("body 60 must reach CreateExtendInput, got no write")
	}
	if fs.createdExtend.Secs != 60 {
		t.Fatalf("CreateExtendInput Secs = %d, want 60", fs.createdExtend.Secs)
	}
	if res.ExtensionSeconds == nil || *res.ExtensionSeconds != 60 {
		t.Fatalf("result ExtensionSeconds = %v, want 60", res.ExtensionSeconds)
	}
}

// TestSubmitInputExtendQueuedRunSucceeds pins PRD #1189 D4: extend is allowed on any
// non-terminal timed run, INCLUDING a parked (queued) one, so an owner can grant time the run
// will need the moment it resumes. This test pins the GO half — the terminalStatuses guard
// (completed/failed/cancelled) admits 'queued', so the extend branch runs and returns the
// extension. The fake CreateExtendInput bypasses the SQL, so the CTE predicate's own admission
// of 'queued' is pinned separately by TestCreateExtendInputQueuedRunLiveDB (store). A queued
// run has no wall deadline yet (runWallClock requires 'running'), so DeadlineAt is nil while
// the extension is still written and returned.
func TestSubmitInputExtendQueuedRunSucceeds(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)
	fs.runByID.Status = "queued" // a parked run (started earlier, requeued); StartedAt stays set
	fs.extendReturn = 3600

	res, err := svc.SubmitInput(context.Background(), user, runID, "extend", "3600", nil)
	if err != nil {
		t.Fatalf("SubmitInput extend on a queued run: %v", err)
	}
	if fs.createdExtend == nil || fs.createdExtend.Secs != 3600 {
		t.Fatalf("queued extend must reach CreateExtendInput with Secs=3600, got %+v", fs.createdExtend)
	}
	if res.ExtensionSeconds == nil || *res.ExtensionSeconds != 3600 {
		t.Fatalf("result ExtensionSeconds = %v, want 3600", res.ExtensionSeconds)
	}
	if res.DeadlineAt != nil {
		t.Fatalf("a queued (not-running) run has no wall deadline yet, want DeadlineAt nil, got %v", *res.DeadlineAt)
	}
}

// TestSubmitInputExtendInvalidBody: a body that is not a whole number of seconds >= 60 is
// rejected with ErrInvalidExtension BEFORE the cap is read or any row is written. 59 (below
// the floor), 0, a negative, a non-integer and the empty string all reject.
func TestSubmitInputExtendInvalidBody(t *testing.T) {
	for _, body := range []string{"59", "0", "-5", "abc", ""} {
		t.Run("body_"+body, func(t *testing.T) {
			fs, svc, user, runID := extendRunFixture(t, 57600)
			_, err := svc.SubmitInput(context.Background(), user, runID, "extend", body, nil)
			if !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("SubmitInput extend %q: err = %v, want ErrInvalidExtension", body, err)
			}
			if fs.createdExtend != nil {
				t.Fatalf("an invalid body must not write an extension, got %+v", fs.createdExtend)
			}
		})
	}
}

// TestSubmitInputExtendDisabled: a cap of 0 is the admin off-switch. A valid body still fails
// with ErrExtendDisabled and never calls CreateExtendInput.
func TestSubmitInputExtendDisabled(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 0) // extending disabled instance-wide

	_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "60", nil)
	if !errors.Is(err, ErrExtendDisabled) {
		t.Fatalf("err = %v, want ErrExtendDisabled", err)
	}
	if fs.createdExtend != nil {
		t.Fatalf("a disabled extend must not write, got %+v", fs.createdExtend)
	}
}

// TestSubmitInputExtendOverCapRequest: the cap is a PER-REQUEST maximum (#2735). A single
// request larger than it can never apply and is refused as ErrExtensionCapExceeded BEFORE the
// CTE, with a message naming the per-extension maximum (no remaining-allowance arithmetic), so
// CreateExtendInput is never called. A run already extended past the cap in total is NOT
// refused for that: a second request within the cap reaches the CTE.
func TestSubmitInputExtendOverCapRequest(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)

	_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "57660", nil)
	if !errors.Is(err, ErrExtensionCapExceeded) {
		t.Fatalf("err = %v, want ErrExtensionCapExceeded", err)
	}
	if got, want := err.Error(), "extension cap exceeded: +16h1m is more than the 16h maximum per extension"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if fs.createdExtend != nil {
		t.Fatalf("a request larger than the cap must not reach the CTE, got %+v", fs.createdExtend)
	}
}

// TestSubmitInputExtendPastOldCumulativeCap: with the cap per request, a run already extended
// to 55000s accepts another 7200s (the old cumulative rule refused it) and the write carries
// the technical-bound parameters.
func TestSubmitInputExtendPastOldCumulativeCap(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)
	fs.runByID.BudgetExtensionSeconds = 55000
	fs.extendReturn = 62200

	res, err := svc.SubmitInput(context.Background(), user, runID, "extend", "7200", nil)
	if err != nil {
		t.Fatalf("SubmitInput extend past the old cumulative cap: %v", err)
	}
	if res.ExtensionSeconds == nil || *res.ExtensionSeconds != 62200 {
		t.Fatalf("ExtensionSeconds = %v, want 62200", res.ExtensionSeconds)
	}
	w := fs.createdExtend
	if w == nil {
		t.Fatal("CreateExtendInput not called")
	}
	if w.MaxWallSeconds != 2147483 || w.FinalizeAllowanceSeconds != 1800 || w.BaseWallUpperSeconds != 72*3600 {
		t.Fatalf("bound params = max %d finalize %d baseWall %d, want 2147483 / 1800 / 259200 (the 72h maximum ceiling)", w.MaxWallSeconds, w.FinalizeAllowanceSeconds, w.BaseWallUpperSeconds)
	}
	if !w.Body.Valid || w.Body.String != "extend +2h (max 16h per extension)" {
		t.Fatalf("audit body = %+v, want %q", w.Body, "extend +2h (max 16h per extension)")
	}
}

// TestSubmitInputExtendOverRoomRefusedBeforeCTE: a request within the per-request cap but
// beyond the room left under MaxRunWallSeconds is ErrExtensionLimitReached without a write,
// naming the total-time limit and the room left.
func TestSubmitInputExtendOverRoomRefusedBeforeCTE(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 604800)
	// testParams: RunTimeout 2h, RunWallCeiling 8h, reserved at the 72h maximum -> room = 2147483 - 259200 - 1800 - ext.
	fs.runByID.BudgetExtensionSeconds = 2147483 - 259200 - 1800 - 3600 // 3600s of room left

	_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "3660", nil)
	if !errors.Is(err, ErrExtensionLimitReached) {
		t.Fatalf("err = %v, want ErrExtensionLimitReached", err)
	}
	if got, want := err.Error(), "total run time limit reached: +1h1m would take this run past the 24d 20h 31m 23s limit on total run time (1h left)"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if fs.createdExtend != nil {
		t.Fatalf("an over-room request must not reach the CTE, got %+v", fs.createdExtend)
	}
	// Exactly the room left applies (the pre-check allows it).
	fs.extendReturn = 2147483 - 259200 - 1800
	res, err := svc.SubmitInput(context.Background(), user, runID, "extend", "3600", nil)
	if err != nil {
		t.Fatalf("a request exactly filling the room must reach the CTE and apply: %v", err)
	}
	if fs.createdExtend == nil || fs.createdExtend.Secs != 3600 {
		t.Fatalf("the exact-room request must be written via CreateExtendInput with secs=3600, got %+v", fs.createdExtend)
	}
	if res.ExtensionSeconds == nil || *res.ExtensionSeconds != int(fs.extendReturn) {
		t.Fatalf("result ExtensionSeconds = %v, want the CTE total %d", res.ExtensionSeconds, fs.extendReturn)
	}
}

// TestSubmitInputExtendSuccess: a valid body under the cap writes via CreateExtendInput with
// the parsed seconds, the served cap, and a non-empty audit body; the result carries the new
// total (the CTE's RETURNING) and a non-nil projected deadline for the CLI to print.
func TestSubmitInputExtendSuccess(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)
	fs.extendReturn = 7200 // the CTE returns the new total budget_extension_seconds

	res, err := svc.SubmitInput(context.Background(), user, runID, "extend", "7200", nil)
	if err != nil {
		t.Fatalf("SubmitInput extend 7200: %v", err)
	}
	if fs.createdExtend == nil {
		t.Fatal("CreateExtendInput not called on a valid extend")
	}
	if fs.createdExtend.ID != runID {
		t.Fatalf("extend write targeted run %v, want %v", fs.createdExtend.ID, runID)
	}
	if fs.createdExtend.Secs != 7200 {
		t.Fatalf("CreateExtendInput Secs = %d, want 7200", fs.createdExtend.Secs)
	}
	if fs.createdExtend.Cap != 57600 {
		t.Fatalf("CreateExtendInput Cap = %d, want 57600 (the served cap)", fs.createdExtend.Cap)
	}
	if !fs.createdExtend.Body.Valid || fs.createdExtend.Body.String == "" {
		t.Fatalf("CreateExtendInput Body = %+v, want a non-empty audit body", fs.createdExtend.Body)
	}
	if res.ExtensionSeconds == nil || *res.ExtensionSeconds != 7200 {
		t.Fatalf("result ExtensionSeconds = %v, want 7200 (the new total)", res.ExtensionSeconds)
	}
	if res.DeadlineAt == nil {
		t.Fatal("result DeadlineAt must be non-nil for a running issue run")
	}
}

// TestSubmitInputExtendDeadlineFromCommittedTotal pins PRD #1189 D4 concurrency correctness
// (CodeRabbit !1259): submitExtend must derive the returned DeadlineAt from the COMMITTED total
// CreateExtendInput returns, NOT from the pre-write run.BudgetExtensionSeconds + secs. If a
// concurrent extend already bumped the row, the pre-write projection is stale; the response must
// reflect the real committed total so ExtensionSeconds and DeadlineAt never disagree. The unfixed
// code computed the deadline from 0+3600 and would fail both the committed-total and the
// not-the-stale-projection assertions below.
func TestSubmitInputExtendDeadlineFromCommittedTotal(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)
	fs.runByID.BudgetExtensionSeconds = 0 // the value we read BEFORE the write
	const committed = 10800               // a concurrent extend already pushed the total past 0+3600
	fs.extendReturn = committed

	res, err := svc.SubmitInput(context.Background(), user, runID, "extend", "3600", nil)
	if err != nil {
		t.Fatalf("SubmitInput extend 3600: %v", err)
	}
	if res.ExtensionSeconds == nil || *res.ExtensionSeconds != committed {
		t.Fatalf("result ExtensionSeconds = %v, want %d (the committed total)", res.ExtensionSeconds, committed)
	}
	run := fs.runByID
	wantDeadline := RunDeadline(run.StartedAt, run.BudgetWallSeconds, run.BudgetPausedSeconds, run.Kind, run.Interactive, run.Status, svc.p.RunTimeout, committed, run.BudgetFinalizeSeconds)
	staleDeadline := RunDeadline(run.StartedAt, run.BudgetWallSeconds, run.BudgetPausedSeconds, run.Kind, run.Interactive, run.Status, svc.p.RunTimeout, 3600, run.BudgetFinalizeSeconds)
	if res.DeadlineAt == nil || wantDeadline == nil {
		t.Fatal("a running issue run must have a non-nil deadline")
	}
	if !res.DeadlineAt.Equal(*wantDeadline) {
		t.Fatalf("DeadlineAt = %v, want %v (from the committed total %d)", res.DeadlineAt, wantDeadline, committed)
	}
	if staleDeadline != nil && res.DeadlineAt.Equal(*staleDeadline) {
		t.Fatal("DeadlineAt matches the stale pre-write projection (0+3600); it must use the committed total")
	}
}

// TestSubmitInputExtendCTERefusal: a 0-row CreateExtendInput (pgx.ErrNoRows) is disambiguated
// from the already-fetched run row. A chat run (which never times out) that somehow reached the
// CTE maps to ErrExtendNotTimed; a run whose column-stored extension leaves no room under the
// cap maps to ErrExtensionCapExceeded. Both reach the CTE (the top guards passed), so the
// classification comes from extendRefusalReason, not the pre-CTE guards.
func TestSubmitInputExtendCTERefusal(t *testing.T) {
	t.Run("untimed kind maps to ErrExtendNotTimed", func(t *testing.T) {
		fs, svc, user, runID := extendRunFixture(t, 57600)
		fs.runByID.Kind = runkind.Chat // a chat run has no wall-clock timeout
		fs.extendErr = pgx.ErrNoRows

		_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "7200", nil)
		if !errors.Is(err, ErrExtendNotTimed) {
			t.Fatalf("err = %v, want ErrExtendNotTimed", err)
		}
	})

	t.Run("0 rows within cap and room maps to ErrExtensionLimitReached", func(t *testing.T) {
		fs, svc, user, runID := extendRunFixture(t, 57600)
		// The pre-check passed on the snapshot but the CTE refused (a concurrent extend consumed
		// the room): the residual cause is the total-time limit, never the per-request cap.
		fs.extendErr = pgx.ErrNoRows

		_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "7200", nil)
		if !errors.Is(err, ErrExtensionLimitReached) {
			t.Fatalf("err = %v, want ErrExtensionLimitReached", err)
		}
	})
}

// TestSubmitInputExtendOwnerScoped: an extend against a run the caller does not own is a
// not-found (GetRun is owner-scoped) and writes nothing.
func TestSubmitInputExtendOwnerScoped(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)
	fs.runByIDErr = pgx.ErrNoRows // GetRun maps this to ErrRunNotFound

	_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "7200", nil)
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
	if fs.createdExtend != nil {
		t.Fatalf("a foreign run must not write an extension, got %+v", fs.createdExtend)
	}
}

// TestSubmitInputExtendTerminalRefusedAtTopGuard: a terminal run is 409'd by SubmitInput's own
// top-of-function terminal guard BEFORE the extend branch, so it never reaches the CTE. This
// pins that a dead run's clock cannot be extended (the same guard scope/pause rely on).
func TestSubmitInputExtendTerminalRefusedAtTopGuard(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)
	fs.runByID.Status = "completed"

	_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "7200", nil)
	if !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("err = %v, want ErrRunTerminal", err)
	}
	if fs.createdExtend != nil {
		t.Fatalf("a terminal run must not reach the extend CTE, got %+v", fs.createdExtend)
	}
}

// TestExtendRefusalReason pins the three 409 classes a 0-row CreateExtendInput collapses into
// (PRD #1189 M1, #2735), decided from the already-fetched run row — mirroring
// TestPauseRefusalReason. Terminal first, then a kind that never times out (chat/judge/
// interactive task), then the per-request cap, then the residual total-time-limit cause.
func TestExtendRefusalReason(t *testing.T) {
	t.Run("terminal maps to ErrRunTerminal", func(t *testing.T) {
		err := extendRefusalReason(store.Run{Status: "failed", Kind: runkind.Issue}, 7200, 57600, 2*time.Hour, 8*time.Hour)
		if !errors.Is(err, ErrRunTerminal) {
			t.Fatalf("err = %v, want ErrRunTerminal", err)
		}
	})
	for _, tc := range []struct {
		name string
		run  store.Run
	}{
		{"chat", store.Run{Status: "running", Kind: runkind.Chat}},
		{"judge", store.Run{Status: "running", Kind: runkind.Judge}},
		{"interactive task", store.Run{Status: "running", Kind: runkind.Task, Interactive: true}},
	} {
		t.Run(tc.name+" maps to ErrExtendNotTimed", func(t *testing.T) {
			err := extendRefusalReason(tc.run, 7200, 57600, 2*time.Hour, 8*time.Hour)
			if !errors.Is(err, ErrExtendNotTimed) {
				t.Fatalf("err = %v, want ErrExtendNotTimed", err)
			}
		})
	}
	t.Run("over-cap maps to ErrExtensionCapExceeded and names the per-extension maximum", func(t *testing.T) {
		// A cumulative total already past the cap is irrelevant: only the request is compared.
		err := extendRefusalReason(store.Run{Status: "running", Kind: runkind.Issue, BudgetExtensionSeconds: 55000}, 57660, 57600, 2*time.Hour, 8*time.Hour)
		if !errors.Is(err, ErrExtensionCapExceeded) {
			t.Fatalf("err = %v, want ErrExtensionCapExceeded", err)
		}
		if got := err.Error(); got != "extension cap exceeded: +16h1m is more than the 16h maximum per extension" {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("within cap maps to ErrExtensionLimitReached and names the room left", func(t *testing.T) {
		run := store.Run{Status: "running", Kind: runkind.Issue, BudgetExtensionSeconds: 2147483 - 259200 - 1800 - 3600}
		err := extendRefusalReason(run, 7200, 57600, 2*time.Hour, 8*time.Hour)
		if !errors.Is(err, ErrExtensionLimitReached) {
			t.Fatalf("err = %v, want ErrExtensionLimitReached", err)
		}
		if got := err.Error(); got != "total run time limit reached: +2h would take this run past the 24d 20h 31m 23s limit on total run time (1h left)" {
			t.Fatalf("message = %q", got)
		}
	})
}
