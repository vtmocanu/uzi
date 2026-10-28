package healthsvc

import (
	"context"
	"errors"
	"fmt"
	"math"
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
			if c.Severity != tc.want || c.AttentionBanner != (tc.want == sevWarn) || c.Scope != "owner" || c.Group != groupHousekeeping || !strings.Contains(c.Summary, "captures currently marked quota-refused") {
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

func TestRecoveryStorageBudgetBoundaries(t *testing.T) {
	for _, budget := range []string{"per-owner recovery", "instance recovery", "shared stored files"} {
		for _, capacity := range []int64{100, 9_000_000_000_000_000_000} {
			// Literal expectations avoid reproducing the percentage multiplication under test.
			boundary := int64(80)
			if capacity > 100 {
				boundary = 7_200_000_000_000_000_000
			}
			for _, delta := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/%d/%d", budget, capacity, delta), func(t *testing.T) {
					usage := boundary + delta
					cfg := Config{}
					row := store.RecoveryStorageHealthRow{}
					switch budget {
					case "per-owner recovery":
						cfg.RecoveryReadyPayloadPerOwner = capacity
						row.MaxOwnerRecoveryBytes = usage
						row.AvailableBytes = usage
					case "instance recovery":
						cfg.RecoveryInstanceBytes = capacity
						row.AvailableBytes = usage - 3
						row.PreparingBytes, row.UploadingBytes = 1, 2
					case "shared stored files":
						cfg.StoredFilesBudgetBytes = capacity
						row.AvailableBytes = usage - 6
						row.PreparingBytes, row.UploadingBytes, row.JobBytes = 1, 2, 3
						row.ReclaimableBytes = 3
					}
					cfg.Store = &fakeStore{storageRows: []store.RecoveryStorageHealthRow{row}}
					c := New(cfg).checkRecoveryStorage(context.Background(), time.Now())
					want := sevOK
					if delta >= 0 {
						want = sevWarn
					}
					if c.Severity != want || c.Scope != "owner" || c.Group != groupHousekeeping {
						t.Fatalf("check: %+v, want %s", c, want)
					}
					if want == sevWarn {
						expected := "At least 80% of budget: " + budget + "; 0 captures currently marked quota-refused."
						if c.Summary != expected || c.Action == nil || *c.Action != "Reduce stored bytes or raise the pressured budgets before recovery uploads are refused." {
							t.Errorf("pressure: %+v", c)
						}
					} else if c.Action != nil || c.Summary != "0 captures currently marked quota-refused." {
						t.Errorf("below threshold: %+v", c)
					}
				})
			}
		}
	}
}

func TestRecoveryStorageAccountingPressure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		row     store.RecoveryStorageHealthRow
		budgets string
	}{
		{"mixed recovery", store.RecoveryStorageHealthRow{MaxOwnerRecoveryBytes: 80, AvailableBytes: 20, PreparingBytes: 30, UploadingBytes: 30}, "per-owner recovery, instance recovery, shared stored files"},
		{"jobs only", store.RecoveryStorageHealthRow{JobBytes: 80, ReclaimableBytes: 80}, "shared stored files"},
		{"jobs do not inflate recovery", store.RecoveryStorageHealthRow{MaxOwnerRecoveryBytes: 79, AvailableBytes: 20, PreparingBytes: 29, UploadingBytes: 30, JobBytes: 80, ReclaimableBytes: 80}, "shared stored files"},
		{"owner maximum independent of total", store.RecoveryStorageHealthRow{MaxOwnerRecoveryBytes: 40, AvailableBytes: 80}, "instance recovery, shared stored files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Config{Store: &fakeStore{storageRows: []store.RecoveryStorageHealthRow{tc.row}},
				RecoveryReadyPayloadPerOwner: 100, RecoveryInstanceBytes: 100, StoredFilesBudgetBytes: 100}).checkRecoveryStorage(context.Background(), time.Now())
			if c.Severity != sevWarn || c.Summary != "At least 80% of budget: "+tc.budgets+"; 0 captures currently marked quota-refused." {
				t.Errorf("check: %+v", c)
			}
			want := map[string]string{
				"Maximum owner recovery bytes": fmt.Sprint(tc.row.MaxOwnerRecoveryBytes),
				"Quota-refused captures":       "0",
				"Reclaimable job bytes":        fmt.Sprint(tc.row.ReclaimableBytes),
			}
			for _, e := range c.Evidence {
				if value, ok := want[e.Label]; ok && e.Value == value {
					delete(want, e.Label)
				}
			}
			if len(want) != 0 {
				t.Errorf("missing evidence: %v; got %+v", want, c.Evidence)
			}
		})
	}
}

