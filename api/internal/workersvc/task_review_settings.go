package workersvc

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TaskReviewInput is the owner's stored task-review preferences for ONE harness, as read from the
// users row (nil = unset). It is the single input of ResolveTaskReview, shared by the review claim
// (assembleClaim) and the effective-settings prediction (predictLead), so the two never drift.
// The effort preference joins this struct with the task-review effort slice (PRD #1570 M2).
type TaskReviewInput struct {
	// Model is the stored review model for the harness (users.task_review_model for Claude,
	// users.codex_task_review_model for Codex).
	Model *string
}

// TaskReviewResolution is what a task-review pass on one harness will use.
type TaskReviewResolution struct {
	// Model is the model the review runs on; "" means the SDK decides (the Claude built-in, which
	// sends no model). ModelSource is agenttmpl.WorkerDefaultSourceUserDefault or
	// agenttmpl.WorkerDefaultSourceBuiltin.
	Model       string
	ModelSource string
	// StoredModel echoes the input preference (nil = unset), including an invalid one the
	// resolution ignored, so a settings cell can show what the owner saved.
	StoredModel *string
	// BuiltinModel is the review model an unset preference resolves to for the harness.
	BuiltinModel string
}

// builtinTaskReviewModel is the harness's built-in review model.
func builtinTaskReviewModel(harness Harness) string {
	if harness == HarnessCodex {
		return agenttmpl.CodexTaskReviewModel
	}
	return agenttmpl.ClaudeTaskReviewModel
}

// ResolveTaskReview resolves the review model of a task-review run on harness. The stored value is
// validated AGAIN here, not trusted: a curated Codex model can leave agenttmpl.CodexRoleModels after
// it was saved, and the worker's advice renderer would then run the provider default instead of the
// built-in, so an invalid or blank value falls back to the built-in with source builtin. Claude
// accepts any model ValidateClaudeLaneModel accepts (custom ids allowed, Codex role ids refused);
// Codex accepts only agenttmpl.CodexRoleModels.
func ResolveTaskReview(harness Harness, in TaskReviewInput) TaskReviewResolution {
	builtin := builtinTaskReviewModel(harness)
	out := TaskReviewResolution{Model: builtin, ModelSource: agenttmpl.WorkerDefaultSourceBuiltin, BuiltinModel: builtin, StoredModel: in.Model}
	if in.Model == nil {
		return out
	}
	if m, ok := validTaskReviewModel(harness, *in.Model); ok {
		out.Model, out.ModelSource = m, agenttmpl.WorkerDefaultSourceUserDefault
	}
	return out
}

// validTaskReviewModel returns the trimmed model and whether it is a usable, non-blank review model
// for the harness.
func validTaskReviewModel(harness Harness, raw string) (string, bool) {
	if harness == HarnessCodex {
		m := strings.TrimSpace(raw)
		if m == "" || !agenttmpl.CodexRoleModels[m] {
			return "", false
		}
		return m, true
	}
	m, err := agenttmpl.ValidateClaudeLaneModel(raw)
	if err != nil || m == "" {
		return "", false
	}
	return m, true
}

// taskReviewInput selects the harness's column of the owner's task-review row.
func taskReviewInput(harness Harness, row store.GetUserTaskReviewSettingsRow) TaskReviewInput {
	col := row.TaskReviewModel
	if harness == HarnessCodex {
		col = row.CodexTaskReviewModel
	}
	return TaskReviewInput{Model: textPtr(col)}
}

// resolveUserTaskReview reads the owner's task-review row and resolves it for harness.
func (s *Service) resolveUserTaskReview(ctx context.Context, userID uuid.UUID, harness Harness) (TaskReviewResolution, error) {
	row, err := s.q.GetUserTaskReviewSettings(ctx, userID)
	if err != nil {
		return TaskReviewResolution{}, fmt.Errorf("task review settings lookup: %w", err)
	}
	return ResolveTaskReview(harness, taskReviewInput(harness, row)), nil
}
