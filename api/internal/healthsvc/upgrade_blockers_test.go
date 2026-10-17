package healthsvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCapacityBlockersRetainedOwnersMultiworker(t *testing.T) {
	f := &fakeStore{}
	var retained []uuid.UUID
	for i := 0; i < 8; i++ {
		owner, worker := uuid.New(), uuid.New()
		wait := store.ListOwnersWaitingNoCapacityRow{RunID: uuid.New(), UserID: owner,
			HealthReason: pgtype.Text{String: strings.Repeat("界", 100) + "\u202e\n", Valid: true},
			HealthSince:  pgtype.Timestamptz{Time: fixedNow.Add(-time.Minute), Valid: true}}
		if i == 7 {
			wait.HealthSince = pgtype.Timestamptz{}
		}
		f.capacityRows = append(f.capacityRows, wait)
		f.workers = append(f.workers, store.ListAllWorkersRow{Worker: store.Worker{
			ID: worker, UserID: owner, Kind: "hosted", DrainingSince: pgtype.Timestamptz{Valid: true, Time: fixedNow},
		}})
		if i < 4 || i == 7 {
			retained = append(retained, owner)
		}
	}
	owner := retained[0]
	a := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	z := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	f.workers[0].Worker.ID = z
	extra := f.workers[0]
	extra.Worker.ID = a
	f.workers = append(f.workers, extra)
	blocker := uuid.New()
	f.blockerRows = []store.ListWorkerUpgradeBlockersRow{
		{WorkerID: z, WorkerOwner: owner, SafeCount: 8, RunID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Kind: pgtype.Text{String: "chat", Valid: true}, Status: pgtype.Text{String: "running", Valid: true}},
		{WorkerID: a, WorkerOwner: owner, SafeCount: 4, RunID: pgtype.UUID{Bytes: blocker, Valid: true}, Kind: pgtype.Text{String: "chat", Valid: true}, Status: pgtype.Text{String: "pool_wait", Valid: true}},
	}
	// Foreign projection rows must never enter another owner's evidence.
	foreign := uuid.New()
	f.blockerRows = append(f.blockerRows, store.ListWorkerUpgradeBlockersRow{
		WorkerID: uuid.New(), WorkerOwner: uuid.New(), SafeCount: 1, RunID: pgtype.UUID{Bytes: foreign, Valid: true},
	})
	s := newSvc(f, nil)
	c := s.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if c.Severity != sevUnknown || len(c.Evidence) != 6 || c.Evidence[5].Value != "3" {
		t.Fatalf("%+v", c)
	}
	if len(f.blockerQueries) != 1 || len(f.blockerQueries[0]) != 6 {
		t.Fatalf("queries %+v", f.blockerQueries)
	}
	expected := map[uuid.UUID]bool{}
	for _, w := range f.workers {
		for _, o := range retained {
			if w.Worker.UserID == o {
				expected[w.Worker.ID] = true
			}
		}
	}
	for _, id := range f.blockerQueries[0] {
		if !expected[id] {
			t.Fatalf("queried omitted owner worker %s", id)
		}
	}
	for i, ev := range c.Evidence[:5] {
		row := i
		if i == 4 {
			row = 7
		}
		for _, want := range []string{f.capacityRows[row].RunID.String(), retained[i].String()} {
			if !strings.Contains(ev.Value, want) {
				t.Fatal(ev.Value)
			}
		}
		if len([]rune(ev.Value)) > 512 || strings.Contains(ev.Value, foreign.String()) {
			t.Fatal(ev.Value)
		}
		for _, r := range ev.Value {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Fatalf("unsafe %q", ev.Value)
			}
		}
		if i > 0 && strings.Contains(ev.Value, blocker.String()) {
			t.Fatal("owner leak", ev.Value)
		}
	}
	got := c.Evidence[0].Value
	for _, want := range []string{blocker.String(), "a pooled token becomes spendable", "11 other safe blocker(s) omitted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing %q", got, want)
		}
	}
	if !strings.Contains(c.Evidence[4].Value, "waited unavailable") {
		t.Fatal(c.Evidence[4])
	}
}