func TestRecoveryStorageDisabledBudgets(t *testing.T) {
	for _, budget := range []string{"owner", "instance", "shared"} {
		for _, disabled := range []int64{0, -1} {
			for _, signal := range []string{"none", "another budget", "refusal"} {
				t.Run(fmt.Sprintf("%s/%d/%s", budget, disabled, signal), func(t *testing.T) {
					// High usage remains below each enabled budget, except the explicit sibling pressure.
					cfg := Config{RecoveryReadyPayloadPerOwner: math.MaxInt64, RecoveryInstanceBytes: math.MaxInt64, StoredFilesBudgetBytes: math.MaxInt64}
					row := store.RecoveryStorageHealthRow{MaxOwnerRecoveryBytes: 1_000_000, AvailableBytes: 1_000_000, JobBytes: 1_000_000}
					switch budget {
					case "owner":
						cfg.RecoveryReadyPayloadPerOwner = disabled
					case "instance":
						cfg.RecoveryInstanceBytes = disabled
					case "shared":
						cfg.StoredFilesBudgetBytes = disabled
					}
					pressured := "per-owner recovery"
					if signal == "another budget" {
						if budget == "owner" {
							cfg.RecoveryInstanceBytes = 100
							pressured = "instance recovery"
						} else {
							cfg.RecoveryReadyPayloadPerOwner = 100
						}
					}
					if signal == "refusal" {
						row.RefusedCount = 1
					}
					cfg.Store = &fakeStore{storageRows: []store.RecoveryStorageHealthRow{row}}
					c := New(cfg).checkRecoveryStorage(context.Background(), time.Now())
					want := sevOK
					if signal != "none" {
						want = sevWarn
					}
					if c.Severity != want {
						t.Errorf("check: %+v, want %s", c, want)
					}
					if signal == "none" && (c.Summary != "0 captures currently marked quota-refused." || c.Action != nil) {
						t.Errorf("no signal: %+v", c)
					}
					if signal == "another budget" && c.Summary != "At least 80% of budget: "+pressured+"; 0 captures currently marked quota-refused." {
						t.Errorf("wrong pressured budgets: %+v", c)
					}
					if signal == "refusal" && (c.Summary != "1 captures currently marked quota-refused." || c.Action == nil || *c.Action != "Review currently quota-refused captures with their owners (uzi run recovery). This observes stored state; an admitted retry clears the marker before upload succeeds.") {
						t.Errorf("refusal only: %+v", c)
					}
				})
			}
		}
	}
}

func TestRecoveryStorageSummaryAndActions(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pressure        bool
		refused         int64
		summary, action string
	}{
		{"refusal only", false, 1, "1 captures currently marked quota-refused.", "Review currently quota-refused captures with their owners (uzi run recovery). This observes stored state; an admitted retry clears the marker before upload succeeds."},
		{"both", true, math.MaxInt64, "At least 80% of budget: per-owner recovery, instance recovery, shared stored files; 9223372036854775807 captures currently marked quota-refused.", "Reduce stored bytes or raise the pressured budgets, and review currently quota-refused captures with their owners (uzi run recovery). An admitted retry clears the marker before upload succeeds."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := store.RecoveryStorageHealthRow{RefusedCount: tc.refused}
			if tc.pressure {
				row.MaxOwnerRecoveryBytes, row.AvailableBytes = 80, 80
			}
			c := New(Config{Store: &fakeStore{storageRows: []store.RecoveryStorageHealthRow{row}}, RecoveryReadyPayloadPerOwner: 100, RecoveryInstanceBytes: 100, StoredFilesBudgetBytes: 100}).checkRecoveryStorage(context.Background(), time.Now())
			if c.Severity != sevWarn || c.Summary != tc.summary || len(c.Summary) >= 200 || c.Action == nil || *c.Action != tc.action {
				t.Errorf("check: %+v", c)
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
			if c.Severity != sevUnknown || c.AttentionBanner {
				t.Errorf("check: %+v", c)
			}
		})
	}
}

func TestRecoveryStorageEvaluateRollup(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  store.RecoveryStorageHealthRow
	}{
		{"refusal only", store.RecoveryStorageHealthRow{RefusedCount: 1}},
		{"pressure only", store.RecoveryStorageHealthRow{MaxOwnerRecoveryBytes: 9, AvailableBytes: 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Store: &fakeStore{storageRows: []store.RecoveryStorageHealthRow{tc.row}},
				RecoveryReadyPayloadPerOwner: 11, RecoveryInstanceBytes: 22, StoredFilesBudgetBytes: 33})
			d, err := s.Evaluate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, c := range d.Checks {
				if c.ID != "recovery.storage" {
					if c.AttentionBanner {
						t.Errorf("unexpected attention opt-in: %+v", c)
					}
					continue
				}
				found = true
				if c.Severity != sevWarn || c.Scope != "owner" || c.Group != groupHousekeeping || !c.AttentionBanner {
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
		})
	}
}
