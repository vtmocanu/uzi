package agentsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ApplyAction is the concrete write Apply performs (or skips) for one role name. It
// is a REFINEMENT of the DiffAction preview: the DiffAction decides add/override/
// conflict/unchanged/remove (shared with M3 via computeDiff so preview and apply can
// never disagree), and the refinement below only disambiguates by the current row's
// scope which of two queries an override/remove actually runs.
type ApplyAction string

const (
	// ActionOverrideBuiltin — UPDATE an existing scope='builtin' row to origin='synced'.
	ActionOverrideBuiltin ApplyAction = "override-builtin"
	// ActionAddGlobal — INSERT a new scope='global', origin='synced' row + seed its allocation.
	ActionAddGlobal ApplyAction = "add-global"
	// ActionUpdateGlobal — UPDATE an existing scope='global', origin='synced' row in place.
	ActionUpdateGlobal ApplyAction = "update-global"
	// ActionConflict — a scope='global' admin row (origin != synced) shares the name: SKIP.
	ActionConflict ApplyAction = "skipped-conflict"
	// ActionUnchanged — the target row already has identical content and correct origin: no write.
	ActionUnchanged ApplyAction = "unchanged"
	// ActionDeprovisionGlobal — DELETE a synced-only global role removed from the source.
	ActionDeprovisionGlobal ApplyAction = "deprovision-global"
	// ActionResetBuiltin — reset an overridden builtin removed from the source to its embedded default.
	ActionResetBuiltin ApplyAction = "deprovision-builtin-reset"
	// ActionSkippedParse — a role that failed to parse at stage time; never applied.
	ActionSkippedParse ApplyAction = "skipped-parse"
)

// ApplyOutcome is the per-role result of an Apply pass, for the admin UI/logs.
type ApplyOutcome struct {
	Name   string      `json:"name"`
	Action ApplyAction `json:"action"`
	Detail string      `json:"detail,omitempty"`
}

// ApplyResult summarizes one Apply pass. AlreadyApplied is true when the staged
// snapshot's SHA already equals the recorded last-applied SHA (a no-op re-apply).
type ApplyResult struct {
	SHA            string         `json:"sha"`
	Applied        int            `json:"applied"`       // override + add + update
	Unchanged      int            `json:"unchanged"`     // no-write matches
	Conflicts      int            `json:"conflicts"`     // admin-global collisions skipped
	Deprovisioned  int            `json:"deprovisioned"` // deletes + builtin resets
	SkippedParse   int            `json:"skipped_parse"` // roles that never parsed
	AlreadyApplied bool           `json:"already_applied"`
	Outcomes       []ApplyOutcome `json:"outcomes"`
	Message        string         `json:"message"`
}

// plannedOp is one refined operation Apply executes inside the transaction. Def
// carries the synced role's fields (add/override/update); Row carries the current
// target row (override/update/deprovision) so the executor has its id.
type plannedOp struct {
	Name   string
	Action ApplyAction
	Def    agenttmpl.Definition
	Row    store.AgentTemplate
	Detail string
}

// ErrStaleApproval is returned by Apply when the SHA the admin approved no longer
// matches the currently-staged snapshot read INSIDE the apply transaction — a
// concurrent restage between review and apply, or nothing staged at all. It is the
// primary supply-chain control: the snapshot that gets written is bound to the exact
// one the admin reviewed. No writes occur; the handler maps it to 409.
var ErrStaleApproval = errors.New("agentsource: staged snapshot changed since it was reviewed")

// applyQuerier is the tx-scoped store surface Apply's executor needs. *store.Queries
// (bound to the transaction via store.New(tx)) satisfies it.
type applyQuerier interface {
	GetAgentSourceStaged(ctx context.Context) (store.AgentSourceStaged, error)
	ListAgentTemplates(ctx context.Context) ([]store.AgentTemplate, error)
	ApplySyncedOverrideBuiltin(ctx context.Context, arg store.ApplySyncedOverrideBuiltinParams) (store.AgentTemplate, error)
	InsertSyncedGlobalTemplate(ctx context.Context, arg store.InsertSyncedGlobalTemplateParams) (store.AgentTemplate, error)
	UpdateSyncedGlobalTemplate(ctx context.Context, arg store.UpdateSyncedGlobalTemplateParams) (store.AgentTemplate, error)
	DeleteSyncedGlobalTemplate(ctx context.Context, name string) (int64, error)
	ResetBuiltinAgentTemplate(ctx context.Context, arg store.ResetBuiltinAgentTemplateParams) (store.AgentTemplate, error)
	SeedSharedTemplateAllocationByName(ctx context.Context, name string) error
}

