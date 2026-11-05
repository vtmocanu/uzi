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
	GetForgeConnectionForUser(ctx context.Context, arg store.GetForgeConnectionForUserParams) (store.ForgeConnection, error)
	UpsertRepo(ctx context.Context, arg store.UpsertRepoParams) (store.Repo, error)
	GetRepoForUser(ctx context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error)
	SetRepoEnabledForUser(ctx context.Context, arg store.SetRepoEnabledForUserParams) (store.Repo, error)
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
	CheckToken(ctx context.Context, f forge.Forge, forgeType forge.Type, isAdmin bool) privcheck.TokenReport
	GuardRepo(ctx context.Context, in privcheck.GuardInput) privcheck.GuardResult
}

// Service is the shared provisioning service.
type Service struct {
	q              Store
	forges         Forges
	guard          Guard
	baseURLAllowed func(raw string) bool
}

// New wires a Service. baseURLAllowed is Config.ForgeBaseURLAllowed.
func New(q Store, forges Forges, guard Guard, baseURLAllowed func(raw string) bool) *Service {
	return &Service{q: q, forges: forges, guard: guard, baseURLAllowed: baseURLAllowed}
}

// ConnectForge validates and stores a bot token for the user: forge type,
// token presence, base-URL allowlist, normalisation, client build, token
// verification, least-privilege check, encryption, upsert. An empty forgeType
// means GitLab.
func (s *Service) ConnectForge(ctx context.Context, userID uuid.UUID, forgeType, baseURL, token string) (store.ForgeConnection, error) {
	forgeType = strings.TrimSpace(forgeType)
	if forgeType == "" {
		forgeType = string(forge.TypeGitLab)
	}
	if forgeType != string(forge.TypeGitLab) && forgeType != string(forge.TypeForgejo) && forgeType != string(forge.TypeGitHub) {
		return store.ForgeConnection{}, &Error{Kind: KindBadRequest, Message: "unsupported forge type"}
	}
	if strings.TrimSpace(token) == "" {
		return store.ForgeConnection{}, &Error{Kind: KindBadRequest, Message: "a bot token is required"}
	}
	if !s.baseURLAllowed(baseURL) {
		return store.ForgeConnection{}, &Error{Kind: KindBadRequest, Message: "base URL is not in the allowed forge list"}
	}
	norm, err := config.NormalizeForgeBaseURL(baseURL)
	if err != nil {
		return store.ForgeConnection{}, &Error{Kind: KindBadRequest, Message: "invalid base URL"}
	}

	f, err := s.forges.ForgeForToken(forge.Type(forgeType), norm, token)
	if err != nil {
		return store.ForgeConnection{}, &Error{Kind: KindBadRequest, Message: "could not initialize forge client"}
	}
	identity, err := f.VerifyToken(ctx)
	if err != nil {
		// err is already PAT-redacted by the driver.
		return store.ForgeConnection{}, &Error{Kind: KindBadGateway, Message: "token verification failed: " + err.Error()}
	}

	// Least-privilege gate (PRD #5): token-level violations block the save.
	if tr := s.guard.CheckToken(ctx, f, forge.Type(forgeType), identity.IsAdmin); len(tr.Violations) > 0 {
		return store.ForgeConnection{}, &Error{
			Kind:       KindOverPrivileged,
			Message:    "the bot token is over-privileged and was not saved; mint a least-privilege token (see the bot setup doc)",
			Violations: tr.Violations,
		}
	}

	ciphertext, err := s.forges.EncryptToken(token)
	if err != nil {
		slog.Error("encrypt token", "error", err)
		return store.ForgeConnection{}, internal()
	}
	conn, err := s.q.UpsertForgeConnection(ctx, store.UpsertForgeConnectionParams{
		UserID:          userID,
		ForgeType:       forgeType,
		BaseUrl:         norm,
		BotUsername:     identity.Username,
		BotForgeUserID:  identity.ForgeUserID,
		TokenCiphertext: ciphertext,
	})
	if err != nil {
		slog.Error("upsert forge connection", "error", err)
		return store.ForgeConnection{}, internal()
	}
	return conn, nil
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
		slog.Error("build forge for connection", "error", err)
		return store.ForgeConnection{}, internal()
	}
	projects, err := f.ListProjects(ctx)
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
			slog.Error("upsert repo", "error", err)
			return store.ForgeConnection{}, internal()
		}
	}
	return conn, nil
}

// EnableRepo turns a repo on: the live guard (honouring the repo's override),
// then the default board columns when the repo has none, then the flip, then
// best-effort settlement of pending override requests. A guard block returns
// KindGuardBlocked with the verdict; a forge that refuses the column labels
// returns KindBadGateway and leaves the repo disabled. Disabling is not
// gated and stays with the caller.
func (s *Service) EnableRepo(ctx context.Context, userID, repoID uuid.UUID) (store.Repo, error) {
	row, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Repo{}, &Error{Kind: KindNotFound, Message: "repo not found"}
		}
		slog.Error("get repo for enable guard", "error", err)
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

	// Columns before the flip: if the forge refuses the labels, the repo stays
	// disabled rather than enabled with a board no card can resolve against.
	f, err := s.forges.ForgeForConnection(row.ForgeType, row.BaseUrl, row.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		return store.Repo{}, internal()
	}
	if err := forgesvc.EnsureBoardColumns(ctx, s.q, f, row.ID, row.ForgeProjectID); err != nil {
		var seedErr *forgesvc.LabelSeedError
		if errors.As(err, &seedErr) {
			return store.Repo{}, &Error{Kind: KindBadGateway, Message: "could not seed board labels on the forge: " + seedErr.Error()}
		}
		slog.Error("ensure board columns", "error", err)
		return store.Repo{}, internal()
	}

	repo, err := s.q.SetRepoEnabledForUser(ctx, store.SetRepoEnabledForUserParams{ID: repoID, Enabled: true, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Repo{}, &Error{Kind: KindNotFound, Message: "repo not found"}
		}
		slog.Error("set repo enabled", "error", err)
		return store.Repo{}, internal()
	}

	// A successful enable makes any pending override request moot. Best effort:
	// the enable already succeeded.
	if err := s.q.DeletePendingGuardrailOverrideRequestsForRepo(ctx, repoID); err != nil {
		slog.Warn("set repo enabled: settle pending override request", "error", err)
	}
	return repo, nil
}
