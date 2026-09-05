package hostedsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// tokenRow models a hosted_worker_tokens ROW, not just a ciphertext.
//
// Modelling the row is the whole point. The previous fake kept a map of
// ciphertexts and short-circuited `if no ciphertext { return 0 rows }`, which the
// real UPDATE never did — and that divergence is precisely what hid a High from
// this suite: against the real query a row with no ciphertext still matches and
// still re-stamps. A fake that is kinder than production is a test that lies, so
// this one mirrors the SQL's states exactly:
//
//	ciphertext != nil, delivered == false -> pending
//	ciphertext == nil,  delivered == true  -> delivered (steady state)
//	ciphertext == nil,  delivered == false -> expired unread
type tokenRow struct {
	ciphertext []byte
	delivered  bool
}

// fakeStore is an in-memory Store: hosted worker rows plus the hosted_worker_tokens
// table, so the handoff can be exercised across polls without a DB.
type fakeStore struct {
	workers []store.ListHostedWorkersForControllerRow
	// rows is the hosted_worker_tokens table, keyed by worker id. An absent key is
	// an absent ROW.
	rows map[uuid.UUID]*tokenRow
	// tokenHashes mirrors workers.token_hash, which the ack's subquery re-checks
	// against the hash the caller proved. Without it the fake could not model a
	// rotation at all — and modelling exactly what the SQL does is this fake's job.
	tokenHashes map[uuid.UUID][]byte
	// listErr / markErr / cordonErr / uncordonErr force the failure paths.
	listErr     error
	markErr     error
	cordonErr   error
	uncordonErr error
	// markCalls counts MarkHostedWorkerTokenDelivered calls, including no-ops.
	markCalls int
	// cordoned records the ids passed to CordonHostedWorker, in order.
	cordoned []uuid.UUID
	// uncordoned records the ids passed to UncordonHostedWorker, in order.
	uncordoned []uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[uuid.UUID]*tokenRow{}, tokenHashes: map[uuid.UUID][]byte{}}
}

// The params (disk-pressure min streak + heartbeat cutoff) drive the real SQL's
// disk_pressure derivation, but this fake models the DERIVED result directly on each Row
// (DiskPressure/Ephemeral set by the test), so it ignores them — the SQL derivation itself
// is exercised by the live-DB tests, not here.
func (f *fakeStore) ListHostedWorkersForController(context.Context, store.ListHostedWorkersForControllerParams) ([]store.ListHostedWorkersForControllerRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Re-project the LEFT JOIN: a worker's ciphertext is whatever its row currently
	// holds, and a worker with no row at all reads as NULL.
	out := make([]store.ListHostedWorkersForControllerRow, 0, len(f.workers))
	for _, w := range f.workers {
		if row := f.rows[w.ID]; row != nil {
			w.TokenCiphertext = row.ciphertext
		} else {
			w.TokenCiphertext = nil
		}
		out = append(out, w)
	}
	return out, nil
}

// CordonHostedWorker mirrors the real UPDATE ... WHERE id = $1 AND kind = 'hosted':
// rows affected = 1 when a hosted worker with that id exists, else 0. Every worker in
// this fake is hosted, so existence in f.workers is the whole predicate.
func (f *fakeStore) CordonHostedWorker(_ context.Context, id uuid.UUID) (int64, error) {
	f.cordoned = append(f.cordoned, id)
	if f.cordonErr != nil {
		return 0, f.cordonErr
	}
	for _, w := range f.workers {
		if w.ID == id {
			return 1, nil
		}
	}
	return 0, nil
}

// UncordonHostedWorker mirrors the real UPDATE ... SET draining_since = NULL WHERE
// id = $1 AND kind = 'hosted': it clears draining_since on the matching hosted row
// and reports rows affected = 1 when such a row exists, else 0. Every worker in this
// fake is hosted, so existence in f.workers is the whole predicate.
func (f *fakeStore) UncordonHostedWorker(_ context.Context, id uuid.UUID) (int64, error) {
	f.uncordoned = append(f.uncordoned, id)
	if f.uncordonErr != nil {
		return 0, f.uncordonErr
	}
	for i := range f.workers {
		if f.workers[i].ID == id {
			f.workers[i].DrainingSince = pgtype.Timestamptz{Valid: false}
			return 1, nil
		}
	}
	return 0, nil
}