// Apply is the approve-and-apply gate (PRD #602 M4). It is the ONLY path that writes
// agent_templates from a sync — the interval reconcile only stages. It re-reads the
// staged snapshot AND the current templates AT APPLY TIME, INSIDE the transaction,
// binds the snapshot to the SHA the admin approved (expectedSHA), and re-classifies
// the staged snapshot's fetched role set against the live templates (never trusting
// the staged diff blob, which is a stale preview), then executes the four-case
// provenance-aware upsert plus de-provisioning in a SINGLE transaction: a genuine DB
// error rolls the WHOLE apply back (never a partial apply); a conflict is an expected
// per-role skip, recorded, not a rollback.
//
// expectedSHA is the fetched SHA of the snapshot the admin reviewed. The tx-read
// snapshot's FetchedSha MUST equal it, or Apply aborts with ErrStaleApproval and
// writes nothing — so a concurrent restage between review and apply (or an apply of a
// snapshot the admin never saw) can never apply an unreviewed snapshot. This bind is
// the primary supply-chain control and is why the staged read moved inside the tx.
//
// actor is the applying admin, recorded as updated_by on every written row (a null
// pgtype.UUID is stored as SQL NULL, which the FK allows).
func (r *Reconciler) Apply(ctx context.Context, actor pgtype.UUID, expectedSHA string) (ApplyResult, error) {
	if r.db == nil {
		return ApplyResult{}, errors.New("agentsource: Apply requires a transaction beginner")
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin apply tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	qtx := store.New(tx)

	// Read the staged snapshot INSIDE the tx and bind it to the reviewed SHA. A
	// mismatch (concurrent restage) or nothing staged aborts with ErrStaleApproval —
	// the rollback fires, so NOTHING is written and the handler returns 409. This
	// check runs BEFORE the already-applied short-circuit so a stale approval 409s
	// rather than silently no-oping.
	staged, err := qtx.GetAgentSourceStaged(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApplyResult{}, ErrStaleApproval
	}
	if err != nil {
		return ApplyResult{}, fmt.Errorf("read staged snapshot: %w", err)
	}
	if staged.FetchedSha == "" || staged.FetchedSha != expectedSHA {
		return ApplyResult{}, ErrStaleApproval
	}

	lastApplied, _ := r.settings.AgentSourceLastAppliedSHA(ctx)
	if staged.FetchedSha == lastApplied {
		return ApplyResult{
			SHA:            staged.FetchedSha,
			AlreadyApplied: true,
			Message:        "snapshot already applied",
		}, nil
	}

	// The parsed role set is the fetched CONTENT (an immutable snapshot). The
	// classification, though, is recomputed against the LIVE templates inside the tx.
	// An undecodable blob is a hard error (never "the source removed everything").
	defs, skipped, err := decodeStagedRoles(staged.Roles)
	if err != nil {
		return ApplyResult{}, err
	}

	current, err := qtx.ListAgentTemplates(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("read current templates: %w", err)
	}

	ops := planApply(defs, current)
	result, err := executeApply(ctx, qtx, ops, actor)
	if err != nil {
		// A genuine DB error: the deferred Rollback fires, so NOTHING is written.
		return ApplyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit apply: %w", err)
	}

	// Carry the parse-time skips into the result (they were never candidates to write).
	for _, s := range skipped {
		result.Outcomes = append(result.Outcomes, s)
		result.SkippedParse++
	}
	result.SHA = staged.FetchedSha
	result.Message = fmt.Sprintf(
		"applied %d, %d unchanged, %d conflict(s) skipped, %d de-provisioned, %d parse-skip",
		result.Applied, result.Unchanged, result.Conflicts, result.Deprovisioned, result.SkippedParse,
	)

	r.recordApplied(ctx, staged.FetchedSha, actor)
	return result, nil
}

// decodeStagedRoles splits a snapshot's roles blob into the successfully-parsed
// definitions (to classify + apply) and the parse-skip outcomes (carried into the
// result, never applied). An UNDECODABLE blob returns an error so Apply aborts and
// writes nothing: an empty def set classifies every origin='synced' row as a removal,
// so swallowing the unmarshal error would de-provision every synced template off a
// corrupt snapshot — the maximally destructive misread, not a "safe degrade".
func decodeStagedRoles(raw []byte) (defs []agenttmpl.Definition, skipped []ApplyOutcome, err error) {
	var roles []StagedRole
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &roles); uerr != nil {
			return nil, nil, fmt.Errorf("decode staged roles: %w", uerr)
		}
	}
	for _, role := range roles {
		if !role.OK {
			skipped = append(skipped, ApplyOutcome{
				Name:   role.Name,
				Action: ActionSkippedParse,
				Detail: role.Reason,
			})
			continue
		}
		defs = append(defs, agenttmpl.Definition{
			Name:        role.Name,
			Description: role.Description,
			Model:       role.Model,
			Tools:       role.Tools,
			PromptBody:  role.PromptBody,
		})
	}
	return defs, skipped, nil
}

