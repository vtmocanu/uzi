package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestJoinWakeOrigin(t *testing.T) {
	cases := []struct{ cur, req, want string }{
		// A manual or demand request upgrades a warm op.
		{WakeOriginWarm, WakeOriginManual, WakeOriginManual},
		{WakeOriginWarm, WakeOriginDemand, WakeOriginDemand},
		// System is not stronger than warm; warm joining warm is a no-op.
		{WakeOriginWarm, WakeOriginSystem, WakeOriginWarm},
		{WakeOriginWarm, WakeOriginWarm, WakeOriginWarm},
		// Nothing ever downgrades to warm.
		{WakeOriginManual, WakeOriginWarm, WakeOriginManual},
		{WakeOriginDemand, WakeOriginWarm, WakeOriginDemand},
		{WakeOriginSystem, WakeOriginWarm, WakeOriginSystem},
		{"", WakeOriginWarm, ""},
		// Only warm is upgraded.
		{WakeOriginSystem, WakeOriginManual, WakeOriginSystem},
		{WakeOriginSystem, WakeOriginDemand, WakeOriginSystem},
		{WakeOriginManual, WakeOriginDemand, WakeOriginManual},
		{WakeOriginDemand, WakeOriginManual, WakeOriginDemand},
		// A legacy op (no valid marker) stays legacy.
		{"", WakeOriginManual, ""},
		{"", WakeOriginDemand, ""},
		{"", WakeOriginSystem, ""},
	}
	for _, c := range cases {
		if got := joinWakeOrigin(c.cur, c.req); got != c.want {
			t.Errorf("joinWakeOrigin(%q, %q) = %q, want %q", c.cur, c.req, got, c.want)
		}
	}
}

func originWorker(op uuid.UUID, markerOp uuid.UUID, admit, wake string, settled, manual *time.Time) store.Worker {
	w := store.Worker{HibernationOpID: hibernationUUID(op)}
	if markerOp != uuid.Nil {
		w.HibernationOriginOpID = hibernationUUID(markerOp)
	}
	w.HibernationAdmitOrigin = textOrNull(admit)
	w.HibernationWakeOrigin = textOrNull(wake)
	if settled != nil {
		w.LastWakeSettledAt = pgconv.Time(*settled)
	}
	if manual != nil {
		w.LastManualWakeSettledAt = pgconv.Time(*manual)
	}
	return w
}

func textOrNull(s string) pgtype.Text { return pgconv.TextOrNull(s) }

func TestCarryManualGrace(t *testing.T) {
	op, other := uuid.New(), uuid.New()
	settled := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	older := settled.Add(-48 * time.Hour)
	newer := settled.Add(time.Hour)
	cases := []struct {
		name   string
		w      store.Worker
		want   *time.Time // last_manual_wake_settled_at after the carry
		wasSet bool
	}{
		{"legacy: no markers is manual-grade", originWorker(op, uuid.Nil, "", "", &settled, nil), &settled, false},
		{"legacy: marker bound to another op is manual-grade", originWorker(op, other, "", WakeOriginWarm, &settled, nil), &settled, false},
		{"legacy: marker bound to another op, system", originWorker(op, other, "", WakeOriginSystem, &settled, nil), &settled, false},
		{"valid manual carries", originWorker(op, op, "", WakeOriginManual, &settled, nil), &settled, false},
		{"valid demand carries", originWorker(op, op, "", WakeOriginDemand, &settled, nil), &settled, false},
		{"valid warm does not carry", originWorker(op, op, "", WakeOriginWarm, &settled, nil), nil, false},
		{"valid system does not carry", originWorker(op, op, "", WakeOriginSystem, &settled, nil), nil, false},
		{"valid warm keeps the grace already earned", originWorker(op, op, "", WakeOriginWarm, &settled, &older), &older, true},
		{"an op that has not woken carries nothing (its settle was classified at admission)",
			originWorker(op, op, AdmitOriginAutoLong, "", &settled, nil), nil, false},
		{"carry never lowers an existing newer grace", originWorker(op, uuid.Nil, "", "", &settled, &newer), &newer, true},
		{"carry raises an older grace", originWorker(op, uuid.Nil, "", "", &settled, &older), &settled, true},
		{"never settled: nothing to carry", originWorker(op, uuid.Nil, "", "", nil, nil), nil, false},
	}
	for _, c := range cases {
		w := c.w
		carryManualGrace(&w)
		switch {
		case c.want == nil && w.LastManualWakeSettledAt.Valid:
			t.Errorf("%s: carried %v, want none", c.name, w.LastManualWakeSettledAt.Time)
		case c.want != nil && (!w.LastManualWakeSettledAt.Valid || !w.LastManualWakeSettledAt.Time.Equal(*c.want)):
			t.Errorf("%s: last_manual = %v, want %v", c.name, w.LastManualWakeSettledAt, *c.want)
		}
	}
}

