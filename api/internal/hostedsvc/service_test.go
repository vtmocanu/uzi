package hostedsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeStore is an in-memory Store: hosted worker rows plus the pending-token
// table, so the delivered-once handoff can be exercised across polls without a DB.
type fakeStore struct {
	workers []store.ListHostedWorkersForControllerRow
	// tokens is the hosted_worker_tokens table, keyed by worker id.
	tokens map[uuid.UUID][]byte
	// listErr / deleteErr force the failure paths.
	listErr   error
	deleteErr error
	// deletes counts MarkHostedWorkerTokenDelivered calls (including no-op re-acks).
	deletes int
	// delivered records which workers have been acked (the delivered_at stamp).
	delivered map[uuid.UUID]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{tokens: map[uuid.UUID][]byte{}, delivered: map[uuid.UUID]bool{}}
}

func (f *fakeStore) ListHostedWorkersForController(context.Context) ([]store.ListHostedWorkersForControllerRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Re-project the LEFT JOIN: a row's ciphertext is whatever the token table
	// currently holds, so an ack in the same call is reflected here.
	out := make([]store.ListHostedWorkersForControllerRow, 0, len(f.workers))
	for _, w := range f.workers {
		w.TokenCiphertext = f.tokens[w.ID]
		out = append(out, w)
	}
	return out, nil
}

// UpsertHostedWorkerToken mirrors the real ON CONFLICT DO UPDATE: a re-park
// replaces the ciphertext and resets delivery, which is the rotation path.
func (f *fakeStore) UpsertHostedWorkerToken(_ context.Context, arg store.UpsertHostedWorkerTokenParams) error {
	f.tokens[arg.WorkerID] = arg.TokenCiphertext
	delete(f.delivered, arg.WorkerID)
	return nil
}

