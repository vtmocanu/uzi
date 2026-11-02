package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

const crossCheckHistoryEnvelopeLimit = 2 * 1024 * 1024

func (h *Handler) CrossCheckHistoryAvailability(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, 401, "authentication required")
		return
	}
	available, err := h.q.CrossCheckHistoryAvailability(r.Context(), user.ID)
	if err != nil {
		httpx.Error(w, 500, "cross-check history unavailable")
		return
	}
	writeCrossCheckHistory(w, apitypes.CrossCheckHistoryAvailabilityDTO{Available: available})
}

func (h *Handler) ListCrossCheckHistory(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, 401, "authentication required")
		return
	}
	p, err := parseCrossCheckHistoryRequest(r)
	if err != nil {
		httpx.Error(w, 400, errCrossCheckHistoryRequest.Error())
		return
	}
	tx, err := h.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		httpx.Error(w, 500, "cross-check history unavailable")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	q := h.q.WithTx(tx)
	args := store.CrossCheckHistoryGroupsParams{
		OwnerID: user.ID, ParentIds: []uuid.UUID{}, Lifecycle: p.lifecycle, StageFilter: p.stage, OutcomeFilter: p.outcome,
		GroupLimit: p.limit + 1, BeforeCreated: pgtype.Timestamptz{Time: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), Valid: true},
	}
	if p.cursor != nil {
		t, _ := time.Parse(time.RFC3339Nano, p.cursor.Time)
		args.BeforeCreated = pgtype.Timestamptz{Time: t, Valid: true}
		args.BeforeID = p.cursor.Parent
	}
	boundaries := map[uuid.UUID]int64{}
	for _, c := range p.continuations {
		args.ParentIds = append(args.ParentIds, c.Parent)
		boundaries[c.Parent] = int64(c.Rank)*2147483648 + int64(c.Round)
	}
	if len(p.continuations) > 0 {
		args.GroupLimit = 8
	}
	groups, err := q.CrossCheckHistoryGroups(r.Context(), args)
	if err != nil {
		httpx.Error(w, 500, "cross-check history unavailable")
		return
	}
	if len(p.continuations) > 0 && len(groups) != len(p.continuations) {
		httpx.Error(w, 404, "cross-check history unavailable")
		return
	}
	response := apitypes.CrossCheckHistoryDTO{Groups: []apitypes.CrossCheckHistoryGroupDTO{}}
	if len(p.continuations) == 0 && len(groups) > int(p.limit) {
		groups = groups[:p.limit]
		last := groups[len(groups)-1]
		response.NextCursor = encodeCrossCheckHistoryCursor(crossCheckHistoryCursor{
			Version: 1, Kind: "group", Filter: p.filter(), Parent: last.LeadRunID, Time: last.LatestAt.Time.UTC().Format(time.RFC3339Nano),
		})
	}
	attemptsArg := store.CrossCheckHistoryAttemptsParams{
		OwnerID: user.ID, StageFilter: p.stage, OutcomeFilter: p.outcome, AttemptLimit: p.attemptLimit + 1,
		ParentIds: []uuid.UUID{}, BeforeKeys: []int64{},
	}
	for _, g := range groups {
		if g.Active != (p.lifecycle == "active") {
			continue
		}
		boundary := int64(2) * 2147483648
		if b, ok := boundaries[g.LeadRunID]; ok {
			boundary = b
		}
		attemptsArg.ParentIds = append(attemptsArg.ParentIds, g.LeadRunID)
		attemptsArg.BeforeKeys = append(attemptsArg.BeforeKeys, boundary)
	}
	// Always one batched attempts read, including empty and continuation-only pages.
	attempts, err := q.CrossCheckHistoryAttempts(r.Context(), attemptsArg)
	if err != nil {
		httpx.Error(w, 500, "cross-check history unavailable")
		return
	}
	byParent := map[uuid.UUID][]store.CrossCheckHistoryAttemptsRow{}
	for _, a := range attempts {
		byParent[a.LeadRunID] = append(byParent[a.LeadRunID], a)
	}
	for _, g := range groups {
		dto := apitypes.CrossCheckHistoryGroupDTO{
			ParentID: g.LeadRunID.String(), ParentTitle: crossCheckHistoryText(g.Title, 512),
			ParentStatus: historyStatus(g.ParentStatus), LatestAt: g.LatestAt.Time, Active: g.Active,
			TotalAttempts: g.TotalAttempts, MatchingAttempts: g.MatchingAttempts, ActiveAttempts: g.ActiveAttempts,
			Attempts: []apitypes.CrossCheckHistoryAttemptDTO{}, RefreshNeeded: g.Active != (p.lifecycle == "active"),
		}
		rows := byParent[g.LeadRunID]
		if len(rows) > int(p.attemptLimit) {
			rows = rows[:p.attemptLimit]
			oldest := rows[len(rows)-1]
			dto.OlderCursor = encodeCrossCheckHistoryCursor(crossCheckHistoryCursor{
				Version: 1, Kind: "attempt", Filter: p.filter(), Parent: g.LeadRunID, Rank: oldest.StageRank, Round: oldest.Round,
			})
			dto.Omission = "Earlier rounds omitted: load older attempts"
		}
		visibleActive := int64(0)
		// The SQL window is newest-first (Code, then descending Plan). Reverse only
		// after deriving the older boundary from the oldest delivered matching key.
		for i := len(rows) - 1; i >= 0; i-- {
			dto.Attempts = append(dto.Attempts, crossCheckHistoryAttempt(rows[i]))
			if rows[i].Active {
				visibleActive++
			}
		}
		dto.ActiveHidden = g.ActiveAttempts - visibleActive
		if dto.ActiveHidden > 0 {
			dto.ActiveContext = "Active attempts outside this slice or filter"
		}
		response.Groups = append(response.Groups, dto)
	}
	if err = tx.Commit(r.Context()); err != nil {
		httpx.Error(w, 500, "cross-check history unavailable")
		return
	}
	writeCrossCheckHistory(w, response)
}

