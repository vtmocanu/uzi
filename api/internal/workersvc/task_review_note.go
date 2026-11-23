package workersvc

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// taskReviewSettingsOmittedEvent is the payload event of the server-authored run-feed line recorded
// when a review claim withholds the owner's task-review settings from a worker that lacks
// capability.TaskReviewSettingsV1 (PRD #1570). Like templateEffortOmittedEvent it avoids the events
// other readers special-case on a status row.
const taskReviewSettingsOmittedEvent = "task_review_settings_omitted"

// taskReviewSettingsOmittedAgent is the stored and the published agent of that line; the same
// value on both sides keeps the live row and the reloaded row identical.
const taskReviewSettingsOmittedAgent = "worker"

// The user-facing names of the withheld settings, joined into the note text.
const (
	taskReviewModelSettingName  = "task review model"
	taskReviewEffortSettingName = "task review effort"
)

type taskReviewSettingsOmittedPayload struct {
	Event string `json:"event"`
	Text  string `json:"text"`
}

// taskReviewSettingsOmittedText is the nonsecret note text naming the withheld settings.
func taskReviewSettingsOmittedText(withheld []string) string {
	return "task review settings omitted (" + strings.Join(withheld, ", ") +
		"): this worker predates task review settings; the review runs on its built-in model and sends no effort"
}

// recordTaskReviewSettingsOmittedNote appends the nonsecret status line to the run's feed and
// advances runs.last_seq, returning the last_seq the claim payload must adopt. It follows
// recordTemplateEffortOmittedNote: best effort (any failure logs and returns startLastSeq), fenced
// on the run's claim_generation so a stale re-assembly persists nothing. The fence rejects stale
// generations only; it is not an idempotency key. One line per claim generation holds because the
// only caller is assembleClaim, whose only caller runs once per ClaimRun commit, and ClaimRun
// advances claim_generation. A new caller must keep that property. withheld names each setting the
// claim dropped (taskReviewModelSettingName, taskReviewEffortSettingName).
func (s *Service) recordTaskReviewSettingsOmittedNote(ctx context.Context, run store.Run, startLastSeq int32, withheld []string) int32 {
	raw, err := json.Marshal(taskReviewSettingsOmittedPayload{
		Event: taskReviewSettingsOmittedEvent,
		Text:  taskReviewSettingsOmittedText(withheld),
	})
	if err != nil {
		slog.Warn("workersvc: marshal task review settings omitted note", "run_id", run.ID.String(), "error", err)
		return startLastSeq
	}
	seq := startLastSeq + 1
	gen := run.ClaimGeneration
	res, err := s.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID:           run.ID,
		Seq:             seq,
		Kind:            "status",
		Agent:           pgconv.TextOrNull(taskReviewSettingsOmittedAgent),
		Payload:         raw,
		ClaimGeneration: pgconv.Int8Ptr(&gen),
	})
	if err != nil {
		slog.Warn("workersvc: insert task review settings omitted note", "run_id", run.ID.String(), "error", err)
		return startLastSeq
	}
	if !res.Inserted {
		return startLastSeq
	}
	if _, err := s.q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: run.ID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&gen)}); err != nil {
		// The row is already inserted: returning the old seq would make the worker's first frame
		// collide with it, so adopt the new seq regardless.
		slog.Warn("workersvc: advance last_seq for task review settings omitted note", "run_id", run.ID.String(), "error", err)
	}
	if s.bcast != nil {
		s.bcast.PublishMessage(run.ID, seq, "status", taskReviewSettingsOmittedAgent, "", "", raw, s.now())
	}
	return seq
}
