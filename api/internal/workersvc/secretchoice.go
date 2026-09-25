package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claimCred is the concrete Anthropic credential ONE claim spends: the identity to
// record on the run, the label to snapshot alongside it, and the plaintext to ship
// in the claim payload. It exists because openAnthropic used to hand back only
// []byte — the credential was chosen and then forgotten, which is exactly why a run
// could never answer "which account paid for this?" (PRD #111 M1, D8).
//
// Token is secret bytes. Nothing may log this struct whole.
type claimCred struct {
	ID    uuid.UUID
	Label string
	Token []byte
}

// Selection reasons recorded in runs.anthropic_select_reason — the MODE that named
// the credential, which is what a user actually needs to read (D20: an auto pick and
// a default fallback can name the same token, and PRD #104's compatibility path
// creates a row labelled literally "default", so the label alone answers nothing).
//
// These are ALIASES, not a second definition. The whole ten-value vocabulary lives
// in autoselect (see Reason there for why it hosts even the non-auto three), and
// the SQL CHECK is the same ten (00089's eight, widened by 00233); these exist only so the claim path reads
// in its own idiom rather than saying string(autoselect.ReasonPinned) on every line.
// Aliasing means a rename upstream is a compile error here, which a second set of
// string literals would not be.
const (
	selectReasonDefault = string(autoselect.ReasonDefault)
	selectReasonPinned  = string(autoselect.ReasonPinned)
	selectReasonJudge   = string(autoselect.ReasonJudge)
	// PRD #1247 M1: the two per-run credential override reasons, same alias idiom as
	// the three above (a rename upstream is a compile error here). run_pinned = a per-run
	// override named a token; run_default = a per-run override of mode 'default'.
	selectReasonRunPinned  = string(autoselect.ReasonRunPinned)
	selectReasonRunDefault = string(autoselect.ReasonRunDefault)
)

// effectiveClaimModeUnknown is effectiveNextClaimMode's answer when the next claim's
// mode cannot be predicted — no recorded worker, or a worker the caller could not load
// (PRD #1247). It is deliberately NOT one of the three bind modes: 6e and the switch
// verb must treat it as "do not promote early", the safe direction.
const effectiveClaimModeUnknown = "unknown"

// secretChoice is WHICH credential a claim should spend and WHY: the override
// openAnthropic takes (nil ⇒ the owner's default), the reason to record, and the
// measured headroom when a reading produced the choice.
//
// It replaced a bare *uuid.UUID because M4 made the answer two-dimensional. An auto
// pick and a default fallback can name the SAME token, so an id alone can no longer
// say what happened — and the fallback reasons (pool_empty, pool_stale) are carried
// by a choice whose id is nil, i.e. by exactly the value that used to mean "nothing
// to say".
//
// headroom is a pointer because NULL is a real answer: only an auto pick has a
// measured headroom, and 0 is a legal one (a fully-consumed token picked
// best-of-pool), so a zero value cannot stand in for absence.
type secretChoice struct {
	secretID *uuid.UUID
	reason   string
	headroom *int16
}

// autoLaneRetryable reports whether a credential the AUTO lane resolved and then
// failed to open earns ONE floor-retry onto ANOTHER pooled token (D14, reshaped by
// #754 M2). It is the precise gate on that retry, and it covers the three auto-lane
// picks that have a pooled alternative to fall to:
//
//   - a selector pick (auto / best_of_pool), and
//   - a floor pick (pool_stale) — #754 made the floor a real pooled spend, so a
//     floored token that will not open must ALSO get one retry onto the next pooled
//     token rather than dying terminally on the first undecryptable row.
//
// It deliberately EXCLUDES open_failed, which is the reason autoFloorRetry itself
// records: a second open failure therefore fails this gate on its REASON conjunct and
// is terminal by STRUCTURE — no counter, and no dependency on an invariant enforced
// three files away. It also excludes pinned / default / judge: the user named those
// credentials, and silently billing a different one is the R4 failure this PRD is
// otherwise built to avoid. The nil-secretID guard keeps the empty-pool hold
// (errAutoPoolEmpty, which never reaches an open) out of the retry entirely.
func (c secretChoice) autoLaneRetryable() bool {
	return c.secretID != nil &&
		(c.reason == string(autoselect.ReasonAuto) ||
			c.reason == string(autoselect.ReasonBestOfPool) ||
			c.reason == string(autoselect.ReasonPoolStale))
}

// staticChoice names the mode that produced a claim's secretID override for the two
// non-auto resolutions. It takes the OVERRIDE rather than the resolved credential on
// purpose: after resolution both cases are just an id, and "the owner's default" and
// "a binding that happens to name the default token" are different facts that a user
// reading the run view needs told apart.
//
// bound is the reason to use when the override is set — selectReasonPinned for a
// worker binding, selectReasonJudge for the judge lane. An UNSET override is
// selectReasonDefault either way, and that asymmetry is correct: a judge lane with no
// binding really did spend the owner's default, and saying "judge" would claim a
// binding chose it.
func staticChoice(secretID *uuid.UUID, bound string) secretChoice {
	if secretID == nil {
		return secretChoice{reason: selectReasonDefault}
	}
	return secretChoice{secretID: secretID, reason: bound}
}

