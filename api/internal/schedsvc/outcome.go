package schedsvc

import "github.com/google/uuid"

// Started records one run a fire actually created: the issue it fired for (nil IssueIID
// for an issue-less prompt schedule), the run it produced, and the issue/prompt title so
// the UI can pair the issue with its run without a second fetch (PRD #308).
type Started struct {
	IssueIID *int64
	RunID    uuid.UUID
	Title    string
}

// Skip records one candidate a fire considered but started nothing for, with the typed
// reason (never free text). IssueIID is nil for an issue-less prompt schedule; Title may
// be empty when the candidate's title was not fetched (a pre-check skip does not pay for
// an extra forge call just to label a skip).
type Skip struct {
	IssueIID *int64
	Title    string
	Reason   SkipReason
}

// FireOutcome is the structured result of one schedule fire (PRD #308). It replaces the
// bare []uuid.UUID the fire path used to return, carrying enough per-candidate detail to
// render the schedules UI and to persist a last-fire summary in a later milestone.
//
// Invariant: Matched == len(Started) + len(Skips). Every candidate a fire considers lands
// in exactly one bucket — a run it started, or a skip with a reason — so the tally always
// balances (PRD #308 Decision 4). A transient per-candidate sweep failure is a
// fetch_failed Skip, not a dropped candidate.
type FireOutcome struct {
	// Matched is the number of candidates considered this fire: sweep = len(the capped
	// candidate set), issue = 0/1 (1 once the pinned issue is considered), prompt = 1
	// (the schedule itself is the single candidate whenever a fire is attempted).
	Matched int
	// Capped is set only by a sweep whose max_issues cap truncated the candidate set, so
	// the "newer issues not reached" hint can be factual. Always false for issue/prompt
	// and for a sweep with a NULL cap (a NULL cap can never truncate).
	Capped  bool
	Started []Started
	Skips   []Skip
}

// RunIDs returns the ids of the runs this fire started, so callers that only need the
// created run ids (the tick path, the run-now handler until it widens) can still get them.
func (o FireOutcome) RunIDs() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(o.Started))
	for _, s := range o.Started {
		ids = append(ids, s.RunID)
	}
	return ids
}
