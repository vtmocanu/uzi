package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TxBeginner is the narrow transaction seam the permit-gated completion runs in (PRD #1226 M2,
// D4). *pgxpool.Pool satisfies it — completeRunWithPermit needs only Begin, because it opens
// exactly one transaction and threads a tx-bound *store.Queries through the consume+complete
// section. Kept its own interface (interface segregation, like CodexTxBeginner) so it is the
// only pool surface this package depends on and a test can supply a fake.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// SetTxBeginner wires the transaction beginner (PRD #1226 M2, D4). Call once at startup, before
// serving; pass the same *pgxpool.Pool the handlers hold. A nil beginner is FAIL-CLOSED: an
// interlocked run's completion errors rather than completing non-atomically, so a deployment
// that never wired it can never complete an interlocked run without the permit transaction.
func (s *Service) SetTxBeginner(b TxBeginner) { s.txBeginner = b }

// Completion-permit DENIAL taxonomy (PRD #1226 M2, D5). These are the reasons the server
// returns when it refuses to issue a permit; every one is NON-TERMINAL — the run keeps its
// current status and the worker may act on the reason (rework, hold, stop). They are also the
// documented vocabulary M3/M4 branch on.
const (
	// CompletionDenyStaleClaim: the run is not owned by this worker in a live claimed state
	// (running or awaiting_input). The claim moved or the run is not yet/no longer implementing.
	CompletionDenyStaleClaim = "stale_claim"
	// CompletionDenyNotInterlocked: the run is LEGACY (completion_contract_version IS NULL) and
	// never permit-gates. This endpoint should not be called for it.
	CompletionDenyNotInterlocked = "not_interlocked"
	// CompletionDenyRevisionDrift: the requested contract_revision does not match the run's
	// frozen contract_revision (a #1227 revision bump raced the request, or the worker is stale).
	CompletionDenyRevisionDrift = "revision_drift"
	// CompletionDenyContractNotFrozen: the run is interlocked but its completion_contract is NULL
	// — the split-state hazard (revision could be 1 with contract NULL if the Go builder errored).
	// FAIL-CLOSED: the permit is DENIED so a corrupt/unfrozen run holds rather than completing.
	CompletionDenyContractNotFrozen = "contract_not_frozen"
	// CompletionDenyMissingMilestones: one or more in-scope structural criteria are not declared
	// complete. The bounded unmet id list accompanies the denial, and an attempt is recorded.
	CompletionDenyMissingMilestones = "missing_milestones"
	// CompletionDenyEmptyHead: the worker-reported head normalizes (NUL-strip + TrimSpace) to the
	// empty string, so it cannot bind a permit's `head` identity — an empty head would issue a
	// permit no completion could ever match. NON-TERMINAL so the worker keeps its session live and
	// re-reports a real head rather than parking. Distinct from missing_milestones (which records a
	// spurious attempt): this denial precedes the UpsertCompletionPermit write and records nothing.
	CompletionDenyEmptyHead = "empty_head"
)

// Sentinel errors the completion-attempt endpoint returns for the claim-fence denials (the
// permit endpoint returns those same reasons as a structured result instead). The handler maps
// these to HTTP status codes.
var (
	// ErrCompletionStaleClaim → 409: the run is not this worker's live claimed run.
	ErrCompletionStaleClaim = errors.New("completion: run is not in a live claimed state for this worker")
	// ErrCompletionNotInterlocked → 400: the run is legacy and never permit-gates.
	ErrCompletionNotInterlocked = errors.New("completion: run is not interlocked")
)

// CompletionPermitRequest is the worker's permit-issue request (PRD #1226 M2, D5): the frozen
// contract revision it believes it is completing against, the source branch, and the EXACT final
// head H the alignment/push path landed. The server recomputes its OWNED predicates and never
// trusts a "milestones done" claim in the request — there is none.
type CompletionPermitRequest struct {
	ContractRevision int
	Branch           string
	Head             string
}

// CompletionAttemptRequest is the same-lead nudge (M3) request (PRD #1226 M2/M3, D4): the worker
// reports the lead's declared milestones_completed plus the current head and worktree fingerprint.
// The SERVER union-merges the declaration into runs.milestones_completed, recomputes the unmet set
// over the merged set, and records a bounded attempt — all in one call (M3 collapses the previous
// "persist declaration THEN attempt" ordering hazard into a single server call). There is
// deliberately no contract_revision here — the attempt path is server-authoritative and fences on
// run.ContractRevision, never on a worker-supplied revision (only the permit endpoint checks a
// requested revision, for revision_drift).
type CompletionAttemptRequest struct {
	// MilestonesCompleted is the lead's signal_done declaration (frozen milestone ids). The
	// server subset-validates it against the run's frozen list (progressParams) and UNIONs it
	// into runs.milestones_completed before recomputing unmet. nil ⇒ nothing declared this
	// attempt (the union is a no-op; the server keeps whatever report_progress already unioned).
	MilestonesCompleted []string
	Head                string
	WorktreeFingerprint string
}

// CompletionPermitDTO is the wire shape of an issued permit. It carries no secret and no forge
// coordinate; finding_ids is empty and audit is omitted under profile=structural.
type CompletionPermitDTO struct {
	ID               string    `json:"id"`
	ContractRevision int       `json:"contract_revision"`
	Branch           string    `json:"branch"`
	Head             string    `json:"head"`
	IssuedAt         time.Time `json:"issued_at"`
	FindingIDs       []string  `json:"finding_ids"`
}

// CompletionPermitResult is the permit endpoint's decision (PRD #1226 M2). Granted true carries
// the Permit; granted false carries a DenyReason from the taxonomy above (and Unmet for
// missing_milestones). Every denial is non-terminal.
type CompletionPermitResult struct {
	Granted    bool                 `json:"granted"`
	DenyReason string               `json:"deny_reason,omitempty"`
	Unmet      []string             `json:"unmet,omitempty"`
	Permit     *CompletionPermitDTO `json:"permit,omitempty"`
}

// CompletionAttemptResult is the attempt endpoint's server-authoritative result (PRD #1226 M2):
// the recomputed unmet set (M3's agent reworks against THIS, never its own belief) and the new
// monotone attempt count.
type CompletionAttemptResult struct {
	Unmet        []string `json:"unmet"`
	AttemptCount int      `json:"attempt_count"`
}

// computeUnmetCriteria is the SERVER-AUTHORITATIVE structural recompute (PRD #1226 M2, D4): the
// set of the run's contract criteria whose milestone_id is NOT present in milestones_completed
// (the server's union-merged declared-complete set). It NEVER trusts a worker claim.
//
// It returns (unmet, verifiable). verifiable is FALSE — the fail-closed split-state hazard —
// when the interlocked run's completion_contract is NULL (or corrupt) while milestones were
// frozen: the permit path must DENY (contract_not_frozen), and to keep the attempt path
// fail-closed too the returned unmet is then the FULL frozen milestone id set (nothing reads as
// done). A genuinely milestone-less run has a NON-NULL contract with criteria:[] (M1 builds
// that), so it returns (empty, true) — vacuously complete.
func computeUnmetCriteria(run store.Run) (unmet []string, verifiable bool) {
	if len(run.CompletionContract) == 0 {
		// Contract NULL: split-state (or a never-frozen interlocked row). Fail closed — treat
		// every frozen milestone as unmet so no path can conclude "complete".
		return frozenMilestoneIDs(run), false
	}
	var c completionContract
	if err := json.Unmarshal(run.CompletionContract, &c); err != nil {
		// A corrupt contract column is the same fail-closed hazard.
		return frozenMilestoneIDs(run), false
	}
	completed, _ := DecodeMilestoneIDs(run.MilestonesCompleted)
	done := make(map[string]bool, len(completed))
	for _, id := range completed {
		done[id] = true
	}
	unmet = make([]string, 0, len(c.Criteria))
	for _, cr := range c.Criteria {
		if !done[cr.MilestoneID] {
			unmet = append(unmet, cr.MilestoneID)
		}
	}
	return unmet, true
}

// frozenMilestoneIDs returns the ids of a run's frozen milestone list (nil/empty when none),
// the fail-closed "everything is unmet" set computeUnmetCriteria hands back for an unverifiable
// contract.
func frozenMilestoneIDs(run store.Run) []string {
	ms, _ := DecodeMilestones(run.MilestonesFrozen)
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	return ids
}

// loadClaimedInterlockedRun loads a run the worker must own and be actively claiming, shared by
// both completion endpoints. It returns ErrRunNotOwned when the run is not this worker's;
// otherwise it returns the run plus a DENIAL reason that is non-empty when the claim fence
// rejects the request: CompletionDenyStaleClaim when the run is not in a live claimed state
// (running or awaiting_input), or CompletionDenyNotInterlocked when it is a legacy run. An empty
// reason means the fence passed.
func (s *Service) loadClaimedInterlockedRun(ctx context.Context, runID uuid.UUID, wkr store.Worker) (store.Run, string, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return store.Run{}, "", err
	}
	if run.Status != "running" && run.Status != "awaiting_input" {
		return run, CompletionDenyStaleClaim, nil
	}
	if !run.CompletionContractVersion.Valid {
		return run, CompletionDenyNotInterlocked, nil
	}
	return run, "", nil
}

