package privcheck

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Store is the narrow set of store methods the privilege service touches.
// *store.Queries satisfies it.
type Store interface {
	ListAllForgeConnections(ctx context.Context) ([]store.ForgeConnection, error)
	ListEnabledReposByConnection(ctx context.Context, connectionID uuid.UUID) ([]store.Repo, error)
	UpdatePrivilegeReport(ctx context.Context, arg store.UpdatePrivilegeReportParams) (int64, error)
}

// ForgeBuilder builds a driver from a stored (encrypted) connection.
// *forgesvc.Service satisfies it, so the decryption path is not duplicated here.
type ForgeBuilder interface {
	ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error)
}

// Service ties the pure Checker to the store and forge builder: it runs a full
// report for a connection and persists it. It is shared by the on-demand
// endpoint and the background sweep.
type Service struct {
	q       Store
	forges  ForgeBuilder
	checker *Checker
	now     func() time.Time
}

// NewService constructs a privilege Service with the default checker.
func NewService(q Store, forges ForgeBuilder) *Service {
	return &Service{q: q, forges: forges, checker: NewChecker(), now: time.Now}
}

// CheckToken runs only the token-level checks against an already-built forge
// client (the save-time path in CreateConnection, which holds the plaintext
// token and the bot identity). It makes at most one forge call (TokenInfo) and
// persists nothing — the caller decides whether the violations block the save.
func (s *Service) CheckToken(ctx context.Context, f forge.Forge, forgeType forge.Type, isAdmin bool) TokenReport {
	return s.checker.CheckToken(ctx, f, forgeType, isAdmin, s.now())
}

// CheckConnection runs the full report for one connection, persists it, and
// returns it. A forge-build or forge-call failure is surfaced as a persisted
// StatusError report (so the badge reflects the problem) rather than a returned
// error; only a genuine DB error (repo list, report write) returns an error.
func (s *Service) CheckConnection(ctx context.Context, conn store.ForgeConnection) (Report, error) {
	f, err := s.forges.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
	if err != nil {
		// Token can't be decrypted (key rotated) or the client won't build: record
		// it, don't crash the sweep.
		rep := errorReport(s.now(), "could not build a forge client for this connection (token undecryptable or misconfigured)")
		if perr := s.persist(ctx, conn.ID, rep); perr != nil {
			return Report{}, perr
		}
		return rep, nil
	}
	repos, err := s.q.ListEnabledReposByConnection(ctx, conn.ID)
	if err != nil {
		return Report{}, err
	}
	rep := s.checker.Check(ctx, f, forge.Type(conn.ForgeType), toRepos(repos), s.now())
	if err := s.persist(ctx, conn.ID, rep); err != nil {
		return Report{}, err
	}
	return rep, nil
}

// CheckAllConnections runs a full report for every connection, persisting each.
// It is the sweep pass (boot + interval). A per-connection error is logged and
// tallied but never aborts the pass — one revoked token must not stop the rest
// from being checked. Cancellation (shutdown) stops it promptly.
func (s *Service) CheckAllConnections(ctx context.Context) (SweepResult, error) {
	conns, err := s.q.ListAllForgeConnections(ctx)
	if err != nil {
		return SweepResult{}, err
	}
	var res SweepResult
	for i := range conns {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rep, err := s.CheckConnection(ctx, conns[i])
		if err != nil {
			slog.Error("privilege sweep: check connection", "connection", conns[i].ID, "error", err)
			res.Errors++
			continue
		}
		res.Checked++
		switch rep.Status {
		case StatusOK:
			res.OK++
		case StatusWarnings:
			res.Warnings++
		case StatusViolations:
			res.Violations++
		case StatusError:
			res.ReportErrors++
		}
	}
	return res, nil
}