// planApply refines the shared computeDiff classification into concrete write ops.
// It calls computeDiff (the SAME classifier M3's preview uses) for the action
// decision, then dispatches an override/remove to the right query by the current
// row's scope. Pure: it takes the fetched defs and the live rows, and returns the
// plan without touching the DB, so the classification is unit-testable in isolation.
func planApply(defs []agenttmpl.Definition, current []store.AgentTemplate) []plannedOp {
	shared := map[string]store.AgentTemplate{}
	for _, t := range current {
		if t.Scope == "user" {
			continue
		}
		shared[t.Name] = t
	}
	defByName := map[string]agenttmpl.Definition{}
	for _, d := range defs {
		defByName[d.Name] = d
	}

	diff := computeDiff(defs, current)
	ops := make([]plannedOp, 0, len(diff))
	for _, e := range diff {
		switch e.Action {
		case DiffAdd:
			ops = append(ops, plannedOp{
				Name: e.Name, Action: ActionAddGlobal, Def: defByName[e.Name], Detail: e.Detail,
			})
		case DiffOverride:
			row := shared[e.Name]
			if row.Scope == "builtin" {
				ops = append(ops, plannedOp{
					Name: e.Name, Action: ActionOverrideBuiltin, Def: defByName[e.Name], Row: row, Detail: e.Detail,
				})
			} else {
				ops = append(ops, plannedOp{
					Name: e.Name, Action: ActionUpdateGlobal, Def: defByName[e.Name], Row: row, Detail: e.Detail,
				})
			}
		case DiffConflict:
			ops = append(ops, plannedOp{
				Name: e.Name, Action: ActionConflict, Row: shared[e.Name],
				Detail: "collides with an admin global template; not overwritten",
			})
		case DiffUnchanged:
			ops = append(ops, plannedOp{Name: e.Name, Action: ActionUnchanged})
		case DiffRemove:
			row := shared[e.Name]
			if row.Scope == "builtin" {
				ops = append(ops, plannedOp{
					Name: e.Name, Action: ActionResetBuiltin, Row: row, Detail: e.Detail,
				})
			} else {
				ops = append(ops, plannedOp{
					Name: e.Name, Action: ActionDeprovisionGlobal, Row: row, Detail: e.Detail,
				})
			}
		}
	}
	return ops
}

