// Package setupcmd implements `/server setup` (PRD #3760): a one-shot operator
// command, run inside the deployment, that connects a forge and enables repos
// for the seed admin. It reuses provisionsvc, the service the web handlers
// call, so the token-privilege check, the repo-enable guard and the board
// columns apply exactly as in the web UI.
//
// The command runs no startup seed, opens no listener and starts no background
// service. It never migrates: it refuses a database that is not at this
// binary's schema head. Exactly one JSON Report goes to stdout; diagnostics go
// to stderr; neither stream carries the forge token.
//
// Steps are methods on runner, run in order by Run: connectForge,
// discoverRepos, enableRepos, then createWorker when --worker-name is given.
// The worker step is create-only; the join token it mints goes into the one
// stdout document and nowhere else.
package setupcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/provisionsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Deadline covers input acquisition and everything after it.
const Deadline = 5 * time.Minute

// Store is the slice of *store.Queries setup uses: the provisioning service's
// own slice plus the owner and repo lookups.
type Store interface {
	provisionsvc.Store
	GetUserByEmail(ctx context.Context, email string) (store.User, error)
	ListReposByConnectionForUser(ctx context.Context, arg store.ListReposByConnectionForUserParams) ([]store.Repo, error)
}

// Backend is everything setup needs from the outside world. Production wires
// it over a two-connection pool; tests wire it over fakes or a test database.
type Backend struct {
	Q      Store
	Forges provisionsvc.Forges
	Guard  provisionsvc.Guard
	// CheckSchema returns nil only when the database schema is at this
	// binary's head.
	CheckSchema func(ctx context.Context) error
	// Workers creates the owner's first worker. Required only when
	// --worker-name is given.
	Workers Workers
}

// WorkerResult is the outcome of Workers.CreateFirst.
type WorkerResult struct {
	// Created is false when the owner already had a worker; nothing was minted.
	Created   bool
	WorkerID  uuid.UUID
	Name      string
	JoinToken string
}

// Workers creates the first worker for an owner. The existence check and the
// creation are one step serialised per owner, so two concurrent setup runs
// cannot both mint a first worker. An owner who already has any worker gets
// Created=false and no token.
type Workers interface {
	CreateFirst(ctx context.Context, ownerID uuid.UUID, name, template string) (WorkerResult, error)
}

// Env supplies configuration and the backend.
type Env struct {
	LoadConfig func() (config.SetupConfig, error)
	// Open connects to the database and returns the backend plus a close func.
	Open func(ctx context.Context, cfg config.SetupConfig) (*Backend, func(), error)
}

// Main is the process entry: it applies the deadline, wires the production
// environment and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	return Run(ctx, args, stdin, stdout, stderr, ProdEnv())
}

// runner carries the state shared by the steps of one invocation.
type runner struct {
	stderr io.Writer
	token  string
	report *Report
}

// clean removes the forge token from s. The token is never meant to reach a
// message; this is the backstop for text that came from outside this package.
func (r *runner) clean(s string) string {
	if r.token == "" {
		return s
	}
	return strings.ReplaceAll(s, r.token, "[redacted]")
}

func (r *runner) diag(format string, a ...any) {
	fmt.Fprintf(r.stderr, "setup: %s\n", r.clean(fmt.Sprintf(format, a...)))
}

// reject writes a rejected report (exit 2): nothing was written.
func (r *runner) reject(stdout io.Writer, err error) int {
	msg := r.clean(err.Error())
	r.diag("%s", msg)
	rep := &Report{SchemaVersion: ReportSchemaVersion, Status: ReportRejected, ExitCode: ExitUsage, Error: msg}
	_ = writeReport(stdout, rep)
	return ExitUsage
}

// Run executes setup and returns the process exit code. See the package doc.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	r := &runner{stderr: stderr}

	in, err := parseArgs(args, stderr)
	if errors.Is(err, errHelp) {
		return ExitOK
	}
	if err != nil {
		return r.reject(stdout, err)
	}
	cfg, err := env.LoadConfig()
	if err != nil {
		return r.reject(stdout, fmt.Errorf("configuration: %w", err))
	}
	if !cfg.ForgeBaseURLAllowed(in.baseURL) {
		return r.reject(stdout, errors.New("--forge-base-url is not in FORGE_ALLOWED_BASE_URLS"))
	}
	normBase, err := config.NormalizeForgeBaseURL(in.baseURL)
	if err != nil {
		return r.reject(stdout, errors.New("--forge-base-url is not a valid https URL"))
	}
	token, err := readToken(ctx, in.tokenFile, stdin)
	if err != nil {
		return r.reject(stdout, err)
	}
	r.token = token

	be, closeBackend, err := env.Open(ctx, cfg)
	if err != nil {
		return r.reject(stdout, fmt.Errorf("database: %w", err))
	}
	defer closeBackend()
	if err := be.CheckSchema(ctx); err != nil {
		return r.reject(stdout, fmt.Errorf("schema: %w", err))
	}
	owner, err := be.Q.GetUserByEmail(ctx, cfg.SeedEmail)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return r.reject(stdout, errors.New("owner: no user has the UZI_SEED_EMAIL address; start the API once so it seeds the admin"))
	case err != nil:
		return r.reject(stdout, errors.New("owner: could not read the user"))
	case !owner.IsAdmin:
		return r.reject(stdout, errors.New("owner: the UZI_SEED_EMAIL user is not an admin; setup never promotes a user"))
	}

	// Every precondition holds. From here on, writes may happen and are kept.
	prov := provisionsvc.New(be.Q, be.Forges, be.Guard, cfg.ForgeBaseURLAllowed)
	r.report = &Report{SchemaVersion: ReportSchemaVersion, Status: ReportOK}
	conn, forgeOK := r.connectForge(ctx, prov, be.Q, owner.ID, in, normBase)
	discovered := forgeOK && r.discoverRepos(ctx, prov, owner.ID, conn.ID)
	if !forgeOK {
		// The report names every step (PRD #3760), so a skipped discovery shows.
		r.report.Discovery = &Step{Status: StatusSkippedDependency}
	}
	r.enableRepos(ctx, prov, be.Q, owner.ID, conn.ID, in.repos, discovered)
	r.createWorker(ctx, be.Workers, owner.ID, in)

	switch {
	case r.report.failed():
		r.report.Status = ReportFailed
		r.report.ExitCode = ExitFailed
	case r.report.Worker.Status == StatusExists:
		r.report.Status = ReportWorkerExists
		r.report.ExitCode = ExitWorkerExists
	}
	if err := writeReport(stdout, r.report); err != nil {
		// The join token, if one was minted, was in the document that did not
		// arrive. It is not repeated here: it stays on stdout only.
		r.diag("could not write the report: %v", err)
		return ExitFailed
	}
	return r.report.ExitCode
}