// GuardrailImpact runs a live, non-persisting pre-flight scan (PRD #66 M3): it
// iterates every forge connection, and for each enabled repo asks whether the new
// guardrail would refuse a run (the bot can push/merge to the default branch, per
// BlocksRun). It PERSISTS NOTHING — it never calls persist/UpdatePrivilegeReport —
// because it is a measurement, not enforcement, and M3 runs before M1's migration
// NULLs the stored reports (R1/R2), so it must re-sweep live rather than read the
// blob.
//
// It fails soft per connection and per repo (R1): a client-build error, a
// VerifyToken error, or a per-repo forge error records that repo as UNEVALUABLE —
// "unknown", counted apart from blocked and never read as safe — and the scan
// continues. Only a genuine DB error (listing connections or repos) returns an
// error, matching CheckConnection. Cancellation (shutdown) stops it promptly.
func (s *Service) GuardrailImpact(ctx context.Context) (ImpactReport, error) {
	conns, err := s.q.ListAllForgeConnections(ctx)
	if err != nil {
		return ImpactReport{}, err
	}
	report := ImpactReport{CheckedAt: s.now(), Repos: []ImpactRepo{}}
	for i := range conns {
		if err := ctx.Err(); err != nil {
			return ImpactReport{}, err
		}
		conn := conns[i]
		rows, err := s.q.ListEnabledReposByConnection(ctx, conn.ID)
		if err != nil {
			return ImpactReport{}, err
		}
		repos := toRepos(rows)

		// Build the client and derive the bot user id once per connection. A failure
		// at either step is not fatal: it marks every one of this connection's repos
		// unevaluable (we could not read the forge to tell), never blocked and never
		// safe.
		f, buildErr := s.forges.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
		var (
			botUserID int64
			connErr   = buildErr
		)
		if connErr == nil {
			identity, verr := f.VerifyToken(ctx)
			if verr != nil {
				connErr = verr
			} else {
				botUserID = identity.ForgeUserID
			}
		}

		for _, repo := range repos {
			ir := ImpactRepo{
				RepoID:       repo.ID,
				Path:         repo.Path,
				UserID:       conn.UserID.String(),
				ConnectionID: conn.ID.String(),
			}
			if connErr != nil {
				ir.Unevaluable = true
			} else if blocked, ok := s.impactForRepo(ctx, f, botUserID, repo); ok {
				ir.Blocked = blocked
			} else {
				ir.Unevaluable = true
			}

			report.EnabledRepoCount++
			switch {
			case ir.Unevaluable:
				report.UnevaluableCount++
			case ir.Blocked:
				report.BlockedCount++
			}
			report.Repos = append(report.Repos, ir)
		}
	}
	return report, nil
}

// impactForRepo runs the same live forge reads the enforcement gate performs for
// one repo (ProjectRole + DefaultBranchProtection) and applies BlocksRun. It
// returns ok=false — the repo is unevaluable — when the repo has no default
// branch to read protection on, when either forge read errors, or when the
// protection read came back ProtectionUnverified (protected but the driver could
// not authoritatively read who may push/merge): fail-closed, "unknown" is not
// "safe" (R1). The role read is exercised (not consumed by BlocksRun, whose
// blocking set is the push/merge fields only) so that a repo whose forge
// round-trips error is counted unevaluable exactly as the live gate would be
// unable to clear it.
func (s *Service) impactForRepo(ctx context.Context, f forge.Forge, botUserID int64, repo Repo) (blocked, ok bool) {
	if repo.DefaultBranch == "" {
		return false, false
	}
	if _, _, err := f.ProjectRole(ctx, repo.ForgeProjectID, botUserID); err != nil {
		return false, false
	}
	bp, err := f.DefaultBranchProtection(ctx, repo.ForgeProjectID, repo.DefaultBranch, botUserID)
	if err != nil {
		return false, false
	}
	// Protected but unreadable (GitHub legacy-branch case, returned with a nil
	// error per forge.BranchProtection): the Can* fields are false because the
	// driver could not tell, not because the branch is safe. Count it unevaluable,
	// matching BlocksRun's contract and what M2's live gate will refuse as
	// protection_unreadable — never as not-affected.
	if bp.ProtectionUnverified {
		return false, false
	}
	return BlocksRun(bp), true
}

// persist writes the report + denormalized status onto the connection. A 0-row
// write (connection deleted mid-sweep) is tolerated silently.
func (s *Service) persist(ctx context.Context, connID uuid.UUID, rep Report) error {
	blob, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	_, err = s.q.UpdatePrivilegeReport(ctx, store.UpdatePrivilegeReportParams{
		ID:                 connID,
		PrivilegeReport:    blob,
		PrivilegeCheckedAt: pgtype.Timestamptz{Time: rep.CheckedAt, Valid: true},
		PrivilegeStatus:    pgtype.Text{String: string(rep.Status), Valid: true},
	})
	return err
}

// toRepos maps enabled store repos onto the checker's minimal Repo view. An
// invalid (NULL) default_branch decodes to "", which the checker skips with a note.
func toRepos(rows []store.Repo) []Repo {
	out := make([]Repo, len(rows))
	for i, r := range rows {
		out[i] = Repo{
			ID:             r.ID.String(),
			Path:           r.PathWithNamespace,
			ForgeProjectID: r.ForgeProjectID,
			DefaultBranch:  r.DefaultBranch.String,
		}
	}
	return out
}

// SweepResult tallies one sweep pass for logging.
type SweepResult struct {
	Checked      int // connections successfully checked (report persisted)
	OK           int
	Warnings     int
	Violations   int
	ReportErrors int // checked, but the report itself is StatusError
	Errors       int // could not check at all (DB error persisting)
}
