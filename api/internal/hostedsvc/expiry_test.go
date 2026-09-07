package hostedsvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// fakeExpiryStore records the cutoff the sweep computes.
type fakeExpiryStore struct {
	gotCutoff pgtype.Timestamptz
	calls     int
	rows      int64
	err       error
}

func (f *fakeExpiryStore) ExpirePendingHostedWorkerTokens(_ context.Context, cutoff pgtype.Timestamptz) (int64, error) {
	f.calls++
	f.gotCutoff = cutoff
	return f.rows, f.err
}

// The sweep passes now-ttl as the cutoff, so only tokens that have sat longer
// than the TTL are destroyed.
func TestExpirePendingTokensComputesTheCutoffFromTheTTL(t *testing.T) {
	st := &fakeExpiryStore{rows: 3}
	before := time.Now()

	n, err := ExpirePendingTokens(context.Background(), st, time.Hour)
	if err != nil {
		t.Fatalf("ExpirePendingTokens: %v", err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want the store's row count", n)
	}
	if !st.gotCutoff.Valid {
		t.Fatal("cutoff was not set")
	}
	// The cutoff must be ~1h in the past: an hour-old pending token expires, a
	// fresh one does not.
	age := before.Sub(st.gotCutoff.Time)
	if age < 59*time.Minute || age > 61*time.Minute {
		t.Fatalf("cutoff is %v before now, want ~1h", age)
	}
}

// ttl <= 0 disables the sweep entirely: no query runs at all (config warns at
// boot that this leaves tokens sealed at rest indefinitely).
func TestExpirePendingTokensDisabledByZeroTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Hour} {
		st := &fakeExpiryStore{rows: 9}
		n, err := ExpirePendingTokens(context.Background(), st, ttl)
		if err != nil {
			t.Fatalf("ttl=%v: %v", ttl, err)
		}
		if n != 0 || st.calls != 0 {
			t.Fatalf("ttl=%v: ran the sweep (calls=%d, n=%d); want it disabled", ttl, st.calls, n)
		}
	}
}

func TestExpirePendingTokensPropagatesErrors(t *testing.T) {
	boom := errors.New("db exploded")
	_, err := ExpirePendingTokens(context.Background(), &fakeExpiryStore{err: boom}, time.Hour)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it propagated", err)
	}
}

// An expired token must NOT be delivered: the ciphertext is gone, and recovery is
// a fresh token (never a resurrected one). Poll reports the worker tokenless rather
// than inventing anything.
func TestPollDoesNotDeliverAnExpiredToken(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	id := newTestWorker(st, "base", "s", 1)
	seal(t, st, box, id, "uzw_will-expire")
	// The sweep cleared the ciphertext without ever stamping delivered_at.
	st.rows[id].ciphertext = nil

	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Workers) != 1 {
		t.Fatalf("%d workers, want the worker still in desired state", len(resp.Workers))
	}
	if resp.Workers[0].JoinToken != nil {
		t.Fatal("an expired token must never be delivered")
	}
}

// Rotation (the strand recovery): re-parking mints a NEW plaintext into the same
// row and makes it pending again. The old plaintext is never reachable.
func TestSealJoinTokenRotatesAStrandedWorker(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	id := newTestWorker(st, "base", "s", 1)

	seal(t, st, box, id, "uzw_original")
	// Delivered, then the Secret was lost — M2/M3 rotate a new token in.
	if err := svc.NoteRegistered(context.Background(), id, hashOf("uzw_original")); err != nil {
		t.Fatalf("NoteRegistered: %v", err)
	}
	if !st.rows[id].delivered {
		t.Fatal("registration did not stamp delivery")
	}

	seal(t, st, box, id, "uzw_rotated")
	if st.rows[id].delivered {
		t.Fatal("rotation must reset delivery, else the fresh token reads as already delivered")
	}
	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Workers[0].JoinToken == nil || *resp.Workers[0].JoinToken != "uzw_rotated" {
		t.Fatalf("join token = %v, want the ROTATED one", resp.Workers[0].JoinToken)
	}
}