// RequestCompletionPermit is the permit-issue endpoint (PRD #1226 M2, D4/D5). It applies the
// claim fence, checks the run is interlocked, matches the requested contract revision, verifies
// the contract is frozen, and recomputes the unmet structural criteria server-side. On a clean
// recompute it issues (idempotently) a permit bound to (run, contract_revision, branch, head).
// Every rejection is a NON-TERMINAL structured denial; a missing-milestones denial also records
// a bounded completion attempt.
func (s *Service) RequestCompletionPermit(ctx context.Context, wkr store.Worker, runID uuid.UUID, req CompletionPermitRequest) (CompletionPermitResult, error) {
	run, deny, err := s.loadClaimedInterlockedRun(ctx, runID, wkr)
	if err != nil {
		return CompletionPermitResult{}, err
	}
	if deny != "" {
		return CompletionPermitResult{Granted: false, DenyReason: deny}, nil
	}
	// Revision drift: the permit fences on the run's FROZEN revision, not the worker's claim.
	// An unfrozen revision (NULL) also lands here, which is correct — there is no revision to
	// bind a permit to yet.
	if !run.ContractRevision.Valid || int(run.ContractRevision.Int32) != req.ContractRevision {
		return CompletionPermitResult{Granted: false, DenyReason: CompletionDenyRevisionDrift}, nil
	}
	unmet, verifiable := computeUnmetCriteria(run)
	if !verifiable {
		// Split-state (contract NULL / corrupt while frozen): fail closed, hold, do not complete.
		return CompletionPermitResult{Granted: false, DenyReason: CompletionDenyContractNotFrozen}, nil
	}
	if len(unmet) > 0 {
		// A gated attempt with missing milestones: record it (bounded) and deny non-terminally.
		// A recompute race that reclaimed the run (ErrNoRows from the write) leaves the denial
		// intact — the recompute was valid at load; only an infra error propagates.
		if _, aerr := s.persistCompletionAttempt(ctx, wkr, run, unmet, req.Head, "", nil); aerr != nil && !errors.Is(aerr, pgx.ErrNoRows) {
			return CompletionPermitResult{}, aerr
		}
		return CompletionPermitResult{Granted: false, DenyReason: CompletionDenyMissingMilestones, Unmet: unmet}, nil
	}
	// Normalize the worker-authored branch and head with the IDENTICAL two-step (NUL-strip THEN
	// TrimSpace) the CONSUME side (completeRunWithPermit) and persistCompletionAttempt apply, so the
	// value stored at issue is byte-for-byte what the consume-side lookup keys on. NUL-strip first:
	// a NUL reaching the permit's `text NOT NULL` columns (migration 00212) raises Postgres 22021,
	// which 500s the issue so the permit is never written — and because the interlocked run's
	// terminal completion transaction can only consume a permit that was issued, that would
	// PERMANENTLY block the run from completing (worse than the attempt path, which only loses an
	// attempt log). TrimSpace second closes the asymmetry that a head issued untrimmed but looked up
	// trimmed can never match: before this the issue side stripped NUL only while the consume side
	// stripped-then-trimmed, so a head with surrounding whitespace stored untrimmed here and was
	// never findable at consume. This is the same worker-field discipline stripNULParam applies
	// everywhere else. Control/bidi/charset rejection is deliberately NOT done here — only NUL-strip
	// + TrimSpace happen at this point, symmetric with the consume side; that render-boundary
	// sanitization is STILL M5's job.
	strippedBranch, _ := stripNUL(req.Branch)
	strippedHead, _ := stripNUL(req.Head)
	cleanBranch := strings.TrimSpace(strippedBranch)
	cleanHead := strings.TrimSpace(strippedHead)
	if cleanHead == "" {
		// An empty-after-normalize head cannot bind a permit's head identity: issuing one would
		// write a permit no completion could ever match. Deny NON-TERMINALLY before the upsert so
		// the worker keeps its session live and re-reports a real head; unlike missing_milestones
		// this records no attempt.
		return CompletionPermitResult{Granted: false, DenyReason: CompletionDenyEmptyHead}, nil
	}
	permit, err := s.q.UpsertCompletionPermit(ctx, store.UpsertCompletionPermitParams{
		RunID:            run.ID,
		ContractRevision: run.ContractRevision.Int32,
		Branch:           cleanBranch,
		Head:             cleanHead,
		IssuedByWorkerID: pgconv.UUID(wkr.ID),
	})
	if err != nil {
		return CompletionPermitResult{}, err
	}
	return CompletionPermitResult{Granted: true, Permit: permitToDTO(permit)}, nil
}

