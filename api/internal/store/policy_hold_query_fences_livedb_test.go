package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// recoveryIdentity reads the required head and the verification pair straight from the row.
func (f *downFx) recoveryIdentity(id uuid.UUID) (required, verifiedHead *string, verifiedGen *int64) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, "SELECT policy_required_head,policy_recovery_verified_head,policy_recovery_verified_generation FROM runs WHERE id=$1", id).
		Scan(&required, &verifiedHead, &verifiedGen); err != nil {
		f.t.Fatal(err)
	}
	return required, verifiedHead, verifiedGen
}

// verifiedPreservingAt2 produces, through the real queries, a running preserving run at claim generation 2
// whose required head r is verified for that generation (VG=2, VH=r).
func (f *downFx) verifiedPreservingAt2(r string) uuid.UUID {
	f.t.Helper()
	id := f.queuedPreserving(r)
	f.claimAt(id, 2, "running")
	if _, err := f.q.RecordPolicyRecoveryVerified(f.ctx, store.RecordPolicyRecoveryVerifiedParams{
		ID: id, UserID: f.owner, WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true},
		ClaimGeneration: pgtype.Int8{Int64: 2, Valid: true}, RequiredHead: txt(r),
	}); err != nil {
		f.t.Fatalf("RecordPolicyRecoveryVerified: %v", err)
	}
	return id
}

// The supersede arm of ApplyPolicyHold needs publication evidence (published) in its own right: a report
// that only says the checkpoint contains the latest work must keep R. The Go service also checks this under
// the same lock, so only a direct query call can see the SQL conjunct.
func TestPolicyApplyHoldPublishedConjunctLiveDB(t *testing.T) {
	r, h := strings.Repeat("a", 40), strings.Repeat("b", 40)
	apply := func(f *downFx, id uuid.UUID, published bool) {
		t := f.t
		t.Helper()
		rows, err := f.q.ApplyPolicyHold(f.ctx, store.ApplyPolicyHoldParams{
			Head: txt(h), Published: published, CheckpointContainsLatest: pgtype.Bool{Bool: true, Valid: true},
			ClaimGeneration: pgtype.Int8{Int64: 2, Valid: true}, Cause: "refused", OutcomeID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
			ContinuationMode: txt("same_conversation"), WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true},
			SourceGeneration: pgtype.Int8{Int64: 2, Valid: true}, ActualRefusal: true, PolicyTag: txt("cyberPolicy"), Phase: txt("implementation"),
			CorrelationID: txt("pr-00000000000000000000000000000000-1"), ID: id, UserID: f.owner,
		})
		if err != nil || rows.ID != id {
			t.Fatalf("ApplyPolicyHold matched no row (the fixture must be a live generation-2 claim): %v", err)
		}
	}
	t.Run("not published keeps R even though the checkpoint contains the latest work", func(t *testing.T) {
		f := newDownFx(t)
		id := f.verifiedPreservingAt2(r)
		apply(f, id, false)
		required, vh, vg := f.recoveryIdentity(id)
		if required == nil || *required != r || vh == nil || *vh != r || vg == nil || *vg != 2 {
			t.Fatalf("unpublished report changed the identity: R=%v VH=%v VG=%v, want R=VH=%s VG=2", required, vh, vg, r)
		}
	})
	t.Run("positive control: published supersedes R with the reported head", func(t *testing.T) {
		f := newDownFx(t)
		id := f.verifiedPreservingAt2(r)
		apply(f, id, true)
		required, vh, vg := f.recoveryIdentity(id)
		if required == nil || *required != h || vh == nil || *vh != h || vg == nil || *vg != 2 {
			t.Fatalf("published report did not supersede: R=%v VH=%v VG=%v, want R=VH=%s VG=2", required, vh, vg, h)
		}
	})
}

// RecordPolicyRecoveryVerified is bound to the row's current claim generation by its own predicate.
func TestPolicyRecordVerifiedGenerationFenceLiveDB(t *testing.T) {
	r := strings.Repeat("a", 40)
	record := func(f *downFx, id uuid.UUID, gen int64) error {
		_, err := f.q.RecordPolicyRecoveryVerified(f.ctx, store.RecordPolicyRecoveryVerifiedParams{
			ID: id, UserID: f.owner, WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true},
			ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}, RequiredHead: txt(r),
		})
		return err
	}
	t.Run("a different generation updates nothing", func(t *testing.T) {
		f := newDownFx(t)
		id := f.queuedPreserving(r)
		f.claimAt(id, 2, "running")
		for _, gen := range []int64{1, 3} {
			if err := record(f, id, gen); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("generation %d against a generation-2 claim: err = %v, want no rows", gen, err)
			}
		}
		required, vh, vg := f.recoveryIdentity(id)
		if required == nil || *required != r || vh != nil || vg != nil {
			t.Fatalf("a foreign generation changed the verification: R=%v VH=%v VG=%v", required, vh, vg)
		}
	})
	t.Run("positive control: the current generation records the pair", func(t *testing.T) {
		f := newDownFx(t)
		id := f.queuedPreserving(r)
		f.claimAt(id, 2, "running")
		if err := record(f, id, 2); err != nil {
			t.Fatalf("current generation: %v", err)
		}
		required, vh, vg := f.recoveryIdentity(id)
		if required == nil || *required != r || vh == nil || *vh != r || vg == nil || *vg != 2 {
			t.Fatalf("pair not recorded: R=%v VH=%v VG=%v", required, vh, vg)
		}
	})
}
