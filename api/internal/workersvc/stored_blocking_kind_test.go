package workersvc

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestStoredBlockingKind(t *testing.T) {
	capacity := func() *RollSignal {
		return &RollSignal{BlockingKind: BlockingKindSchedulerCapacity, Phase: PhaseStuck, PodPhase: "Pending",
			BlockingReason: "scheduler capacity: insufficient cpu"}
	}
	cases := []struct {
		name string
		sig  *RollSignal
		want string
	}{
		{"nil", nil, ""},
		{"capacity under the effective predicate", capacity(), BlockingKindSchedulerCapacity},
		{"size_refused while stuck", &RollSignal{BlockingKind: BlockingKindSizeRefused, Phase: PhaseStuck}, BlockingKindSizeRefused},
		{"pvc_rejected while stuck", &RollSignal{BlockingKind: BlockingKindPVCRejected, Phase: PhaseStuck, BlockingReason: "x"}, BlockingKindPVCRejected},
		{"size_refused while rolling is dropped", &RollSignal{BlockingKind: BlockingKindSizeRefused, Phase: PhaseRolling}, ""},
		{"size_refused while settled is dropped", &RollSignal{BlockingKind: BlockingKindSizeRefused, Phase: PhaseSettled}, ""},
		{"unknown kind while stuck is dropped", &RollSignal{BlockingKind: "made_up", Phase: PhaseStuck}, ""},
		{"capacity failing the predicate is dropped", &RollSignal{BlockingKind: BlockingKindSchedulerCapacity, Phase: PhaseStuck, PodPhase: "Pending", BlockingReason: "other"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StoredBlockingKind(tc.sig); got != tc.want {
				t.Errorf("StoredBlockingKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// Readers keep going through EffectiveBlockingKind: a stored refusal must never read as
// capacity_blocked.
func TestEffectiveBlockingKindIgnoresRefusals(t *testing.T) {
	for _, kind := range []string{BlockingKindSizeRefused, BlockingKindPVCRejected} {
		sig := &RollSignal{BlockingKind: kind, Phase: PhaseStuck, PodPhase: "Pending", BlockingReason: "scheduler capacity: insufficient cpu"}
		if got := EffectiveBlockingKind(sig); got != "" {
			t.Errorf("EffectiveBlockingKind(%s) = %q", kind, got)
		}
	}
}

func TestAdvertisementFresh(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: now.Add(-ago), Valid: true}
	}
	v := func(n int32) pgtype.Int4 { return pgtype.Int4{Int32: n, Valid: true} }
	cases := []struct {
		name string
		ver  pgtype.Int4
		at   pgtype.Timestamptz
		poll pgtype.Int4
		want bool
	}{
		{"fresh", v(1), at(5 * time.Second), v(3), true},
		{"just inside 2x poll", v(1), at(5*time.Second + 900*time.Millisecond), v(3), true},
		{"at 2x poll is stale", v(1), at(6 * time.Second), v(3), false},
		{"version too low", v(0), at(time.Second), v(3), false},
		{"no version", pgtype.Int4{}, at(time.Second), v(3), false},
		{"no timestamp", v(1), pgtype.Timestamptz{}, v(3), false},
		{"absent poll reads as the 300s maximum", v(1), at(599 * time.Second), pgtype.Int4{}, true},
		{"poll above the clamp is capped", v(1), at(601 * time.Second), v(100000), false},
		{"poll below the clamp is raised to 1s", v(1), at(3 * time.Second), v(-5), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AdvertisementFresh(tc.ver, tc.at, tc.poll, 1, now); got != tc.want {
				t.Errorf("AdvertisementFresh = %v, want %v", got, tc.want)
			}
		})
	}
}
