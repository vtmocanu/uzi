package healthsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWaitingM2Evaluation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		age                time.Duration
		genuine            bool
		fail               bool
		wantCap, wantQueue string
	}{
		{"observed 40m", 40 * time.Minute, false, false, sevOK, sevOK},
		{"just below 24h", 24*time.Hour - time.Second, false, false, sevOK, sevOK},
		{"24h boundary", 24 * time.Hour, false, false, sevDanger, sevOK},
		{"mixed", 40 * time.Minute, true, false, sevDanger, sevDanger},
		{"failed", 40 * time.Minute, false, true, sevDanger, sevDanger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, owner := uuid.New(), uuid.New()
			since := pgtype.Timestamptz{Time: fixedNow.Add(-tc.age), Valid: true}
			reason := pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}
			f := &fakeStore{capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: id, UserID: owner, HealthSince: since, HealthReason: reason}}, waitingRows: []store.ListWaitingWorkerRunsRow{{RunID: id, UserID: owner, HealthSince: since, HealthReason: reason}}}
			if tc.genuine {
				f.waitingRows = append(f.waitingRows, store.ListWaitingWorkerRunsRow{RunID: uuid.New(), UserID: owner, HealthSince: since})
				f.capacityRows = append(f.capacityRows, store.ListOwnersWaitingNoCapacityRow{RunID: uuid.New(), UserID: owner, HealthSince: since})
			}
			f.controller = pgtype.Timestamptz{Time: fixedNow, Valid: true}
			s := newSvc(f, &fakeSettings{healthEnabled: true})
			calls := 0
			s.cfg.WorkerEligibilityForHealth = func(context.Context, time.Time, uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
				calls++
				if tc.fail {
					return store.CountOnlineWorkersClaimableForRunRow{}, errors.New("failed")
				}
				return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 1, LatestSuitableDrainingSince: since}, nil
			}
			d, err := s.Evaluate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range d.Checks {
				if c.ID == "fleet.capacity" && c.Severity != tc.wantCap || c.ID == "queue.waiting" && c.Severity != tc.wantQueue {
					t.Fatalf("check=%+v", c)
				}
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			if tc.name == "observed 40m" {
				for _, c := range d.Checks {
					if c.ID == "queue.waiting" && c.Summary != "Runs are waiting while workers finish their current runs before an upgrade." {
						t.Fatalf("confirmed queue summary=%q", c.Summary)
					}
				}
			}
			if tc.name == "observed 40m" && d.Blocking {
				t.Fatal("confirmed roll blocked instance")
			}
		})
	}
}

func TestWaitingM2InvalidAndEvidence(t *testing.T) {
	for _, bad := range []pgtype.Timestamptz{{}, {Valid: true, InfinityModifier: pgtype.Infinity}, {Valid: true, InfinityModifier: pgtype.NegativeInfinity}, {Valid: true}, {Valid: true, Time: fixedNow.Add(time.Second)}} {
		for _, danger := range []bool{false, true} {
			f := &fakeStore{}
			for i := 0; i < 7; i++ {
				age := time.Minute
				if danger && i == 6 {
					age = time.Hour
				}
				f.waitingRows = append(f.waitingRows, store.ListWaitingWorkerRunsRow{RunID: uuid.New(), UserID: uuid.New(), HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-age), Valid: true}, HealthReason: pgtype.Text{Valid: true, String: "spoof; waited unavailable; reason"}})
			}
			id := uuid.New()
			f.waitingRows = append(f.waitingRows, store.ListWaitingWorkerRunsRow{RunID: id, UserID: uuid.New(), HealthSince: bad, HealthReason: pgtype.Text{Valid: true, String: "hostile\\nreason" + string(rune(27)) + "[31m"}})
			s := newSvc(f, &fakeSettings{})
			c := s.checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled)
			want := sevUnknown
			if danger {
				want = sevDanger
			}
			if c.Severity != want {
				t.Fatalf("invalid=%+v danger=%v check=%+v", bad, danger, c)
			}
			if !danger && c.Since != nil {
				t.Fatal("invalid Since")
			}
			text := ""
			named := 0
			for _, e := range c.Evidence {
				text += e.Value
				if e.Label == "Waiting run" {
					named++
				}
			}
			omitted := false
			for _, e := range c.Evidence {
				if e.Label == "Waiting runs omitted" && e.Value == "3" {
					omitted = true
				}
			}
			if !omitted || named != 5 || !strings.Contains(text, id.String()) || !strings.Contains(text, "waited unavailable") || strings.ContainsRune(text, rune(27)) {
				t.Fatalf("evidence=%+v", c.Evidence)
			}
		}
	}
}

