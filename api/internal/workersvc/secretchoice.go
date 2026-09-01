package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
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
// These are ALIASES, not a second definition. The whole eight-value vocabulary lives
// in autoselect (see Reason there for why it hosts even the non-auto three), and
// migration 00089's CHECK is the same eight; these exist only so the claim path reads
// in its own idiom rather than saying string(autoselect.ReasonPinned) on every line.
// Aliasing means a rename upstream is a compile error here, which a second set of
// string literals would not be.
const (
	selectReasonDefault = string(autoselect.ReasonDefault)
	selectReasonPinned  = string(autoselect.ReasonPinned)
	selectReasonJudge   = string(autoselect.ReasonJudge)
)

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
func (s *Service) recordRunCredential(ctx context.Context, run store.Run, cred claimCred, choice secretChoice) error {
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
	n, err := s.q.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		AnthropicSecretID:     pgUUID(cred.ID),
		AnthropicSecretLabel:  pgText(cred.Label),
		AnthropicSelectReason: pgText(choice.reason),
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
	return nil
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
	if run.Kind == RunKindSelfImprove {
		id, err := s.judgeSecretID(ctx, run.UserID)
		if err != nil {
			return secretChoice{}, err
		}
		return staticChoice(id, selectReasonJudge), nil
	}
	if wkr.AnthropicBindMode == BindModeAuto {
		return s.autoChoice(ctx, run)
	}
	return staticChoice(workerSecretID(wkr), selectReasonPinned), nil
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
	if !run.LimitDeadSecretID.Valid {
		return uuid.Nil
	}
	// Window still closed → keep excluding. Relax (Nil) once it has reopened, and also
	// when there is no reset stamp to wait on (nothing says the window is closed).
	if run.RetryNotBefore.Valid && run.RetryNotBefore.Time.After(s.now()) {
		return uuid.UUID(run.LimitDeadSecretID.Bytes)
	}
	return uuid.Nil
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
// is deliberate in the same way judgeSecretID's is: "the database was unreachable for
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
//     recoverClaimAssembly holds in the non-locking pool_wait status (PRD #754 M4) so
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
	// an empty-pool hold that recoverClaimAssembly holds in pool_wait (PRD #754 M4).
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
		// default (#754); recoverClaimAssembly fails the run on errCredentialUnavailable.
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
