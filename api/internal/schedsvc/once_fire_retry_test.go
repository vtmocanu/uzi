package schedsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4512 (ADR-4007 T3a), unit level: the scheduler's progress handling against the
// in-memory ledger and summary writer (once_fire_fakes_test.go). The PostgreSQL 17 proofs of the
// same behaviour are in once_fire_retry_livedb_test.go. Letters follow the #4512 M3 list.

func onceSweep(h *harness, cap int32) store.RunSchedule {
	s := h.sweepSchedule(pgtype.Int4{Int32: cap, Valid: cap > 0})
	s.Timing, s.CronExpr = "once", pgtype.Text{}
	s.FireEpoch = 7 // a non-zero claimed epoch proves the epoch is threaded, not defaulted
	return s
}

func sweepRows(iids ...int64) []store.ListSweepCandidateIssuesRow {
	out := make([]store.ListSweepCandidateIssuesRow, 0, len(iids))
	for _, iid := range iids {
		out = append(out, store.ListSweepCandidateIssuesRow{ForgeIssueIid: iid})
	}
	return out
}

func lastWritten(t *testing.T, raw []byte) lastFireRecord {
	t.Helper()
	var rec lastFireRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// The discriminator: only a scheduled tick on a one-time sweep loads progress and passes a
// progress context; RunNow, recurring sweeps and the issue target never do.
func TestOnceFireRetry_ProgressOnlyForScheduledOnceSweep(t *testing.T) {
	t.Run("scheduled once sweep threads the claimed epoch, identity and cap", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96, 97)
		s := onceSweep(h, 2)
		h.st.due = []store.RunSchedule{s}
		h.sched.Boot(context.Background())
		if len(h.runs.progressSeen) != 2 || h.st.progressReads != 1 {
			t.Fatalf("progress contexts = %d, ledger reads = %d, want 2 and 1", len(h.runs.progressSeen), h.st.progressReads)
		}
		p := h.runs.progressSeen[0]
		if p.ScheduleID != s.ID || p.UserID != h.owner || p.RepoID != h.repoID || p.FireEpoch != 7 || p.IssueIID != 96 || p.EffectiveCap == nil || *p.EffectiveCap != 2 {
			t.Fatalf("progress = %+v", p)
		}
	})
	t.Run("RunNow reads and writes nothing", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96, 97)
		h.runs.ledger.seed(uuid.New(), 7, nil, 1, "x") // an unrelated fire in the ledger
		s := onceSweep(h, 2)
		out, err := h.sched.RunNow(context.Background(), s)
		if err != nil || len(out.Started) != 2 {
			t.Fatalf("RunNow = %+v, %v", out, err)
		}
		if h.st.progressReads != 0 || len(h.runs.progressSeen) != 0 || h.runs.summaryCalls != 0 || len(h.st.advanceCalls)+len(h.st.partialCalls) != 0 {
			t.Fatalf("RunNow touched the once-fire machinery: reads=%d progress=%d summary=%d", h.st.progressReads, len(h.runs.progressSeen), h.runs.summaryCalls)
		}
	})
	t.Run("recurring sweep", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96)
		h.st.due = []store.RunSchedule{h.sweepSchedule(pgtype.Int4{Int32: 2, Valid: true})}
		h.sched.Boot(context.Background())
		if h.st.progressReads != 0 || len(h.runs.progressSeen) != 0 || h.runs.summaryCalls != 0 || len(h.st.advanceCalls) != 1 {
			t.Fatalf("recurring sweep used progress: reads=%d progress=%d summary=%d", h.st.progressReads, len(h.runs.progressSeen), h.runs.summaryCalls)
		}
		if h.st.writeEpochs[0] != 0 {
			t.Fatalf("recurring advance epoch = %d", h.st.writeEpochs[0])
		}
	})
	t.Run("once issue target advances through the guarded once advance", func(t *testing.T) {
		h := newHarness()
		s := h.issueSchedule()
		s.Timing, s.CronExpr, s.FireEpoch = "once", pgtype.Text{}, 5
		h.st.due = []store.RunSchedule{s}
		h.sched.Boot(context.Background())
		if len(h.runs.progressSeen) != 0 || h.runs.summaryCalls != 0 || len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" || h.st.writeEpochs[0] != 5 {
			t.Fatalf("once issue fire: progress=%d summary=%d advances=%+v epochs=%v", len(h.runs.progressSeen), h.runs.summaryCalls, h.st.advanceCalls, h.st.writeEpochs)
		}
	})
}