// openAnthropic resolves AND opens the Anthropic credential for one run — the one
// secret the run lane, the judge lane and the chat lane all deliver, and the ONE
// place credential resolution happens. The vault-dispatch logic (dek needs unlock,
// legacy master opens regardless, nil vault → master box) lives in secretopen,
// shared with the rate-limit poller (PRD #53); this method maps its sentinels back
// to workersvc's domain errors, preserving the exact prior behavior: a lock
// surfaces as errVaultLocked (requeue, never fail), and a missing/undecryptable
// token as errCredentialUnavailable with its original failure-reason text (which
// never includes secret bytes).
//
// secretID is the binding-else-default seam (PRD #104 M1): nil resolves the user's
// default token, non-nil resolves that specific credential. The run lane passes a
// worker's anthropic_secret_id (M3) or the owner's judge binding for self_improve
// (M4), the judge lane its own, and the chat lane always nil (chat is deliberately
// not bindable, D5). Keeping every lane on this one function is what keeps
// resolution in one place instead of three copies drifting apart (R4). A bound id
// that is not the caller's is ErrNoSecret, i.e. errCredentialUnavailable, never
// another user's credential (D11).
//
// 🔴 IT NOW RESOLVES THE DEFAULT EXPLICITLY, AND THAT IS THE POINT (PRD #111 D8).
// The nil case used to hand the whole job to secretopen.Open, which resolves
// "the user's default of this kind" INSIDE its ciphertext query and returns only
// plaintext — so there was no id for the caller to record, and a run could not name
// what it spent. Now the default is resolved to (id, label) first and the open
// always goes by id, which makes the recorded id provably the opened one.
//
// The resolution is equivalent, not merely similar: GetDefaultUserSecretMeta's
// predicate (user_id AND kind AND is_default) is character-identical to
// GetUserSecretCiphertext's, both match at most one row under 00077's partial unique
// index, and the open then reads that row's own sealed_with and kind for the DEK AAD.
// Same row, same crypto.
//
// The one thing that DOES change is a race window, and it is deliberate: between
// resolving the id and opening it the user could set a different default, and this
// run now opens the id it resolved rather than whatever the default became. That is
// D8's entire purpose — recorded id == opened id — and it is the safer of the two
// orderings, so do not "fix" it. The narrow cost, accepted knowingly: if the token is
// DELETED inside that window the open now fails (errCredentialUnavailable, a terminal
// run failure) where the single-statement form would have opened the new default.
// PRD #111 D14 adds a retry for the auto lane specifically; a run whose owner deletes
// the credential mid-claim failing is the same outcome as deleting it a moment
// earlier.
//
// Both metadata lookups are OWNER-SCOPED IN THEIR OWN PREDICATE, and that is not
// decoration: they run BEFORE the open, so an unscoped by-id lookup would put another
// user's label in hand at exactly the point M1 records it — a claim that then fails
// on the open, having already leaked what it was going to record.
//
// TWO ROUND TRIPS PER CLAIM, AND THAT IS SETTLED, NOT PENDING (PRD #111 A7, decided
// in M4). The obvious tightening is to project `label` from secretopen's ciphertext
// query so one read serves both, which would also make the label provably come from
// the row that was decrypted — D8's own argument, one level down. Declined, for two
// reasons that are worth writing down because the idea recurs:
//
//   - The provenance gain is nil, unlike D8's. D8 closed a real gap: the default was
//     resolved INSIDE the ciphertext query and no id ever escaped, so there was
//     nothing to record. Here both reads name the SAME id under the same predicate
//     on a primary key, so they cannot return different rows. All the projection
//     would buy is a label read microseconds later — and the label is a point-in-time
//     SNAPSHOT that a later rename deliberately does not update anyway (00086).
//   - The cost lands on the wrong package. secretopen is shared with the rate-limit
//     poller; widening its return type would ripple into usagepoller's TokenOpener
//     seam for the claim lane's convenience, and it would make a function that
//     currently returns only secret bytes return a struct mixing plaintext with
//     safe-to-log metadata — a second claimCred-shaped thing to never log whole.
//
// M4's auto lane does not change this arithmetic, which was the reason the decision
// waited: it is the same shape as M3's pinned lane, not a third case.
//
// 🔴 A CORRECTION TO HOW THAT WAS FIRST WRITTEN, because the clause argued against
// its own conclusion. It read "it arrives here with a label already in hand from the
// ranking query" — offered as the reason the second read is harmless, when that is
// exactly what would make it redundant. The ranking query does select the label, and
// autoselect.Outcome carried it as far as M5, where it was found to have no reader
// and was DELETED rather than wired up.
//
// The positive reason the second read is right, which the original clause never gave:
// this one is SAME-CALL. The label and the ciphertext come out of consecutive reads
// of one row inside this function, so a rename between the ranking query and the open
// cannot make the run name an account it did not bill. The ranking query's copy is
// older and belongs to a different call. Spending same-call provenance to save a
// primary-key lookup would invert D8 on precisely the lane where the SELECTOR, not
// the user, chose the credential.
func (s *Service) openAnthropic(ctx context.Context, userID uuid.UUID, secretID *uuid.UUID) (claimCred, error) {
	var meta struct {
		ID    uuid.UUID
		Label string
	}
	var err error
	if secretID != nil {
		var row store.GetUserSecretMetaByIDRow
		row, err = s.q.GetUserSecretMetaByID(ctx, store.GetUserSecretMetaByIDParams{
			ID:     *secretID,
			UserID: userID,
		})
		meta.ID, meta.Label = row.ID, row.Label
	} else {
		var row store.GetDefaultUserSecretMetaRow
		row, err = s.q.GetDefaultUserSecretMeta(ctx, store.GetDefaultUserSecretMetaParams{
			UserID: userID,
			Kind:   store.KindAnthropicToken,
		})
		meta.ID, meta.Label = row.ID, row.Label
	}
	if err != nil {
		// pgx.ErrNoRows here is "no such credential for this user", which is the
		// SAME fact secretopen.ErrNoSecret carried before and must keep producing
		// the identical failure-reason text: a token-less user's run has always
		// failed with this string, and it is read by e2e and handler assertions.
		// Anything else is a real lookup error, surfaced verbatim (no secret bytes).
		if errors.Is(err, pgx.ErrNoRows) {
			return claimCred{}, fmt.Errorf("%w: no Anthropic token configured for this user", errCredentialUnavailable)
		}
		return claimCred{}, fmt.Errorf("anthropic credential lookup: %w", err)
	}

	tok, err := secretopen.OpenByID(ctx, s.q, s.vlt, s.box, userID, meta.ID)
	switch {
	case err == nil:
		return claimCred{ID: meta.ID, Label: meta.Label, Token: tok}, nil
	case errors.Is(err, secretopen.ErrVaultLocked):
		return claimCred{}, errVaultLocked
	case errors.Is(err, secretopen.ErrNoSecret):
		return claimCred{}, fmt.Errorf("%w: no Anthropic token configured for this user", errCredentialUnavailable)
	case errors.Is(err, secretopen.ErrUndecryptable):
		return claimCred{}, fmt.Errorf("%w: Anthropic token could not be decrypted", errCredentialUnavailable)
	default:
		// A DB lookup/internal error, surfaced verbatim (carries no secret bytes).
		return claimCred{}, err
	}
}

