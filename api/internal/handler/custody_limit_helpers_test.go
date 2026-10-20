package handler

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// fakeCustodyLimit is a workersvc.CustodyHoldLimitReader returning a fixed value or error and
// counting reads.
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

// defaultCustodyLimit is the reader a handler-test workersvc.Params carries when the test does
// not care about the limit: the settings default, parsed rather than restated.
func defaultCustodyLimit() workersvc.CustodyHoldLimitReader {
	n, err := strconv.ParseInt(settings.DefaultCustodyHoldLimit, 10, 32)
	if err != nil {
		panic("settings.DefaultCustodyHoldLimit is not an integer: " + err.Error())
	}
	return &fakeCustodyLimit{n: int32(n)}
}

func TestDefaultCustodyLimitIsSettingsDefault(t *testing.T) {
	got, err := defaultCustodyLimit().CustodyHoldLimit(context.Background())
	want, _ := strconv.ParseInt(settings.DefaultCustodyHoldLimit, 10, 32)
	if err != nil || int64(got) != want || got < 1 {
		t.Fatalf("default reader = %d, %v; want %d", got, err, want)
	}
}

// defaultCustodyLimitFunc is recovery.Limits.CustodyHoldLimit for a handler test that does not
// care about the limit: the same settings default defaultCustodyLimit reads.
func defaultCustodyLimitFunc() func(context.Context) (int32, error) {
	return defaultCustodyLimit().CustodyHoldLimit
}