// MarkHostedWorkerTokenDelivered mirrors the real UPDATE: the ciphertext is
// destroyed and delivered_at is stamped, so the row records that it WAS delivered
// rather than vanishing.
func (f *fakeStore) MarkHostedWorkerTokenDelivered(_ context.Context, workerID uuid.UUID) (int64, error) {
	f.deletes++
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	if _, ok := f.tokens[workerID]; !ok {
		return 0, nil
	}
	delete(f.tokens, workerID)
	f.delivered[workerID] = true
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

func newTestService(t *testing.T, st Store) *Service {
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
	return New(st, box)
}

// The happy path of the whole handoff, across the three polls a real worker's
// token lives through.
func TestPollDeliversTokenUntilAcked(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	id := uuid.New()
	st.addWorker(id, "base", "s", 1)

	if err := svc.SealJoinToken(context.Background(), id, "uzw_the-plaintext"); err != nil {
		t.Fatalf("SealJoinToken: %v", err)
	}
	// The sealed copy must not be the plaintext sitting in a bytea.
	if string(st.tokens[id]) == "uzw_the-plaintext" {
		t.Fatal("join token was stored in plaintext")
	}

	// Poll 1: the controller has observed nothing, so the token is delivered.
	resp, err := svc.Poll(context.Background(), PollRequest{})
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

	// Poll 2: still unacked (the controller crashed before writing the Secret), so
	// the SAME token is re-delivered. This is the at-least-once property: a lost
	// response must never be the end of the only recoverable copy.
	resp, err = svc.Poll(context.Background(), PollRequest{})
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if resp.Workers[0].JoinToken == nil || *resp.Workers[0].JoinToken != "uzw_the-plaintext" {
		t.Fatal("poll 2: an unacked token must be re-delivered")
	}

	// Poll 3: the controller acks. The sealed copy is destroyed, and this very
	// response must already reflect that — acks are applied before desired state is
	// computed, so one response can never both ack and re-deliver.
	resp, err = svc.Poll(context.Background(), PollRequest{Materialized: []string{id.String()}})
	if err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	if resp.Workers[0].JoinToken != nil {
		t.Fatal("poll 3: the acked token must not be delivered again in the acking response")
	}
	if _, exists := st.tokens[id]; exists {
		t.Fatal("poll 3: the sealed copy must be deleted on ack")
	}

	// Poll 4: the worker is still desired state, just tokenless forever after.
	resp, err = svc.Poll(context.Background(), PollRequest{})
	if err != nil {
		t.Fatalf("poll 4: %v", err)
	}
	if len(resp.Workers) != 1 || resp.Workers[0].JoinToken != nil {
		t.Fatalf("poll 4: worker = %+v, want it still desired with no token", resp.Workers[0])
	}
}

// A re-ack is the normal case, not an error: the controller derives its acks from
// what it observes in the cluster, and the Secret stays there for the worker's
// life, so it re-acks every single poll forever.
func TestPollReAckIsIdempotent(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	id := uuid.New()
	st.addWorker(id, "base", "m", 3)
	if err := svc.SealJoinToken(context.Background(), id, "uzw_x"); err != nil {
		t.Fatalf("SealJoinToken: %v", err)
	}

	for i := range 3 {
		resp, err := svc.Poll(context.Background(), PollRequest{Materialized: []string{id.String()}})
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		if resp.Workers[0].JoinToken != nil {
			t.Fatalf("poll %d: token delivered after an ack", i)
		}
	}
	if st.deletes != 3 {
		t.Fatalf("deletes = %d, want 3 (every re-ack reaches the store)", st.deletes)
	}
}

// An ack for a worker that never had a pending token (or was deleted mid-flight)
// deletes nothing and must not fail the cycle — the rest of the fleet still needs
// reconciling.
func TestPollAckForUnknownWorkerIsNotAnError(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	id := uuid.New()
	st.addWorker(id, "base", "s", 1)

	resp, err := svc.Poll(context.Background(), PollRequest{Materialized: []string{uuid.New().String()}})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(resp.Workers) != 1 {
		t.Fatalf("%d workers, want the fleet reported regardless", len(resp.Workers))
	}
}

// A malformed ack is a broken or foreign controller. Fail loudly rather than skip
// the entry — silently ignoring it would leave that worker's token sealed forever
// with no signal anywhere.
func TestPollRejectsMalformedAck(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Poll(context.Background(), PollRequest{Materialized: []string{"not-a-uuid"}})
	if !errors.Is(err, ErrBadWorkerID) {
		t.Fatalf("err = %v, want ErrBadWorkerID", err)
	}
}

// The AAD binds a ciphertext to one worker id: an operator who moves a sealed
// token onto another worker's row gets a decrypt failure, not a working token.
func TestSealedTokenIsBoundToItsWorker(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	victim, attacker := uuid.New(), uuid.New()
	st.addWorker(attacker, "base", "s", 1)
	if err := svc.SealJoinToken(context.Background(), victim, "uzw_victims-token"); err != nil {
		t.Fatalf("SealJoinToken: %v", err)
	}
	// Lift the victim's ciphertext onto the attacker's row.
	st.tokens[attacker] = st.tokens[victim]

	resp, err := svc.Poll(context.Background(), PollRequest{})
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
	svc := newTestService(t, st)
	broken, fine := uuid.New(), uuid.New()
	st.addWorker(broken, "base", "s", 1)
	st.addWorker(fine, "jvm", "l", 2)
	st.tokens[broken] = []byte("not a valid ciphertext at all")
	if err := svc.SealJoinToken(context.Background(), fine, "uzw_good"); err != nil {
		t.Fatalf("SealJoinToken: %v", err)
	}

	resp, err := svc.Poll(context.Background(), PollRequest{})
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
	if _, err := newTestService(t, st).Poll(context.Background(), PollRequest{}); !errors.Is(err, boom) {
		t.Fatalf("list error: err = %v, want it propagated", err)
	}

	st = newFakeStore()
	st.deleteErr = boom
	_, err := newTestService(t, st).Poll(context.Background(), PollRequest{Materialized: []string{uuid.New().String()}})
	if !errors.Is(err, boom) {
		t.Fatalf("delete error: err = %v, want it propagated", err)
	}
}

// An empty fleet must marshal as [] rather than null: the controller reads the
// desired set as authoritative, and `null` is the shape most likely to be
// mishandled into "no opinion" by a future consumer.
func TestPollReturnsEmptySliceNotNil(t *testing.T) {
	resp, err := newTestService(t, newFakeStore()).Poll(context.Background(), PollRequest{})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Workers == nil {
		t.Fatal("Workers is nil; want an empty slice")
	}
}