// recordRunCredential persists WHICH credential a claim spent, on the run it was
// assembled for (PRD #111 M1). Called by all three lanes, always AFTER a successful
// open — an unopened credential was never spent and must never be recorded as if it
// were.
//
// A failure here fails the claim, deliberately. The alternative (log and carry on)
// would deliver a payload whose spend is attributable to nothing, which is the
// silent-wrong-attribution failure this milestone exists to remove; and it is not
// costly, because the run is 'claimed' with no payload delivered, which
// SweepClaimedNeverStarted already requeues at ClaimGrace. A 0-row result is the one
// case that is NOT an error: it means the run vanished under us (its forge
// connection cascade-deleted the repo → run), which every other claim-path reader
// treats as errRunVanished and drops.
func (s *Service) recordRunCredential(ctx context.Context, run store.Run, cred claimCred, choice secretChoice, emitSwitchMessage bool) (int32, error) {
	// The headroom recorded is the RAW headroom of the pick — what the user's own
	// meters show — never the in-flight-penalised rank, which is an internal ordering
	// key that appears nowhere else in the product. NULL for every non-auto lane,
	// because there is no reading behind those choices; NULL also on D14's retry,
	// where the credential actually spent is the fallback and the measured headroom
	// described the one that would not open.
	headroom := pgtype.Int2{}
	if choice.headroom != nil {
		headroom = pgtype.Int2{Int16: *choice.headroom, Valid: true}
	}
	// The RUN-LANE claim path (emitSwitchMessage) emits a 'credential_switch' run message on an
	// APPLIED token switch, ATOMICALLY with the credential write + epoch journal, so the message
	// and the journal can never disagree about which token this claim spends (PRD #1247 M9, D7/D14).
	// That needs a transaction; when none is wired — the chat/judge lanes (emitSwitchMessage=false),
	// or a degraded/test deployment with no pool — fall back to the two-write, no-message path,
	// byte-identical to the pre-M9 behaviour. The two-write path returns run.LastSeq unchanged
	// (its callers discard it).
	if !emitSwitchMessage || s.txBeginner == nil {
		if err := writeCredentialAndEpoch(ctx, s.q, run, cred, choice, headroom); err != nil {
			return 0, err
		}
		return run.LastSeq, nil
	}
	return s.recordRunCredentialTx(ctx, run, cred, choice, headroom)
}

// credentialWriter is the two-statement credential-record surface shared by the transactional
// (qtx: *store.Queries) and the non-transactional (s.q: workersvc Store) recordRunCredential
// paths, so the SetRunAnthropicSecret + RecordRunCredentialEpoch pair is written once and both
// paths cannot drift (PRD #1247 M9). Both satisfy it.
type credentialWriter interface {
	SetRunAnthropicSecret(ctx context.Context, arg store.SetRunAnthropicSecretParams) (int64, error)
	RecordRunCredentialEpoch(ctx context.Context, arg store.RecordRunCredentialEpochParams) error
}

// writeCredentialAndEpoch records WHICH credential a claim spent (SetRunAnthropicSecret) and
// appends the attribution-journal epoch for THIS claim (RecordRunCredentialEpoch), keyed by the
// run's existing claim_generation (00223, PRD #1349 — this PRD never increments it). The epoch is
// written AFTER SetRunAnthropicSecret confirmed a live run (n>0), so the FK to runs cannot fail on
// a vanished run and the epoch can never disagree with runs.anthropic_secret_id about which token
// this claim spent. Idempotent on the composite PK, so a claim retried at the same generation
// re-records rather than duplicating. A 0-row SetRunAnthropicSecret is errRunVanished (the run's
// forge connection cascade-deleted the repo → run), the one non-error 0-row case.
func writeCredentialAndEpoch(ctx context.Context, w credentialWriter, run store.Run, cred claimCred, choice secretChoice, headroom pgtype.Int2) error {
	n, err := w.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		AnthropicSecretID:     pgconv.UUID(cred.ID),
		AnthropicSecretLabel:  pgconv.TextOrNull(cred.Label),
		AnthropicSelectReason: pgconv.TextOrNull(choice.reason),
		AnthropicHeadroomPct:  headroom,
		ID:                    run.ID,
		UserID:                run.UserID,
	})
	if err != nil {
		return fmt.Errorf("record run anthropic credential: %w", err)
	}
	if n == 0 {
		return errRunVanished
	}
	if err := w.RecordRunCredentialEpoch(ctx, store.RecordRunCredentialEpochParams{
		RunID:           run.ID,
		ClaimGeneration: run.ClaimGeneration,
		SecretID:        pgconv.UUID(cred.ID),
		Label:           pgconv.TextOrNull(cred.Label),
		SelectReason:    pgconv.TextOrNull(choice.reason),
	}); err != nil {
		return fmt.Errorf("record run credential epoch: %w", err)
	}
	return nil
}

// credentialSwitchMessageKind is the run_messages.kind for the per-applied-switch feed message
// (PRD #1247 M9, task c). There is NO CHECK constraint on run_messages.kind (kinds like 'plan' /
// 'question' were added without a migration), so minting a new kind needs no migration. The fold
// filters kind IN ('status','error'), so this kind is invisible to usage folding by construction.
const credentialSwitchMessageKind = "credential_switch" //nolint:gosec // G101: a run_messages.kind label, not a credential

// maxSwitchSeqAttempts bounds the gapless seq-collision recovery loop (PRD #1247 M9, task c step
// 7). A worker's InsertRunMessage is NOT serialized by the run-row FOR UPDATE lock the switch
// transaction holds, so a worker frame can occupy the seq we try; one re-read + retry normally
// lands it (at most one competing frame at a time). The bound is generous defense against a
// pathological burst; on exhaustion the mandatory credential write still commits and only the
// best-effort message is skipped (see insertCredentialSwitchMessage).
//
// A var, not a const, only so the seq-collision regression test can lower it to force the loop to
// exhaust; production never reassigns it (default 32).
var maxSwitchSeqAttempts = 32

