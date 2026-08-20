package hostedsvc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Store is the narrow store dependency of the controller protocol.
// *store.Queries satisfies it, and so does a *store.Queries bound to a
// transaction via WithTx — which is what the provision path (M2) needs in order
// to insert a worker row and park its sealed token atomically.
type Store interface {
	ListHostedWorkersForController(ctx context.Context) ([]store.ListHostedWorkersForControllerRow, error)
	UpsertHostedWorkerToken(ctx context.Context, arg store.UpsertHostedWorkerTokenParams) error
	MarkHostedWorkerTokenDelivered(ctx context.Context, arg store.MarkHostedWorkerTokenDeliveredParams) (int64, error)
	CordonHostedWorker(ctx context.Context, id uuid.UUID) (int64, error)
}

// ExpiryStore is the slice of the store the pending-token expiry sweep needs. It
// is separate from Store, and deliberately so: the sweep must run whether or not
// WORKER_HOSTING_ENABLED is set (a stack that provisioned hosted workers and then
// disabled hosting is exactly the case that would otherwise strand sealed
// ciphertext at rest forever), so it is wired independently of the Service, which
// exists only when hosting is on.
type ExpiryStore interface {
	ExpirePendingHostedWorkerTokens(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)
}

// Service is the api's half of the controller protocol.
type Service struct {
	q Store
	// box seals the pending join-token plaintext. Same master key (UZI_SECRET_KEY)
	// as every other secret at rest; rotating it invalidates pending tokens, which
	// for a buffer that lives minutes is a non-event.
	box *secretbox.Box
}

// New constructs a Service.
func New(q Store, box *secretbox.Box) *Service { return &Service{q: q, box: box} }

// tokenAAD is the additional authenticated data every pending join token is
// sealed under (secretbox.SealWithAAD, whose doc names exactly this use: "so a
// DB-write operator cannot swap a ciphertext onto a different owner/kind and have
// it still authenticate").
//
// It binds worker_id || kind. Without it, an operator who can write the DB moves
// worker A's sealed token onto worker B's row, the controller faithfully delivers
// A's token into B's pod, and B's owner registers as A — claiming A's runs and
// receiving A's decrypted forge PAT and Anthropic token in the claim response.
// The kind is pinned in the AAD, not just the row, so a ciphertext can never be
// replayed across a future token kind sealed by the same master key.
func tokenAAD(workerID uuid.UUID) []byte {
	return []byte("hosted_worker_join_token|" + workerID.String() + "|hosted")
}

// SealJoinToken parks a freshly minted join token's plaintext for the controller
// to collect on its next poll. The provision path (M2) calls it inside the same
// transaction that inserts the worker row, so a hosted worker can never exist with
// a token_hash whose plaintext was never queued for anyone. It is also the
// ROTATION path: re-parking replaces the ciphertext and resets delivery, so a
// stranded worker recovers with a NEW token, never a resurrected one.
//
// A free function over Store rather than a *Service method, because this repo's
// transaction idiom binds the queries at the CALL SITE: the handler begins the tx
// and passes `qtx := h.q.WithTx(tx)` (uniform across handler/template_allocations.go,
// skill_allocations.go, auth.go, oidc.go, agent_templates.go, settings.go). A method
// whose store was fixed at construction could not join M2's provision transaction at
// all. ExpirePendingTokens below has the same shape for the same reason.
//
// The plaintext is the caller's to hold only for the moment it takes to seal it:
// nothing here logs it, and the api never reads it back except to answer a poll.
func SealJoinToken(ctx context.Context, q Store, box *secretbox.Box, workerID uuid.UUID, token string) error {
	sealed, err := box.SealWithAAD([]byte(token), tokenAAD(workerID))
	if err != nil {
		return fmt.Errorf("hostedsvc: seal join token: %w", err)
	}
	return q.UpsertHostedWorkerToken(ctx, store.UpsertHostedWorkerTokenParams{
		WorkerID:        workerID,
		TokenCiphertext: sealed,
	})
}

