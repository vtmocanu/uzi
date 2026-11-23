package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// Pure tests of the shared selection kernel (decideAuto) and the pure exclusion rule
// (claimExcludeAt). The claim path's wiring is pinned by the auto_select tests; these pin the
// decision itself, with no store, no context and no clock.

func kernelCand(id uuid.UUID, label string, pooled bool, weeklyPct int16, synced time.Duration) autoselect.Candidate {
	pct := weeklyPct
	reset := autoNow.Add(48 * time.Hour)
	at := autoNow.Add(-synced)
	return autoselect.Candidate{SecretID: id, Label: label, AutoEligible: pooled, HasReading: true,
		SevenDayPct: &pct, SevenResetsAt: &reset, WeeklyResetsAt: &reset, SyncedAt: &at}
}

func kernelPolicy() autoselect.Policy { return autoParams().Autoselect }

func gateBlocking(ids ...uuid.UUID) *reserveGate {
	snap := &reserveSnap{userID: uuid.New(), threshold: i16p(70), claude: map[uuid.UUID]usagereserve.Verdict{}}
	for _, id := range ids {
		snap.claude[id] = held(90)
		snap.pooled = append(snap.pooled, id)
	}
	return &reserveGate{snap: snap, now: autoNow}
}

func TestDecideAutoKinds(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	cases := []struct {
		name     string
		cands    []autoselect.Candidate
		exclude  uuid.UUID
		gate     *reserveGate
		wantKind autoDecisionKind
		wantID   uuid.UUID
		wantHeld bool
	}{
		{"picks the roomier measurable token", []autoselect.Candidate{kernelCand(a, "a", true, 90, time.Minute), kernelCand(b, "b", true, 20, time.Minute)}, uuid.Nil, nil, autoPicked, b, false},
		{"exclude drops the dead credential from the ranking", []autoselect.Candidate{kernelCand(a, "a", true, 90, time.Minute), kernelCand(b, "b", true, 20, time.Minute)}, b, nil, autoPicked, a, false},
		{"nothing measurable floors onto a stale pooled token", []autoselect.Candidate{kernelCand(a, "a", true, 10, 3*time.Hour)}, uuid.Nil, nil, autoFloor, a, false},
		{"only the excluded token is pooled is an empty pool", []autoselect.Candidate{kernelCand(a, "a", true, 10, time.Minute)}, a, nil, autoPoolEmpty, uuid.Nil, false},
		{"an unpooled token is never spent", []autoselect.Candidate{kernelCand(a, "a", false, 10, time.Minute)}, uuid.Nil, nil, autoPoolEmpty, uuid.Nil, false},
		{"empty candidates", nil, uuid.Nil, nil, autoPoolEmpty, uuid.Nil, false},
		{"the reserve drops a blocked token and the pool is not crossed", []autoselect.Candidate{kernelCand(a, "a", true, 10, time.Minute), kernelCand(b, "b", true, 20, time.Minute)}, uuid.Nil, gateBlocking(a), autoPicked, b, false},
		{"the reserve emptying the pool is a hold, not an empty pool", []autoselect.Candidate{kernelCand(a, "a", true, 10, time.Minute)}, uuid.Nil, gateBlocking(a), autoReserveHeld, uuid.Nil, true},
		{"a reserve-blocked excluded token leaves an empty pool, not a hold", []autoselect.Candidate{kernelCand(a, "a", true, 10, time.Minute)}, a, gateBlocking(a), autoPoolEmpty, uuid.Nil, false},
	}
	for _, c := range cases {
		got := decideAuto(autoInput{cands: c.cands, exclude: c.exclude, gate: c.gate, policy: kernelPolicy(), now: autoNow})
		if got.kind != c.wantKind {
			t.Errorf("%s: kind = %d, want %d", c.name, got.kind, c.wantKind)
			continue
		}
		if c.wantID != uuid.Nil && got.id != c.wantID {
			t.Errorf("%s: id = %v, want %v", c.name, got.id, c.wantID)
		}
		if c.wantHeld != errors.Is(got.heldErr, errReserveHeld) {
			t.Errorf("%s: heldErr = %v, want held=%v", c.name, got.heldErr, c.wantHeld)
		}
		switch c.wantKind {
		case autoPicked:
			if got.reason == "" {
				t.Errorf("%s: picked decision carries no reason", c.name)
			}
		case autoFloor:
			if got.reason != string(autoselect.ReasonPoolStale) {
				t.Errorf("%s: floor reason = %q, want pool_stale", c.name, got.reason)
			}
		}
	}
}

func TestClaimExcludeAtBoundary(t *testing.T) {
	dead := uuid.New()
	deadPG := pgtype.UUID{Bytes: dead, Valid: true}
	cases := []struct {
		name  string
		dead  pgtype.UUID
		retry pgtype.Timestamptz
		want  uuid.UUID
	}{
		{"no dead credential excludes nothing", pgtype.UUID{}, pgconv.Time(autoNow.Add(time.Hour)), uuid.Nil},
		{"window still closed excludes", deadPG, pgconv.Time(autoNow.Add(time.Nanosecond)), dead},
		{"retry exactly now is relaxed", deadPG, pgconv.Time(autoNow), uuid.Nil},
		{"retry passed is relaxed", deadPG, pgconv.Time(autoNow.Add(-time.Second)), uuid.Nil},
		{"no stamp to wait on is relaxed", deadPG, pgtype.Timestamptz{}, uuid.Nil},
	}
	for _, c := range cases {
		if got := claimExcludeAt(c.dead, c.retry, autoNow); got != c.want {
			t.Errorf("%s: claimExcludeAt = %v, want %v", c.name, got, c.want)
		}
		// The Service form is the same rule at the service clock.
		svc := &Service{now: func() time.Time { return autoNow }}
		if got := svc.claimExcludeFor(c.dead, c.retry); got != c.want {
			t.Errorf("%s: claimExcludeFor = %v, want %v", c.name, got, c.want)
		}
	}
	// A bare-literal Service has no clock; a run with nothing to compare must not need one.
	if got := (&Service{}).claimExcludeFor(deadPG, pgtype.Timestamptz{}); got != uuid.Nil {
		t.Errorf("clockless claimExcludeFor = %v, want Nil", got)
	}
}
