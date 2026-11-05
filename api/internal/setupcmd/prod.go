package setupcmd

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// poolMaxConns bounds the setup process's database pool.
const poolMaxConns = 2

// ProdEnv wires setup to the process environment and a real database.
func ProdEnv() Env {
	return Env{LoadConfig: config.LoadSetup, Open: openProd}
}

func openProd(ctx context.Context, cfg config.SetupConfig) (*Backend, func(), error) {
	pool, err := store.OpenPool(ctx, cfg.DatabaseURL, func(c *pgxpool.Config) { c.MaxConns = poolMaxConns })
	if err != nil {
		return nil, nil, err
	}
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	q := store.New(pool)
	// The label settings are read only by the issue sync, which setup does not
	// run; the cache is required by the forge service's constructor.
	cache := settings.New(q, 5*time.Second)
	cache.ConfigureSecrets(box, map[string]string{})
	svc := forgesvc.New(q, box, cfg.ForgeHTTPTimeout, cache)
	return &Backend{
		Q:           q,
		Forges:      svc,
		Guard:       privcheck.NewService(q, svc),
		CheckSchema: func(ctx context.Context) error { return CheckSchemaAtHead(ctx, pool) },
		Workers:     NewPoolWorkers(pool, box),
	}, pool.Close, nil
}

// poolWorkers is the production Workers: the check and the create run in one
// transaction that holds a per-owner advisory lock, so a second setup run for
// the same owner waits, then sees the first run's worker and creates nothing.
type poolWorkers struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
}

// NewPoolWorkers returns the Workers that serialises per owner over pool.
func NewPoolWorkers(pool *pgxpool.Pool, box *secretbox.Box) Workers {
	return &poolWorkers{pool: pool, box: box}
}

func (p *poolWorkers) CreateFirst(ctx context.Context, ownerID uuid.UUID, name, template string) (WorkerResult, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return WorkerResult{}, err
	}
	// Rollback after Commit is a no-op; it releases the lock on every error path.
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('uzi-setup-first-worker:' || $1::text, 0))`, ownerID.String()); err != nil {
		return WorkerResult{}, err
	}
	q := store.New(tx)
	existing, err := q.ListWorkersByUser(ctx, ownerID)
	if err != nil {
		return WorkerResult{}, err
	}
	if len(existing) > 0 {
		return WorkerResult{Created: false}, nil
	}
	wkr, token, err := workersvc.New(q, p.box, workersvc.Params{}).CreateWorker(ctx, ownerID, name, template, "")
	if err != nil {
		return WorkerResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkerResult{}, err
	}
	return WorkerResult{Created: true, WorkerID: wkr.ID, Name: wkr.Name, JoinToken: token}, nil
}

// CheckSchemaAtHead returns nil only when every migration this binary embeds
// is applied. Setup never migrates; a database behind (or ahead of) the binary
// is refused.
func CheckSchemaAtHead(ctx context.Context, pool *pgxpool.Pool) error {
	applied, head, atHead, err := store.SchemaVersionStatus(ctx, pool)
	if err != nil {
		return fmt.Errorf("could not read the schema version: %w", err)
	}
	if !atHead {
		return fmt.Errorf("the database is at migration %d but this binary expects %d; start the API once so it migrates, then retry", applied, head)
	}
	return nil
}
