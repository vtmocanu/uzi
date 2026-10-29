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

func TestQueueWaitingReadableOwnerAndPriority(t *testing.T) {
	owner, run := uuid.New(), uuid.New()
	since := fixedNow.Add(-48 * time.Minute)
	f := &fakeStore{
		waitingRows: []store.ListWaitingWorkerRunsRow{{
			RunID: run, UserID: owner,
			HealthSince:  pgtype.Timestamptz{Valid: true, Time: since},
			HealthReason: pgtype.Text{Valid: true, String: workersvc.ReasonPriorityRestored},
		}},
		userLookup: func(_ context.Context, id uuid.UUID) (store.User, error) {
			if id != owner {
				t.Fatalf("looked up wrong owner: %s", id)
			}
			return store.User{ID: owner, Email: "private@example.com", DisplayName: pgtype.Text{Valid: true, String: "Alex Example"}}, nil
		},
	}
	c := newSvc(f, &fakeSettings{}).checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled)
	want := "Owner: Alex Example (" + owner.String()[:8] + "); waited 48m. This background run is no longer deprioritized, but has not yet been picked up."
	if c.Evidence[0].RunID != run.String() || c.Evidence[0].OwnerID != owner.String() {
		t.Fatalf("lost run link: %+v", c.Evidence[0])
	}
	if c.Evidence[0].Value != want {
		t.Fatalf("evidence = %q, want %q", c.Evidence[0].Value, want)
	}
	if c.Severity != sevDanger || c.Since == nil || *c.Since != since.Format(time.RFC3339) {
		t.Fatalf("presentation changed verdict or clock: %+v", c)
	}
	if strings.Contains(c.Evidence[0].Value, "private@example.com") || strings.Contains(c.Evidence[0].Value, owner.String()) {
		t.Fatalf("diagnostic identity leaked into main copy: %+v", c.Evidence)
	}
}

func TestQueueWaitingDeprioritizedAndCapacityUnchanged(t *testing.T) {
	owner, run := uuid.New(), uuid.New()
	since := pgtype.Timestamptz{Valid: true, Time: fixedNow.Add(-48 * time.Minute)}
	f := &fakeStore{
		waitingRows: []store.ListWaitingWorkerRunsRow{{RunID: run, UserID: owner, HealthSince: since, HealthReason: pgtype.Text{Valid: true, String: workersvc.ReasonDeprioritized}}},
		userLookup: func(context.Context, uuid.UUID) (store.User, error) {
			return store.User{ID: owner, DisplayName: pgtype.Text{Valid: true, String: "Alex Example"}}, nil
		},
	}
	c := newSvc(f, &fakeSettings{}).checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled)
	if !strings.HasSuffix(c.Evidence[0].Value, "This background run is deliberately waiting behind interactive work.") {
		t.Fatalf("unexplained deliberate wait: %+v", c.Evidence[0])
	}
	e := waitEvidence{}
	e.append(run, owner, pgtype.Text{Valid: true, String: workersvc.ReasonPriorityRestored}, since, fixedNow)
	want := "run " + run.String() + "; owner " + owner.String() + "; waited 48m; stored reason " + workersvc.ReasonPriorityRestored
	if e.rows[0].Value != want || e.rows[0].RunID != "" || e.rows[0].OwnerID != "" {
		t.Fatalf("capacity diagnostic format changed: %+v", e.rows[0])
	}
}

func TestQueueWaitingOwnerFallbackAndSanitizing(t *testing.T) {
	for _, tc := range []struct {
		name, display, want string
		fail                bool
	}{
		{name: "missing", want: "Owner name unavailable"},
		{name: "failed", display: "Wrong name", want: "Owner name unavailable", fail: true},
		{name: "hostile", display: "Alex\x1b[31m" + string(rune(0x202e)) + "\nExample", want: "Owner: Alex[31m Example"},
		{name: "long unicode", display: strings.Repeat("😀", 200), want: "Owner: "},
		{name: "blank", display: "  \n\t", want: "Owner name unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := uuid.New()
			f := &fakeStore{
				waitingRows: []store.ListWaitingWorkerRunsRow{{RunID: uuid.New(), UserID: owner}},
				userLookup: func(context.Context, uuid.UUID) (store.User, error) {
					if tc.fail {
						return store.User{}, errors.New("unavailable")
					}
					return store.User{ID: owner, Email: "private@example.com", DisplayName: pgtype.Text{Valid: true, String: tc.display}}, nil
				},
			}
			c := newSvc(f, &fakeSettings{}).checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled)
			value := c.Evidence[0].Value
			if !strings.Contains(value, tc.want) || strings.Contains(value, "private@example.com") || strings.ContainsAny(value, "\x1b"+string(rune(0x202e))+"\n\t") {
				t.Fatalf("unsafe or missing identity: %q", value)
			}
			if !strings.Contains(value, "waited unavailable") || c.Severity != sevUnknown {
				t.Fatalf("lost unknown age: %+v", c)
			}
			if tc.name == "long unicode" && !strings.HasPrefix(value, "Owner: "+strings.Repeat("😀", 24)+" ("+owner.String()[:8]+"); waited unavailable.") {
				t.Fatalf("unicode name was not bounded safely: %q", value)
			}
			if len(value) > 512 {
				t.Fatalf("unbounded evidence: %q", value)
			}
		})
	}
}

func TestQueueWaitingOwnerLookupBoundedToRetainedRows(t *testing.T) {
	owner := uuid.New()
	calls := 0
	f := &fakeStore{userLookup: func(context.Context, uuid.UUID) (store.User, error) {
		calls++
		return store.User{ID: owner, DisplayName: pgtype.Text{Valid: true, String: "Alex Example"}}, nil
	}}
	for i := 0; i < 20; i++ {
		f.waitingRows = append(f.waitingRows, store.ListWaitingWorkerRunsRow{
			RunID: uuid.New(), UserID: owner,
			HealthSince: pgtype.Timestamptz{Valid: true, Time: fixedNow.Add(-time.Hour)},
		})
	}
	last := uuid.New()
	f.waitingRows = append(f.waitingRows, store.ListWaitingWorkerRunsRow{RunID: last, UserID: owner})
	c := newSvc(f, &fakeSettings{}).checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled)
	if calls != 1 {
		t.Fatalf("owner lookups = %d, want one for repeated retained owner", calls)
	}
	if len(c.Evidence) != 6 || c.Evidence[5].Label != "Waiting runs omitted" || c.Evidence[5].Value != "16" {
		t.Fatalf("retention changed: %+v", c.Evidence)
	}
	if !strings.Contains(c.Evidence[4].Value, "waited unavailable") {
		t.Fatalf("unknown age lost: %+v", c.Evidence)
	}
}