// UpsertHostedWorkerToken mirrors the real ON CONFLICT DO UPDATE: a re-park
// replaces the ciphertext and RESETS delivery, which is the rotation path.
func (f *fakeStore) UpsertHostedWorkerToken(_ context.Context, arg store.UpsertHostedWorkerTokenParams) error {
	f.rows[arg.WorkerID] = &tokenRow{ciphertext: arg.TokenCiphertext, delivered: false}
	return nil
}

// MarkHostedWorkerTokenDelivered mirrors the real UPDATE verbatim, both predicates
// included:
//
//	WHERE worker_id = $1
//	  AND worker_id IN (SELECT id FROM workers
//	                    WHERE kind = 'hosted' AND token_hash = $2)
//	  AND (token_ciphertext IS NOT NULL OR delivered_at IS NULL)
//
// Two things the fake must show, because both were once got wrong:
//
//   - No "has a ciphertext" short-circuit. An expired row (no ciphertext, no
//     delivered_at) satisfies the guard's second disjunct and DOES match, stamping
//     delivered_at — the deliberate self-heal. The old fake short-circuited here and
//     that divergence hid a High.
//   - The token_hash re-check. A caller whose proved hash is no longer current (a
//     rotation committed between its auth and this write) matches zero rows.
//
// The kind scope is not modelled — every worker in this fake is hosted.
func (f *fakeStore) MarkHostedWorkerTokenDelivered(_ context.Context, arg store.MarkHostedWorkerTokenDeliveredParams) (int64, error) {
	f.markCalls++
	if f.markErr != nil {
		return 0, f.markErr
	}
	// The subquery: the worker must exist AND still carry the hash the caller proved.
	cur, ok := f.tokenHashes[arg.WorkerID]
	if !ok || !bytes.Equal(cur, arg.ProvedTokenHash) {
		return 0, nil
	}
	row, ok := f.rows[arg.WorkerID]
	if !ok {
		return 0, nil // no row
	}
	if row.ciphertext == nil && row.delivered {
		return 0, nil // already delivered: the guard excludes it (idempotence)
	}
	row.ciphertext = nil
	row.delivered = true
	return 1, nil
}

func (f *fakeStore) addWorker(id uuid.UUID, template, size string, generation int64) {
	f.workers = append(f.workers, store.ListHostedWorkersForControllerRow{
		ID:               id,
		TemplateDeclared: pgtype.Text{String: template, Valid: true},
		HostedSize:       pgtype.Text{String: size, Valid: true},
		HostedGeneration: generation,
	})
}

// newTestWorker seeds a hosted worker and returns its id.
func newTestWorker(f *fakeStore, template, size string, generation int64) uuid.UUID {
	id := uuid.New()
	f.addWorker(id, template, size, generation)
	return id
}

func newTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	// A varied (not all-identical) key: secretbox.LoadKey rejects those as
	// placeholders, and a test should not depend on a shape production refuses.
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

func newTestService(t *testing.T, st Store) *Service {
	t.Helper()
	return New(st, newTestBox(t), time.Now, time.Hour)
}