func TestCapacityBlockerProjectionFailureDoesNotChangeSeverity(t *testing.T) {
	for _, mode := range []string{"error", "missing", "mismatch", "empty", "safe", "wrong-owner"} {
		t.Run(mode, func(t *testing.T) {
			o, w := uuid.New(), uuid.New()
			f := &fakeStore{
				capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: uuid.New(), UserID: o, HealthSince: pgtype.Timestamptz{Valid: true, Time: fixedNow.Add(-time.Hour)}}},
				workers:      []store.ListAllWorkersRow{{Worker: store.Worker{ID: w, UserID: o, Kind: "hosted", DrainingSince: pgtype.Timestamptz{Valid: true}}}},
			}
			b := store.ListWorkerUpgradeBlockersRow{WorkerID: w, WorkerOwner: o}
			switch mode {
			case "error":
				f.blockerErr = errors.New("projection broke")
			case "missing":
			case "mismatch":
				b.OwnerMismatch = true
				f.blockerRows = []store.ListWorkerUpgradeBlockersRow{b}
			case "empty":
				f.blockerRows = []store.ListWorkerUpgradeBlockersRow{b}
			case "safe":
				b.SafeCount = 1
				b.RunID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
				b.Status = pgtype.Text{String: "running", Valid: true}
				f.blockerRows = []store.ListWorkerUpgradeBlockersRow{b}
			case "wrong-owner":
				b.WorkerOwner = uuid.New()
				f.blockerRows = []store.ListWorkerUpgradeBlockersRow{b}
			}
			s := newSvc(f, nil)
			got := s.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
			baseline := s.checkFleetCapacityWithWorkers(context.Background(), fixedNow, healthDetectorEnabled, nil, nil)
			got.Evidence = nil
			baseline.Evidence = nil
			if !reflect.DeepEqual(got, baseline) || got.Severity != sevDanger {
				t.Fatalf("severity changed: %+v baseline %+v", got, baseline)
			}
			value := s.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled).Evidence[0].Value
			if mode != "empty" && mode != "safe" && !strings.Contains(value, "unavailable") {
				t.Fatal(value)
			}
			if mode == "empty" && !strings.Contains(value, "custody clearance unestablished") {
				t.Fatal(value)
			}
		})
	}
}

func TestCapacityBlockersComposedBudgetPreservesActions(t *testing.T) {
	for _, status := range []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "paused", "pool_wait", "limit_wait", "recovery_wait"} {
		t.Run(status, func(t *testing.T) {
			o, w, run := uuid.New(), uuid.New(), uuid.New()
			long := pgtype.Text{String: strings.Repeat("界", 200) + "\n\u202e", Valid: true}
			stamp := pgtype.Timestamptz{Time: fixedNow.Add(time.Hour), Valid: true}
			f := &fakeStore{
				capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: uuid.New(), UserID: o, HealthReason: long, HealthSince: pgtype.Timestamptz{}}},
				workers:      []store.ListAllWorkersRow{{Worker: store.Worker{ID: w, UserID: o, Kind: "hosted", DrainingSince: stamp}}},
				blockerRows: []store.ListWorkerUpgradeBlockersRow{{WorkerID: w, WorkerOwner: o, OwnerMismatch: true, SafeCount: 999999,
					RunID: pgtype.UUID{Bytes: run, Valid: true}, Kind: pgtype.Text{String: "cross_check", Valid: true}, Status: pgtype.Text{String: status, Valid: true},
					HealthReason: long, HoldReason: long, RecoveryWaitCause: long, LimitResetsAt: stamp, RetryNotBefore: stamp, RecoveryRetryNotBefore: stamp}},
			}
			c := newSvc(f, nil).checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
			v := c.Evidence[0].Value
			if len([]rune(v)) > 512 {
				t.Fatalf("%d runes: %s", len([]rune(v)), v)
			}
			for _, want := range []string{run.String(), "waited unavailable", "999998 other safe blocker(s) omitted", "other blockers unavailable", "release requires terminal or unassignment"} {
				if !strings.Contains(v, want) {
					t.Fatalf("%s missing %s", v, want)
				}
			}
			if status == "pool_wait" && !strings.Contains(v, "runs `uzi run resume-now`") {
				t.Fatal(v)
			}
			if status == "limit_wait" && !strings.Contains(v, "no roll time promised") {
				t.Fatal(v)
			}
			if status == "queued" && !strings.Contains(v, "exact fence unavailable, inspect run") {
				t.Fatal(v)
			}
			if status == "recovery_wait" && !strings.Contains(v, "automatic retry not before") {
				t.Fatal(v)
			}
		})
	}
}

