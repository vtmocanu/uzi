package workersvc

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

// TestOnceFireCapOutsideInt4FailsClosed: a cap outside 0..MaxInt32 fails the pre-lock argument check as
// ErrOnceFireIdentity (it must never be narrowed into the header's int4); the boundaries and nil pass.
func TestOnceFireCapOutsideInt4FailsClosed(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	mk := func(c *int) *OnceFireProgress {
		return &OnceFireProgress{ScheduleID: uuid.New(), UserID: user, RepoID: repo, IssueIID: 4, EffectiveCap: c}
	}
	ptr := func(n int) *int { return &n }
	for name, tc := range map[string]struct {
		cap     *int
		refused bool
	}{
		"nil is unlimited":  {nil, false},
		"zero":              {ptr(0), false},
		"max int4":          {ptr(math.MaxInt32), false},
		"negative":          {ptr(-1), true},
		"max int4 plus one": {ptr(math.MaxInt32 + 1), true},
		"max int":           {ptr(math.MaxInt), true},
	} {
		err := checkOnceFireArgs(mk(tc.cap), user, repo, 4)
		if tc.refused != errors.Is(err, ErrOnceFireIdentity) || (!tc.refused && err != nil) {
			t.Errorf("%s: checkOnceFireArgs = %v, refused want %v", name, err, tc.refused)
		}
	}
	if got, err := onceFireCapArg(ptr(math.MaxInt32)); err != nil || !got.Valid || got.Int32 != math.MaxInt32 {
		t.Fatalf("onceFireCapArg(MaxInt32) = %+v %v", got, err)
	}
	if got, err := onceFireCapArg(nil); err != nil || got.Valid {
		t.Fatalf("onceFireCapArg(nil) = %+v %v, want NULL", got, err)
	}
}
