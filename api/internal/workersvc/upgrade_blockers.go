package workersvc

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// UpgradeBlockers is display evidence only; actions enable continuation. Assignment
// stops blocking only at terminal status or assignment removal, independently of custody.
type UpgradeBlockers struct {
	Owner       uuid.UUID
	Count       int64
	Runs        []string
	Unavailable bool
}

// GroupUpgradeBlockers accepts the safe projection, retaining mismatch as an
// unavailable signal without rendering any foreign identifiers or counts.
func GroupUpgradeBlockers(rows []store.ListWorkerUpgradeBlockersRow) map[uuid.UUID]UpgradeBlockers {
	grouped := make(map[uuid.UUID][]store.ListWorkerUpgradeBlockersRow)
	for _, row := range rows {
		grouped[row.WorkerID] = append(grouped[row.WorkerID], row)
	}
	out := make(map[uuid.UUID]UpgradeBlockers, len(grouped))
	for id, group := range grouped {
		sort.Slice(group, func(i, j int) bool {
			return uuid.UUID(group[i].RunID.Bytes).String() < uuid.UUID(group[j].RunID.Bytes).String()
		})
		b := UpgradeBlockers{Owner: group[0].WorkerOwner, Count: group[0].SafeCount}
		for _, row := range group {
			b.Unavailable = b.Unavailable || row.OwnerMismatch
			if row.RunID.Valid && len(b.Runs) < 5 {
				b.Runs = append(b.Runs, fmt.Sprintf("run %s (%s, %s): %s", uuid.UUID(row.RunID.Bytes), blockerFact(row.Kind), blockerFact(row.Status), blockerAction(row)))
			}
		}
		out[id] = b
	}
	return out
}

func (b UpgradeBlockers) Summary() string {
	parts := append([]string(nil), b.Runs...)
	if b.Count > int64(len(b.Runs)) {
		parts = append(parts, fmt.Sprintf("%d additional assigned run(s) omitted", b.Count-int64(len(b.Runs))))
	}
	if b.Unavailable || b.Runs == nil && b.Count != 0 {
		parts = append(parts, "Assigned run blockers unavailable; inspect worker and runs")
	} else if len(b.Runs) == 0 {
		parts = append(parts, "No assigned nonterminal runs; custody clearance is not established")
	}
	parts = append(parts, "Actions enable continuation; assignment releases only when the run ends or its assignment is removed")
	return strings.Join(parts, "; ")
}

func blockerFact(t pgtype.Text) string {
	if !t.Valid {
		return "unavailable"
	}
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, t.String)
	r := []rune(strings.TrimSpace(s))
	if len(r) > 96 {
		r = append(r[:95], '…')
	}
	if len(r) == 0 {
		return "unavailable"
	}
	return string(r)
}

func blockerTime(t pgtype.Timestamptz) string {
	if !t.Valid || t.InfinityModifier != pgtype.Finite || t.Time.IsZero() {
		return "unavailable"
	}
	return t.Time.UTC().Format(time.RFC3339)
}

func blockerAction(r store.ListWorkerUpgradeBlockersRow) string {
	switch r.Status.String {
	case "awaiting_approval":
		return "owner approves or reviews the plan"
	case "awaiting_input":
		return "owner answers the input request"
	case "awaiting_followup":
		return "owner follows up or ends the run"
	case "paused":
		return "owner resumes the run"
	case "claimed", "running":
		return "run must finish"
	case "pool_wait":
		return "a pooled token becomes spendable, or the owner adds or selects a usable token / runs `uzi run resume-now`"
	case "limit_wait":
		return fmt.Sprintf("retry_not_before %s gates promotion; limit reset %s is informational; owner may inspect or resume; no roll time promised", blockerTime(r.RetryNotBefore), blockerTime(r.LimitResetsAt))
	case "recovery_wait":
		cause := blockerFact(r.RecoveryWaitCause)
		switch r.RecoveryWaitCause.String {
		case recoveryCauseCodexAccountUnavailable:
			return fmt.Sprintf("recorded cause %s; wait for the same Codex account to become available; inspect account recovery; no timer", cause)
		case "worker_requeue_exhausted":
			return fmt.Sprintf("recorded cause %s; owner reviews recovery evidence and chooses Resume; no timer", cause)
		}
		action := "owner action if needed: inspect run"
		if r.RecoveryWaitCause.Valid && r.RecoveryWaitCause.String == recoveryCauseVaultLocked {
			action = "owner unlocks the vault"
		}
		if !r.RecoveryRetryNotBefore.Valid || r.RecoveryRetryNotBefore.InfinityModifier != pgtype.Finite || r.RecoveryRetryNotBefore.Time.IsZero() {
			return fmt.Sprintf("retry timing unavailable; recorded cause %s; %s", cause, action)
		}
		return fmt.Sprintf("automatic retry not before %s; recorded cause %s; %s", blockerTime(r.RecoveryRetryNotBefore), cause, action)
	case "queued":
		return fmt.Sprintf("own continuation must become claimable; recorded reason %s; hold %s; exact fence unavailable, inspect run", blockerFact(r.HealthReason), blockerFact(r.HoldReason))
	default:
		return "releasing action unavailable; inspect run"
	}
}
