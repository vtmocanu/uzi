package hostedsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

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
	// listErr / markErr force the failure paths.
	listErr error
	markErr error
	// markCalls counts MarkHostedWorkerTokenDelivered calls, including no-ops.
	markCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[uuid.UUID]*tokenRow{}, tokenHashes: map[uuid.UUID][]byte{}}
}

func (f *fakeStore) ListHostedWorkersForController(context.Context) ([]store.ListHostedWorkersForControllerRow, error) {
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
	return New(st, newTestBox(t))
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
	svc := New(st, box)
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
	svc := New(st, box)
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
	svc := New(st, box)
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
	svc := New(st, box)
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

// The busy/draining poll fields (PRD #422 M3) must round-trip from the store row into
// the DesiredWorker DTO the controller reads. The two workers carry discriminating
// values (busy-not-draining vs draining-not-busy) so a swapped or dropped field is
// caught here, not just by the wire golden.
func TestPollCarriesBusyAndDraining(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)

	busyID := uuid.New()
	drainID := uuid.New()
	st.workers = append(st.workers,
		store.ListHostedWorkersForControllerRow{
			ID:               busyID,
			TemplateDeclared: pgtype.Text{String: "base", Valid: true},
			HostedSize:       pgtype.Text{String: "s", Valid: true},
			HostedGeneration: 1,
			Busy:             true,
			Draining:         false,
		},
		store.ListHostedWorkersForControllerRow{
			ID:               drainID,
			TemplateDeclared: pgtype.Text{String: "jvm", Valid: true},
			HostedSize:       pgtype.Text{String: "l", Valid: true},
			HostedGeneration: 2,
			Busy:             false,
			Draining:         true,
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
	if got := byID[busyID.String()]; !got.Busy || got.Draining {
		t.Fatalf("busy worker = %+v, want Busy=true Draining=false", got)
	}
	if got := byID[drainID.String()]; got.Busy || !got.Draining {
		t.Fatalf("draining worker = %+v, want Busy=false Draining=true", got)
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
	svc := New(st, box)
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
	svc := New(st, box)
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