// NoteRegistered records that a hosted worker has PROVED it holds its join token,
// and destroys the api's sealed copy. It is the delivery acknowledgement, and the
// whole of it.
//
// The proof is the registration itself: RequireWorker resolved this worker by
// looking up sha256(whatever the caller presented) against workers.token_hash, so
// reaching here means a pod is holding the CURRENT token and using it successfully.
// Unforgeable, and version-exact — after a rotation to T2 a pod still holding T1
// fails auth outright.
//
// provedTokenHash is that proof, carried into the write. It MUST be the hash from
// the AUTHENTICATED CONTEXT (mw.WorkerFromContext), never a post-register re-read of
// the worker row: a DB round trip separates auth from this call, so an unqualified
// destroy would blow away a token parked by a rotation that committed in the gap —
// the original defect, one layer down. The statement re-checks the hash is still
// current, so a mid-flight rotation matches zero rows and leaves T2 pending for the
// pod that actually holds it.
//
// This whole mechanism replaced a controller-supplied ack, which could only assert
// "a Secret exists for worker W", never which token was in it. Deriving delivery
// from auth removes the controller from the token-destruction path entirely — the
// api decides from its own auth result rather than from a report by the component
// this PRD exists to bound.
//
// Callers MUST treat a failure as non-fatal: this is cleanup, and a worker that
// has successfully registered must never be failed because a buffer could not be
// cleared. The TTL sweep is the backstop.
func (s *Service) NoteRegistered(ctx context.Context, workerID uuid.UUID, provedTokenHash []byte) error {
	// The guard inside MarkHostedWorkerTokenDelivered makes a re-registration (a pod
	// rescheduled onto another node presents the same token again) a no-op rather
	// than a re-stamp.
	if _, err := s.q.MarkHostedWorkerTokenDelivered(ctx, store.MarkHostedWorkerTokenDeliveredParams{
		WorkerID:        workerID,
		ProvedTokenHash: provedTokenHash,
	}); err != nil {
		return fmt.Errorf("hostedsvc: note registered: %w", err)
	}
	return nil
}

// ExpirePendingTokens clears every sealed join token that has sat undelivered for
// longer than ttl, and reports how many it cleared. It is the sweep half of the
// at-rest bound documented on ExpirePendingHostedWorkerTokens.
//
// A package-level function over ExpiryStore, not a Service method, because it must
// run with hosting DISABLED — a stack that provisioned hosted workers and then
// flipped WORKER_HOSTING_ENABLED off is precisely the case that would otherwise
// leave master-key-sealed plaintext in Postgres forever, and no *Service exists in
// that state to hang a method on.
//
// ttl <= 0 disables the sweep; the caller decides, and main warns.
func ExpirePendingTokens(ctx context.Context, q ExpiryStore, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		return 0, nil
	}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-ttl), Valid: true}
	n, err := q.ExpirePendingHostedWorkerTokens(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("hostedsvc: expire pending tokens: %w", err)
	}
	return n, nil
}

// Poll returns the desired state of the whole hosted fleet.
//
// It is a pure read: the controller asserts nothing and the api destroys nothing
// here. Delivery is settled by NoteRegistered, on the worker's own authenticated
// registration — see protocol.go's header for why a controller ack was removed
// rather than refined.
//
// A worker whose token is still pending gets its plaintext. One whose pod already
// proved it holds the token, or whose buffer expired unread, gets a nil JoinToken
// and is reconciled without one.
func (s *Service) Poll(ctx context.Context) (PollResponse, error) {
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
			// pgtype.Bool zero-value is {Bool:false, Valid:false}, so a NULL column
			// reads as false here — exactly the "no sidecar" the controller wants. No
			// need to branch on Valid.
			Docker: row.DockerEnabled.Bool,
			// busy/draining feed the controller's cordon/defer-roll decision (PRD #422 M3).
			// Both are computed as SQL booleans in ListHostedWorkersForController, so they
			// arrive as plain Go bools here.
			Busy:     row.Busy,
			Draining: row.Draining,
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

// Cordon marks a hosted worker draining (PRD #422 M4, Decision 8). It is the api
// end of the controller's cordon-write control channel: on spec-hash drift the
// controller cordons a BUSY worker instead of hard-killing it, so the claim gate
// idles it, it finishes its in-flight runs, and the controller rolls it once idle.
//
// Unlike Poll (a pure read) and NoteRegistered (settled by proof of possession),
// this MUTATES desired state, which is why it rides a distinct authenticated
// endpoint rather than being folded into the display-only status report.
//
// Idempotent: CordonHostedWorker COALESCEs draining_since, so a repeat cordon does
// not reset the drain-deadline clock. Returns found=false (no error) when no hosted
// worker has that id — the handler answers 404 — so a controller cordoning a worker
// the api has since dropped gets a clean, actionable negative rather than a 500.
func (s *Service) Cordon(ctx context.Context, workerID uuid.UUID) (bool, error) {
	rows, err := s.q.CordonHostedWorker(ctx, workerID)
	if err != nil {
		return false, fmt.Errorf("hostedsvc: cordon hosted worker: %w", err)
	}
	return rows > 0, nil
}