// hashOf is the test-side stand-in for jointoken.Hash.
func hashOf(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// seal is the test-side provision/rotation path. It writes workers.token_hash AND
// parks the sealed ciphertext together, because that atomicity is the premise the
// ack's token_hash predicate rests on: "the hash I proved is still current" only
// implies "the parked ciphertext is the token I proved" if one transaction ever
// writes both. M2's rotation is required to do the same (pinned in the PRD), so the
// fake must not model a shape M2 is forbidden to produce.
func seal(t *testing.T, st *fakeStore, box *secretbox.Box, id uuid.UUID, token string) {
	t.Helper()
	if err := SealJoinToken(context.Background(), st, box, id, token); err != nil {
		t.Fatalf("SealJoinToken: %v", err)
	}
	st.tokenHashes[id] = hashOf(token)
}

// The handoff end to end, across the polls a real worker's token lives through.
func TestPollDeliversTokenUntilTheWorkerRegisters(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	id := newTestWorker(st, "base", "s", 1)
	seal(t, st, box, id, "uzw_the-plaintext")

	// The sealed copy must not be the plaintext sitting in a bytea.
	if string(st.rows[id].ciphertext) == "uzw_the-plaintext" {
		t.Fatal("join token was stored in plaintext")
	}

	// Poll 1: no pod has proved possession, so the token is delivered.
	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if len(resp.Workers) != 1 {
		t.Fatalf("poll 1: %d workers, want 1", len(resp.Workers))
	}
	got := resp.Workers[0]
	if got.ID != id.String() || got.Template != "base" || got.Size != "s" || got.Generation != 1 {
		t.Fatalf("poll 1: desired = %+v, want the seeded worker", got)
	}
	if got.JoinToken == nil || *got.JoinToken != "uzw_the-plaintext" {
		t.Fatalf("poll 1: join token = %v, want the sealed plaintext round-tripped", got.JoinToken)
	}
	// Proof-of-possession stands on three legs; this pins the one that otherwise
	// rests on inspection alone: a POLL WRITES NOTHING. Only a worker's registration
	// settles delivery, so no number of polls may ever reach the ack statement.
	if st.markCalls != 0 {
		t.Fatalf("the poll reached MarkHostedWorkerTokenDelivered %d times; a poll must be a pure read", st.markCalls)
	}

	// Poll 2: the pod has not booted yet (a slow image pull), so the SAME token is
	// re-delivered. A lost poll response must never be the end of the only
	// recoverable copy.
	resp, err = svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if resp.Workers[0].JoinToken == nil || *resp.Workers[0].JoinToken != "uzw_the-plaintext" {
		t.Fatal("poll 2: an undelivered token must be re-delivered")
	}

	// The pod boots and registers: proof of possession. The buffer is destroyed.
	if err := svc.NoteRegistered(context.Background(), id, hashOf("uzw_the-plaintext")); err != nil {
		t.Fatalf("NoteRegistered: %v", err)
	}
	if st.rows[id].ciphertext != nil {
		t.Fatal("the sealed copy must be destroyed once a pod proves it holds the token")
	}
	if !st.rows[id].delivered {
		t.Fatal("delivered_at must be stamped")
	}

	// Poll 3: still desired state, tokenless forever after.
	resp, err = svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	if len(resp.Workers) != 1 || resp.Workers[0].JoinToken != nil {
		t.Fatalf("poll 3: worker = %+v, want it still desired with no token", resp.Workers[0])
	}
}

// THE REGRESSION THIS DESIGN EXISTS FOR.
//
// This drives the exact interleaving that broke the old protocol, and it FAILS
// without the token_hash predicate on the ack — it is a regression test, not a
// description.
//
// The ordering: a healthy pod holding T1 begins a register (auth proves T1 at T0).
// While its RegisterWorker round trip is in flight, a rotation commits T2 —
// workers.token_hash becomes sha256(T2) and T2 is parked. The in-flight request
// then reaches NoteRegistered still carrying the hash it proved: T1's.
//
// Unqualified (matching worker_id alone) that request destroys T2 — a token no one
// has received — and stamps delivered_at, reproducing the original defect one layer
// down: a row reading "delivered, steady state" while no pod holds the current
// token. Qualified by the proved hash, it matches zero rows and T2 survives for the
// pod that will actually hold it.
func TestInFlightRegisterCannotDestroyARotatedToken(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	id := newTestWorker(st, "base", "s", 1)

	// T1 delivered and in use by a healthy pod.
	seal(t, st, box, id, "uzw_T1")
	provedByTheInFlightRequest := hashOf("uzw_T1") // what its auth established at T0
	if err := svc.NoteRegistered(context.Background(), id, provedByTheInFlightRequest); err != nil {
		t.Fatalf("NoteRegistered T1: %v", err)
	}

	// A rotation commits while that pod's next register is mid-flight: token_hash and
	// the parked ciphertext both move to T2, atomically.
	seal(t, st, box, id, "uzw_T2")

	// The in-flight request lands, still carrying T1's hash. This is the write that
	// used to destroy T2.
	if err := svc.NoteRegistered(context.Background(), id, provedByTheInFlightRequest); err != nil {
		t.Fatalf("stale NoteRegistered: %v", err)
	}
	if st.rows[id].ciphertext == nil {
		t.Fatal("a register that proved only T1 destroyed the freshly rotated T2 — the qualification is missing or ineffective")
	}
	if st.rows[id].delivered {
		t.Fatal("T2 was stamped delivered by a request that never held it")
	}

	// T2 is still there to be collected.
	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Workers[0].JoinToken == nil || *resp.Workers[0].JoinToken != "uzw_T2" {
		t.Fatalf("join token = %v, want the rotated T2 still deliverable", resp.Workers[0].JoinToken)
	}

	// The T2-bearing pod boots and registers. Only now is the buffer destroyed.
	if err := svc.NoteRegistered(context.Background(), id, hashOf("uzw_T2")); err != nil {
		t.Fatalf("NoteRegistered T2: %v", err)
	}
	if st.rows[id].ciphertext != nil || !st.rows[id].delivered {
		t.Fatalf("row = %+v, want delivered and destroyed", st.rows[id])
	}
}

// Re-registration (a pod rescheduled onto another node presents the same token
// again) must be a no-op, not a re-stamp. That is what the guard's first disjunct
// buys, and why the guard is about idempotence rather than churn.
func TestNoteRegisteredIsIdempotentOnReRegistration(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	id := newTestWorker(st, "base", "m", 3)
	seal(t, st, box, id, "uzw_x")

	for i := range 3 {
		if err := svc.NoteRegistered(context.Background(), id, hashOf("uzw_x")); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		if st.rows[id].ciphertext != nil || !st.rows[id].delivered {
			t.Fatalf("register %d: row = %+v, want delivered and destroyed", i, st.rows[id])
		}
	}
	if st.markCalls != 3 {
		t.Fatalf("markCalls = %d, want 3 (every registration reaches the store)", st.markCalls)
	}
}

// A late registration against an EXPIRED row is a correct self-heal, not
// laundering. The trigger is a pod proving it holds the token, so "the Secret was
// written after all and the pod finally booted" is the truth — this is what lets a
// worker stuck in ImagePullBackOff past the TTL recover without a rotation it never
// needed. (Under the old controller-assertion ack the same transition WOULD have
// been laundering, because "a Secret exists" cannot prove the token arrived.)
func TestLateRegistrationSelfHealsAnExpiredRow(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	id := newTestWorker(st, "base", "s", 1)
	seal(t, st, box, id, "uzw_slow-pull")

	// The TTL sweep cleared the buffer before the pod booted.
	st.rows[id].ciphertext = nil
	if st.rows[id].delivered {
		t.Fatal("precondition: an expired row must not be marked delivered")
	}

	// The pod finally boots, reads the Secret the controller had written, registers.
	// The self-heal survives the hash predicate: this pod holds the current token, it
	// merely booted late.
	if err := svc.NoteRegistered(context.Background(), id, hashOf("uzw_slow-pull")); err != nil {
		t.Fatalf("NoteRegistered: %v", err)
	}
	if !st.rows[id].delivered {
		t.Fatal("a late registration must correct an expired row to delivered")
	}
}

// The busy/draining_since poll fields (PRD #422 M3/M5) must round-trip from the store
// row into the DesiredWorker DTO the controller reads. The two workers carry
// discriminating values (busy-not-draining vs draining-not-busy) so a swapped or dropped
// field is caught here, not just by the wire golden. M5 asserts on DrainingSince
// (nil vs the mapped timestamp), not a bool: draining == DrainingSince != nil.
func TestPollCarriesBusyAndDraining(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)

	busyID := uuid.New()
	drainID := uuid.New()
	drainingAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	st.workers = append(st.workers,
		store.ListHostedWorkersForControllerRow{
			ID:               busyID,
			TemplateDeclared: pgtype.Text{String: "base", Valid: true},
			HostedSize:       pgtype.Text{String: "s", Valid: true},
			HostedGeneration: 1,
			Busy:             true,
			DrainingSince:    pgtype.Timestamptz{Valid: false},
		},
		store.ListHostedWorkersForControllerRow{
			ID:               drainID,
			TemplateDeclared: pgtype.Text{String: "jvm", Valid: true},
			HostedSize:       pgtype.Text{String: "l", Valid: true},
			HostedGeneration: 2,
			Busy:             false,
			DrainingSince:    pgtype.Timestamptz{Time: drainingAt, Valid: true},
		},
	)

	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Workers) != 2 {
		t.Fatalf("%d workers, want 2", len(resp.Workers))
	}

	byID := map[string]DesiredWorker{}
	for _, w := range resp.Workers {
		byID[w.ID] = w
	}
	if got := byID[busyID.String()]; !got.Busy || got.DrainingSince != nil {
		t.Fatalf("busy worker = %+v, want Busy=true DrainingSince=nil", got)
	}
	if got := byID[drainID.String()]; got.Busy || got.DrainingSince == nil || !got.DrainingSince.Equal(drainingAt) {
		t.Fatalf("draining worker = %+v, want Busy=false DrainingSince=%v", got, drainingAt)
	}
}