// RecordCompletionAttempt is the same-lead nudge endpoint (M3) (PRD #1226 M2/M3, D4). It applies
// the claim fence, PERSISTS the lead's declared milestones (union-merged into
// runs.milestones_completed, subset-validated by progressParams), recomputes the unmet set
// server-side over the merged set (fail-closed for an unverifiable contract) and records a bounded
// attempt (counter++, latest summary, pruned log) — all in the single RecordCompletionAttempt
// statement, which removes the M2-review-flagged "agent must persist its declaration before the
// attempt" ordering hazard. It returns the server-authoritative unmet set and the new attempt
// count. Claim-fence denials come back as sentinel errors the handler maps to status codes.
func (s *Service) RecordCompletionAttempt(ctx context.Context, wkr store.Worker, runID uuid.UUID, req CompletionAttemptRequest) (CompletionAttemptResult, error) {
	run, deny, err := s.loadClaimedInterlockedRun(ctx, runID, wkr)
	if err != nil {
		return CompletionAttemptResult{}, err
	}
	switch deny {
	case CompletionDenyStaleClaim:
		return CompletionAttemptResult{}, ErrCompletionStaleClaim
	case CompletionDenyNotInterlocked:
		return CompletionAttemptResult{}, ErrCompletionNotInterlocked
	}
	// PRD #1226 M3: subset-validate the lead's declaration against the frozen list — the SAME
	// kind/membership gate SetRunRunning/SetRunCompleted apply (progressParams). declaredJSON is
	// the jsonb the RecordCompletionAttempt statement UNIONs into milestones_completed; nil when
	// nothing valid was declared (the union is then a no-op).
	var declaredPtr *[]string
	if req.MilestonesCompleted != nil {
		declaredPtr = &req.MilestonesCompleted
	}
	declaredJSON, _ := progressParams(run.Kind, run.MilestonesFrozen, declaredPtr, nil)
	// Recompute unmet over the MERGED set (existing ∪ declared) so the persisted unmet is
	// consistent with the milestones_completed the same statement persists. computeUnmetCriteria
	// reads run.MilestonesCompleted, so hand it a run copy whose completed set is the merge.
	merged := run
	merged.MilestonesCompleted = unionMilestoneIDs(run.MilestonesCompleted, declaredJSON)
	unmet, _ := computeUnmetCriteria(merged)
	count, err := s.persistCompletionAttempt(ctx, wkr, run, unmet, req.Head, req.WorktreeFingerprint, declaredJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The run was reclaimed between the load and the write — the fence no longer holds.
			return CompletionAttemptResult{}, ErrCompletionStaleClaim
		}
		return CompletionAttemptResult{}, err
	}
	return CompletionAttemptResult{Unmet: unmet, AttemptCount: int(count)}, nil
}