// (h) once + remove_label_on_dispatch stays refused with config_not_supported, advanced through
// the summary writer with no start.
func TestOnceFireRetry_H_RemoveLabelOnceStaysRefused(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = sweepRows(96)
	s := onceSweep(h, 2)
	s.RemoveLabelOnDispatch = true
	h.st.due = []store.RunSchedule{s}
	h.sched.Boot(context.Background())
	if len(h.runs.autopilot) != 0 || len(h.st.advanceCalls) != 1 {
		t.Fatalf("created=%d advances=%+v", len(h.runs.autopilot), h.st.advanceCalls)
	}
	rec := lastWritten(t, h.st.advanceCalls[0].LastFire)
	if len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipConfigNotSupported) || len(rec.Started) != 0 {
		t.Fatalf("summary = %+v", rec)
	}
}

type captureHandler struct {
	slog.Handler
	warns *int
}

func (c captureHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		*c.warns++
	}
	return c.Handler.Handle(ctx, r)
}

func capacityOnceSweep(h *harness, cap, limit, needed int32) store.RunSchedule {
	s := onceSweep(h, cap)
	s.CapacityLimit = pgtype.Int4{Int32: limit, Valid: true}
	s.CapacityRoomNeeded = pgtype.Int4{Int32: needed, Valid: true}
	return s
}

// (i) a capacity-blocked continuation holds quietly and visibly and is not consumed; two held
// ticks with different in-flight counts write once.
func TestOnceFireRetry_I_CapacityBlockedContinuationHoldsQuietly(t *testing.T) {
	h := newHarness()
	var warns int
	h.sched.logger = slog.New(captureHandler{Handler: slog.NewTextHandler(&bytes.Buffer{}, nil), warns: &warns})
	h.st.sweepRows = sweepRows(96, 97, 98)
	s := capacityOnceSweep(h, 3, 2, 2)
	h.runs.ledger.seed(s.ID, 7, intPtr(3), 96, "t96")
	h.st.due = []store.RunSchedule{s}
	h.st.inFlight = 5
	h.sched.Boot(context.Background())
	h.st.inFlight = 3 // a different in-flight count; room is still 0
	h.sched.Boot(context.Background())
	if len(h.runs.autopilot) != 0 || len(h.st.advanceCalls) != 0 || len(h.st.statusCalls) != 0 {
		t.Fatalf("a held continuation must create, advance and park nothing: created=%d advances=%d", len(h.runs.autopilot), len(h.st.advanceCalls))
	}
	if warns != 0 {
		t.Fatalf("a quiet hold logged %d warnings", warns)
	}
	if len(h.st.partialCalls) != 1 {
		t.Fatalf("partial writes = %d, want exactly 1 across two held ticks with different in-flight counts", len(h.st.partialCalls))
	}
	rec := lastWritten(t, h.st.partialCalls[0].LastFire)
	if len(rec.Started) != 1 || rec.Started[0].Title != "t96" || rec.Capacity == nil || !rec.Capacity.Blocked || rec.Capacity.Limit != 2 || rec.Capacity.InFlight != 5 || rec.Capacity.Room != 0 || len(rec.Skips) != 0 {
		t.Fatalf("held summary = %+v, want the folded start, the observed in-flight 5, a blocked capacity and no skip", rec)
	}
	if len(h.fb.f.getIID) != 0 {
		t.Fatalf("a held continuation must make no forge call: %v", h.fb.f.getIID)
	}
	// Room returns: the fire resumes with only its remaining slots and completes.
	h.st.inFlight = 0
	h.sched.Boot(context.Background())
	if len(h.runs.autopilot) != 2 || h.runs.autopilot[0].issueIID != 97 || len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
		t.Fatalf("resume created %+v advances=%+v", h.runs.autopilot, h.st.advanceCalls)
	}
}