func TestWaitingM2SharedCapAndRevalidation(t *testing.T) {
	f := &fakeStore{}
	reason := pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}
	since := pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}
	for i := 0; i < 200; i++ {
		id := uuid.New()
		f.capacityRows = append(f.capacityRows, store.ListOwnersWaitingNoCapacityRow{RunID: id, UserID: uuid.New(), HealthReason: reason, HealthSince: since})
	}
	first := f.capacityRows[0]
	f.waitingRows = []store.ListWaitingWorkerRunsRow{{RunID: first.RunID, UserID: first.UserID, HealthReason: reason, HealthSince: since}, {RunID: uuid.New(), UserID: uuid.New(), HealthReason: reason, HealthSince: since}}
	f.controller = pgtype.Timestamptz{Time: fixedNow, Valid: true}
	s := newSvc(f, &fakeSettings{healthEnabled: true})
	calls := 0
	s.cfg.WorkerEligibilityForHealth = func(context.Context, time.Time, uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
		calls++
		return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 1, LatestSuitableDrainingSince: since}, nil
	}
	d, err := s.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 200 {
		t.Fatalf("calls=%d", calls)
	}
	for _, c := range d.Checks {
		if c.ID == "queue.waiting" && c.Severity != sevDanger {
			t.Fatalf("cap overflow=%+v", c)
		}
	}
	// Same run's stored facts change between the two real check calls.
	for _, change := range []string{"reason", "future", "overlap"} {
		f.capacityRows = f.capacityRows[:1]
		f.waitingRows = f.waitingRows[:1]
		f.waitingRows[0].HealthReason = reason
		f.waitingRows[0].HealthSince = since
		if change == "reason" {
			f.waitingRows[0].HealthReason = pgtype.Text{}
		}
		if change == "future" {
			f.waitingRows[0].HealthSince.Time = fixedNow.Add(time.Hour)
		}
		if change == "overlap" {
			f.waitingRows[0].HealthSince.Time = fixedNow.Add(-time.Minute)
		}
		calls = 0
		coord := &waitConfirmations{}
		s.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled, coord)
		c := s.checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled, coord)
		if change == "overlap" {
			ok, overlap := coord.confirm(s, context.Background(), fixedNow, first.RunID, reason, f.waitingRows[0].HealthSince)
			if !ok || !overlap.Equal(fixedNow.Add(-time.Minute)) || calls != 1 {
				t.Fatalf("memo overlap=%v confirmed=%v calls=%d", overlap, ok, calls)
			}
		}
		coord.close()
		want := sevOK
		if change == "reason" {
			want = sevDanger
		}
		if change == "future" {
			want = sevUnknown
		}
		if c.Severity != want || calls != 1 {
			t.Fatalf("%s check=%+v calls=%d", change, c, calls)
		}
	}
}

func TestWaitingM2LateSuccessAndSharedBudget(t *testing.T) {
	reason := pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}
	since := pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}
	first, second, third := uuid.New(), uuid.New(), uuid.New()
	f := &fakeStore{capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: first, HealthReason: reason, HealthSince: since}, {RunID: second, HealthReason: reason, HealthSince: since}, {RunID: third, HealthReason: reason, HealthSince: since}}, waitingRows: []store.ListWaitingWorkerRunsRow{{RunID: first, HealthReason: reason, HealthSince: since}, {RunID: second, HealthReason: reason, HealthSince: since}, {RunID: third, HealthReason: reason, HealthSince: since}, {RunID: uuid.New(), HealthReason: reason, HealthSince: since}}}
	f.controller = pgtype.Timestamptz{Time: fixedNow, Valid: true}
	s := newSvc(f, &fakeSettings{healthEnabled: true})
	calls := 0
	s.cfg.WorkerEligibilityForHealth = func(ctx context.Context, _ time.Time, id uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
		calls++
		if id != first {
			<-ctx.Done()
		}
		return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 1, LatestSuitableDrainingSince: since}, nil
	}
	d, err := s.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("shared budget calls=%d", calls)
	}
	for _, c := range d.Checks {
		if c.ID == "queue.waiting" {
			if c.Severity != sevDanger {
				t.Fatalf("late success=%+v", c)
			}
			late := map[uuid.UUID]bool{second: false, third: false}
			for _, e := range c.Evidence {
				if strings.Contains(e.Value, first.String()) {
					t.Fatal("timely memo was discarded after expiry")
				}
				for id := range late {
					if strings.Contains(e.Value, id.String()) {
						late[id] = true
					}
				}
			}
			for id, present := range late {
				if !present {
					t.Fatalf("late successful callback wrongly exempted run %s from queue evidence", id)
				}
			}
		}
	}
}

func TestWaitingM2DetectorSkipsQueries(t *testing.T) {
	for _, state := range []healthDetectorState{healthDetectorDisabled, healthDetectorUnknown} {
		f := &fakeStore{}
		s := newSvc(f, &fakeSettings{})
		s.checkFleetCapacity(context.Background(), fixedNow, state)
		s.checkQueueWaiting(context.Background(), fixedNow, state)
		if f.capacityQueries.Load() != 0 || f.waitingQueries.Load() != 0 {
			t.Fatal("disabled detector queried waiting population")
		}
	}
}