// The disk_pressure/ephemeral poll fields (PRD #837 M4) must round-trip from the store
// row into the DesiredWorker DTO the controller reads. The two workers carry
// discriminating values (pressured-not-ephemeral vs ephemeral-not-pressured) so a swapped
// or dropped field is caught here, not just by the wire golden. The SQL DERIVATION of
// disk_pressure (streak>=2 AND fresh) is a live-DB concern; this test pins only the Go
// mapping, so the fake sets the already-derived bool on the row.
func TestPollCarriesDiskPressureAndEphemeral(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)

	pressuredID := uuid.New()
	ephemeralID := uuid.New()
	st.workers = append(st.workers,
		store.ListHostedWorkersForControllerRow{
			ID:               pressuredID,
			TemplateDeclared: pgtype.Text{String: "base", Valid: true},
			HostedSize:       pgtype.Text{String: "s", Valid: true},
			HostedGeneration: 1,
			DiskPressure:     true,
			Ephemeral:        false,
		},
		store.ListHostedWorkersForControllerRow{
			ID:               ephemeralID,
			TemplateDeclared: pgtype.Text{String: "jvm", Valid: true},
			HostedSize:       pgtype.Text{String: "l", Valid: true},
			HostedGeneration: 2,
			DiskPressure:     false,
			Ephemeral:        true,
		},
	)

	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	byID := map[string]DesiredWorker{}
	for _, w := range resp.Workers {
		byID[w.ID] = w
	}
	if got := byID[pressuredID.String()]; !got.DiskPressure || got.Ephemeral {
		t.Fatalf("pressured worker = %+v, want DiskPressure=true Ephemeral=false", got)
	}
	if got := byID[ephemeralID.String()]; got.DiskPressure || !got.Ephemeral {
		t.Fatalf("ephemeral worker = %+v, want DiskPressure=false Ephemeral=true", got)
	}
}

