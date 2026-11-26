package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type ownedStoreTestDB struct {
	name     string
	dsn      string
	adminDSN string
}

func createOwnedStoreTestDB(ctx context.Context, adminDSN, prefix, template string) (ownedStoreTestDB, error) {
	u, err := url.Parse(adminDSN)
	if err != nil {
		return ownedStoreTestDB{}, fmt.Errorf("parse test database URL: %w", err)
	}
	name := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	pool, err := store.OpenPool(ctx, adminDSN)
	if err != nil {
		return ownedStoreTestDB{}, fmt.Errorf("open admin pool: %w", err)
	}
	defer pool.Close()
	statement := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		statement += " TEMPLATE " + pgx.Identifier{template}.Sanitize() + " ALLOW_CONNECTIONS true"
	}
	if _, err := pool.Exec(ctx, statement); err != nil {
		return ownedStoreTestDB{}, fmt.Errorf("create database %s: %w", name, err)
	}
	u.Path = "/" + name
	return ownedStoreTestDB{name: name, dsn: u.String(), adminDSN: adminDSN}, nil
}

func (db ownedStoreTestDB) drop(ctx context.Context) error {
	pool, err := store.OpenPool(ctx, db.adminDSN)
	if err != nil {
		return fmt.Errorf("open admin pool to drop %s: %w", db.name, err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db.name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", db.name, err)
	}
	return nil
}

var headQueryTemplate struct {
	once sync.Once
	db   ownedStoreTestDB
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if db := headQueryTemplate.db; db.name != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := db.drop(ctx)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cleanup head-query template:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func prepareHeadQueryTemplate(ctx context.Context, adminDSN string) (ownedStoreTestDB, error) {
	db, err := createOwnedStoreTestDB(ctx, adminDSN, "head_query_template_", "")
	if err != nil {
		return db, err
	}
	if err := store.Migrate(ctx, db.dsn); err != nil {
		return db, fmt.Errorf("migrate head-query template: %w", err)
	}
	pool, err := store.OpenPool(ctx, adminDSN)
	if err != nil {
		return db, fmt.Errorf("open admin pool to seal head-query template: %w", err)
	}
	defer pool.Close()
	// Like newCJMigrationTemplate, seal the migrated database once its connections are closed.
	// Clone connections are allowed, but tests can never use the template as their work database.
	if _, err := pool.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{db.name}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
		return db, fmt.Errorf("seal head-query template: %w", err)
	}
	return db, nil
}

// standHeadQueryDB is for tests that query the current schema and do not test migration history.
// Every caller owns a separate writable database, even when assertions fail.
func standHeadQueryDB(t *testing.T, prefix string) (context.Context, string) {
	t.Helper()
	adminDSN := liveStoreTestDSN(t)
	ctx := context.Background()
	headQueryTemplate.once.Do(func() {
		started := time.Now()
		headQueryTemplate.db, headQueryTemplate.err = prepareHeadQueryTemplate(ctx, adminDSN)
		t.Logf("head-query template setup: %s", time.Since(started))
	})
	if headQueryTemplate.err != nil {
		t.Fatal(headQueryTemplate.err)
	}
	if headQueryTemplate.db.adminDSN != adminDSN {
		t.Fatal("head-query template belongs to a different test database URL")
	}
	started := time.Now()
	db, err := createOwnedStoreTestDB(ctx, adminDSN, prefix, headQueryTemplate.db.name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("head-query clone setup: %s", time.Since(started))
	t.Cleanup(func() {
		if err := db.drop(ctx); err != nil {
			t.Error(err)
		}
	})
	return ctx, db.dsn
}

func TestHeadQueryTemplateCloneIsolationLiveDB(t *testing.T) {
	ctx, firstDSN := standHeadQueryDB(t, "head_query_first_")
	first, err := store.OpenPool(ctx, firstDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	owner, _ := newProductUser(ctx, t, first)
	var rows int
	if err := first.QueryRow(ctx, "SELECT count(*) FROM users WHERE id = $1", owner).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("write fixture: %d rows, err=%v", rows, err)
	}

	_, secondDSN := standHeadQueryDB(t, "head_query_second_")
	second, err := store.OpenPool(ctx, secondDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	if err := second.QueryRow(ctx, "SELECT count(*) FROM users WHERE id = $1", owner).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("write from first clone visible in second: %d rows, err=%v", rows, err)
	}

	var sealed bool
	err = second.QueryRow(ctx, "SELECT NOT datallowconn FROM pg_database WHERE datname = $1", headQueryTemplate.db.name).Scan(&sealed)
	if err != nil || !sealed {
		t.Fatalf("template must be sealed while clones are in use: sealed=%t err=%v", sealed, err)
	}
}
