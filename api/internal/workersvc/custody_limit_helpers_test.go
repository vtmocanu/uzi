package workersvc

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/settings"
)

// testCustodyLimit is the test-side mirror of settings.DefaultCustodyHoldLimit, kept as an
// untyped constant so fixtures can use it in int and int64 arithmetic.
// TestCustodyLimitMirrorsSettingsDefault fails if the two ever drift.
const testCustodyLimit = 8

func TestCustodyLimitMirrorsSettingsDefault(t *testing.T) {
	want, err := strconv.ParseInt(settings.DefaultCustodyHoldLimit, 10, 32)
	if err != nil {
		t.Fatalf("settings.DefaultCustodyHoldLimit not an int: %v", err)
	}
	if int64(testCustodyLimit) != want {
		t.Fatalf("testCustodyLimit = %d, settings default = %d", testCustodyLimit, want)
	}
}

// fakeCustodyLimit is a CustodyHoldLimitReader returning a fixed value or error and counting
// reads, so tests can assert each consumer reads the setting once per claim / pass.
type fakeCustodyLimit struct {
	n     int32
	err   error
	calls atomic.Int32
}

func (f *fakeCustodyLimit) CustodyHoldLimit(context.Context) (int32, error) {
	f.calls.Add(1)
	if f.err != nil {
		return 0, f.err
	}
	return f.n, nil
}

// defaultCustodyLimitReader is the reader testParams wires: the settings default.
func defaultCustodyLimitReader() CustodyHoldLimitReader {
	return &fakeCustodyLimit{n: testCustodyLimit}
}

// fixedCustodyLimit adapts a constant to the recovery.Limits func shape.
func fixedCustodyLimit(n int32) func(context.Context) (int32, error) {
	return func(context.Context) (int32, error) { return n, nil }
}

// thWithLimit builds health thresholds carrying an available custody limit n (all other
// thresholds zero), the shape queuedReason tests need.
func thWithLimit(n int32) healthThresholds {
	return healthThresholds{custodyLimit: n, custodyLimitOK: true}
}
