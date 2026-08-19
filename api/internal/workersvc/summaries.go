package workersvc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// Summary field caps (PRD #362 M1). The summary text bound is generous — a plain-English
// paragraph, not a plan — but finite so a runaway/prompt-injected model turn cannot store
// an unbounded blob on the user's run. Each delta text is short by construction, and the
// per-list cap bounds how many the model may report. These bound the RAW bytes the worker
// may send; the service renders every field inert (strip control/bidi + secret scrub)
// before it reaches the store, matching the findings ingest hygiene (Decision 10).
const (
	MaxSummaryBytes          = 4000
	MaxSummaryDeltas         = 50
	MaxSummaryDeltaTextBytes = 1000
)

// validSummaryDeltaKinds is the closed enum a delta's `kind` must belong to (Decision 6):
// the tag describing how the proposed plan diverged from the original ask.
var validSummaryDeltaKinds = map[string]bool{"added": true, "changed": true, "dropped": true}

// Summary sentinel errors, mapped to HTTP status by the handlers.
var (
	// ErrSummaryRepoRequired rejects a summary on a repo-less run (a chat run, or a run
	// whose repo_id is NULL). Autonomous issue runs always have one, so this is
	// defense-in-depth, mirroring the finding guard → 409.
	ErrSummaryRepoRequired = errors.New("run has no repository; a summary needs one")
	// ErrSummaryDeltasInvalid rejects a deltas list that is over the cap, carries an
	// unknown kind, or an empty/over-long text (Decision 6, validated-on-persist) → 400.
	ErrSummaryDeltasInvalid = errors.New("summary deltas are invalid")
	// ErrSummaryPlanStale rejects a plan-summary write whose plan_md no longer matches
	// runs.plan_md — the plan was revised under a slower earlier generation (Decision 3
	// stale-write guard). Distinct from ErrRunNotFound so the handler can answer 409
	// (a superseded plan, not an error that should fail the run).
	ErrSummaryPlanStale = errors.New("plan changed since this summary was generated")
)

// SetIntentSummary persists a run's plain-English INTENT summary (PRD #362 M1). The
// worker NEVER sends a user id: the service derives ownership from the CLAIMED run
// (GetRunByIDForUser), exactly like every other worker write. Guards: the run is the
// worker's user's run, non-terminal, and has a repo. It is IDEMPOTENT-ON-SET (Decision
// 3): when summary_intent is already present it is a no-op SUCCESS — a re-claim/resume
// must not re-spend the owner's token or churn the summary — and returns written=false.
// On a real write it emits a live-update frame (PublishState with the run's CURRENT
// status) so a summary landing mid-`running` (no state transition) surfaces without a
// manual refresh. Returns the run (for the caller) and whether a row was written.
func (s *Service) SetIntentSummary(ctx context.Context, wkr store.Worker, runID uuid.UUID, summary string) (store.Run, bool, error) {
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, false, ErrRunNotFound
		}
		return store.Run{}, false, err
	}
	if !run.RepoID.Valid {
		return store.Run{}, false, ErrSummaryRepoRequired
	}
	if terminalStatuses[run.Status] {
		return store.Run{}, false, ErrRunTerminal
	}
	// Idempotency (Decision 3): the "already set" decision lives here, not in SQL, so it
	// is testable and the read the guards already needed also answers it. A present
	// summary_intent (even empty-string, which a generation would never write) is a no-op
	// success — no token re-spend, no WS churn.
	if run.SummaryIntent.Valid {
		return run, false, nil
	}

	rows, err := s.q.SetRunIntentSummary(ctx, store.SetRunIntentSummaryParams{
		ID:            runID,
		SummaryIntent: pgtype.Text{String: sanitizeSummaryText(summary, MaxSummaryBytes), Valid: true},
	})
	if err != nil {
		return store.Run{}, false, err
	}
	if rows == 0 {
		// The row vanished between the guarded read and the write (a delete race). Treat
		// it as not-found rather than a silent success.
		return store.Run{}, false, ErrRunNotFound
	}
	s.publishRunState(run)
	return run, true, nil
}