func TestCapacityBlockerRecoveryHoldCauseSurvivesBounds(t *testing.T) {
	for _, cause := range []string{"codex_account_unavailable", "worker_requeue_exhausted"} {
		owner, worker, run := uuid.New(), uuid.New(), uuid.New()
		stamp := pgtype.Timestamptz{Valid: true, Time: fixedNow.Add(time.Hour)}
		f := &fakeStore{
			capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: uuid.New(), UserID: owner, HealthSince: stamp}},
			workers:      []store.ListAllWorkersRow{{Worker: store.Worker{ID: worker, UserID: owner, Kind: "hosted", DrainingSince: stamp}}},
			blockerRows:  []store.ListWorkerUpgradeBlockersRow{{WorkerID: worker, WorkerOwner: owner, SafeCount: 5, RunID: pgtype.UUID{Valid: true, Bytes: run}, Kind: pgtype.Text{Valid: true, String: "chat"}, Status: pgtype.Text{Valid: true, String: "recovery_wait"}, RecoveryWaitCause: pgtype.Text{Valid: true, String: cause}, RecoveryRetryNotBefore: stamp}},
		}
		value := newSvc(f, nil).checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled).Evidence[0].Value
		if !strings.Contains(value, run.String()) || !strings.Contains(value, cause) || !strings.Contains(value, "no timer") || strings.Contains(value, "automatic retry") || !strings.Contains(value, "4 other safe blocker(s) omitted") || len([]rune(value)) > 512 {
			t.Fatalf("lost recovery hold under bounds: %s", value)
		}
	}
}

func TestCapacityBlockersEvaluateReusesWorkerSnapshot(t *testing.T) {
	o, w := uuid.New(), uuid.New()
	f := &fakeStore{
		capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: uuid.New(), UserID: o}},
		workers:      []store.ListAllWorkersRow{{Worker: store.Worker{ID: w, UserID: o, Kind: "hosted", DrainingSince: pgtype.Timestamptz{Valid: true}}}},
		blockerRows:  []store.ListWorkerUpgradeBlockersRow{{WorkerID: w, WorkerOwner: o}},
	}
	if _, err := newSvc(f, &fakeSettings{healthEnabled: true}).Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.workerQueries.Load() != 1 || len(f.blockerQueries) != 1 {
		t.Fatalf("workers=%d blockers=%+v", f.workerQueries.Load(), f.blockerQueries)
	}
}

func TestCapacityBlockersWorkerSnapshotFailure(t *testing.T) {
	o := uuid.New()
	f := &fakeStore{capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: uuid.New(), UserID: o, HealthSince: pgtype.Timestamptz{Valid: true, Time: fixedNow.Add(-time.Minute)}}}, workersErr: errors.New("workers unavailable")}
	c := newSvc(f, nil).checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if c.Severity != sevOK || !strings.Contains(c.Evidence[0].Value, "assigned run blockers unavailable") || len(f.blockerQueries) != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestCapacityBlockersSkipEmptyIDs(t *testing.T) {
	f := &fakeStore{capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: uuid.New(), UserID: uuid.New()}}}
	newSvc(f, nil).checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if len(f.blockerQueries) != 0 {
		t.Fatal(f.blockerQueries)
	}
}