func intPtr(n int) *int { return &n }

// (i) a continuation skips the room-needed admission check: room 1 < room needed 2 still starts
// one new run, bounded by the room.
func TestOnceFireRetry_I_ContinuationSkipsRoomNeededCheck(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = sweepRows(96, 97, 98)
	s := capacityOnceSweep(h, 4, 2, 2)
	h.runs.ledger.seed(s.ID, 7, intPtr(4), 96, "t96")
	h.st.due = []store.RunSchedule{s}
	h.st.inFlight = 1 // room 1, room needed 2
	h.sched.Boot(context.Background())
	if len(h.runs.autopilot) != 1 || h.runs.autopilot[0].issueIID != 97 {
		t.Fatalf("created %+v, want only 97 (room bounds the new starts)", h.runs.autopilot)
	}
	if p := h.runs.progressSeen[0]; p.EffectiveCap == nil || *p.EffectiveCap != 4 {
		t.Fatalf("a continuation passes the stored cap, got %+v", p.EffectiveCap)
	}
}

// (i) a continuation whose prior starts reach the cap advances without waiting on capacity or the forge.
func TestOnceFireRetry_I_CapSpentContinuationAdvancesWithoutWaiting(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = sweepRows(96, 97)
	s := capacityOnceSweep(h, 2, 1, 1)
	h.runs.ledger.seed(s.ID, 7, intPtr(1), 96, "t96")
	h.st.due = []store.RunSchedule{s}
	h.st.inFlight = 9 // no room at all
	h.sched.Boot(context.Background())
	if h.st.capacityCalls != 0 || h.st.sweepCountCalls != 0 || len(h.fb.f.getIID) != 0 || len(h.runs.autopilot) != 0 {
		t.Fatalf("a cap-spent continuation touched capacity (%d), the scan (%d) or the forge (%v)", h.st.capacityCalls, h.st.sweepCountCalls, h.fb.f.getIID)
	}
	if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
		t.Fatalf("advances = %+v, want the completing advance", h.st.advanceCalls)
	}
	rec := lastWritten(t, h.st.advanceCalls[0].LastFire)
	if len(rec.Started) != 1 || rec.Matched != 1 || rec.Started[0].RunID == uuid.Nil.String() {
		t.Fatalf("summary = %+v", rec)
	}
}

// (j) a failed partial write never blocks the retry; a failed completing write never advances.
func TestOnceFireRetry_J_FailedSummaryWriteStillRetries(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96, 97, 98)
		h.runs.errByIssue = map[int64]error{97: workersvc.ErrTerminalConsumerGateBusy}
		h.runs.summaryErr = errors.New("55P03 lock timeout")
		h.st.due = []store.RunSchedule{onceSweep(h, 2)}
		h.sched.Boot(context.Background())
		if len(h.st.advanceCalls) != 0 || len(h.st.partialCalls) != 0 || len(h.runs.autopilot) != 1 {
			t.Fatalf("a failed partial write must leave the row due: advances=%d partial=%d", len(h.st.advanceCalls), len(h.st.partialCalls))
		}
		h.runs.summaryErr, h.runs.errByIssue = nil, nil
		h.sched.Boot(context.Background())
		if len(h.runs.autopilot) != 2 || len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
			t.Fatalf("retry created %d, advances=%+v", len(h.runs.autopilot), h.st.advanceCalls)
		}
	})
	t.Run("completion", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96)
		h.runs.summaryErr = errors.New("ledger read failed")
		h.st.due = []store.RunSchedule{onceSweep(h, 1)}
		h.sched.Boot(context.Background())
		if len(h.st.advanceCalls) != 0 || len(h.runs.autopilot) != 1 {
			t.Fatalf("a failed completing write must not advance: advances=%d", len(h.st.advanceCalls))
		}
		h.runs.summaryErr = nil
		h.sched.Boot(context.Background())
		if len(h.runs.autopilot) != 1 || len(h.st.advanceCalls) != 1 {
			t.Fatalf("retry must advance without a second start: created=%d advances=%d", len(h.runs.autopilot), len(h.st.advanceCalls))
		}
	})
	t.Run("progress read", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96)
		h.st.progressErr = errors.New("db down")
		h.st.due = []store.RunSchedule{onceSweep(h, 1)}
		h.sched.Boot(context.Background())
		if len(h.runs.autopilot) != 0 || len(h.st.advanceCalls) != 0 {
			t.Fatal("an unreadable ledger must neither start nor advance")
		}
	})
}