// A registration for a worker with no token row at all deletes nothing and must not
// error — the worker still registered successfully.
func TestNoteRegisteredForWorkerWithNoRowIsNotAnError(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	if err := svc.NoteRegistered(context.Background(), uuid.New(), hashOf("uzw_whatever")); err != nil {
		t.Fatalf("NoteRegistered: %v", err)
	}
}

// The AAD binds a ciphertext to one worker id: an operator who moves a sealed token
// onto another worker's row gets a decrypt failure, not a working token.
func TestSealedTokenIsBoundToItsWorker(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	victim, attacker := uuid.New(), uuid.New()
	st.addWorker(attacker, "base", "s", 1)
	seal(t, st, box, victim, "uzw_victims-token")
	// Lift the victim's ciphertext onto the attacker's row.
	st.rows[attacker] = &tokenRow{ciphertext: st.rows[victim].ciphertext}

	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Workers[0].JoinToken != nil {
		t.Fatal("a ciphertext sealed for another worker must not open")
	}
}

// One unopenable token (a rotated master key, a tampered row) must not blank the
// whole fleet's desired state — the other workers still need reconciling, and the
// affected one still needs its Deployment.
func TestPollSurvivesAnUnopenableToken(t *testing.T) {
	st := newFakeStore()
	box := newTestBox(t)
	svc := New(st, box, time.Now, time.Hour)
	broken := newTestWorker(st, "base", "s", 1)
	fine := newTestWorker(st, "jvm", "l", 2)
	st.rows[broken] = &tokenRow{ciphertext: []byte("not a valid ciphertext at all")}
	seal(t, st, box, fine, "uzw_good")

	resp, err := svc.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Workers) != 2 {
		t.Fatalf("%d workers, want both", len(resp.Workers))
	}
	if resp.Workers[0].JoinToken != nil {
		t.Fatal("the unopenable token must be reported as absent, not as garbage")
	}
	if resp.Workers[1].JoinToken == nil || *resp.Workers[1].JoinToken != "uzw_good" {
		t.Fatal("the healthy worker's token must still be delivered")
	}
}