func TestWaitingM2QueueConfirmationFallback(t *testing.T) {
	valid := pgtype.Timestamptz{Time: fixedNow.Add(-40 * time.Minute), Valid: true}
	reason := pgtype.Text{Valid: true, String: workersvc.ReasonWorkersUpgrading}
	type fallback struct {
		name                        string
		wait, drain                 pgtype.Timestamptz
		reason                      pgtype.Text
		nonDraining, veto, draining int64
		absent, fail                bool
		want                        string
		calls                       int
	}
	cases := []fallback{
		{name: "non-draining", wait: valid, drain: valid, reason: reason, draining: 1, nonDraining: 1, want: sevDanger, calls: 1},
		{name: "own-draining veto", wait: valid, drain: valid, reason: reason, draining: 1, veto: 1, want: sevDanger, calls: 1},
		{name: "zero eligible", wait: valid, drain: valid, reason: reason, want: sevDanger, calls: 1},
		{name: "stale reason", wait: valid, drain: valid, reason: pgtype.Text{Valid: true, String: "old reason"}, draining: 1, want: sevDanger},
		{name: "absent callback", wait: valid, drain: valid, reason: reason, draining: 1, absent: true, want: sevDanger},
		{name: "failed callback", wait: valid, drain: valid, reason: reason, draining: 1, fail: true, want: sevDanger, calls: 1},
	}
	for _, bad := range []struct {
		name string
		time pgtype.Timestamptz
	}{
		{"null", pgtype.Timestamptz{}},
		{"infinity", pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}},
		{"negative infinity", pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}},
		{"zero", pgtype.Timestamptz{Valid: true}},
		{"future", pgtype.Timestamptz{Valid: true, Time: fixedNow.Add(time.Second)}},
	} {
		cases = append(cases,
			fallback{name: "stored " + bad.name, wait: bad.time, drain: valid, reason: reason, draining: 1, want: sevUnknown},
			fallback{name: "drain " + bad.name, wait: valid, drain: bad.time, reason: reason, draining: 1, want: sevDanger, calls: 1},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			f := &fakeStore{waitingRows: []store.ListWaitingWorkerRunsRow{{RunID: id, UserID: uuid.New(), HealthSince: tc.wait, HealthReason: tc.reason}}}
			s := newSvc(f, &fakeSettings{})
			calls := 0
			if !tc.absent {
				s.cfg.WorkerEligibilityForHealth = func(_ context.Context, now time.Time, run uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
					calls++
					if run != id || !now.Equal(fixedNow) {
						t.Fatal("incorrect confirmation arguments")
					}
					if tc.fail {
						return store.CountOnlineWorkersClaimableForRunRow{}, errors.New("confirmation failed")
					}
					return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: tc.draining, NonDrainingEligible: tc.nonDraining, SuitableOwnDraining: tc.veto, LatestSuitableDrainingSince: tc.drain}, nil
				}
			}
			c := s.checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled)
			if c.Severity != tc.want || calls != tc.calls {
				t.Fatalf("check=%+v calls=%d want=%d", c, calls, tc.calls)
			}
			if tc.want == sevUnknown {
				if c.Since != nil || c.Summary != "Waiting run age is unavailable." || !strings.Contains(c.Evidence[0].Value, "waited unavailable;") {
					t.Fatalf("unknown wait=%+v", c)
				}
			} else if c.Since == nil || *c.Since != valid.Time.Format(time.RFC3339) || c.Summary != "A run has been waiting for a worker for 40m." {
				t.Fatalf("genuine fallback=%+v", c)
			}
		})
	}
}

func TestWaitingM2CapacityUnknownEvidenceBeyondCap(t *testing.T) {
	f := &fakeStore{}
	for i := 0; i < 7; i++ {
		f.capacityRows = append(f.capacityRows, store.ListOwnersWaitingNoCapacityRow{
			RunID: uuid.New(), UserID: uuid.New(),
			HealthSince:  pgtype.Timestamptz{Valid: true, Time: fixedNow.Add(-time.Duration(7-i) * time.Second)},
			HealthReason: pgtype.Text{Valid: true, String: "spoof; waited unavailable; reason"},
		})
	}
	id := uuid.New()
	f.capacityRows = append(f.capacityRows, store.ListOwnersWaitingNoCapacityRow{RunID: id, UserID: uuid.New()})
	s := newSvc(f, &fakeSettings{})
	c := s.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if c.Severity != sevUnknown || len(c.Evidence) != 6 {
		t.Fatalf("check=%+v", c)
	}
	for i := 0; i < 4; i++ {
		if !strings.Contains(c.Evidence[i].Value, f.capacityRows[i].RunID.String()) {
			t.Fatalf("oldest row lost: %+v", c.Evidence)
		}
	}
	if !strings.Contains(c.Evidence[4].Value, id.String()) || !strings.Contains(c.Evidence[4].Value, "; waited unavailable;") || c.Evidence[5].Label != "Waiting runs omitted" || c.Evidence[5].Value != "3" {
		t.Fatalf("unknown/omitted evidence=%+v", c.Evidence)
	}
}
