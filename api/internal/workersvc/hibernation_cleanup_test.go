package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4206: the attempt state machine's pure rules, without a database (the LiveDB suite in
// hibernation_cleanup_livedb_test.go runs the same rules against Postgres).

func cleanupWorker(phase string) store.Worker {
	return store.Worker{
		HibernationState: "preparing", CleanupPhase: phase,
		CleanupOpID: pgconv.UUID(uuid.New()), CleanupRegisterNonce: pgconv.TextOrNull("n1"),
		CleanupPodUid: pgconv.UUID(uuid.New()), SnapshotRegisterNonce: pgconv.TextOrNull("n1"),
	}
}

func TestIssueCleanupAttemptByWorkerKind(t *testing.T) {
	now := time.Now()
	docker := pgtype.Bool{Bool: true, Valid: true}
	caps := []string{capability.HibernationCachePruneV1}
	nonce := pgconv.TextOrNull("n1")
	for _, tc := range []struct {
		name    string
		w       store.Worker
		phase   string
		outcome string
		reason  string
	}{
		{"plain", store.Worker{SnapshotRegisterNonce: nonce, ProtocolCapabilities: caps}, CleanupPhaseNone, "", ""},
		{"docker supported", store.Worker{DockerEnabled: docker, SnapshotRegisterNonce: nonce, ProtocolCapabilities: caps}, CleanupPhaseRequested, "", ""},
		{"docker without the capability", store.Worker{DockerEnabled: docker, SnapshotRegisterNonce: nonce}, CleanupPhaseNone, HibernationCleanupSkipped, CleanupReasonUnsupported},
		{"docker without a nonce", store.Worker{DockerEnabled: docker, ProtocolCapabilities: caps}, CleanupPhaseNone, HibernationCleanupSkipped, CleanupReasonUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.w
			// A previous hibernation's leftovers are reset.
			w.CleanupPhase, w.HibernationCleanup, w.CleanupReconcileSeq = CleanupPhaseSettled, pgconv.TextOrNull(HibernationCleanupPruned), 9
			issueCleanupAttempt(&w, now)
			if w.CleanupPhase != tc.phase || w.HibernationCleanup.String != tc.outcome || w.HibernationCleanupReason.String != tc.reason || w.CleanupReconcileSeq != 0 {
				t.Fatalf("attempt = %q %q/%q seq=%d", w.CleanupPhase, w.HibernationCleanup.String, w.HibernationCleanupReason.String, w.CleanupReconcileSeq)
			}
			if (tc.phase == CleanupPhaseRequested) != (w.CleanupOpID.Valid && w.CleanupRegisterNonce.String == "n1" && w.CleanupRequestedAt.Valid) {
				t.Fatalf("binding = %+v", w)
			}
		})
	}
}

func TestRequestCleanupCancelNeverAbandonsAStartedAttempt(t *testing.T) {
	now := time.Now()
	w := cleanupWorker(CleanupPhaseRequested)
	if !requestCleanupCancel(&w, now) || w.CleanupPhase != CleanupPhaseSettled || w.HibernationCleanup.String != HibernationCleanupSkipped || w.HibernationCleanupReason.String != CleanupReasonCancelled {
		t.Fatalf("requested attempt: %+v", w)
	}
	for _, phase := range []string{CleanupPhaseStarted, CleanupPhaseUnknown} {
		w := cleanupWorker(phase)
		if !requestCleanupCancel(&w, now) || !w.CleanupCancelRequested || w.CleanupPhase != phase {
			t.Fatalf("%s attempt: %+v", phase, w)
		}
		if requestCleanupCancel(&w, now) {
			t.Fatalf("%s: a second cancel reported a change", phase)
		}
	}
	for _, phase := range []string{CleanupPhaseNone, CleanupPhaseSettled} {
		w := cleanupWorker(phase)
		if requestCleanupCancel(&w, now) || w.CleanupCancelRequested {
			t.Fatalf("%s: cancel touched a row with nothing to cancel", phase)
		}
	}
}

func TestMoveToWakingRequestsCancelWithoutSettlingAStartedAttempt(t *testing.T) {
	w := cleanupWorker(CleanupPhaseStarted)
	w.HibernationState = "preparing"
	if !moveToWaking(&w, time.Now(), "operator") || w.HibernationState != "waking" || w.CleanupPhase != CleanupPhaseStarted || !w.CleanupCancelRequested {
		t.Fatalf("moveToWaking = %q %q cancel=%v", w.HibernationState, w.CleanupPhase, w.CleanupCancelRequested)
	}
	// A wake of a row already waking with nothing left to cancel changes nothing.
	if moveToWaking(&w, time.Now(), "again") {
		t.Fatal("a repeated wake reported a change")
	}
}

func TestTrySettleWaitsForTheCleanupAttempt(t *testing.T) {
	pod := pgconv.UUID(uuid.New())
	op := pgconv.UUID(uuid.New())
	svc := &Service{p: Params{WorkerHeartbeatStale: time.Minute}}
	for _, phase := range []string{CleanupPhaseRequested, CleanupPhaseStarted, CleanupPhaseUnknown, CleanupPhaseSettled, CleanupPhaseNone} {
		w := store.Worker{
			HibernationState: "waking", HibernationOpID: op, WakeReadyOpID: op, WakeReadyPodUid: pod,
			HibernationAdmissionPodUid: pod, LastHeartbeatAt: pgconv.Time(time.Now()), CleanupPhase: phase,
		}
		want := !cleanupUnsettled(phase)
		if got := svc.trySettle(&w, time.Now()); got != want {
			t.Fatalf("phase %s: trySettle = %v, want %v", phase, got, want)
		}
	}
}