// credentialSwitchMessagePayload is the 'credential_switch' run message body (PRD #1247 M9, task
// c): enough for a downstream renderer to say "Switched to token <label> (<reason>)" and for the
// Slack resume DM to name the token. Label is a pointer so an unlabelled token serialises as JSON
// null rather than "".
type credentialSwitchMessagePayload struct {
	Label        *string `json:"label"`
	SelectReason string  `json:"select_reason"`
	SecretID     string  `json:"secret_id"`
}

// pendingSwitchBroadcast carries the just-inserted switch message out of the transaction so the
// caller can broadcast it AFTER commit (never before — a spare-slot reclaim must not see a message
// the tx might roll back). nil ⇒ no message was inserted (no switch, or an idempotent skip).
type pendingSwitchBroadcast struct {
	seq     int32
	payload json.RawMessage
}

// recordRunCredentialTx is recordRunCredential's transactional (run-lane claim) path (PRD #1247
// M9, task c). In ONE transaction it: locks the run row (FOR UPDATE) and re-validates the claim
// fence under the lock; writes the credential + epoch; detects an APPLIED token switch by the
// epoch delta; and, on a switch, emits an idempotent 'credential_switch' run message at a gapless
// seq and advances runs.last_seq to the seq it landed. It returns the (possibly bumped) last_seq —
// the caller sets ClaimPayload.LastSeq to it (NOT the stale run.LastSeq snapshot) so the worker
// resumes past the server-inserted message and never re-uses its seq. The switch message is
// broadcast AFTER commit, best-effort; a nil broadcaster is covered by REST replay (?after=<seq>).
func (s *Service) recordRunCredentialTx(ctx context.Context, run store.Run, cred claimCred, choice secretChoice, headroom pgtype.Int2) (int32, error) {
	gen := run.ClaimGeneration

	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("record run credential: begin tx: %w", err)
	}
	// A no-op after a successful Commit; on every early return it undoes the lock and any write.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	// Step 1: lock the run row and re-read the fence columns (generation, released, last_seq).
	locked, err := qtx.GetRunByIDForUpdate(ctx, run.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errRunVanished
		}
		return 0, fmt.Errorf("record run credential: lock run: %w", err)
	}
	// Step 2: fence re-validate UNDER THE LOCK. A raced release/reclaim — the claim was released
	// (claim_released_at set) or a reclaim bumped the generation past this claim's — makes this
	// assembly stale. Abort WITHOUT writing and treat it like the vanished-run path
	// (finishRunClaim drops errRunVanished to an idle no-op; the run is requeued and
	// re-claimed fresh). Defense in depth: assembleClaim runs synchronously right after ClaimRun,
	// so the fence normally holds.
	if locked.ClaimGeneration != gen || locked.ClaimReleasedAt.Valid {
		return 0, errRunVanished
	}

	// Steps 3+4: the credential write and the epoch journal, in the tx.
	if err := writeCredentialAndEpoch(ctx, qtx, run, cred, choice, headroom); err != nil {
		return 0, err
	}

	lastSeq := locked.LastSeq
	var broadcast *pendingSwitchBroadcast

	// Step 5: applied-switch detection (epoch delta). Belt-and-braces skip for self_improve (D10):
	// that lane follows the judge binding, so its epoch delta will not fire anyway.
	if run.Kind != runkind.SelfImprove {
		switched, err := priorEpochIsDifferentToken(ctx, qtx, run.ID, gen, cred.ID)
		if err != nil {
			return 0, err
		}
		if switched {
			// Steps 6+7+8: idempotent insert at a gapless seq + last_seq advance, all in the tx.
			// newLast is the last_seq this tx adopts even when no message was inserted (an idempotent
			// skip returns the locked high-water; exhaustion returns MAX(seq)).
			newLast, bc, err := s.insertCredentialSwitchMessage(ctx, qtx, run.ID, gen, cred, choice, locked.LastSeq)
			if err != nil {
				return 0, err
			}
			lastSeq = newLast
			if bc != nil {
				broadcast = bc
			}
		}
	}

	// Step 9: commit.
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("record run credential: commit: %w", err)
	}

	// AFTER commit only (never before): broadcast the new message to the run's WS channel. A nil
	// broadcaster (tests / a deployment without the WS hub) is covered by REST replay — last_seq
	// was advanced in the tx, so a client polling ?after=<prev> picks the message up.
	if broadcast != nil && s.bcast != nil {
		s.bcast.PublishMessage(run.ID, broadcast.seq, credentialSwitchMessageKind, "", "", "", broadcast.payload, s.now())
	}
	return lastSeq, nil
}

// priorEpochIsDifferentToken reports whether the run's epoch IMMEDIATELY BEFORE gen named a
// DIFFERENT, known token than the current claim spends — i.e. this claim is an APPLIED switch (PRD
// #1247 M9, task c step 5). No prior epoch (pgx.ErrNoRows) is a FIRST claim → not a switch. A
// prior epoch naming the SAME token is a same-token reclaim → not a switch. A prior epoch with no
// recorded secret (never happens for a successful claim) is treated conservatively as NOT a switch,
// so a message is never emitted on evidence that cannot name the prior token. Robust and decoupled
// from the #1422-deferred switch-stamp clear.
func priorEpochIsDifferentToken(ctx context.Context, q *store.Queries, runID uuid.UUID, gen int64, current uuid.UUID) (bool, error) {
	prior, err := q.GetPriorRunCredentialEpoch(ctx, store.GetPriorRunCredentialEpochParams{RunID: runID, ClaimGeneration: gen})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("record run credential: prior epoch: %w", err)
	}
	if !prior.SecretID.Valid {
		return false, nil
	}
	return uuid.UUID(prior.SecretID.Bytes) != current, nil
}

