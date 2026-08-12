package schedsvc

import (
	"encoding/json"
	"time"
)

// The persisted last-fire wire shape (PRD #308 M2, Decision 1). This is the serialized
// form of a FireOutcome written into run_schedules.last_fire on the success/benign
// advance path only. The json tags below are a CONTRACT: M3 mirrors them in
// apitypes.LastFire (which json.Unmarshals the raw column bytes), and the CLI/web read
// them, so they must not drift. Keep the tags exactly as written.
//
// The types are package-internal (exported-lowercase): the marshal side lives here, and
// the M3 handler unmarshals the raw bytes into its OWN apitypes struct with matching
// tags — it does not need these Go types.

// lastFireStarted is one run a fire actually created.
type lastFireStarted struct {
	IssueIID *int64 `json:"issue_iid"` // nil for a prompt schedule
	RunID    string `json:"run_id"`    // uuid string
	Title    string `json:"title"`
}

// lastFireSkip is one candidate a fire considered but started nothing for, with its
// typed reason (a SkipReason string, never free text).
type lastFireSkip struct {
	IssueIID *int64 `json:"issue_iid"`
	Title    string `json:"title"`
	Reason   string `json:"reason"` // a SkipReason string
}

// lastFireRecord is the top-level persisted summary of the last scheduled fire. FiredAt
// is the advance instant (the scheduler's now at persist time), so the UI can show WHEN
// the last fire happened alongside WHAT it did.
type lastFireRecord struct {
	FiredAt time.Time         `json:"fired_at"`
	Matched int               `json:"matched"`
	Capped  bool              `json:"capped"`
	Started []lastFireStarted `json:"started"`
	Skips   []lastFireSkip    `json:"skips"`
}

// marshalLastFire builds a lastFireRecord from a fire outcome and serializes it to the
// jsonb bytes AdvanceSchedule persists. Started/Skips are non-nil empty slices so the
// JSON carries "started":[]/"skips":[] rather than null (matching the mock's convention
// and keeping clients simple). It is called only on the success/benign advance path; a
// marshal error is the caller's cue to persist SQL NULL rather than wedge the cadence.
func marshalLastFire(out FireOutcome, firedAt time.Time) ([]byte, error) {
	rec := lastFireRecord{
		FiredAt: firedAt,
		Matched: out.Matched,
		Capped:  out.Capped,
		Started: make([]lastFireStarted, 0, len(out.Started)),
		Skips:   make([]lastFireSkip, 0, len(out.Skips)),
	}
	for _, s := range out.Started {
		rec.Started = append(rec.Started, lastFireStarted{
			IssueIID: s.IssueIID,
			RunID:    s.RunID.String(),
			Title:    s.Title,
		})
	}
	for _, s := range out.Skips {
		rec.Skips = append(rec.Skips, lastFireSkip{
			IssueIID: s.IssueIID,
			Title:    s.Title,
			Reason:   string(s.Reason),
		})
	}
	return json.Marshal(rec)
}