// All untrusted metadata is SQL-bounded before transfer. Normalize and scrub the
// entire accepted value before applying this UTF-8 byte cap.
func crossCheckHistoryText(s string, max int) string {
	s = secretscrub.Scrub(termsafe.CellText(s))
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return strings.TrimSpace(s[:max])
}

func historyStatus(s string) string {
	if historyMember(s, "queued", "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait", "pool_wait", "paused", "recovery_wait", "completed", "failed", "cancelled") {
		return s
	}
	return "unknown"
}

func historyOptional(s string) *string {
	if s == "" {
		return nil
	}
	v := crossCheckHistoryText(s, 128)
	if v == "" {
		return nil
	}
	return &v
}

func crossCheckHistoryAttempt(row store.CrossCheckHistoryAttemptsRow) apitypes.CrossCheckHistoryAttemptDTO {
	dto := apitypes.CrossCheckHistoryAttemptDTO{
		Stage: row.Stage, Round: row.Round, Result: row.Result, Review: "none", Execution: "unknown",
		Reason: "Cause not recorded for this attempt", CreatedAt: row.CreatedAt.Time, DeadlineAt: row.DeadlineAt.Time,
		ChildContext: "No checker run available", Model: historyOptional(row.Model), Effort: historyOptional(row.Effort),
		Historical: row.Historical, Active: row.Active, Effect: "Effect not recorded", NextAction: "No action recorded",
	}
	if !historyMember(dto.Stage, "plan", "code") {
		dto.Stage = "unknown"
	}
	if row.Stage == "plan" {
		if !historyMember(dto.Result, "pending", "approve", "revise", "block", "failed") {
			dto.Result = "unknown"
		}
		switch {
		case dto.Result == "approve" && row.ReasonClass == "approve":
			dto.Review = "approved"
		case dto.Result == "revise" && row.ReasonClass == "revise":
			dto.Review = "changes_requested"
		case dto.Result == "block" && row.ReasonClass == "block":
			dto.Review = "blocked"
		case dto.Result == "failed" && row.ReasonClass == "approved_not_stored":
			dto.Review = "approved"
			dto.Historical = true
		}
	} else {
		if !historyMember(dto.Result, "pending", "completed", "failed") {
			dto.Result = "unknown"
		}
		if dto.Result == "completed" && row.ReasonClass == "" && row.FindingsCount >= 0 {
			dto.Review = "approved"
			if row.FindingsCount > 0 {
				dto.Review = "changes_requested"
			}
		}
	}
	if row.FindingsCount >= 0 {
		count := row.FindingsCount
		dto.FindingsCount = &count
	}
	if row.DecidedAt.Valid {
		t := row.DecidedAt.Time
		dto.DecidedAt = &t
	}
	if row.InterruptedAt.Valid {
		t := row.InterruptedAt.Time
		dto.InterruptedAt = &t
	}
	reasons := map[string]string{
		"approve": "Review approved", "revise": "Changes requested", "block": "Review blocked",
		"malformed": "Checker response could not be read", "model_error": "Checker model failed",
		"model_timeout": "Checker model timed out", "checker_unavailable": "Checker unavailable",
		"confinement_failed": "Checker confinement failed", "timed_out": "Checker timed out",
		"superseded": "Attempt replaced", "approved_not_stored": "Approval was not stored",
		"worker_unsupported": "Worker does not support this check", "snapshot_failed": "Snapshot could not be created",
	}
	if label, ok := reasons[row.ReasonClass]; ok {
		dto.Reason = label
	}
	if dto.Result == "pending" {
		dto.Execution = "pending"
	}
	if dto.Result == "failed" {
		dto.Execution = "didnt_finish"
	}
	if !row.ChildID.Valid && dto.Result == "failed" && historyMember(row.ReasonClass, "worker_unsupported", "snapshot_failed") {
		dto.Execution = "couldnt_start"
	}
	if row.ChildID.Valid && row.ChildStatus.Valid {
		id := uuid.UUID(row.ChildID.Bytes).String()
		status := historyStatus(row.ChildStatus.String)
		dto.ChildID = &id
		dto.ChildStatus = &status
		dto.ChildContext = "Checker run available"
		switch status {
		case "completed":
			if dto.Review != "none" {
				dto.Execution = "finished"
			}
		case "failed", "cancelled":
			dto.Execution = "didnt_finish"
		default:
			if status != "unknown" {
				dto.Execution = "running"
			}
		}
	}
	// A recorded review survives missing child metadata, but cannot certify execution.
	// Persisted interruption/replacement takes precedence over a completed child.
	if row.Category == "replaced" {
		dto.Execution = "replaced"
		if row.InterruptedAt.Valid {
			dto.Execution = "interrupted"
		}
		dto.Historical = true
	}
	return dto
}

func writeCrossCheckHistory(w http.ResponseWriter, value any) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw)+1 > crossCheckHistoryEnvelopeLimit {
		httpx.Error(w, 500, "cross-check history unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(raw, '\n'))
}