// cancelledActiveStore fails the active-run check on a cancelled context the way the real
// database does.
type cancelledActiveStore struct{ *fakeStore }

func (s cancelledActiveStore) HasActiveRunForIssue(ctx context.Context, arg store.HasActiveRunForIssueParams) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.fakeStore.HasActiveRunForIssue(ctx, arg)
}

// cancelAfterRuns cancels the run context right after candidate 96 is admitted.
type cancelAfterRuns struct {
	*fakeRuns
	cancel context.CancelFunc
}

func (r *cancelAfterRuns) CreateScheduledAutopilotRun(ctx context.Context, user, repo uuid.UUID, iid int64, description string, wait *bool, rework *bool, model *string, subagent bool, credential *workersvc.CredentialOverride, harness *workersvc.Harness, progress *workersvc.OnceFireProgress) (store.Run, error) {
	run, err := r.fakeRuns.CreateScheduledAutopilotRun(ctx, user, repo, iid, description, wait, rework, model, subagent, credential, harness, progress)
	if iid == 96 {
		r.cancel()
	}
	return run, err
}

// Cancellation (shutdown) after one admission must not consume the partial once fire: the cancelled
// context would turn the remaining candidates into fetch_failed skips and the completing advance
// would spend the unused slots. The fire is returned as transient instead: the row is not
// advanced and the ledger keeps 96 for the resume. On the pre-fix code the active-run check of 97
// fails on the cancelled context, becomes a skip, and the completing advance is written.
func TestOnceFireRetry_CancelMidSweepDoesNotConsumePartialFire(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.sched.store = cancelledActiveStore{h.st}
	h.sched.runs = &cancelAfterRuns{fakeRuns: h.runs, cancel: cancel}
	h.st.sweepRows = sweepRows(96, 97, 98)
	s := onceSweep(h, 3)
	h.st.due = []store.RunSchedule{s}
	h.sched.Boot(ctx)
	if len(h.st.advanceCalls) != 0 || len(h.st.statusCalls) != 0 {
		t.Fatalf("a cancelled once fire advanced or parked: advances=%+v", h.st.advanceCalls)
	}
	if rows := h.runs.ledger.rows(s.ID, 7); len(rows) != 1 || rows[0].IssueIid != 96 {
		t.Fatalf("ledger = %+v, want only 96", rows)
	}
	out, err := h.sched.fireOne(ctx, s, true)
	if !errors.Is(err, context.Canceled) || len(out.Started) != 0 {
		t.Fatalf("fireOne on a cancelled context = %+v, %v, want the context error", out, err)
	}
	// The resume on a live context spends only the remaining slots and completes.
	h.sched.store, h.sched.runs = h.st, h.runs
	h.sched.Boot(context.Background())
	if len(h.runs.autopilot) != 3 || len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
		t.Fatalf("resume created %d, advances=%+v", len(h.runs.autopilot), h.st.advanceCalls)
	}
}

// cancelOnLastActiveStore cancels the run context from inside the active-run check of one
// candidate and fails that check, so a cancellation lands AFTER the loop-top check of the last
// candidate: only the post-loop check can see it.
type cancelOnLastActiveStore struct {
	*fakeStore
	cancel context.CancelFunc
	last   int64
}

func (s cancelOnLastActiveStore) HasActiveRunForIssue(ctx context.Context, arg store.HasActiveRunForIssueParams) (bool, error) {
	if arg.IssueIid.Int64 == s.last {
		s.cancel()
		return false, ctx.Err()
	}
	return s.fakeStore.HasActiveRunForIssue(ctx, arg)
}

