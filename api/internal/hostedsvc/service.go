package hostedsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Store is the narrow store dependency of the controller protocol.
// *store.Queries satisfies it, and so does a *store.Queries bound to a
// transaction via WithTx — which is what the provision path (M2) needs in order
// to insert a worker row and park its sealed token atomically.
type Store interface {
	ListHostedWorkersForController(ctx context.Context) ([]store.ListHostedWorkersForControllerRow, error)
	CreateHostedWorkerToken(ctx context.Context, arg store.CreateHostedWorkerTokenParams) error
	DeleteHostedWorkerToken(ctx context.Context, workerID uuid.UUID) (int64, error)
}

// ErrBadWorkerID is returned by Poll when an ack carries something that is not a
// uuid. Surfaced as a 400: a controller that cannot form a uuid is broken or is
// not our controller, and either way the api should say so rather than quietly
// skip the entry and let a token linger sealed forever.
var ErrBadWorkerID = errors.New("hostedsvc: malformed worker id")

// Service is the api's half of the controller protocol.
type Service struct {
	q Store
	// box seals the pending join-token plaintext. Same master key (UZI_SECRET_KEY)
	// as every other secret at rest; rotating it invalidates pending tokens, which
	// for a token that lives seconds-to-a-poll is a non-event.
	box *secretbox.Box
}

// New constructs a Service.
func New(q Store, box *secretbox.Box) *Service { return &Service{q: q, box: box} }

// tokenAAD is the additional authenticated data every pending join token is
// sealed under: it binds the ciphertext to one worker id, so a DB-write operator
// cannot lift a sealed token onto a different worker's row and have it open. The
// domain prefix keeps these ciphertexts from being interchangeable with any other
// secret sealed by the same master key.
func tokenAAD(workerID uuid.UUID) []byte {
	return []byte("hosted_worker_join_token|" + workerID.String())
}

// SealJoinToken parks a freshly minted join token's plaintext for the controller
// to collect on its next poll. The provision path (M2) calls it inside the same
// transaction that inserts the worker row, so a hosted worker can never exist
// with a token_hash whose plaintext was never queued for anyone.
//
// The plaintext is the caller's to hold only for the moment it takes to seal it:
// nothing here logs it, and the api never reads it back except to answer a poll.
func (s *Service) SealJoinToken(ctx context.Context, workerID uuid.UUID, token string) error {
	sealed, err := s.box.SealWithAAD([]byte(token), tokenAAD(workerID))
	if err != nil {
		return fmt.Errorf("hostedsvc: seal join token: %w", err)
	}
	return s.q.CreateHostedWorkerToken(ctx, store.CreateHostedWorkerTokenParams{
		WorkerID:        workerID,
		TokenCiphertext: sealed,
	})
}

// Poll is the whole controller protocol: acknowledge, then report desired state.
//
// The ordering is load-bearing and is what makes the handoff safe. Acks are
// applied FIRST, so a worker the controller has just confirmed materialized has
// its sealed copy destroyed before this same response is computed — the token is
// never handed out again once its delivery is durable, and the response never
// both acks and re-delivers the same worker.
//
// Delivery is at-least-once against that ack, deliberately NOT the at-most-once
// reading of "delivered once" (Decision 3). Deleting the sealed copy the moment
// it is written to a response would mean a response lost to a controller crash,
// an api crash after commit, or a network partition destroys the only recoverable
// copy of a token whose hash is already committed to the workers row: the worker
// could never authenticate, the api could never tell it apart from one that simply
// had not started yet, and the user would be left with a permanently dead worker
// and no signal. So a pending token is re-delivered on every poll until the
// controller reports it materialized, and re-delivery is harmless because the
// controller's write of the k8s Secret is idempotent.
//
// Decision 3's actual security property is preserved exactly: the sealed plaintext
// is destroyed as soon as it has been consumed, and there is no reveal path. It is
// only the trigger that moves — from "a response was written" to "the cluster
// durably holds it", which is the earliest moment at which destroying it is
// non-destructive.
//
// After the ack, the worker's k8s Secret is the sole holder of the plaintext,
// which is Decision 3's documented residual (plaintext in etcd for the worker's
// lifetime). Deleting that Secret out of band strands the worker exactly as losing
// a hand-run worker's token does today; the recovery is the same one — delete and
// reprovision. Detecting it is the controller's drift/orphan pass (M3, Decision 9).
func (s *Service) Poll(ctx context.Context, req PollRequest) (PollResponse, error) {
	for _, raw := range req.Materialized {
		id, err := uuid.Parse(raw)
		if err != nil {
			return PollResponse{}, fmt.Errorf("%w: %q", ErrBadWorkerID, raw)
		}
		// A 0-row delete is the normal idempotent case (the controller re-acks a
		// worker it already acked, because its ack is derived from observed cluster
		// state and the Secret is still there). Nothing to report.
		if _, err := s.q.DeleteHostedWorkerToken(ctx, id); err != nil {
			return PollResponse{}, fmt.Errorf("hostedsvc: ack token delivery: %w", err)
		}
	}

	rows, err := s.q.ListHostedWorkersForController(ctx)
	if err != nil {
		return PollResponse{}, fmt.Errorf("hostedsvc: list hosted workers: %w", err)
	}
	out := PollResponse{Workers: make([]DesiredWorker, 0, len(rows))}
	for _, row := range rows {
		dw := DesiredWorker{
			ID:         row.ID.String(),
			Template:   row.TemplateDeclared.String,
			Size:       row.HostedSize.String,
			Generation: row.HostedGeneration,
		}
		if len(row.TokenCiphertext) > 0 {
			plain, err := s.box.OpenWithAAD(row.TokenCiphertext, tokenAAD(row.ID))
			if err != nil {
				// The master key was rotated out from under a pending token, or the row
				// was tampered with. Neither is recoverable here and neither is a reason
				// to fail the whole fleet's reconcile: report the rest of the desired
				// state and leave this worker tokenless, so its Deployment is still
				// reconciled and the stuck-offline worker is the visible symptom. Never
				// log the ciphertext.
				slog.Error("open pending hosted join token", "worker_id", row.ID, "error", err)
				out.Workers = append(out.Workers, dw)
				continue
			}
			token := string(plain)
			dw.JoinToken = &token
		}
		out.Workers = append(out.Workers, dw)
	}
	return out, nil
}
