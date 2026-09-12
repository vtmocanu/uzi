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
// will need the moment it resumes. Both gates admit 'queued' — the Go terminal guard
// (terminalStatuses = completed/failed/cancelled) and the CTE predicate (status NOT IN those
// three) — and a future tightening of either to status='running' would silently break D4,
// which this test would catch. A queued run has no wall deadline yet (runWallClock requires
// 'running'), so DeadlineAt is nil while the extension is still written and returned.
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

// TestSubmitInputExtendOverCapRequest: a single request larger than the WHOLE cap can never
// apply and is refused as ErrExtensionCapExceeded BEFORE the CTE (guarding the int32 overflow
// the addition would otherwise risk), so CreateExtendInput is never called.
func TestSubmitInputExtendOverCapRequest(t *testing.T) {
	fs, svc, user, runID := extendRunFixture(t, 57600)

	_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "57601", nil)
	if !errors.Is(err, ErrExtensionCapExceeded) {
		t.Fatalf("err = %v, want ErrExtensionCapExceeded", err)
	}
	if fs.createdExtend != nil {
		t.Fatalf("a request larger than the whole cap must not reach the CTE, got %+v", fs.createdExtend)
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

	t.Run("column over cap maps to ErrExtensionCapExceeded", func(t *testing.T) {
		fs, svc, user, runID := extendRunFixture(t, 57600)
		// Already extended to 55000s; a further 7200 would push past 57600, so the CTE's
		// cap predicate refuses (0 rows) even though 7200 <= the whole cap.
		fs.runByID.BudgetExtensionSeconds = 55000
		fs.extendErr = pgx.ErrNoRows

		_, err := svc.SubmitInput(context.Background(), user, runID, "extend", "7200", nil)
		if !errors.Is(err, ErrExtensionCapExceeded) {
			t.Fatalf("err = %v, want ErrExtensionCapExceeded", err)
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
// (PRD #1189 M1), decided from the already-fetched run row — mirroring TestPauseRefusalReason.
// Terminal first, then a kind that never times out (chat/judge/interactive task), then the
// residual cap cause, whose message names the remaining allowance.
func TestExtendRefusalReason(t *testing.T) {
	t.Run("terminal maps to ErrRunTerminal", func(t *testing.T) {
		err := extendRefusalReason(store.Run{Status: "failed", Kind: runkind.Issue}, 7200, 57600)
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
			err := extendRefusalReason(tc.run, 7200, 57600)
			if !errors.Is(err, ErrExtendNotTimed) {
				t.Fatalf("err = %v, want ErrExtendNotTimed", err)
			}
		})
	}
	t.Run("over-cap maps to ErrExtensionCapExceeded and names the remaining allowance", func(t *testing.T) {
		// Already extended to 55000s of a 57600s cap → 2600s (43m) of allowance remains.
		err := extendRefusalReason(store.Run{Status: "running", Kind: runkind.Issue, BudgetExtensionSeconds: 55000}, 7200, 57600)
		if !errors.Is(err, ErrExtensionCapExceeded) {
			t.Fatalf("err = %v, want ErrExtensionCapExceeded", err)
		}
		if got := err.Error(); got != "extension cap exceeded: 2h exceeds the remaining extension cap (43m left of 16h)" {
			t.Fatalf("message = %q, want it to name the remaining allowance (43m of 16h)", got)
		}
	})
}