// SetPlanSummary persists a run's PLAN summary + deltas with the Decision 3 stale-write
// guard (PRD #362 M1). Ownership is derived from the CLAIMED run; guards are owner /
// non-terminal / has-repo, same as SetIntentSummary. The deltas are VALIDATED-ON-PERSIST
// (Decision 6): the list is capped, each kind must be in {added,changed,dropped}, and
// each text must be non-empty and bounded — an invalid list is ErrSummaryDeltasInvalid
// (400), never stored. The write carries the plan_md the summary was generated from as
// the stale-write guard: SetRunPlanSummary updates ONLY IF that still matches
// runs.plan_md, so a slower earlier generation cannot overwrite the summary of a newer,
// revised plan; a 0-row write is ErrSummaryPlanStale (409, a superseded plan), NOT a
// failure of the run. On a successful write it emits a live-update frame with the run's
// current status. Returns the run for the caller.
func (s *Service) SetPlanSummary(ctx context.Context, wkr store.Worker, runID uuid.UUID, summary string, deltas []apitypes.RunSummaryDelta, planMd string) (store.Run, error) {
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotFound
		}
		return store.Run{}, err
	}
	if !run.RepoID.Valid {
		return store.Run{}, ErrSummaryRepoRequired
	}
	if terminalStatuses[run.Status] {
		return store.Run{}, ErrRunTerminal
	}

	deltasJSON, err := validateAndMarshalDeltas(deltas)
	if err != nil {
		return store.Run{}, err
	}

	rows, err := s.q.SetRunPlanSummary(ctx, store.SetRunPlanSummaryParams{
		ID:             runID,
		SummaryPlan:    pgtype.Text{String: sanitizeSummaryText(summary, MaxSummaryBytes), Valid: true},
		SummaryDeltas:  deltasJSON,
		ExpectedPlanMd: pgtype.Text{String: planMd, Valid: true},
	})
	if err != nil {
		return store.Run{}, err
	}
	if rows == 0 {
		// The run exists (the guarded read above succeeded), so a 0-row write is the
		// stale-write guard firing: runs.plan_md no longer equals the plan this summary
		// was generated from (a re-plan landed first). Reject as a conflict, not an error
		// that fails the run.
		return store.Run{}, ErrSummaryPlanStale
	}
	s.publishRunState(run)
	return run, nil
}

// publishRunState emits a best-effort live-update frame for a run whose summary just
// persisted (PRD #362 M1). It carries the run's CURRENT status (a summary write never
// transitions status) so the browser's useRunStream refetches the run DTO and the new
// summary surfaces without a manual refresh. Nil-safe: a Service with no Broadcaster
// (tests, or a deployment without the hub) is a no-op, and the Broadcaster contract is
// itself non-blocking.
func (s *Service) publishRunState(run store.Run) {
	if s.bcast != nil {
		s.bcast.PublishState(run.ID, run.Status)
	}
}

// validateAndMarshalDeltas enforces the Decision 6 persist-time contract on a plan
// summary's deltas and returns the jsonb to store. A nil/empty list is legal — it marshals
// to `[]` ("no deviations"). Every entry must carry a known kind and a non-empty, bounded
// text; the list itself is capped. Each text is rendered INERT (strip control/bidi +
// secret scrub) for storage, matching the findings ingest hygiene — the untrusted,
// model-authored text is bounded and validated on the RAW input first, then sanitised.
func validateAndMarshalDeltas(deltas []apitypes.RunSummaryDelta) ([]byte, error) {
	if len(deltas) > MaxSummaryDeltas {
		return nil, ErrSummaryDeltasInvalid
	}
	out := make([]apitypes.RunSummaryDelta, 0, len(deltas))
	for _, d := range deltas {
		if !validSummaryDeltaKinds[d.Kind] {
			return nil, ErrSummaryDeltasInvalid
		}
		if d.Text == "" || len(d.Text) > MaxSummaryDeltaTextBytes {
			return nil, ErrSummaryDeltasInvalid
		}
		out = append(out, apitypes.RunSummaryDelta{
			Kind: d.Kind,
			Text: sanitizeSummaryText(d.Text, MaxSummaryDeltaTextBytes),
		})
	}
	return json.Marshal(out)
}

// DecodeSummaryDeltas reads a runs.summary_deltas jsonb column into the wire shape for
// the run DTO. A NULL/empty column yields a NIL slice ("no deltas"), which marshals to
// JSON null — the back-compat contract for every pre-feature run. Malformed jsonb is an
// error the caller degrades to nil-and-log (Decision 6 tolerate-on-read): the column is
// data a prior write left behind, not an invariant of this read, so a bad value renders
// as no deltas and never crashes the renderer. Mirrors DecodeMilestones.
func DecodeSummaryDeltas(raw []byte) ([]apitypes.RunSummaryDelta, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []apitypes.RunSummaryDelta
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// sanitizeSummaryText renders one untrusted, model-authored summary string INERT for
// storage: strip terminal-control / bidi-override runes and bound the byte length
// rune-safely, then scrub secret shapes. Order matches the findings/judge ingest
// (ScrubSecrets(SanitizeBounded(...))): sanitise+cap FIRST so the scrubber sees whole
// runes and the cap applies before redaction rewrites.
func sanitizeSummaryText(s string, max int) string {
	return secretscrub.Scrub(termsafe.SanitizeBounded(s, max))
}