// SetRunCompletionHold parks an owned, interlocked run on the completion interlock's dedicated
// HOLD transition (PRD #1226 M4, D6): running/awaiting_input -> paused with
// hold_reason='completion_blocked' and the worker-captured head recorded. It is the service seam
// the worker's park order calls; applied is true IFF a row came back (the SetRunCompletionHold
// guard admitted the run), which is the ACK the worker keys its cleanup carve-out off — a
// non-paused ack (applied=false) means RETAIN the run live, never clean up.
//
// The captured head is normalized with the SAME worker-field discipline the permit path applies
// (NUL-strip THEN TrimSpace); an empty captured head is ALLOWED (the column is nullable) and
// stored NULL via pgconv.TextOrNull.
//
// A guard-failed hold is 0 rows (pgx.ErrNoRows): NON-TERMINAL, applied=false, no error. On that
// path it RE-READS the owned run so the caller returns 409 with the run's REAL (non-paused)
// status — exactly the SetState/WorkerRunState applied=false contract, where the worker reads its
// live status off the body and retains the run. The re-read predates no snapshot: the hold's guard
// and the re-read both key on (id, worker_id), so a run that failed the guard while still owned
// re-reads cleanly, while a genuinely reclaimed run surfaces as ErrRunNotOwned (-> 404) — the
// worker never sees `paused` in either case, so it never wrongly cleans up. Only an infra error
// propagates.
func (s *Service) SetRunCompletionHold(ctx context.Context, wkr store.Worker, runID uuid.UUID, capturedHead string) (store.Run, bool, error) {
	// NUL-strip BEFORE the trim (a NUL is not whitespace, so a "\x00 h \x00" would survive a
	// trim) — the same order RequestCompletionPermit / persistCompletionAttempt use for every
	// worker-authored text field, and required because hold_captured_head accepts arbitrary
	// worker input (a NUL in a text column raises Postgres 22021).
	clean, _ := stripNUL(capturedHead)
	run, err := s.q.SetRunCompletionHold(ctx, store.SetRunCompletionHoldParams{
		ID:               runID,
		WorkerID:         pgconv.UUID(wkr.ID),
		HoldCapturedHead: pgconv.TextOrNull(strings.TrimSpace(clean)),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Guard rejected the hold (wrong status, not interlocked, or no recorded completion
			// attempt) while the run may still be owned: re-read the authoritative row so the
			// worker receives its real status through the applied=false / 409 contract. A reclaim
			// re-reads as ErrRunNotOwned, which the handler maps to 404.
			current, rerr := s.runOwnedByWorker(ctx, runID, wkr)
			if rerr != nil {
				return store.Run{}, false, rerr
			}
			return current, false, nil
		}
		return store.Run{}, false, err
	}
	return run, true, nil
}

// persistCompletionAttempt writes one bounded completion attempt through the store query
// (insert row + increment counter + union the declaration into milestones_completed + set
// summary + prune to N), returning the run's new attempt count. It is the single writer both
// endpoints share: the attempt endpoint passes the subset-validated declaration jsonb
// (milestonesCompletedJSON) to union-merge, while the permit path passes nil (it carries no
// declaration, so the union CASE leaves milestones_completed untouched). pgx.ErrNoRows means the
// guard (owned + interlocked) failed at write time — a raced reclaim — which each caller interprets.
func (s *Service) persistCompletionAttempt(ctx context.Context, wkr store.Worker, run store.Run, unmet []string, head, worktreeFingerprint string, milestonesCompletedJSON []byte) (int32, error) {
	unmetJSON, err := encodeJSONArray(unmet)
	if err != nil {
		return 0, err
	}
	// NUL-strip the worker-authored head and worktree_fingerprint before they are stored — the
	// same worker-field discipline stripNULParam/sanitizeFailureReason apply to every other
	// worker-authored text field: a NUL in a text/jsonb column raises Postgres 22021. Here it
	// would fire on BOTH the run_completion_attempts text columns AND the
	// jsonb_build_object(...) latest_completion_attempt summary, 500 the attempt, and leave an
	// otherwise-complete run's completion unrecorded. NUL is removed BEFORE the trim (a NUL is
	// not whitespace, so a "\x00 h \x00" would survive a trim), matching stopReasonParam's
	// order; the TrimSpace + ""->NULL behavior is unchanged. Control/bidi/charset rejection is
	// deliberately NOT done here — that render-boundary sanitization is M5's job.
	cleanHead, _ := stripNUL(head)
	cleanFingerprint, _ := stripNUL(worktreeFingerprint)
	return s.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
		MilestonesCompleted: milestonesCompletedJSON,
		Unmet:               unmetJSON,
		Head:                pgconv.TextOrNull(strings.TrimSpace(cleanHead)),
		WorktreeFingerprint: pgconv.TextOrNull(strings.TrimSpace(cleanFingerprint)),
		RunID:               run.ID,
		WorkerID:            pgconv.UUID(wkr.ID),
		ContractRevision:    run.ContractRevision,
	})
}