// A cancellation during the LAST candidate ends the loop with no next iteration to notice it:
// the candidate would be a fetch_failed skip and the completing advance would spend the unused
// slot. The post-loop check returns the context error instead; the ledger keeps only 96.
func TestOnceFireRetry_CancelDuringLastCandidateDoesNotConsumePartialFire(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.sched.store = cancelOnLastActiveStore{fakeStore: h.st, cancel: cancel, last: 97}
	h.st.sweepRows = sweepRows(96, 97)
	s := onceSweep(h, 3)
	out, err := h.sched.fireOne(ctx, s, true)
	if !errors.Is(err, context.Canceled) || len(out.Started) != 0 || len(out.Skips) != 0 {
		t.Fatalf("fireOne = %+v, %v, want the context error and no outcome", out, err)
	}
	if len(h.st.advanceCalls) != 0 || len(h.st.statusCalls) != 0 || len(h.st.partialCalls) != 0 {
		t.Fatalf("a cancelled once fire advanced, parked or wrote: advances=%+v parks=%d", h.st.advanceCalls, len(h.st.statusCalls))
	}
	if rows := h.runs.ledger.rows(s.ID, 7); len(rows) != 1 || rows[0].IssueIid != 96 {
		t.Fatalf("ledger = %+v, want only 96", rows)
	}
}

// (l) unit: a lost acknowledgement (committed create, errored call) is in Started exactly once,
// consumes its slot, and leaves no fetch_failed skip.
func TestOnceFireRetry_L_LostAckStartedOnce(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = sweepRows(96, 97, 98)
	h.runs.lostAck = map[int64]error{96: errors.New("connection reset")}
	h.st.due = []store.RunSchedule{onceSweep(h, 2)}
	h.sched.Boot(context.Background())
	if len(h.st.advanceCalls) != 1 {
		t.Fatalf("advances = %d", len(h.st.advanceCalls))
	}
	rec := lastWritten(t, h.st.advanceCalls[0].LastFire)
	if len(rec.Started) != 2 || len(rec.Skips) != 0 || rec.Matched != 2 || *rec.Started[0].IssueIID != 96 || *rec.Started[1].IssueIID != 97 {
		t.Fatalf("summary = %+v, want 96 once (from the ledger) and 97, no fetch_failed skip", rec)
	}
}

// The admission sentinels from create.
func TestOnceFireRetry_AdmissionSentinels(t *testing.T) {
	run := func(t *testing.T, mutate func(h *harness)) *harness {
		h := newHarness()
		h.st.sweepRows = sweepRows(96, 97, 98)
		mutate(h)
		h.st.due = []store.RunSchedule{onceSweep(h, 2)}
		h.sched.Boot(context.Background())
		return h
	}
	t.Run("stale stops without advancing, parking or writing", func(t *testing.T) {
		h := run(t, func(h *harness) { h.runs.ledger.stale = true })
		if len(h.st.advanceCalls)+len(h.st.statusCalls)+len(h.st.partialCalls)+len(h.st.heldFireCalls) != 0 || len(h.runs.autopilot) != 0 {
			t.Fatal("a stale fire wrote or created something")
		}
	})
	t.Run("identity mismatch is transient and creates nothing", func(t *testing.T) {
		h := run(t, func(h *harness) { h.runs.errByIssue = map[int64]error{96: workersvc.ErrOnceFireIdentity} })
		if len(h.st.advanceCalls)+len(h.st.statusCalls)+len(h.st.partialCalls) != 0 || len(h.runs.autopilot) != 0 {
			t.Fatal("an identity mismatch advanced, parked or created")
		}
	})
	t.Run("cap spent breaks the loop with no skip", func(t *testing.T) {
		h := run(t, func(h *harness) { h.runs.errByIssue = map[int64]error{97: workersvc.ErrOnceFireCapSpent} })
		rec := lastWritten(t, h.st.advanceCalls[0].LastFire)
		if len(rec.Started) != 1 || len(rec.Skips) != 0 || len(h.runs.autopilot) != 1 {
			t.Fatalf("summary = %+v created=%d, want 96 only and no skip for 97 or 98", rec, len(h.runs.autopilot))
		}
	})
	t.Run("candidate recorded is a spent slot, not a skip", func(t *testing.T) {
		h := run(t, func(h *harness) { h.runs.errByIssue = map[int64]error{96: workersvc.ErrOnceFireCandidateRecorded} })
		rec := lastWritten(t, h.st.advanceCalls[0].LastFire)
		if len(rec.Skips) != 0 || len(rec.Started) != 2 {
			t.Fatalf("summary = %+v, want no skip for the recorded slot", rec)
		}
	})
}

