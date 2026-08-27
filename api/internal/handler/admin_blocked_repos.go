package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
)

// AdminListBlockedRepos is the admin cross-user blocked-repos read (PRD #66 M9, D8).
// It is ADMIN-ONLY, mounted under the admin READ group (RequireUser + RequireAdminRO),
// backed by the UNSCOPED AdminListReposWithPrivilege query (precedent AdminListRuns).
//
// For each repo it computes the block state from the STORED privilege_report — cheap
// and display-appropriate, unlike M3's live re-sweep — applying the admin per-repo
// override through the SINGLE shared privcheck.DowngradeOverridden, then Blocks(). It
// returns only the repos in the admin's action set: those that are blocked OR carry an
// active override. Because the override never waives protection_unreadable (D8/R8), an
// overridden repo whose only finding is unreadable protection still reports Blocked=true.
//
// R1 caveat: a connection whose privilege_status is NULL was never checked (e.g. under
// UZI_PRIVILEGE_CHECK_INTERVAL=0), so its repos read Blocked=false here — that is
// "unknown", not "none blocked". The envelope's ChecksUnknown flag surfaces that so the
// UI can say the list may be incomplete rather than rendering empty as clean.
func (h *Handler) AdminListBlockedRepos(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.AdminListReposWithPrivilege(r.Context())
	if err != nil {
		slog.Error("admin list blocked repos", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Parse each connection's stored report exactly once — a factory can have many
	// repos on one connection, and the blob is the same for all of them.
	reports := map[uuid.UUID]*privcheck.Report{}
	parsed := map[uuid.UUID]bool{}

	out := make([]apitypes.BlockedRepoDTO, 0, len(rows))
	checksUnknown := false
	for _, row := range rows {
		// R1: any connection never checked makes the whole list "possibly incomplete",
		// computed across ALL rows before the blocked-or-overridden filter below.
		if !row.PrivilegeStatus.Valid {
			checksUnknown = true
		}

		if !parsed[row.ConnectionID] {
			reports[row.ConnectionID] = parsePrivilegeReport(row.PrivilegeReport, row.ConnectionID)
			parsed[row.ConnectionID] = true
		}

		overridden := row.GuardrailOverrideReason.Valid

		var blocked bool
		// Initialized to non-nil so an overridden-but-never-swept repo (kept because
		// overridden=true, but rep is nil) still serializes block_messages as [] rather
		// than null — the BlockedRepoDTO "Never null" contract the non-nullable TS
		// string[] relies on.
		msgs := []string{}
		if rep := reports[row.ConnectionID]; rep != nil {
			for _, rr := range rep.Repos {
				if rr.RepoID != row.ID.String() {
					continue
				}
				downgraded := privcheck.DowngradeOverridden(rr.Findings, overridden)
				blocked = privcheck.RepoReport{Findings: downgraded}.Blocks()
				// Reuse the single BlockMessages primitive rather than re-filtering
				// SeverityBlock here, so "what counts as a block reason" lives in one
				// place with the gate's 422 body.
				msgs = privcheck.GuardResult{Findings: downgraded}.BlockMessages()
				break
			}
		}

		// The admin's action set: a repo they can allow-anyway (blocked) or revoke
		// (overridden). Everything else is omitted.
		if !blocked && !overridden {
			continue
		}

		dto := apitypes.BlockedRepoDTO{
			ID:            row.ID.String(),
			Path:          row.PathWithNamespace,
			OwnerID:       row.OwnerID.String(),
			OwnerEmail:    row.OwnerEmail,
			ForgeType:     row.ForgeType,
			Blocked:       blocked,
			BlockMessages: msgs,
		}
		if overridden {
			ov := &apitypes.GuardrailOverrideDTO{Reason: row.GuardrailOverrideReason.String}
			// By is the actor's email when resolvable (LEFT JOIN), else the raw uuid —
			// the "resolve if easy, else id" contract (D8).
			if row.OverrideByEmail.Valid {
				ov.By = row.OverrideByEmail.String
			} else if row.GuardrailOverrideBy.Valid {
				ov.By = uuid.UUID(row.GuardrailOverrideBy.Bytes).String()
			}
			if row.GuardrailOverrideAt.Valid {
				ov.At = row.GuardrailOverrideAt.Time
			}
			dto.Override = ov
		}
		if row.PrivilegeStatus.Valid {
			s := row.PrivilegeStatus.String
			dto.PrivilegeStatus = &s
		}
		if row.PrivilegeCheckedAt.Valid {
			t := row.PrivilegeCheckedAt.Time
			dto.PrivilegeCheckedAt = &t
		}
		out = append(out, dto)
	}

	httpx.JSON(w, http.StatusOK, apitypes.AdminBlockedReposDTO{Repos: out, ChecksUnknown: checksUnknown})
}