// TestOriginStampedAtEverySite pins, per call site, the origin the site passes and that the
// primitive both stamps it against the new op and carries the earlier settle's grace forward first.
func TestOriginStampedAtEverySite(t *testing.T) {
	settled := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	now := settled.Add(24 * time.Hour)
	legacy := func(state string) store.Worker {
		return store.Worker{HibernationState: state, HibernationOpID: hibernationUUID(uuid.New()),
			LastWakeSettledAt: pgconv.Time(settled), CleanupPhase: CleanupPhaseNone}
	}
	admitSites := []struct{ site, origin string }{
		{"HibernateWorker", AdmitOriginManual},
		{"autoAdmit (long rule)", AdmitOriginAutoLong},
		{"surplus admission", AdmitOriginAutoSurplus},
	}
	for _, s := range admitSites {
		w := legacy("none")
		op := admitHibernation(&w, now, "", s.origin)
		if !originMarkersValid(&w) || w.HibernationOriginOpID.Bytes != op ||
			w.HibernationAdmitOrigin.String != s.origin || !w.HibernationAdmitOrigin.Valid || w.HibernationWakeOrigin.Valid {
			t.Errorf("%s: markers not stamped for the new op: %+v", s.site, w)
		}
		if !w.LastManualWakeSettledAt.Valid || !w.LastManualWakeSettledAt.Time.Equal(settled) {
			t.Errorf("%s: legacy settle not carried forward: %v", s.site, w.LastManualWakeSettledAt)
		}
	}
	wakeSites := []struct{ site, state, origin string }{
		{"WakeWorker with an actor", "hibernated", WakeOriginManual},
		{"WakeWorker without an actor", "hibernated", WakeOriginSystem},
		{"ApplyTransition refused after scale-down", "preparing", WakeOriginSystem},
		{"settle: preparation deadline", "preparing", WakeOriginSystem},
		{"settle: hibernation disabled", "hibernated", WakeOriginSystem},
		{"DemandWake", "hibernated", WakeOriginDemand},
		{"withdrawForCleanup", "preparing", WakeOriginSystem},
		{"warm wake (U2b)", "hibernated", WakeOriginWarm},
	}
	for _, s := range wakeSites {
		w := legacy(s.state)
		prev := w.HibernationOpID
		if !moveToWaking(&w, now, "r", s.origin) {
			t.Errorf("%s: no change reported", s.site)
			continue
		}
		if w.HibernationOpID == prev || !originMarkersValid(&w) || w.HibernationWakeOrigin.String != s.origin ||
			w.HibernationAdmitOrigin.Valid {
			t.Errorf("%s: markers not stamped for the new op: %+v", s.site, w)
		}
		if !w.LastManualWakeSettledAt.Valid || !w.LastManualWakeSettledAt.Time.Equal(settled) {
			t.Errorf("%s: legacy settle not carried forward: %v", s.site, w.LastManualWakeSettledAt)
		}
	}
	// withdrawForCleanup passes system.
	w := legacy("preparing")
	withdrawForCleanup(&w, now, "reason")
	if validWakeOrigin(&w) != WakeOriginSystem {
		t.Errorf("withdrawForCleanup wake origin = %q, want system", validWakeOrigin(&w))
	}
	// A warm op's settle is not carried by the next op, and a manual one is.
	w = legacy("hibernated")
	moveToWaking(&w, now, "r", WakeOriginWarm)
	w.LastWakeSettledAt = pgconv.Time(now)
	w.HibernationState = "none"
	w.LastManualWakeSettledAt = pgtype.Timestamptz{}
	admitHibernation(&w, now.Add(time.Hour), "", AdmitOriginAutoLong)
	if w.LastManualWakeSettledAt.Valid {
		t.Errorf("a warm settle was carried as manual-grade: %v", w.LastManualWakeSettledAt)
	}
}

// A manual or demand request on a waking warm op upgrades the origin in place: same op, same
// first wake time and readiness evidence, and the change is reported so the caller writes it.
func TestMoveToWakingJoinUpgradesWarmInPlace(t *testing.T) {
	for _, req := range []string{WakeOriginManual, WakeOriginDemand} {
		w := store.Worker{HibernationState: "hibernated", HibernationOpID: hibernationUUID(uuid.New()), CleanupPhase: CleanupPhaseNone}
		t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		if !moveToWaking(&w, t0, "warm", WakeOriginWarm) {
			t.Fatal("warm wake changed nothing")
		}
		w.WakeReadyPodUid = hibernationUUID(uuid.New())
		before := w
		if !moveToWaking(&w, t0.Add(time.Minute), "later", req) {
			t.Fatalf("%s upgrade of a warm op reported no change", req)
		}
		if w.HibernationOpID != before.HibernationOpID || w.WakeRequestedAt != before.WakeRequestedAt ||
			w.WakeReadyPodUid != before.WakeReadyPodUid || validWakeOrigin(&w) != req {
			t.Fatalf("%s upgrade: op/wake time/evidence moved or origin = %q", req, validWakeOrigin(&w))
		}
		if moveToWaking(&w, t0.Add(2*time.Minute), "again", req) || moveToWaking(&w, t0.Add(2*time.Minute), "again", WakeOriginWarm) {
			t.Fatal("a repeated or warm join changed the row")
		}
		if validWakeOrigin(&w) != req {
			t.Fatalf("warm join downgraded %s to %q", req, validWakeOrigin(&w))
		}
	}
}