// The mid-fire credential arm judges THIS attempt's starts: a continuation that started nothing
// new holds (with the earlier starts kept), one that started something records and advances.
func TestOnceFireRetry_CredentialArmJudgesThisAttemptsStarts(t *testing.T) {
	disabled := workersvc.ErrCredentialDisabled
	t.Run("nothing started this attempt holds", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96, 97)
		s := onceSweep(h, 3)
		h.runs.ledger.seed(s.ID, 7, intPtr(3), 96, "t96")
		h.runs.errByIssue = map[int64]error{97: disabled}
		h.st.due = []store.RunSchedule{s}
		h.sched.Boot(context.Background())
		if len(h.st.advanceCalls) != 0 || len(h.st.partialCalls) != 1 {
			t.Fatalf("advances=%d partial=%d, want a hold", len(h.st.advanceCalls), len(h.st.partialCalls))
		}
		rec := lastWritten(t, h.st.partialCalls[0].LastFire)
		if len(rec.Started) != 1 || len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipCredentialDisabled) {
			t.Fatalf("held summary = %+v", rec)
		}
	})
	t.Run("started this attempt records the skip and advances", func(t *testing.T) {
		h := newHarness()
		h.st.sweepRows = sweepRows(96, 97, 98)
		s := onceSweep(h, 3)
		h.runs.errByIssue = map[int64]error{97: disabled}
		h.st.due = []store.RunSchedule{s}
		h.sched.Boot(context.Background())
		if len(h.st.advanceCalls) != 1 || h.st.advanceCalls[0].Status != "fired" {
			t.Fatalf("advances = %+v", h.st.advanceCalls)
		}
		rec := lastWritten(t, h.st.advanceCalls[0].LastFire)
		if len(rec.Started) != 1 || len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipCredentialDisabled) {
			t.Fatalf("summary = %+v", rec)
		}
	})
}

// The guarded park: zero rows is a stale fire and sends no notification; the park carries the epoch.
func TestOnceFireRetry_GuardedParkStaleSendsNoNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows int64
		want int
	}{{"current", 1, 1}, {"stale", 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			s := h.issueSchedule()
			s.FireEpoch = 9
			h.st.repoErr = pgx.ErrNoRows
			h.st.guardedRows = &tc.rows
			h.st.due = []store.RunSchedule{s}
			h.sched.Boot(context.Background())
			if len(h.st.statusCalls) != 1 || h.st.writeEpochs[0] != 9 || len(h.notif.notifications) != tc.want {
				t.Fatalf("park calls=%d epochs=%v notifications=%d, want %d", len(h.st.statusCalls), h.st.writeEpochs, len(h.notif.notifications), tc.want)
			}
		})
	}
}

// A stale recurring advance (zero rows) is only logged.
func TestOnceFireRetry_RecurringAdvanceCarriesEpoch(t *testing.T) {
	h := newHarness()
	s := h.sweepSchedule(pgtype.Int4{})
	s.FireEpoch = 4
	h.st.sweepRows = sweepRows(96)
	zero := int64(0)
	h.st.guardedRows = &zero
	h.st.due = []store.RunSchedule{s}
	h.sched.Boot(context.Background())
	if len(h.st.advanceCalls) != 1 || h.st.writeEpochs[0] != 4 {
		t.Fatalf("advances=%d epochs=%v", len(h.st.advanceCalls), h.st.writeEpochs)
	}
}
