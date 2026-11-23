// Package provisionsvc holds the decision logic of the forge-provisioning
// steps (connect a forge, discover its repos, turn a repo on), so the web
// handlers and the operator setup command share one set of security rules.
//
// It returns typed errors (*Error); each caller maps them to its own wire
// format. The handler-local repo-sync invalidation stays in the caller.
package provisionsvc

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Kind classifies a provisioning failure so callers can pick a status code.
type Kind int

const (
	// KindBadRequest: the input is invalid (HTTP 400).
	KindBadRequest Kind = iota
	// KindNotFound: the connection or repo is absent or not owned (404).
	KindNotFound
	// KindBadGateway: the forge refused or failed (502).
	KindBadGateway
	// KindOverPrivileged: the token failed the least-privilege check (422);
	// Violations carries the reasons.
	KindOverPrivileged
	// KindGuardBlocked: the enable guard blocked the repo (422); Guard
	// carries the verdict.
	KindGuardBlocked
	// KindInternal: a database or crypto failure, already logged (500).
	KindInternal
	// KindConflict: the request conflicts with the repo's history (409): it was
	// linked to an issue tracker before it was removed from uzi.
	KindConflict
)

// Error is the typed failure of every Service method. Message is safe to
// show a client (forge text is PAT-redacted by the driver).
type Error struct {
	Kind       Kind
	Message    string
	Violations []string
	Guard      privcheck.GuardResult
}

func (e *Error) Error() string { return e.Message }

func internal() *Error { return &Error{Kind: KindInternal, Message: "internal error"} }

// Store is the slice of *store.Queries the service uses.
type Store interface {
	UpsertForgeConnection(ctx context.Context, arg store.UpsertForgeConnectionParams) (store.ForgeConnection, error)
	CreateForgeConnectionIfAbsent(ctx context.Context, arg store.CreateForgeConnectionIfAbsentParams) (store.ForgeConnection, error)
	GetForgeConnectionByTarget(ctx context.Context, arg store.GetForgeConnectionByTargetParams) (store.ForgeConnection, error)
	GetForgeConnectionForUser(ctx context.Context, arg store.GetForgeConnectionForUserParams) (store.ForgeConnection, error)
	UpsertRepo(ctx context.Context, arg store.UpsertRepoParams) (store.Repo, error)
	GetRepoForUser(ctx context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error)
	LockRepoForEnableForUser(ctx context.Context, arg store.LockRepoForEnableForUserParams) (store.LockRepoForEnableForUserRow, error)
	EnableRepoForUser(ctx context.Context, arg store.EnableRepoForUserParams) (store.Repo, error)
	DeletePendingGuardrailOverrideRequestsForRepo(ctx context.Context, repoID uuid.UUID) error
	forgesvc.BoardColumnStore
}

// Forges builds forge clients and seals tokens (*forgesvc.Service).
type Forges interface {
	EncryptToken(pat string) ([]byte, error)
	ForgeForToken(forgeType forge.Type, baseURL, token string) (forge.Forge, error)
	ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error)
}

// Guard runs the least-privilege checks (*privcheck.Service).
type Guard interface {
	CheckToken(ctx context.Context, f forge.CodeHost, forgeType forge.Type, isAdmin bool) privcheck.TokenReport
	GuardRepo(ctx context.Context, in privcheck.GuardInput) privcheck.GuardResult
}

// Service is the shared provisioning service.
type Service struct {
	q              Store
	forges         Forges
	guard          Guard
	baseURLAllowed func(raw string) bool
	log            *slog.Logger // nil: slog.Default() at each call
	// begin and bind run a unit of work in one transaction (see UseTransactions).
	begin TxBeginner
	bind  func(pgx.Tx) Store
}

// TxBeginner opens a transaction; *pgxpool.Pool satisfies it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// UseTransactions makes EnableRepo hold the repo row lock (PRD #4173 D13)
// while it seeds the board columns and flips the repo on: begin opens a
// transaction and bind returns the Store whose statements run in it
// (store.Queries.WithTx in production). Without it EnableRepo runs on the
// Service's own Store with no lock, which cannot order the enable against a
// concurrent issue tracker link, so production wiring always sets it. A nil
// begin or bind leaves transactions off; pass a nil interface, never a typed
// nil such as a nil *pgxpool.Pool. It returns s for chaining and must be called before
// the Service is shared.
func (s *Service) UseTransactions(begin TxBeginner, bind func(pgx.Tx) Store) *Service {
	s.begin, s.bind = begin, bind
	return s
}