// Store failures surface; they never present as an empty fleet, which the
// controller would read as "delete everything".
func TestPollPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("db exploded")
	st := newFakeStore()
	st.listErr = boom
	if _, err := newTestService(t, st).Poll(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("list error: err = %v, want it propagated", err)
	}
}

// NoteRegistered surfaces its store error so the handler can log it — the handler
// is what decides it is non-fatal, not this layer.
func TestNoteRegisteredPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("db exploded")
	st := newFakeStore()
	st.markErr = boom
	if err := newTestService(t, st).NoteRegistered(context.Background(), uuid.New(), hashOf("uzw_x")); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it propagated", err)
	}
}

// An empty fleet must marshal as [] rather than null: the controller reads the
// desired set as authoritative, and `null` is the shape most likely to be
// mishandled into "no opinion" by a future consumer.
func TestPollReturnsEmptySliceNotNil(t *testing.T) {
	resp, err := newTestService(t, newFakeStore()).Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Workers == nil {
		t.Fatal("Workers is nil; want an empty slice")
	}
}

// Cordon returns found=true and passes the id through when a hosted worker exists.
func TestCordonMarksAKnownWorkerDraining(t *testing.T) {
	st := newFakeStore()
	id := newTestWorker(st, "base", "m", 0)
	found, err := newTestService(t, st).Cordon(context.Background(), id)
	if err != nil {
		t.Fatalf("cordon: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true for a known hosted worker")
	}
	if len(st.cordoned) != 1 || st.cordoned[0] != id {
		t.Fatalf("cordoned = %v, want exactly [%s]", st.cordoned, id)
	}
}

// Cordon returns found=false (no error) for an unknown worker: the handler answers 404.
func TestCordonUnknownWorkerReturnsNotFound(t *testing.T) {
	st := newFakeStore()
	found, err := newTestService(t, st).Cordon(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("cordon: %v", err)
	}
	if found {
		t.Fatal("found = true, want false for an unknown worker")
	}
}

// Cordon surfaces its store error rather than reporting a spurious not-found.
func TestCordonPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("db exploded")
	st := newFakeStore()
	st.cordonErr = boom
	if _, err := newTestService(t, st).Cordon(context.Background(), uuid.New()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it propagated", err)
	}
}

// Uncordon returns found=true, passes the id through, and clears draining_since when a
// hosted worker that was cordoned exists (issue #458 reverted-drift recovery).
func TestUncordonClearsAKnownDrainingWorker(t *testing.T) {
	st := newFakeStore()
	id := newTestWorker(st, "base", "m", 0)
	// Seed it as draining, the state a reverted-drift recovery has to undo.
	st.workers[0].DrainingSince = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	found, err := newTestService(t, st).Uncordon(context.Background(), id)
	if err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true for a known hosted worker")
	}
	if len(st.uncordoned) != 1 || st.uncordoned[0] != id {
		t.Fatalf("uncordoned = %v, want exactly [%s]", st.uncordoned, id)
	}
	if st.workers[0].DrainingSince.Valid {
		t.Fatal("DrainingSince still valid, want cleared")
	}
}

// Uncordon returns found=false (no error) for an unknown worker: the handler answers 404.
func TestUncordonUnknownWorkerReturnsNotFound(t *testing.T) {
	st := newFakeStore()
	found, err := newTestService(t, st).Uncordon(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if found {
		t.Fatal("found = true, want false for an unknown worker")
	}
}

// Uncordon surfaces its store error rather than reporting a spurious not-found.
func TestUncordonPropagatesStoreErrors(t *testing.T) {
	boom := errors.New("db exploded")
	st := newFakeStore()
	st.uncordonErr = boom
	if _, err := newTestService(t, st).Uncordon(context.Background(), uuid.New()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it propagated", err)
	}
}