func TestNoteCleanupRegistered(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		phase, nonce, wantPhase, wantReason string
		changed                             bool
	}{
		{CleanupPhaseRequested, "n2", CleanupPhaseSettled, CleanupReasonWorkerRestarted, true},
		{CleanupPhaseStarted, "n2", CleanupPhaseUnknown, "", true},
		{CleanupPhaseUnknown, "n2", CleanupPhaseUnknown, "", false},
		{CleanupPhaseSettled, "n2", CleanupPhaseSettled, "", false},
		{CleanupPhaseStarted, "n1", CleanupPhaseStarted, "", false}, // same registration: nothing changed
	} {
		w := cleanupWorker(tc.phase)
		w.SnapshotRegisterNonce = pgconv.TextOrNull(tc.nonce)
		if got := noteCleanupRegistered(&w, now); got != tc.changed || w.CleanupPhase != tc.wantPhase || w.HibernationCleanupReason.String != tc.wantReason {
			t.Fatalf("%s/%s: changed=%v phase=%q reason=%q", tc.phase, tc.nonce, got, w.CleanupPhase, w.HibernationCleanupReason.String)
		}
		if tc.wantPhase == CleanupPhaseUnknown && tc.changed && !w.CleanupUnknownAt.Valid {
			t.Fatal("unknown without unknown_at")
		}
	}
}

func TestReconcileCleanupPacing(t *testing.T) {
	now := time.Now()
	w := cleanupWorker(CleanupPhaseStarted)
	w.CleanupStartedAt = pgconv.Time(now.Add(-HibernationCleanupResultTimeout + time.Second))
	if reconcileCleanup(&w, now) {
		t.Fatal("a started attempt inside the timeout changed")
	}
	w.CleanupStartedAt = pgconv.Time(now.Add(-HibernationCleanupResultTimeout))
	if !reconcileCleanup(&w, now) || w.CleanupPhase != CleanupPhaseUnknown || w.CleanupReconcileSeq != 1 {
		t.Fatalf("timeout: phase=%q seq=%d", w.CleanupPhase, w.CleanupReconcileSeq)
	}
	if reconcileCleanup(&w, now.Add(HibernationCleanupReconcileEvery-time.Second)) || w.CleanupReconcileSeq != 1 {
		t.Fatal("reconciled before the pacing interval")
	}
	if !reconcileCleanup(&w, now.Add(HibernationCleanupReconcileEvery)) || w.CleanupReconcileSeq != 2 {
		t.Fatalf("seq = %d, want 2 at the pacing interval", w.CleanupReconcileSeq)
	}
}

func TestApplyCleanupStoppedBindsOpAndPod(t *testing.T) {
	now := time.Now()
	w := cleanupWorker(CleanupPhaseUnknown)
	op, pod := uuid.UUID(w.CleanupOpID.Bytes), w.CleanupPodUid
	for name, tc := range map[string]struct {
		op  uuid.UUID
		pod pgtype.UUID
	}{
		"other op":  {uuid.New(), pod},
		"other pod": {op, pgconv.UUID(uuid.New())},
		"no pod":    {op, pgtype.UUID{}},
	} {
		c := w
		if _, err := applyCleanupStopped(&c, now, tc.op, tc.pod); !errors.Is(err, ErrHibernationConflict) || c.CleanupPhase != CleanupPhaseUnknown {
			t.Fatalf("%s: err=%v phase=%q", name, err, c.CleanupPhase)
		}
	}
	started := cleanupWorker(CleanupPhaseStarted)
	if _, err := applyCleanupStopped(&started, now, uuid.UUID(started.CleanupOpID.Bytes), started.CleanupPodUid); !errors.Is(err, ErrHibernationConflict) {
		t.Fatalf("a started attempt was stopped: %v", err)
	}
	if changed, err := applyCleanupStopped(&w, now, op, pod); err != nil || !changed || w.CleanupPhase != CleanupPhaseSettled ||
		w.HibernationCleanup.String != HibernationCleanupFailed || w.HibernationCleanupReason.String != CleanupReasonInterrupted {
		t.Fatalf("matching report: %v %v %+v", changed, err, w)
	}
	if changed, err := applyCleanupStopped(&w, now, op, pod); err != nil || changed {
		t.Fatalf("replay: %v %v", changed, err)
	}
}

func TestPreparingDeadlineStartIsTheLaterOfRequestAndSettlement(t *testing.T) {
	at := time.Now()
	w := store.Worker{HibernationRequestedAt: pgconv.Time(at)}
	if !preparingDeadlineStart(w).Equal(at) {
		t.Fatal("no settlement: want the request time")
	}
	w.CleanupSettledAt = pgconv.Time(at.Add(-time.Hour))
	if !preparingDeadlineStart(w).Equal(at) {
		t.Fatal("an earlier settlement moved the start")
	}
	w.CleanupSettledAt = pgconv.Time(at.Add(time.Hour))
	if !preparingDeadlineStart(w).Equal(at.Add(time.Hour)) {
		t.Fatal("a later settlement did not move the start")
	}
}

func TestHibernationCachePruneProjection(t *testing.T) {
	if HibernationCachePruneFromWorker(store.Worker{}) != nil || HibernationCachePruneFromWorker(cleanupWorker(CleanupPhaseNone)) != nil {
		t.Fatal("a worker with no attempt must project null")
	}
	w := cleanupWorker(CleanupPhaseUnknown)
	w.CleanupCancelRequested, w.CleanupReconcileSeq = true, 3
	p := HibernationCachePruneFromWorker(w)
	if p == nil || p.OpID != uuid.UUID(w.CleanupOpID.Bytes).String() || p.RegisterNonce != "n1" || p.Phase != CleanupPhaseUnknown || !p.CancelRequested || p.ReconcileSeq != 3 {
		t.Fatalf("projection = %+v", p)
	}
}
