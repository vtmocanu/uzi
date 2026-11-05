package setupcmd

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/provisionsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// outcome maps a provisionsvc error to a report status, a safe message and the
// reasons a refusal carries (the same reasons the web UI shows as 422 bodies).
func (r *runner) outcome(err error) (status, msg string, findings []string) {
	var pe *provisionsvc.Error
	if !errors.As(err, &pe) {
		return StatusFailed, "internal error", nil
	}
	switch pe.Kind {
	case provisionsvc.KindOverPrivileged:
		return StatusRefused, r.clean(pe.Message), cleanAll(r, pe.Violations)
	case provisionsvc.KindGuardBlocked:
		return StatusRefused, r.clean(pe.Message), cleanAll(r, blockMessages(pe.Guard))
	default:
		return StatusFailed, r.clean(pe.Message), nil
	}
}

func cleanAll(r *runner, in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = r.clean(s)
	}
	return out
}

// blockMessages lists the messages of the SeverityBlock findings only: an
// overridden or warn finding is not a reason the repo was refused.
func blockMessages(res privcheck.GuardResult) []string {
	var msgs []string
	for _, f := range res.Findings {
		if f.Severity == privcheck.SeverityBlock {
			msgs = append(msgs, f.Message)
		}
	}
	return msgs
}

// connectForge reuses an existing connection for the same user, forge type
// and base URL untouched (present: not re-verified, never overwritten), or
// creates one create-only after the shared validation. The bool reports
// whether a usable connection exists.
func (r *runner) connectForge(ctx context.Context, prov *provisionsvc.Service, q Store, userID uuid.UUID, in input, normBase string) (store.ForgeConnection, bool) {
	step := &Step{}
	r.report.Forge = step

	existing, err := q.GetForgeConnectionByTarget(ctx, store.GetForgeConnectionByTargetParams{
		UserID: userID, ForgeType: in.forgeType, BaseUrl: normBase,
	})
	switch {
	case err == nil:
		r.diag("forge connection already present; reusing it untouched")
		fillConnection(step, existing, StatusPresent)
		return existing, true
	case !errors.Is(err, pgx.ErrNoRows):
		step.Status, step.Message = StatusFailed, "could not read the existing connection"
		return store.ForgeConnection{}, false
	}

	r.diag("connecting the forge")
	conn, created, err := prov.ConnectForgeCreateOnly(ctx, userID, in.forgeType, in.baseURL, r.token)
	if err != nil {
		step.Status, step.Message, step.Findings = r.outcome(err)
		return store.ForgeConnection{}, false
	}
	if created {
		fillConnection(step, conn, StatusConnected)
	} else {
		fillConnection(step, conn, StatusPresent)
	}
	return conn, true
}

func fillConnection(step *Step, conn store.ForgeConnection, status string) {
	step.Status = status
	step.ConnectionID = conn.ID.String()
	step.BotUsername = conn.BotUsername
}

// discoverRepos lists the forge's projects into the repos table. It reports
// whether the repos can now be looked up.
func (r *runner) discoverRepos(ctx context.Context, prov *provisionsvc.Service, userID, connID uuid.UUID) bool {
	r.diag("discovering repos")
	step := &Step{}
	r.report.Discovery = step
	if _, err := prov.DiscoverRepos(ctx, userID, connID); err != nil {
		_, step.Message, _ = r.outcome(err)
		step.Status = StatusFailed
		return false
	}
	step.Status = StatusDone
	return true
}

// enableRepos evaluates every requested repo independently through
// provisionsvc.EnableRepo (the guard, then the board columns, then the flip).
// With no usable discovery every repo is skipped_dependency. A missing or
// refused repo does not stop the others.
func (r *runner) enableRepos(ctx context.Context, prov *provisionsvc.Service, q Store, userID, connID uuid.UUID, paths []string, discovered bool) {
	if len(paths) == 0 {
		return
	}
	r.report.Repos = make([]RepoStep, 0, len(paths))
	byPath := map[string]store.Repo{}
	if discovered {
		repos, err := q.ListReposByConnectionForUser(ctx, store.ListReposByConnectionForUserParams{ConnectionID: connID, UserID: userID})
		if err != nil {
			for _, p := range paths {
				r.report.Repos = append(r.report.Repos, RepoStep{Path: p, Status: StatusFailed, Message: "could not read the discovered repos"})
			}
			return
		}
		for _, rp := range repos {
			byPath[rp.PathWithNamespace] = rp
		}
	}
	for _, p := range paths {
		step := RepoStep{Path: p}
		switch repo, found := byPath[p]; {
		case !discovered:
			step.Status = StatusSkippedDependency
		case !found:
			step.Status = StatusNotFound
			step.Message = "the bot cannot see this repo on the forge"
		default:
			r.diag("enabling %s", p)
			enabled, err := prov.EnableRepo(ctx, userID, repo.ID)
			if err != nil {
				step.Status, step.Message, step.Findings = r.outcome(err)
				break
			}
			step.Status = StatusEnabled
			step.RepoID = enabled.ID.String()
			r.report.IssueSync = "pending"
		}
		r.report.Repos = append(r.report.Repos, step)
	}
}