// insertCredentialSwitchMessage inserts the 'credential_switch' feed message for an applied switch
// and advances runs.last_seq, all inside the caller's transaction (PRD #1247 M9, task c steps
// 6/7/8). It returns the last_seq the transaction should adopt, plus the inserted message for the
// caller to broadcast after commit (nil broadcast ⇒ nothing was inserted — an idempotent skip, or
// the bounded seq-collision loop exhausted).
//
//   - Step 6 (idempotency): a 'credential_switch' message already recorded for this
//     (run_id, claim_generation) means a same-generation retry — skip, return startLastSeq with a
//     nil broadcast, no duplicate. startLastSeq is the locked high-water, which already reflects any
//     earlier committed advance.
//   - Step 7 (gapless seq): seq starts at last_seq+1; a worker frame may already occupy it (its
//     InsertRunMessage is not serialized by our run-row lock), so on the ON CONFLICT (Inserted
//     false) re-read MAX(seq) and retry at max+1, leaving no gap and losing no worker frame. Under
//     the lock the generation fence always holds, so Inserted false is only a seq collision. On a
//     successful insert, return that seq.
//   - Step 8: advance runs.last_seq (GREATEST) to the seq that actually landed, in the same tx.
//
// On exhaustion the mandatory credential write + epoch already committed in this tx, so only the
// best-effort message is skipped — but runs.last_seq is STILL advanced (GREATEST, gen-fenced) to
// MAX(seq) floored at startLastSeq, and that value is returned, so ClaimPayload.LastSeq is never
// below the true high-water even when the attribution message is skipped and the worker never
// resumes at a seq already present in run_messages.
func (s *Service) insertCredentialSwitchMessage(ctx context.Context, q *store.Queries, runID uuid.UUID, gen int64, cred claimCred, choice secretChoice, startLastSeq int32) (int32, *pendingSwitchBroadcast, error) {
	n, err := q.CountCredentialSwitchMessages(ctx, store.CountCredentialSwitchMessagesParams{RunID: runID, ClaimGeneration: gen})
	if err != nil {
		return 0, nil, fmt.Errorf("record run credential: idempotency check: %w", err)
	}
	if n > 0 {
		// Already emitted for this generation; no duplicate, no broadcast. Keep the locked
		// high-water (it already reflects any earlier committed advance).
		return startLastSeq, nil, nil
	}

	payload := credentialSwitchMessagePayload{SelectReason: choice.reason, SecretID: cred.ID.String()}
	if strings.TrimSpace(cred.Label) != "" {
		label := cred.Label
		payload.Label = &label
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("record run credential: marshal switch payload: %w", err)
	}

	seq := startLastSeq + 1
	for attempt := 0; attempt < maxSwitchSeqAttempts; attempt++ {
		res, err := q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID:           runID,
			Seq:             seq,
			Kind:            credentialSwitchMessageKind,
			Payload:         raw,
			ClaimGeneration: pgconv.Int8Ptr(&gen),
		})
		if err != nil {
			return 0, nil, fmt.Errorf("record run credential: insert switch message: %w", err)
		}
		if res.Inserted {
			if _, err := q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: runID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&gen)}); err != nil {
				return 0, nil, fmt.Errorf("record run credential: advance last_seq: %w", err)
			}
			return seq, &pendingSwitchBroadcast{seq: seq, payload: raw}, nil
		}
		// ON CONFLICT (run_id, seq): a worker frame beat us to this seq. Re-read the high-water
		// mark and retry at max+1 (no gap, no lost frame).
		maxSeq, err := q.MaxRunMessageSeq(ctx, runID)
		if err != nil {
			return 0, nil, fmt.Errorf("record run credential: re-read max seq: %w", err)
		}
		seq = maxSeq + 1
	}
	// Exhausted the bounded retry — extraordinarily unlikely. Do NOT fail the claim over a
	// best-effort attribution message: the mandatory credential write + epoch already committed in
	// this tx. Skip only the message, but still advance runs.last_seq to the true high-water so the
	// caller's ClaimPayload.LastSeq never sits below a seq already present in run_messages (a stale
	// last_seq would resume the worker onto an occupied seq).
	maxSeq, err := q.MaxRunMessageSeq(ctx, runID)
	if err != nil {
		return 0, nil, fmt.Errorf("record run credential: re-read max seq: %w", err)
	}
	newLast := maxSeq
	if newLast < startLastSeq {
		newLast = startLastSeq
	}
	if _, err := q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: runID, Seq: newLast, ClaimGeneration: pgconv.Int8Ptr(&gen)}); err != nil {
		return 0, nil, fmt.Errorf("record run credential: advance last_seq: %w", err)
	}
	slog.Warn("workersvc: credential switch message seq collision retry exhausted",
		"run_id", runID.String(), "generation", gen)
	return newLast, nil, nil
}

// claimSecretID resolves WHICH credential a run-lane claim spends, and is the one
// place that decision is made (R4: three copies of resolution drift, and a wrong
// fallback spends the wrong account silently).
//
//   - self_improve → the owner's JUDGE binding (PRD #104 M4). This branch is not
//     cosmetic and it is not automatic: a self_improve run is repo-ful and rides
//     the ordinary run lane, NOT assembleJudgeClaim, so without it "self-improve
//     follows the judge binding" would simply be false while appearing to be
//     handled. It belongs with the judge because it is uzi reviewing and improving
//     itself — the same activity the judge binding exists to bill separately —
//     not work the user asked a particular worker to do. It is checked FIRST, so a
//     worker's bind mode — auto included — never applies to it.
//   - everything else on this lane (issue, ci_fix) → the claiming worker's BIND
//     MODE decides.
//
// Judge runs never reach here; they fork to assembleJudgeClaim earlier.
func (s *Service) claimSecretID(ctx context.Context, wkr store.Worker, run store.Run) (secretChoice, error) {
	if run.Kind == runkind.SelfImprove {
		// Full judge resolution, including the `auto` pool ranker and D4's empty-pool
		// fallback (PRD #1140 M2): self_improve is uzi reviewing itself and follows the
		// judge's credential, not a worker's. The run-lane open-failed retry still wraps
		// this because self_improve rides assembleClaim (via openWithAutoRetry). Checked
		// FIRST, so the run override below can never apply to self_improve (D10: that lane
		// is not switchable — the verb refuses it rather than writing an override its
		// ladder would ignore).
		return s.judgeChoice(ctx, run)
	}
	// PRD #1247 M1 (D2): a per-run credential override outranks the worker binding for
	// THIS run — the user chose it for this run specifically. It sits BETWEEN the
	// self_improve/judge branch above and the worker bind-mode branch below. NULL mode
	// (every pre-feature run and every run created without a choice) and a `pinned` mode
	// whose id was nulled by a token delete both fall through to the worker binding
	// (inherit, D1), so behaviour is byte-identical to today when no override is set.
	if choice, ok, err := s.runOverrideChoice(ctx, run); err != nil {
		return secretChoice{}, err
	} else if ok {
		return choice, nil
	}
	if wkr.AnthropicBindMode == BindModeAuto {
		return s.autoChoice(ctx, run)
	}
	return staticChoice(workerSecretID(wkr), selectReasonPinned), nil
}

