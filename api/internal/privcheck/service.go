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
func (s *Service) CheckToken(ctx context.Context, f forge.Forge, isAdmin bool) TokenReport {
	return s.checker.CheckToken(ctx, f, isAdmin, s.now())
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
	rep := s.checker.Check(ctx, f, toRepos(repos), s.now())
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