// inTx runs fn in one transaction (READ COMMITTED, the pool default), rolling
// back when fn fails and committing otherwise. With transactions off it runs fn
// on the Service's own Store.
func (s *Service) inTx(ctx context.Context, fn func(Store) error) error {
	if s.begin == nil || s.bind == nil {
		return fn(s.q)
	}
	tx, err := s.begin.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after a successful Commit
	if err := fn(s.bind(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// New wires a Service. baseURLAllowed is Config.ForgeBaseURLAllowed.
func New(q Store, forges Forges, guard Guard, baseURLAllowed func(raw string) bool) *Service {
	return &Service{q: q, forges: forges, guard: guard, baseURLAllowed: baseURLAllowed}
}

// WithLogger makes the Service write its log lines to l instead of the
// process default. The setup command gives each run its own scrubbed logger,
// so two runs at the same time never share a log destination.
func (s *Service) WithLogger(l *slog.Logger) *Service {
	s.log = l
	return s
}

func (s *Service) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// ConnectForge validates and stores a bot token for the user: forge type,
// token presence, base-URL allowlist, normalisation, client build, token
// verification, least-privilege check, encryption, upsert. An empty forgeType
// means GitLab.
func (s *Service) ConnectForge(ctx context.Context, userID uuid.UUID, forgeType, baseURL, token string) (store.ForgeConnection, error) {
	p, err := s.validateConnection(ctx, forgeType, baseURL, token)
	if err != nil {
		return store.ForgeConnection{}, err
	}
	conn, err := s.q.UpsertForgeConnection(ctx, store.UpsertForgeConnectionParams{
		UserID:          userID,
		ForgeType:       p.forgeType,
		BaseUrl:         p.baseURL,
		BotUsername:     p.identity.Username,
		BotForgeUserID:  p.identity.ForgeUserID,
		TokenCiphertext: p.ciphertext,
	})
	if err != nil {
		s.logger().Error("upsert forge connection", "error", err)
		return store.ForgeConnection{}, internal()
	}
	return conn, nil
}

// ConnectForgeCreateOnly runs the same validation as ConnectForge but stores
// the connection create-only (the operator setup command, PRD #3760 D12). When
// a connection for the same user, forge type and base URL already exists, it
// is returned unchanged with created=false: its token and bot identity are
// never rotated, including when the row won a race with this call.
func (s *Service) ConnectForgeCreateOnly(ctx context.Context, userID uuid.UUID, forgeType, baseURL, token string) (conn store.ForgeConnection, created bool, err error) {
	p, err := s.validateConnection(ctx, forgeType, baseURL, token)
	if err != nil {
		return store.ForgeConnection{}, false, err
	}
	conn, err = s.q.CreateForgeConnectionIfAbsent(ctx, store.CreateForgeConnectionIfAbsentParams{
		UserID:          userID,
		ForgeType:       p.forgeType,
		BaseUrl:         p.baseURL,
		BotUsername:     p.identity.Username,
		BotForgeUserID:  p.identity.ForgeUserID,
		TokenCiphertext: p.ciphertext,
	})
	if err == nil {
		return conn, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.logger().Error("create forge connection", "error", err)
		return store.ForgeConnection{}, false, internal()
	}
	// Conflict: a connection already holds this target. Reuse the winner.
	conn, err = s.q.GetForgeConnectionByTarget(ctx, store.GetForgeConnectionByTargetParams{
		UserID: userID, ForgeType: p.forgeType, BaseUrl: p.baseURL,
	})
	if err != nil {
		s.logger().Error("read winning forge connection", "error", err)
		return store.ForgeConnection{}, false, internal()
	}
	return conn, false, nil
}

// validatedConnection is the result of the shared validation steps.
type validatedConnection struct {
	forgeType  string
	baseURL    string
	identity   forge.BotIdentity
	ciphertext []byte
}

// validateConnection is the validation half of a connect: forge type, token
// presence, base-URL allowlist, normalisation, client build, token
// verification, least-privilege check, encryption. Shared by ConnectForge and
// ConnectForgeCreateOnly so the two cannot drift.
func (s *Service) validateConnection(ctx context.Context, forgeType, baseURL, token string) (validatedConnection, error) {
	// The caller names the forge; uzi never guesses it from the URL or a
	// default (issue #3785), since a guessed driver verifies the token against
	// the wrong forge.
	rawType := strings.TrimSpace(forgeType)
	if rawType == "" {
		return validatedConnection{}, &Error{Kind: KindBadRequest, Message: "forge_type is required; supported values: " + forge.SupportedTypesList()}
	}
	parsedType, ok := forge.ParseType(rawType)
	if !ok {
		return validatedConnection{}, &Error{Kind: KindBadRequest, Message: "unsupported forge type; supported values: " + forge.SupportedTypesList()}
	}
	forgeType = string(parsedType)
	if strings.TrimSpace(token) == "" {
		return validatedConnection{}, &Error{Kind: KindBadRequest, Message: "a bot token is required"}
	}
	if !s.baseURLAllowed(baseURL) {
		return validatedConnection{}, &Error{Kind: KindBadRequest, Message: "base URL is not in the allowed forge list"}
	}
	norm, err := config.NormalizeForgeBaseURL(baseURL)
	if err != nil {
		return validatedConnection{}, &Error{Kind: KindBadRequest, Message: "invalid base URL"}
	}

	f, err := s.forges.ForgeForToken(forge.Type(forgeType), norm, token)
	if err != nil {
		return validatedConnection{}, &Error{Kind: KindBadRequest, Message: "could not initialize forge client"}
	}
	// PRD #4173 D3: connection validation uses the code-host half only.
	var ch forge.CodeHost = f
	identity, err := ch.VerifyToken(ctx)
	if err != nil {
		// err is already PAT-redacted by the driver.
		return validatedConnection{}, &Error{Kind: KindBadGateway, Message: "token verification failed: " + err.Error()}
	}

	// Least-privilege gate (PRD #5): token-level violations block the save.
	if tr := s.guard.CheckToken(ctx, ch, forge.Type(forgeType), identity.IsAdmin); len(tr.Violations) > 0 {
		return validatedConnection{}, &Error{
			Kind:       KindOverPrivileged,
			Message:    "the bot token is over-privileged and was not saved; mint a least-privilege token (see the bot setup doc)",
			Violations: tr.Violations,
		}
	}

	ciphertext, err := s.forges.EncryptToken(token)
	if err != nil {
		s.logger().Error("encrypt token", "error", err)
		return validatedConnection{}, internal()
	}
	return validatedConnection{forgeType: forgeType, baseURL: norm, identity: identity, ciphertext: ciphertext}, nil
}

// DiscoverRepos lists the connection's forge projects and upserts each as a
// repo row (enabled=false). It returns the connection so callers can reuse it.
func (s *Service) DiscoverRepos(ctx context.Context, userID, connID uuid.UUID) (store.ForgeConnection, error) {
	conn, err := s.q.GetForgeConnectionForUser(ctx, store.GetForgeConnectionForUserParams{ID: connID, UserID: userID})
	if err != nil {
		return store.ForgeConnection{}, &Error{Kind: KindNotFound, Message: "connection not found"}
	}
	f, err := s.forges.ForgeForConnection(conn.ForgeType, conn.BaseUrl, conn.TokenCiphertext)
	if err != nil {
		s.logger().Error("build forge for connection", "error", err)
		return store.ForgeConnection{}, internal()
	}
	// PRD #4173 D3: discovery lists projects on the code-host half.
	var ch forge.CodeHost = f
	projects, err := ch.ListProjects(ctx)
	if err != nil {
		return store.ForgeConnection{}, &Error{Kind: KindBadGateway, Message: "could not list projects: " + err.Error()}
	}
	for _, p := range projects {
		if _, err := s.q.UpsertRepo(ctx, store.UpsertRepoParams{
			ConnectionID:      conn.ID,
			ForgeProjectID:    p.ForgeProjectID,
			PathWithNamespace: p.PathWithNamespace,
			WebUrl:            p.WebURL,
			DefaultBranch:     pgconv.TextOrNull(p.DefaultBranch),
		}); err != nil {
			s.logger().Error("upsert repo", "error", err)
			return store.ForgeConnection{}, internal()
		}
	}
	return conn, nil
}

// EnableRepo turns a repo on: the live guard (honouring the repo's override),
// then, in one transaction holding the repo row lock, the default board
// columns when the repo has none and the flip, then best-effort settlement of
// pending override requests after the commit. A guard block returns
// KindGuardBlocked with the verdict; a forge that refuses the column labels
// returns KindBadGateway and leaves the repo disabled. Disabling is not
// gated and stays with the caller.
//
// The row lock is the enable's half of PRD #4173 D13: it conflicts with the
// lock the issue tracker link takes, and whether the repo is linked is read
// only after it is held. A link that committed first is seen under the lock,
// so only the column rows are written and the forge gets no label request; a
// link that arrives later waits for this transaction and is then refused. The
// forge label requests therefore run while the row lock and one pool
// connection are held. The forge HTTP timeout bounds each request, not the
// whole seed: a seed makes several (a project lookup, a label listing, one
// create per missing label), so the hold can be several timeouts long. The
// guard runs before the transaction: it makes
// network calls of its own and reads nothing the link changes.
func (s *Service) EnableRepo(ctx context.Context, userID, repoID uuid.UUID) (store.Repo, error) {
	row, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Repo{}, &Error{Kind: KindNotFound, Message: "repo not found"}
		}
		s.logger().Error("get repo for enable guard", "error", err)
		return store.Repo{}, internal()
	}
	res := s.guard.GuardRepo(ctx, privcheck.GuardInput{
		ForgeType:       row.ForgeType,
		BaseURL:         row.BaseUrl,
		TokenCiphertext: row.TokenCiphertext,
		Repo: privcheck.Repo{
			ID:             row.ID.String(),
			Path:           row.PathWithNamespace,
			ForgeProjectID: row.ForgeProjectID,
			DefaultBranch:  row.DefaultBranch.String,
		},
		// A non-NULL guardrail_override_reason means the admin override is
		// live; GuardRepo downgrades the waivable findings (never
		// protection_unreadable).
		Overridden: row.GuardrailOverrideReason.Valid,
	})
	if res.Blocked {
		return store.Repo{}, &Error{Kind: KindGuardBlocked, Message: "this repo cannot be enabled", Guard: res}
	}

	f, err := s.forges.ForgeForConnection(row.ForgeType, row.BaseUrl, row.TokenCiphertext)
	if err != nil {
		s.logger().Error("build forge for connection", "error", err)
		return store.Repo{}, internal()
	}

	// Columns before the flip: if the forge refuses the labels, the repo stays
	// disabled rather than enabled with a board no card can resolve against.
	var repo store.Repo
	err = s.inTx(ctx, func(q Store) error {
		lock, err := q.LockRepoForEnableForUser(ctx, store.LockRepoForEnableForUserParams{ID: repoID, UserID: userID})
		if err != nil {
			return err
		}
		// A project linked to an issue tracker before it was removed keeps that issue source: it
		// is refused here, before any label work reaches the forge (the repos trigger from
		// migration 00475 refuses the flip too, and enableError maps it the same way).
		if lock.SourceLocked {
			return removedLinkedRepoError()
		}
		// PRD #4173 D3: board columns are labels, on the issue-tracker half. A repo linked to an
		// external issue tracker records the column rows only: uzi creates no column labels or
		// tags there (a column is not a tag, D9), and the forge's project is not where its issues live.
		// The choice reads the locked row, not the one read before the guard.
		if lock.IssueTrackerConnectionID.Valid {
			if err := forgesvc.EnsureBoardColumnRows(ctx, q, row.ID); err != nil {
				s.logger().Error("ensure board column rows", "error", err)
				return internal()
			}
		} else if err := forgesvc.EnsureBoardColumns(ctx, q, forge.IssueTracker(f), row.ID, row.ForgeProjectID); err != nil {
			var seedErr *forgesvc.LabelSeedError
			if errors.As(err, &seedErr) {
				return &Error{Kind: KindBadGateway, Message: "could not seed board labels on the forge: " + seedErr.Error()}
			}
			s.logger().Error("ensure board columns", "error", err)
			return internal()
		}
		// EnableRepoForUser also records the first-import intent (a 'waiting' import the
		// poller runs; issue #4399), so the enable never imports issues itself and the row
		// lock is not held across one.
		repo, err = q.EnableRepoForUser(ctx, store.EnableRepoForUserParams{ID: repoID, UserID: userID})
		return err
	})
	if err != nil {
		return store.Repo{}, s.enableError(err)
	}

	// A successful enable makes any pending override request moot. Best effort:
	// the enable already succeeded.
	if err := s.q.DeletePendingGuardrailOverrideRequestsForRepo(ctx, repoID); err != nil {
		s.logger().Warn("set repo enabled: settle pending override request", "error", err)
	}
	return repo, nil
}

// removedLinkedRepoSQLState is the SQLSTATE the repos trigger from migration 00475 raises when
// a repo that was linked to an issue tracker before its removal is enabled unlinked.
const removedLinkedRepoSQLState = "UZ414"

// removedLinkedRepoError is the 409 for a project that was linked to an issue tracker before
// it was removed from uzi.
func removedLinkedRepoError() *Error {
	return &Error{Kind: KindConflict, Message: "this project was linked to an issue tracker before it was removed from uzi; its issue source cannot change, so it cannot be enabled or linked again"}
}

// enableError maps an EnableRepo transaction failure to the typed error the
// caller returns: a typed *Error passes through, a missing row is not found,
// the removed-linked-repo refusal is a conflict, and anything else is logged and becomes internal.
func (s *Service) enableError(err error) *Error {
	var te *Error
	if errors.As(err, &te) {
		return te
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &Error{Kind: KindNotFound, Message: "repo not found"}
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == removedLinkedRepoSQLState {
		return removedLinkedRepoError()
	}
	s.logger().Error("enable repo", "error", err)
	return internal()
}
