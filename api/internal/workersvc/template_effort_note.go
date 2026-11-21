package workersvc

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// templateEffortOmittedEvent is the payload event of the server-authored run-feed line recorded when
// a claim withholds stored subagent effort pins from a worker that lacks capability.TemplateEffortV1
// (issue #3258 N6). It deliberately avoids the events other readers special-case on a status row.
const templateEffortOmittedEvent = "template_effort_omitted"

// templateEffortOmittedAgent is the stored and the published agent of that line; the same value on
// both sides keeps the live row and the reloaded row identical.
const templateEffortOmittedAgent = "worker"

type templateEffortOmittedPayload struct {
	Event string `json:"event"`
	Text  string `json:"text"`
}

// hasWithheldTemplateEffort reports whether a claim to a worker without template effort support
// drops at least one stored pin: a delivered, non-lead-shaped template with a valid effort for the
// run's harness. The lead template's own effort is not a subagent pin (it rides default_effort
// through the lead tier), so it never triggers the notice.
func hasWithheldTemplateEffort(rows []store.AgentTemplate, harness string) bool {
	for _, t := range rows {
		if !agenttmpl.IsLeadName(t.Name) && templateEffortPin(t, harness) != "" {
			return true
		}
	}
	return false
}

// recordTemplateEffortOmittedNote appends the nonsecret status line to the run's feed and advances
// runs.last_seq, returning the last_seq the claim payload must adopt. It follows
// recordHarnessModelFallbackNote: best effort (any failure logs and returns startLastSeq), fenced on
// the run's claim_generation so a stale re-assembly persists nothing. The fence rejects stale
// generations only; it is not an idempotency key. One line per claim generation holds because the
// only caller is assembleClaim, whose only caller (assembleAndFinishRunClaimRef) runs once per
// ClaimRun commit, and ClaimRun advances claim_generation. A new caller must keep that property.
func (s *Service) recordTemplateEffortOmittedNote(ctx context.Context, run store.Run, startLastSeq int32) int32 {
	raw, err := json.Marshal(templateEffortOmittedPayload{
		Event: templateEffortOmittedEvent,
		Text:  "stored subagent effort pins omitted: this worker predates template effort; those subagents use the run's effort",
	})
	if err != nil {
		slog.Warn("workersvc: marshal template effort omitted note", "run_id", run.ID.String(), "error", err)
		return startLastSeq
	}
	seq := startLastSeq + 1
	gen := run.ClaimGeneration
	res, err := s.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID:           run.ID,
		Seq:             seq,
		Kind:            "status",
		Agent:           pgconv.TextOrNull(templateEffortOmittedAgent),
		Payload:         raw,
		ClaimGeneration: pgconv.Int8Ptr(&gen),
	})
	if err != nil {
		slog.Warn("workersvc: insert template effort omitted note", "run_id", run.ID.String(), "error", err)
		return startLastSeq
	}
	if !res.Inserted {
		return startLastSeq
	}
	if _, err := s.q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: run.ID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&gen)}); err != nil {
		// The row is already inserted: returning the old seq would make the worker's first frame
		// collide with it, so adopt the new seq regardless.
		slog.Warn("workersvc: advance last_seq for template effort omitted note", "run_id", run.ID.String(), "error", err)
	}
	if s.bcast != nil {
		s.bcast.PublishMessage(run.ID, seq, "status", templateEffortOmittedAgent, "", "", raw, s.now())
	}
	return seq
}
