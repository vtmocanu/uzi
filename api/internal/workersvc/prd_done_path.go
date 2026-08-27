package workersvc

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/prdpath"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// clampWirePRDDonePath narrows a worker's declared PRD path to what may be stored
// (PRD #72 M4). It DROPS AND WARNS; it never returns an error.
//
// That shape is a requirement, not a style choice. This runs on the `completed`
// transition — the run's TERMINAL report, sent after the branch is already pushed
// and the MR already opened. Failing it would 4xx a report the worker cannot
// usefully retry and strand the run with its work published and no terminal state.
// `runningStateParams` states the principle in its own doc comment ("a terminal
// report is the one call a worker must not be able to fail on a technicality"),
// and it deliberately DOES return an error for a bad roster — because that is a
// `running` report, where failing costs nothing. `clampWireFixVerdict` in this
// package is the existing clamp-shaped precedent on this same path; follow it,
// never `validateRepoAgents`.
//
// This is also the AUTHORITATIVE run-kind gate and the authoritative validator.
// The worker gates the tool schema and clamps length, but the worker is untrusted
// input reachable with a Bearer join token, and this field drives a forge write
// against an issue description. `runs.kind` is NOT NULL, so the check here needs
// no default.
func clampWirePRDDonePath(run store.Run, p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	// Decision 13. A `self_improve` run is the one most likely to trip this into a
	// wrong write: it runs against uzi's own repo, which HAS a prds/ directory, and
	// its issue is a stable container reused across cycles whose description is the
	// accumulated improve_uzi backlog. A `ci_fix` run carries no issue at all.
	if run.Kind != "issue" {
		slog.Warn("dropping declared PRD path: not an issue run",
			"run_id", run.ID, "kind", run.Kind, "declared", *p)
		return pgtype.Text{}
	}
	if err := prdpath.Validate(*p); err != nil {
		// The path is agent/user data and explicitly not a secret, so log it: an
		// operator debugging "why did the link not get patched" needs to see what
		// was actually declared, not just that something was refused.
		slog.Warn("dropping declared PRD path: failed validation",
			"run_id", run.ID, "declared", *p, "error", err)
		return pgtype.Text{}
	}
	return pgtype.Text{String: *p, Valid: true}
}