// unionMilestoneIDs returns the monotone, DISTINCT-deduped union of two milestones_completed
// jsonb id-arrays (existing ∪ declared), mirroring the SetRunRunning SQL union so the Go-side
// recompute over the merged set matches the merge the RecordCompletionAttempt statement persists.
// Set membership is order-independent, so computeUnmetCriteria's result is identical regardless of
// the union output order. A nil/empty declared leaves the existing set unchanged; on an encode
// error it falls back to the existing bytes (the recompute then simply omits the declaration —
// fail-safe, never a panic).
func unionMilestoneIDs(existing, declared []byte) []byte {
	a, _ := DecodeMilestoneIDs(existing)
	b, _ := DecodeMilestoneIDs(declared)
	// Pre-size to the base (existing) set only. CodeQL flags `len(a)+len(b)` as a
	// (theoretical) allocation-size overflow; both sets are subset-validated to the
	// frozen milestone list (<= maxMilestonesPerRun each, see progressParams), so the
	// sum can never overflow, but a single-len hint matches the local idiom
	// (validateProgressIDs) and clears the scanner. The map/slice grow for b's ids as
	// needed.
	seen := make(map[string]bool, len(a))
	out := make([]string, 0, len(a))
	for _, id := range a {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range b {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	enc, err := encodeJSONArray(out)
	if err != nil {
		return existing
	}
	return enc
}

// completeRunWithPermit runs an INTERLOCKED run's terminal completion through a single pgx
// transaction (PRD #1226 M2, D4): lock the run FOR UPDATE, consume the permit issued for the
// exact (run, contract_revision, head) identity, write `completed`, and (via the documented
// seam below) leave room for later generation activation — all atomically.
//
// It returns (rows, idempotent, err):
//   - rows==1, idempotent==false: a genuine new completion applied; SetState runs its terminal
//     automation.
//   - rows==0, idempotent==false: NON-TERMINAL — no matching unconsumed permit (missing, wrong
//     head/revision, wrong worker), a raced consume, or the run left the completable state. The
//     transaction is rolled back so nothing (not even the permit) is consumed, and SetState
//     surfaces applied=false (409). This is "a gated report without a valid permit updates
//     nothing and remains non-terminal".
//   - rows==0, idempotent==true: the run is ALREADY completed and THIS worker's permit for the
//     identity was already consumed — a retry after response loss. SetState returns success
//     WITHOUT re-running the terminal automation (it fired on the original completion).
//
// A nil txBeginner is fail-closed (error): an interlocked run never completes non-atomically.
func (s *Service) completeRunWithPermit(ctx context.Context, wkr store.Worker, owned store.Run, req StateRequest, completedParams store.SetRunCompletedParams) (rows int64, idempotent bool, err error) {
	if s.txBeginner == nil {
		return 0, false, fmt.Errorf("completion transaction unavailable: no tx beginner wired for run %s", owned.ID)
	}
	head := ""
	if req.Head != nil {
		// NUL-strip BEFORE the trim (a NUL is not whitespace, so a "\x00 h \x00" would survive a
		// trim) — the SAME helper and order the ISSUE side (RequestCompletionPermit) and
		// persistCompletionAttempt use, so a real hex head, or a NUL that strips to one, matches the
		// permit's stored head in the GetUnconsumed/GetConsumedCompletionPermit `WHERE head=$x`
		// lookups below. Without it a NUL in the reported head raises Postgres 22021 on those params
		// and 500s the completion. The TrimSpace + empty->non-terminal handling is unchanged.
		clean, _ := stripNUL(*req.Head)
		head = strings.TrimSpace(clean)
	}
	// The branch identity the permit lookups fence on is derived from the WORKER-REPORTED
	// req.Branch with the IDENTICAL NUL-strip-then-TrimSpace the ISSUE side normalized with, so a
	// permit issued for branch A cannot be consumed by a completion reporting branch B at the same
	// head. It MUST come from req.Branch, NOT the locked-row run.Branch: runs.branch is written only
	// by SetRunCompleted / ReconcileRunMR, both of which run AT or AFTER this completion, so
	// run.Branch is NULL at this lookup for a first completion — pinning to it would make every
	// legitimate interlocked completion fail forever. (This differs from revision, correctly
	// re-derived from the locked row because it is server-authoritative and always present
	// post-freeze.)
	branch := ""
	if req.Branch != nil {
		clean, _ := stripNUL(*req.Branch)
		branch = strings.TrimSpace(clean)
	}
	// No head or an unfrozen revision cannot match a permit → non-terminal (fail-closed). This is
	// a cheap early-out on the pre-tx `owned` snapshot before we open a transaction; the
	// AUTHORITATIVE revision is re-derived from the LOCKED row below.
	if head == "" || !owned.ContractRevision.Valid {
		return 0, false, nil
	}
	workerID := pgconv.UUID(wkr.ID)

	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	// A no-op after a successful Commit; on any early return it undoes the FOR UPDATE lock and,
	// on the failed-consume path, the permit consume — so a completion that does not apply never
	// spends its permit.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	run, err := qtx.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: owned.ID, WorkerID: workerID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil // no longer this worker's run → non-terminal
		}
		return 0, false, err
	}

	// The revision that binds the permit identity MUST come from the LOCKED row, not the pre-tx
	// `owned` snapshot. Revision is immutable post-freeze today, so the two agree; but #1227 will
	// introduce contract-revision bumps, and a bump racing this completion would let a stale
	// snapshot revision consume an OLD-revision permit under the lock — a revision-drift TOCTOU
	// bypass. Reading `run.ContractRevision` closes it: if a bump landed in the race, the permit
	// lookups below find nothing for the new revision → non-terminal → fail-closed, forcing a
	// re-permit at the new revision. If the locked row is unfrozen (revision NULL — e.g. a
	// split-state row), fail closed the same way the pre-tx no-head guard does.
	if !run.ContractRevision.Valid {
		return 0, false, nil
	}
	rev := run.ContractRevision.Int32

	// Retry after response loss: the run is already completed. If THIS worker's permit for the
	// identity was already consumed, we completed it once and the response was lost → idempotent
	// success. Otherwise the run is terminal but not by us → non-terminal signal.
	if run.Status == "completed" {
		if _, cerr := qtx.GetConsumedCompletionPermit(ctx, store.GetConsumedCompletionPermitParams{
			RunID: run.ID, ContractRevision: rev, Head: head, Branch: branch, IssuedByWorkerID: workerID,
		}); cerr != nil {
			if errors.Is(cerr, pgx.ErrNoRows) {
				return 0, false, nil
			}
			return 0, false, cerr
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return 0, false, cerr
		}
		return 0, true, nil
	}

	// Live run: fetch the unconsumed permit for the exact identity. None → non-terminal.
	permit, perr := qtx.GetUnconsumedCompletionPermit(ctx, store.GetUnconsumedCompletionPermitParams{
		RunID: run.ID, ContractRevision: rev, Head: head, Branch: branch, IssuedByWorkerID: workerID,
	})
	if perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, perr
	}
	consumed, cerr := qtx.ConsumeCompletionPermit(ctx, store.ConsumeCompletionPermitParams{ID: permit.ID, IssuedByWorkerID: workerID})
	if cerr != nil {
		return 0, false, cerr
	}
	if consumed != 1 {
		return 0, false, nil // raced consume → non-terminal, rolled back
	}
	rows, err = qtx.SetRunCompleted(ctx, completedParams)
	if err != nil {
		return 0, false, err
	}
	if rows != 1 {
		// The run left the completable state under the lock (e.g. a superseding terminal). Roll
		// back so the permit is NOT consumed and the report stays non-terminal.
		return 0, false, nil
	}

	// Generation-activation hook seam (PRD #1226 M2, D4). This is a DELIBERATE no-op today:
	// #1229 (durable context) and #1214 will activate the run generation HERE, in this same
	// transaction, so a later child joins the terminal write atomically. It is placed after the
	// permit consume and the `completed` write and before Commit precisely so that join is
	// all-or-nothing with the completion — do NOT replace this terminal seam with an isolated
	// SQL update.

	if cerr := tx.Commit(ctx); cerr != nil {
		return 0, false, cerr
	}
	return rows, false, nil
}

// permitToDTO maps a stored permit to its coordinate-free wire shape.
func permitToDTO(p store.RunCompletionPermit) *CompletionPermitDTO {
	findings := p.FindingIds
	if findings == nil {
		findings = []string{}
	}
	return &CompletionPermitDTO{
		ID:               p.ID.String(),
		ContractRevision: int(p.ContractRevision),
		Branch:           p.Branch,
		Head:             p.Head,
		IssuedAt:         p.IssuedAt.Time,
		FindingIDs:       findings,
	}
}