// executeApply runs each planned op against the tx-scoped querier and tallies the
// result. A conflict/unchanged/parse-skip performs no write. Any DB error is
// returned so the caller rolls back the WHOLE apply (fail-safe: never partial).
func executeApply(ctx context.Context, q applyQuerier, ops []plannedOp, actor pgtype.UUID) (ApplyResult, error) {
	var res ApplyResult
	for _, op := range ops {
		out := ApplyOutcome{Name: op.Name, Action: op.Action, Detail: op.Detail}
		switch op.Action {
		case ActionOverrideBuiltin:
			model, tools, err := storeColumns(op.Def)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("encode %q: %w", op.Name, err)
			}
			if _, err := q.ApplySyncedOverrideBuiltin(ctx, store.ApplySyncedOverrideBuiltinParams{
				Description: op.Def.Description, Model: model, Tools: tools,
				PromptBody: op.Def.PromptBody, UpdatedBy: actor, ID: op.Row.ID,
			}); err != nil {
				return ApplyResult{}, fmt.Errorf("override builtin %q: %w", op.Name, err)
			}
			res.Applied++

		case ActionAddGlobal:
			model, tools, err := storeColumns(op.Def)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("encode %q: %w", op.Name, err)
			}
			if _, err := q.InsertSyncedGlobalTemplate(ctx, store.InsertSyncedGlobalTemplateParams{
				Name: op.Def.Name, Description: op.Def.Description, Model: model, Tools: tools,
				PromptBody: op.Def.PromptBody, UpdatedBy: actor,
			}); err != nil {
				return ApplyResult{}, fmt.Errorf("insert global %q: %w", op.Name, err)
			}
			// Allocation, not table presence, is what makes a template reach a claim.
			if err := q.SeedSharedTemplateAllocationByName(ctx, op.Def.Name); err != nil {
				return ApplyResult{}, fmt.Errorf("seed allocation %q: %w", op.Name, err)
			}
			res.Applied++

		case ActionUpdateGlobal:
			model, tools, err := storeColumns(op.Def)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("encode %q: %w", op.Name, err)
			}
			if _, err := q.UpdateSyncedGlobalTemplate(ctx, store.UpdateSyncedGlobalTemplateParams{
				Description: op.Def.Description, Model: model, Tools: tools,
				PromptBody: op.Def.PromptBody, UpdatedBy: actor, Name: op.Def.Name,
			}); err != nil {
				return ApplyResult{}, fmt.Errorf("update global %q: %w", op.Name, err)
			}
			res.Applied++

		case ActionDeprovisionGlobal:
			if _, err := q.DeleteSyncedGlobalTemplate(ctx, op.Name); err != nil {
				return ApplyResult{}, fmt.Errorf("delete global %q: %w", op.Name, err)
			}
			res.Deprovisioned++

		case ActionResetBuiltin:
			// Reset an overridden builtin to its EMBEDDED default (origin back to
			// 'embedded'). The embedded body is the shipped definition, not the row.
			def, ok := agenttmpl.BuiltinByName(op.Name)
			if !ok {
				// No embedded default exists (should not happen for a scope='builtin'
				// row); leave it as-is rather than blank it, and record why.
				out.Detail = "no embedded default found; left as-is"
				res.Outcomes = append(res.Outcomes, out)
				continue
			}
			model, tools, err := storeColumns(def)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("encode embedded %q: %w", op.Name, err)
			}
			if _, err := q.ResetBuiltinAgentTemplate(ctx, store.ResetBuiltinAgentTemplateParams{
				Description: def.Description, Model: model, Tools: tools,
				PromptBody: def.PromptBody, UpdatedBy: actor, ID: op.Row.ID,
			}); err != nil {
				return ApplyResult{}, fmt.Errorf("reset builtin %q: %w", op.Name, err)
			}
			res.Deprovisioned++

		case ActionConflict:
			res.Conflicts++

		case ActionUnchanged:
			res.Unchanged++
		}
		res.Outcomes = append(res.Outcomes, out)
	}
	return res, nil
}

// recordApplied writes the engine-managed last-applied keys (SHA + timestamp) and
// invalidates the settings cache so the next status read is fresh. Best-effort: a
// write failure is logged, never fatal — the apply already committed, so at worst the
// status panel and the pending flag lag until the next apply.
func (r *Reconciler) recordApplied(ctx context.Context, sha string, actor pgtype.UUID) {
	writes := []store.UpsertAppSettingParams{
		{Key: settings.KeyAgentSourceLastAppliedAt, Value: r.now().UTC().Format(time.RFC3339), UpdatedBy: actor},
		{Key: settings.KeyAgentSourceLastAppliedSHA, Value: sha, UpdatedBy: actor},
	}
	for _, w := range writes {
		if _, err := r.store.UpsertAppSetting(ctx, w); err != nil {
			r.logger.Error("agentsource: persist last-applied status", "key", w.Key, "error", err)
		}
	}
	r.settings.Invalidate()
}

// storeColumns converts a synced role's Definition into the model/tools column types
// on write: Model an empty string → NULL pgtype.Text, and Tools a non-empty slice →
// its jsonb encoding (an empty/nil slice → NULL = inherit-all). The agentsource-local
// twin of the handler's storeColumns (that one is not importable here).
func storeColumns(def agenttmpl.Definition) (pgtype.Text, []byte, error) {
	var model pgtype.Text
	if def.Model != "" {
		model = pgtype.Text{String: def.Model, Valid: true}
	}
	var tools []byte
	if len(def.Tools) > 0 {
		b, err := json.Marshal(def.Tools)
		if err != nil {
			return pgtype.Text{}, nil, err
		}
		tools = b
	}
	return model, tools, nil
}