// runOverrideChoice resolves the per-run credential override rung of the ladder (PRD
// #1247 M1, D1/D2/D9). ok is false when there is NO override to apply and the caller
// must fall through to the worker binding: a NULL/empty mode, a `pinned` mode whose id
// was nulled by a token delete (D1, mirroring #104's worker-binding rule), or an
// unrecognised mode (impossible through the validator + the 00233 CHECK, resolved as
// inherit — the safe direction). ok is true when the override decides the credential:
//
//   - pinned + a live id → staticChoice(id, run_pinned), but ONLY after confirming the
//     id is the caller's own anthropic_token via the kind-scoped lookup. A foreign or
//     wrong-kind id resolves to errCredentialUnavailable (D9) rather than falling through
//     to inherit — inheriting would silently spend the worker's binding and misattribute.
//     openAnthropic then opens by id with its own owner-scoped read; the existing
//     worker/judge lanes keep their non-kind-scoped lookup unchanged.
//   - auto → the SAME autoChoice ranker the auto worker uses. errAutoPoolEmpty propagates
//     exactly as it does for an auto worker (the caller holds the run in pool_wait).
//   - default → an EXPLICITLY constructed choice with reason run_default. It cannot use
//     staticChoice(nil, …), which always reports `default`: run_default is a deliberate
//     per-run choice of the owner default, distinct from an unset binding, and D20 makes
//     the run view name that difference.
func (s *Service) runOverrideChoice(ctx context.Context, run store.Run) (secretChoice, bool, error) {
	if !run.CredentialOverrideMode.Valid || run.CredentialOverrideMode.String == "" {
		return secretChoice{}, false, nil
	}
	switch run.CredentialOverrideMode.String {
	case BindModePinned:
		if !run.CredentialOverrideSecretID.Valid {
			// The FK nulled the id when the token was deleted; the mode stays. Resolve
			// as inherit (D1), exactly the worker-pin rule for a deleted binding.
			return secretChoice{}, false, nil
		}
		id := uuid.UUID(run.CredentialOverrideSecretID.Bytes)
		// Kind-scoped resolution (D9): the override id must name the caller's OWN
		// anthropic_token. A foreign id, a deleted-but-not-nulled id, or a wrong-kind
		// id all return pgx.ErrNoRows here → errCredentialUnavailable, never another
		// lane's credential and never a silent inherit.
		if _, err := s.q.GetUserSecretMetaByIDOfKind(ctx, store.GetUserSecretMetaByIDOfKindParams{
			ID:     id,
			UserID: run.UserID,
			Kind:   store.KindAnthropicToken,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return secretChoice{}, true, fmt.Errorf("%w: run credential override is not an available Anthropic token", errCredentialUnavailable)
			}
			return secretChoice{}, true, fmt.Errorf("run credential override lookup: %w", err)
		}
		return staticChoice(&id, selectReasonRunPinned), true, nil
	case BindModeAuto:
		choice, err := s.autoChoice(ctx, run)
		if err != nil {
			return secretChoice{}, true, err
		}
		return choice, true, nil
	case BindModeDefault:
		// The owner default, chosen for THIS run. Built directly so the reason is
		// run_default rather than staticChoice's `default`.
		return secretChoice{reason: selectReasonRunDefault}, true, nil
	default:
		// Unrecognised mode (the CHECK and validator forbid it): inherit, the safe
		// direction — spending the worker binding is what a run did before overrides.
		return secretChoice{}, false, nil
	}
}

// effectiveNextClaimMode is the shared PURE policy for "which mode will resolve the
// run's NEXT claim" (PRD #1247, the one seam consulted by Decision 6e at park time, the
// duration-time pass, and the verb's D6 warnings). It is derived from the run + owner +
// worker rows, NEVER from anthropic_select_reason (which describes the PREVIOUS claim and
// goes stale the moment a binding changes while parked).
//
//   - self_improve → the owner's judge bind mode, resolved by the caller and passed in
//     (self_improve follows the judge binding, D10). The override never applies here.
//   - else the run override: pinned/auto/default, EXCEPT a `pinned` whose id was nulled
//     (inherit), which falls through to the worker.
//   - else the recorded worker_id's bind mode.
//   - a missing recorded worker, or a deleted worker (a zero-value worker row), → unknown.
//
// It has no production caller in M1 — M3 wires it into decideLimitPark — and is kept
// alive by the workersvc unit tests that exercise every rung.
func effectiveNextClaimMode(run store.Run, ownerJudgeMode string, worker store.Worker) string {
	if run.Kind == runkind.SelfImprove {
		return ownerJudgeMode
	}
	if run.CredentialOverrideMode.Valid && run.CredentialOverrideMode.String != "" {
		switch run.CredentialOverrideMode.String {
		case BindModePinned:
			if run.CredentialOverrideSecretID.Valid {
				return BindModePinned
			}
			// nulled pin → inherit; fall through to the worker binding below.
		case BindModeAuto:
			return BindModeAuto
		case BindModeDefault:
			return BindModeDefault
		}
	}
	// Inherit the recorded worker's bind mode. No recorded worker, or a worker the
	// caller could not load (zero-value row), is unknown — the next claim's mode cannot
	// be predicted, so 6e and the verb must not promote it early.
	if !run.WorkerID.Valid || worker.ID == uuid.Nil {
		return effectiveClaimModeUnknown
	}
	return worker.AnthropicBindMode
}

