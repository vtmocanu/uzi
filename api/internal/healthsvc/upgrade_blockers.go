package healthsvc

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// enrichCapacityBlockers uses only owners in the retained evidence. One batched
// projection covers their draining hosted workers; failure leaves severity intact.
func (s *Service) enrichCapacityBlockers(ctx context.Context, evidence *waitEvidence, workers []store.ListAllWorkersRow, workersErr error) {
	if len(evidence.owners) == 0 {
		return
	}
	owners := make(map[uuid.UUID]bool)
	for _, owner := range evidence.owners {
		owners[owner] = true
	}
	var ids []uuid.UUID
	workerOwners := make(map[uuid.UUID]uuid.UUID)
	for _, row := range workers {
		w := row.Worker
		if owners[w.UserID] && w.Kind == "hosted" && w.DrainingSince.Valid {
			ids = append(ids, w.ID)
			workerOwners[w.ID] = w.UserID
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	var rows []store.ListWorkerUpgradeBlockersRow
	err := workersErr
	if err == nil && len(ids) > 0 {
		rows, err = s.cfg.Store.ListWorkerUpgradeBlockers(ctx, ids)
	}
	// Facts have the existing safe96 ceiling. A tighter per-fact budget leaves
	// room for the retained waiting UUID/owner/age and one complete blocker action.
	fact := func(t pgtype.Text, limit int) pgtype.Text {
		if t.Valid {
			r := []rune(safe(t.String))
			if len(r) > limit {
				r = append(r[:limit-1], '…')
			}
			t.String = string(r)
		}
		return t
	}
	for i := range rows {
		rows[i].Kind = fact(rows[i].Kind, 24)
		rows[i].Status = fact(rows[i].Status, 24)
		if rows[i].HealthReason.Valid && (apitypes.IsCodexAdmissionReason(rows[i].HealthReason.String) || rows[i].HealthReason.String == apitypes.AccountCooldownReason) {
			rows[i].HealthReason.String = "waiting: admission held"
		}
		rows[i].HealthReason = fact(rows[i].HealthReason, 24)
		rows[i].HoldReason = fact(rows[i].HoldReason, 24)
		rows[i].RecoveryWaitCause = fact(rows[i].RecoveryWaitCause, 32)
	}
	grouped := workersvc.GroupUpgradeBlockers(rows)
	type ownerBlockers struct {
		first       string
		count       int64
		unavailable bool
	}
	byOwner := make(map[uuid.UUID]ownerBlockers)
	for _, id := range ids {
		owner := workerOwners[id]
		ob := byOwner[owner]
		b, found := grouped[id]
		ob.unavailable = ob.unavailable || err != nil || !found || b.Owner != owner || b.Unavailable
		if found && b.Owner == owner {
			ob.count += b.Count
			if ob.first == "" && len(b.Runs) > 0 {
				ob.first = b.Runs[0]
			}
		}
		byOwner[owner] = ob
	}
	for i, owner := range evidence.owners {
		ob := byOwner[owner]
		suffix := "; assigned run blockers unavailable"
		switch {
		case ob.first != "":
			suffix = "; blocker " + ob.first + fmt.Sprintf("; %d other safe blocker(s) omitted", ob.count-1)
			suffix += "; enables continuation; release requires terminal or unassignment"
			if ob.unavailable {
				suffix += "; other blockers unavailable"
			}
		case err == nil && !ob.unavailable:
			suffix = "; no assigned nonterminal run blockers on draining hosted workers; custody clearance unestablished"
		}
		// Spend the remaining budget on the stored waiting reason, retaining the
		// complete identities, age and blocker action rather than clipping an action.
		prefix, reason, _ := strings.Cut(evidence.rows[i].Value, "; stored reason ")
		prefix += "; stored reason "
		budget := 512 - len([]rune(prefix)) - len([]rune(suffix))
		if budget < 1 {
			suffix = "; assigned run blockers unavailable; inspect worker and runs"
			budget = 512 - len([]rune(prefix)) - len([]rune(suffix))
		}
		if r := []rune(reason); len(r) > budget {
			reason = string(r[:budget-1]) + "…"
		}
		evidence.rows[i].Value = prefix + reason + suffix
	}
}
