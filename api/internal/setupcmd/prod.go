package setupcmd

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
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
	}, pool.Close, nil
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