// ownerJudgeBindMode reads the run owner's judge-lane bind mode for
// effectiveNextClaimMode's self_improve rung (PRD #1247). It reuses judgeChoice's own
// GetUserJudgeAnthropicBinding read — the SAME query that resolves the credential at
// claim time — so the mode 6e and the D8 pass predict a self_improve resume against is
// exactly the one its next claim will use. Only a self_improve run needs it; every
// other kind passes "" without a read, since effectiveNextClaimMode ignores
// ownerJudgeMode for them. A lookup error is propagated (never swallowed into a wrong
// mode), matching judgeChoice.
func (s *Service) ownerJudgeBindMode(ctx context.Context, userID uuid.UUID) (string, error) {
	bound, err := s.q.GetUserJudgeAnthropicBinding(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("owner judge binding lookup: %w", err)
	}
	return bound.JudgeAnthropicBindMode, nil
}

// claimExcludeFor is claimExclude's pure core (PRD #1247 M3): the credential a resume
// must not re-pick, given only the run's dead-credential id and its retry cadence. It
// exists so a caller holding just those two columns (the D8 re-eval pass, the widened
// pool-wait resume) can ask the same question the full-row claimExclude asks, without
// materialising a whole store.Run. uuid.Nil means "exclude nothing".
func (s *Service) claimExcludeFor(limitDead pgtype.UUID, retryNotBefore pgtype.Timestamptz) uuid.UUID {
	if !limitDead.Valid {
		return uuid.Nil
	}
	// Window still closed → keep excluding. Relax (Nil) once retry_not_before has
	// reopened, and also when there is no reset stamp to wait on.
	if retryNotBefore.Valid && retryNotBefore.Time.After(s.now()) {
		return uuid.UUID(limitDead.Bytes)
	}
	return uuid.Nil
}

// claimExclude is the credential this claim must NOT resolve onto: the run's
// just-parked dead credential (PRD #217), but only WHILE it is not yet due to retry.
// retry_not_before is the run's retry CADENCE, not a proof the token's real Anthropic
// window has reopened — decideLimitPark can set it below the true reset (Decision 6e
// lowers it to a pooled alternative's availability; the report-less fallback is a
// 15m-doubling guess, limitwait.go). So once retry_not_before has passed — which is
// exactly what let PromoteLimitWaitRuns return the run to queued — the run is DUE for
// another attempt, and #754 M3 relaxes the exclusion so the resume re-picks or floors
// onto that very token instead of holding or switching accounts. That is how a
// single-pooled-token user "continues on cristi": each cadence it re-floors onto the
// token; if the window is genuinely still closed the worker re-parks with a fresh
// real-reset report, and the thrash converges in ~one cycle (bounded overall by
// RUN_LIMIT_MAX_WAITS, whose terminus is a failed run, not a mis-spend — the
// deliberate #754 tradeoff). A run with no dead credential (every non-resume claim)
// excludes nothing.
//
// The "still excluding" branch (a claimable run whose retry_not_before is still in the
// future) is DEFENSIVE: limit_wait's only production exit is PromoteLimitWaitRuns at
// retry_not_before <= now, so no normal resume reaches this function with a future
// stamp. It is kept because excluding is the safe answer should any future transition
// ever hand a not-yet-due run to the claim path, and the M2 exclusion tests inject a
// future stamp to exercise it.
func (s *Service) claimExclude(run store.Run) uuid.UUID {
	return s.claimExcludeFor(run.LimitDeadSecretID, run.RetryNotBefore)
}

