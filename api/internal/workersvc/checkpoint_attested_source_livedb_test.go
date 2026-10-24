package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_attested_source_livedb_test.go pins the api half of issue #3194 against a REAL
// Postgres: a publish's (tip, attested source) pair is frozen at admission, rides its attempt row,
// and reaches runs.checkpoint_attested_source only inside the same successful checkpoint-tip write
// (set-only), whichever writer lands it: the publish itself or the live or terminal reconciler.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const (
	attestTipA = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	attestTipB = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	attestTipC = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	attestTipD = "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
	attestS1   = "1010101010101010101010101010101010101010"
	attestS2   = "2020202020202020202020202020202020202020202020202020202020202020"
	attestSRun = "9090909090909090909090909090909090909090"
)

func attestText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// runAttestation reads runID's (checkpoint_tip, checkpoint_attested_source).
func (f *supersedeFix) runAttestation(t *testing.T, runID uuid.UUID) (tip, source pgtype.Text) {
	t.Helper()
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT checkpoint_tip, checkpoint_attested_source FROM runs WHERE id = $1`, runID).
		Scan(&tip, &source); err != nil {
		t.Fatalf("read run attestation: %v", err)
	}
	return tip, source
}

func (f *supersedeFix) assertAttestation(t *testing.T, step, wantTip string, wantSource pgtype.Text) {
	t.Helper()
	tip, source := f.runAttestation(t, f.oldRun)
	if tip != attestText(wantTip) || source != wantSource {
		t.Fatalf("%s: run = (tip %+v, attested %+v), want (tip %s, attested %+v)", step, tip, source, wantTip, wantSource)
	}
}

// TestPublishAttestedSourceSetOnlyLiveDB: through the real publish path on a live run, an attested
// publish stores its source with its tip; a lost-ack retry of the same tuple is idempotent; an
// unattested publish (stamped or legacy) advances the tip and leaves the stored source alone; a
// later attested publish replaces it; a stale-generation attested request is refused and writes
// nothing.
func TestPublishAttestedSourceSetOnlyLiveDB(t *testing.T) {
	f := newLiveAttemptFix(t)
	f.forge.descends[attestTipA] = lateTip
	f.forge.descends[attestTipB] = attestTipA
	f.forge.descends[attestTipC] = attestTipB
	f.forge.descends[attestTipD] = attestTipC
	wkr := boundedLiveWorker(t, f)
	gen := int64(1)
	s1, s2 := attestS1, attestS2
	publish := func(step, tip string, source *string, generation *int64) {
		t.Helper()
		res, err := f.svc1.PublishAttested(f.e.ctx, wkr, f.oldRun, tip, source, []byte("pack"), generation)
		if err != nil || !res.Published {
			t.Fatalf("%s: publish = %+v (err %v), want published", step, res, err)
		}
		if n := f.attemptCount(t, f.oldRun); n != 0 {
			t.Fatalf("%s: attempt rows = %d, want 0 (persisted and tracked)", step, n)
		}
	}

	publish("attested", attestTipA, &s1, &gen)
	f.assertAttestation(t, "attested", attestTipA, attestText(attestS1))

	// Lost acknowledgement: the worker resends the identical frozen tuple.
	publish("lost-ack retry", attestTipA, &s1, &gen)
	f.assertAttestation(t, "lost-ack retry", attestTipA, attestText(attestS1))

	publish("unattested", attestTipB, nil, &gen)
	f.assertAttestation(t, "unattested", attestTipB, attestText(attestS1))

	if _, err := f.svc1.Publish(f.e.ctx, wkr, f.oldRun, attestTipC, []byte("pack")); err != nil {
		t.Fatalf("legacy publish: %v", err)
	}
	f.assertAttestation(t, "legacy", attestTipC, attestText(attestS1))

	stale := int64(2)
	if _, err := f.svc1.PublishAttested(f.e.ctx, wkr, f.oldRun, attestTipD, &s2, []byte("pack"), &stale); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale generation: err = %v, want ErrStaleClaim", err)
	}
	if _, err := f.svc1.PublishAttested(f.e.ctx, wkr, f.oldRun, attestTipD, &s2, []byte("pack"), nil); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("attested without generation: err = %v, want ErrMissingClaimGeneration", err)
	}
	f.assertAttestation(t, "refused requests", attestTipC, attestText(attestS1))

	publish("re-attested", attestTipD, &s2, &gen)
	f.assertAttestation(t, "re-attested", attestTipD, attestText(attestS2))
}

// TestLatePublishNeverBindsSourceToAnotherTipLiveDB: an attested push whose compare-and-set base is
// stale (a newer publish persisted its own tip and source meanwhile) writes neither: its source is
// never attached to the newer tip, and the newer pair stays intact.
func TestLatePublishNeverBindsSourceToAnotherTipLiveDB(t *testing.T) {
	f := newLiveAttemptFix(t)
	late := &checkpointPush{s: f.svc1, runID: f.oldRun, checkpointKey: f.branch, ref: f.branchRef, attestedSource: attestText(attestS1)}
	if err := late.observePublishBase(f.e.ctx); err != nil {
		t.Fatalf("observePublishBase: %v", err)
	}
	newer := &checkpointPush{s: f.svc1, runID: f.oldRun, checkpointKey: f.branch, ref: f.branchRef, attestedSource: attestText(attestS2)}
	if err := newer.observePublishBase(f.e.ctx); err != nil {
		t.Fatalf("observePublishBase: %v", err)
	}
	if !f.svc1.persistPublishedTip(f.e.ctx, newer, attestTipB) {
		t.Fatal("setup: the newer publish did not persist")
	}
	if f.svc1.persistPublishedTip(f.e.ctx, late, attestTipA) {
		t.Fatal("a late publish over a moved checkpoint tip reported persisted")
	}
	f.assertAttestation(t, "late write", attestTipB, attestText(attestS2))

	// The late push of the SAME tip as the stored one (idempotent arm) carries its own frozen
	// value with that tip only; an unattested one leaves the pair alone.
	unattested := &checkpointPush{s: f.svc1, runID: f.oldRun, checkpointKey: f.branch, ref: f.branchRef}
	if err := unattested.observePublishBase(f.e.ctx); err != nil {
		t.Fatalf("observePublishBase: %v", err)
	}
	if !f.svc1.persistPublishedTip(f.e.ctx, unattested, attestTipB) {
		t.Fatal("same-tip unattested persist reported not persisted")
	}
	f.assertAttestation(t, "same-tip unattested", attestTipB, attestText(attestS2))
}

// TestPreparedAttemptAttestationIsIdentityLiveDB: a prepared live attempt is reused only for the
// identical (tip, attested source) tuple; a different source, or none, for the same attempt id is
// refused and never rebinds the row.
func TestPreparedAttemptAttestationIsIdentityLiveDB(t *testing.T) {
	f := newLiveAttemptFix(t)
	wkr := boundedLiveWorker(t, f)
	params := func(id uuid.UUID, source pgtype.Text) store.RecordLiveCheckpointPublishAttemptParams {
		return store.RecordLiveCheckpointPublishAttemptParams{
			RecordCheckpointPublishAttemptParams: store.RecordCheckpointPublishAttemptParams{
				RunID: f.oldRun, Branch: f.branch, Ref: f.branchRef, Tip: attestTipA, AttestedSource: source,
			},
			ExpectedWorkerID: pgtype.UUID{Bytes: wkr.ID, Valid: true}, ExpectedClaimGeneration: 1, AttemptID: id,
		}
	}
	id, err := f.e.q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params(uuid.Nil, attestText(attestS1)))
	if err != nil || id == uuid.Nil {
		t.Fatalf("prepare: id %v err %v", id, err)
	}
	if again, err := f.e.q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params(id, attestText(attestS1))); err != nil || again != id {
		t.Fatalf("same tuple: id %v err %v, want %v", again, err, id)
	}
	for name, source := range map[string]pgtype.Text{"different source": attestText(attestS2), "no source": {}} {
		if got, err := f.e.q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params(id, source)); err == nil {
			t.Fatalf("%s: admitted as attempt %v, want refused", name, got)
		}
	}
	if a := f.attemptRow(t, id); a.AttestedSource != attestText(attestS1) || a.Tip != attestTipA {
		t.Fatalf("prepared row rebound: %+v", a)
	}
	if n := f.attemptCount(t, f.oldRun); n != 1 {
		t.Fatalf("attempt rows = %d, want 1", n)
	}
}

// TestLiveReconcileStoresAttemptFrozenSourceLiveDB: after an api restart (a Service that never saw
// the publish), the live reconciler confirms a ready attempt and writes exactly that row's frozen
// source with its tip: a different value already on the run is replaced by the row's own, and a
// row without one leaves the run's value (set-only, never copied).
func TestLiveReconcileStoresAttemptFrozenSourceLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attempt pgtype.Text
		want    pgtype.Text
	}{
		{"attested attempt", attestText(attestS1), attestText(attestS1)},
		{"unattested attempt", pgtype.Text{}, attestText(attestSRun)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLiveAttemptFix(t)
			f.e.exec(t, `UPDATE runs SET checkpoint_attested_source = $2 WHERE id = $1`, f.oldRun, attestSRun)
			id, err := f.svc1.recordPublishAttempt(f.e.ctx, f.oldRun, f.branch, f.branchRef, lateTip, tc.attempt)
			if err != nil {
				t.Fatalf("record attempt: %v", err)
			}
			if n, err := f.e.q.MarkCheckpointPublishAttemptReady(f.e.ctx, id); err != nil || n != 1 {
				t.Fatalf("mark ready: rows=%d err=%v", n, err)
			}
			f.reconcileAttempts(t, f.svc2)
			assertLiveEvidence(t, f, id, lateTip, true)
			f.assertAttestation(t, "confirmed", lateTip, tc.want)
		})
	}
}

// TestTerminalReconcileStoresAttemptFrozenSourceLiveDB: the terminal attempts arm re-records a
// late-landed tip with exactly that attempt row's frozen source (or, for a row without one, keeps
// the run's value).
func TestTerminalReconcileStoresAttemptFrozenSourceLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attempt pgtype.Text
		want    pgtype.Text
	}{
		{"attested attempt", attestText(attestS2), attestText(attestS2)},
		{"unattested attempt", pgtype.Text{}, attestText(attestSRun)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSupersedeFix(t)
			f.e.exec(t, `UPDATE runs SET checkpoint_attested_source = $2 WHERE id = $1`, f.oldRun, attestSRun)
			if !f.forge.land(f.branchRef, retentionTestTip, newerTip) {
				t.Fatal("setup: the late push did not land")
			}
			if _, err := f.svc1.recordPublishAttempt(f.e.ctx, f.oldRun, f.branch, f.branchRef, newerTip, tc.attempt); err != nil {
				t.Fatalf("record attempt: %v", err)
			}
			f.coolOff(t)
			f.reconcileAttempts(t, f.svc2)
			if r := f.row(t); r.Tip != newerTip {
				t.Fatalf("record tip = %s, want the late tip re-recorded", r.Tip)
			}
			if n := f.attemptCount(t, f.oldRun); n != 0 {
				t.Fatalf("attempt rows = %d, want 0", n)
			}
			f.assertAttestation(t, "re-recorded", newerTip, tc.want)
		})
	}
}
