package healthsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRecoveryStorageCurrentSignal(t *testing.T) {
	for _, tc := range []struct {
		name           string
		refused, bytes int64
		want           string
	}{
		{"empty", 0, 0, sevOK}, {"pressure alone", 0, 9999999999, sevOK}, {"persisted refusal", 2, 0, sevWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Store: &fakeStore{storageRows: []store.RecoveryStorageHealthRow{{RefusedCount: tc.refused, AvailableBytes: tc.bytes}}}})
			c := s.checkRecoveryStorage(context.Background(), time.Now())
			if c.Severity != tc.want || c.Scope != "owner" || c.Group != groupHousekeeping || !strings.Contains(c.Summary, "captures currently marked quota-refused") {
				t.Errorf("check: %+v", c)
			}
			disabled := 0
			for _, e := range c.Evidence {
				if e.Value == "disabled" {
					disabled++
				}
			}
			if disabled != 3 {
				t.Errorf("disabled limits: %+v", c.Evidence)
			}
		})
	}
}

func TestRecoveryStorageReadFailureAndDeadline(t *testing.T) {
	for _, tc := range []string{"error", "missing aggregate", "expired deadline", "bounded deadline"} {
		t.Run(tc, func(t *testing.T) {
			f := &fakeStore{}
			ctx := context.Background()
			switch tc {
			case "error":
				f.storageErr = errors.New("read failed")
			case "missing aggregate":
				f.storageRead = func(context.Context, store.RecoveryStorageHealthParams) ([]store.RecoveryStorageHealthRow, error) {
					return nil, nil
				}
			case "expired deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "bounded deadline":
				f.storageRead = func(ctx context.Context, p store.RecoveryStorageHealthParams) ([]store.RecoveryStorageHealthRow, error) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > recoveryStorageReadBudget || p.ExampleLimit != 8 {
						t.Errorf("unbounded read: %v %+v", deadline, p)
					}
					return nil, context.DeadlineExceeded
				}
			}
			c := New(Config{Store: f}).checkRecoveryStorage(ctx, time.Now())
			if c.Severity != sevUnknown {
				t.Errorf("check: %+v", c)
			}
		})
	}
}

func TestRecoveryStorageEvaluateRollup(t *testing.T) {
	s := New(Config{Store: &fakeStore{storageRows: []store.RecoveryStorageHealthRow{{RefusedCount: 1}}},
		RecoveryReadyPayloadPerOwner: 11, RecoveryInstanceBytes: 22, StoredFilesBudgetBytes: 33})
	d, err := s.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range d.Checks {
		if c.ID != "recovery.storage" {
			continue
		}
		found = true
		if c.Severity != sevWarn || c.Scope != "owner" {
			t.Errorf("check: %+v", c)
		}
		for _, want := range []string{"11 bytes", "22 bytes", "33 bytes"} {
			matched := false
			for _, e := range c.Evidence {
				if e.Value == want {
					matched = true
				}
			}
			if !matched {
				t.Errorf("missing limit %s", want)
			}
		}
	}
	if !found || d.Blocking || d.Status != sevWarn || d.Counts.Warn < 1 {
		t.Errorf("rollup: %+v", d)
	}
}