// autoChoice runs the selector for an `auto` worker (PRD #111 M4, #754 M2).
//
// It is BEHIND claimSecretID, never beside it. PRD #104's R4 is that three copies of
// credential resolution drift and a wrong fallback spends the wrong account silently;
// keeping the selector under the one function that answers "which credential" means
// openAnthropic and assembleClaim never learn that auto exists.
//
// The three impure steps live here and nowhere else: the query, the clock, and the
// policy. autoselect.Select is pure, which is what lets the whole ranking be tested
// against hand-written fixtures with no database.
//
// A query error FAILS the claim rather than degrading to the owner default, and that
// is deliberate in the same way judgeChoice's is: "the database was unreachable for
// a moment" and "you have no pooled tokens" are different facts, and quietly treating
// the first as the second spends an account the user did not choose while raising
// nothing. The run is retried; a silent mis-spend is not retried, because nobody
// learns it happened.
//
// # The pooled-only invariant (#754)
//
// An auto worker NEVER spends a non-pooled credential — the owner default is NOT
// auto-eligible unless the user pooled it, and an auto lane that quietly bills it is
// the exact bug #754 fixes. That reshapes the old D7 fallback (which resolved
// workerSecretID(wkr) ⇒ the owner default) into a three-rung ladder, every rung of
// which stays inside the pool:
//
//   - Ranking exit (out.Picked): the selector named a measurable pooled token. Record
//     it with its reason and measured headroom. `exclude` (the run's just-parked
//     dead credential, PRD #217 M2) is passed to Select, which drops it from the
//     ranking so it can be neither picked nor the anchor.
//   - Floor (out not Picked, but a pooled token remains): the pool has tokens but none
//     is measurable — a measurable one would have been Picked as best_of_pool — so
//     autoselect.Floor spends the best pooled token anyway (stale/unmeasured
//     included), recorded as pool_stale with no headroom. Floor honours `exclude`
//     exactly as Select does, so the dead credential is never floored onto.
//   - Empty-pool hold (Floor.ok == false): there is genuinely nothing pooled to spend
//     — an empty pool, or the only pooled token is the excluded dead credential. Do NOT
//     spend the non-pooled default and do NOT hard-fail; signal errAutoPoolEmpty, which
//     finishRunClaim holds in the non-locking pool_wait status (PRD #754 M4) so
//     the run waits rather than billing an account the user did not pool.
//
// Floor.ok, not Select's PoolNonEmpty, decides floor-vs-hold: they diverge in the
// excluded-sole-token case (PoolNonEmpty counts before the exclude skip, Floor.ok
// after), and the credential we may actually spend NOW is Floor's question.
func (s *Service) autoChoice(ctx context.Context, run store.Run) (secretChoice, error) {
	userID := run.UserID
	// exclude comes from claimExclude (window-aware): the just-parked dead credential
	// while its window is still closed, uuid.Nil once retry_not_before has reopened it
	// (#754 M3 exclude-relax) or when there is no dead credential.
	exclude := s.claimExclude(run)

	rows, err := s.q.ListAutoSelectCandidates(ctx, userID)
	if err != nil {
		return secretChoice{}, fmt.Errorf("auto-select candidates: %w", err)
	}
	cands := make([]autoselect.Candidate, 0, len(rows))
	for _, row := range rows {
		cands = append(cands, autoselectrow.FromCandidateRow(row))
	}
	out := autoselect.Select(cands, exclude, s.p.Autoselect, s.now())
	if out.Picked {
		id := out.SecretID
		// The gauge is a SMALLINT 0..100 and headroom is derived from it by subtraction,
		// so the value is in range by construction and the narrowing cannot truncate.
		// runs.anthropic_headroom_pct carries a CHECK BETWEEN 0 AND 100 as the backstop.
		h := int16(out.Headroom) //nolint:gosec // G115: Headroom is 0..100 by construction (see comment above; DB CHECK 0..100), so the narrowing cannot truncate
		return secretChoice{secretID: &id, reason: string(out.Reason), headroom: &h}, nil
	}
	// NOT picked. The auto lane NEVER resolves the non-pooled owner default (#754).
	// Floor spends the best pooled token — always unmeasured here, since a measurable
	// one would have been Picked as best_of_pool — recorded as pool_stale, no headroom.
	if floorID, ok := autoselect.Floor(cands, exclude, s.now()); ok {
		id := floorID
		return secretChoice{secretID: &id, reason: string(autoselect.ReasonPoolStale)}, nil
	}
	// Genuinely nothing pooled to spend (empty pool, or the only pooled token is the
	// excluded dead credential). Do NOT spend the default and do NOT hard-fail — signal
	// an empty-pool hold that finishRunClaim holds in pool_wait (PRD #754 M4).
	return secretChoice{}, errAutoPoolEmpty
}

// autoFloorRetry recomputes an auto credential after a picked (or floored) token
// failed to open (D14, #754 M2). It re-lists the user's pooled candidates, drops
// failedID (the token that just would not decrypt), and floors over the rest — still
// honouring the run's just-parked dead credential — so an undecryptable pooled pick
// falls to ANOTHER pooled token, NEVER the non-pooled owner default.
//
// It records reason=open_failed, which fails autoLaneRetryable, so a second open
// failure is terminal by structure. Returns errCredentialUnavailable when no other
// pooled token is available (terminal; the caller fails the run rather than spending
// the default). A candidate-query error propagates as-is — a DB blip is not "the pool
// is empty", the same reasoning autoChoice uses.
func (s *Service) autoFloorRetry(ctx context.Context, run store.Run, failedID uuid.UUID) (secretChoice, error) {
	// exclude comes from claimExclude (window-aware): the just-parked dead credential
	// while its window is still closed, uuid.Nil once retry_not_before has reopened it
	// (#754 M3 exclude-relax) or when there is no dead credential.
	exclude := s.claimExclude(run)
	rows, err := s.q.ListAutoSelectCandidates(ctx, run.UserID)
	if err != nil {
		return secretChoice{}, fmt.Errorf("auto-floor-retry candidates: %w", err)
	}
	cands := make([]autoselect.Candidate, 0, len(rows))
	for _, row := range rows {
		c := autoselectrow.FromCandidateRow(row)
		if c.SecretID == failedID {
			continue // the pick that just failed to open must not be re-floored onto
		}
		cands = append(cands, c)
	}
	floorID, ok := autoselect.Floor(cands, exclude, s.now())
	if !ok {
		// No OTHER pooled token to spend — terminal. Never fall to the non-pooled
		// default (#754); finishRunClaim fails the run on errCredentialUnavailable.
		return secretChoice{}, fmt.Errorf("%w: no other pooled Anthropic token after open failure", errCredentialUnavailable)
	}
	id := floorID
	return secretChoice{secretID: &id, reason: string(autoselect.ReasonOpenFailed)}, nil
}

// workerSecretID is a worker's Anthropic binding as openAnthropic's override: nil
// means "the owner's default", which is what a nil resolves to downstream.
//
// The MODE is what decides, and the id is read in exactly one of the three
// (PRD #111 M3):
//
//   - pinned → the named credential. A NULL id here is D9: it resolves as default,
//     which is what this function already did for an unset binding before the mode
//     column existed, so the rule is kept true rather than newly invented. It is
//     also not a hypothetical — 00078's FK nulls the id when the token is deleted
//     and deliberately leaves the mode, so every pinned worker whose credential is
//     removed lands here.
//   - default → nil, and the id is NOT read. A stale id left behind by a mode
//     change therefore cannot leak into a claim.
//   - auto → nil FOR NOW. M3 ships the mode; M4 fills in this arm with the
//     selector. Until then an auto worker behaves exactly as a default one, which
//     is also the state auto degrades to when the pool is empty or stale (D7/R2) —
//     so the interim behaviour is a supported outcome of the finished feature, not
//     a placeholder that does something the design forbids.
//
// An unrecognised mode is impossible through the API (00088's CHECK and
// ValidBindMode both reject it) and resolves as default if one ever appears, which
// is the safe direction: spending the owner's default is what every worker did
// before any of this existed.
func workerSecretID(wkr store.Worker) *uuid.UUID {
	if wkr.AnthropicBindMode != BindModePinned {
		return nil
	}
	if !wkr.AnthropicSecretID.Valid {
		return nil
	}
	id := uuid.UUID(wkr.AnthropicSecretID.Bytes)
	return &id
}
