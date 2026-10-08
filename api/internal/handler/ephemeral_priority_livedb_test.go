package handler

import (
	"context"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The generated queries have global LIMITs. Use a dedicated database, even when the
// live-DB sweep shares its server with other packages, so leftover candidates cannot
// consume the window. Subtests deliberately run serially and reset their candidates.
func ephemeralPriorityDSN(t *testing.T, dsn, name string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		u.RawPath = ""
		query := u.Query()
		query.Set("dbname", name)
		u.RawQuery = query.Encode()
		return u.String()
	}
	// pgx uses the last occurrence of a keyword. Preserve all original settings
	// while overriding dbname, escaping the two special characters in quoted values.
	name = strings.ReplaceAll(name, "\\", "\\\\")
	name = strings.ReplaceAll(name, "'", "\\'")
	return dsn + " dbname='" + name + "'"
}

func assertPriorityDatabase(t *testing.T, ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, name string) {
	t.Helper()
	var actual string
	if err := db.QueryRow(ctx, "SELECT current_database()").Scan(&actual); err != nil {
		t.Fatalf("verify dedicated priority database: %v", err)
	}
	if actual != name {
		t.Fatalf("priority database = %q, want dedicated database %q", actual, name)
	}
}

func ephemeralPriorityDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	name := "eph_priority_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create dedicated priority database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop dedicated priority database: %v", err)
		}
	})
	dedicatedDSN := ephemeralPriorityDSN(t, dsn, name)
	conn, err := pgx.Connect(ctx, dedicatedDSN)
	if err != nil {
		t.Fatalf("connect dedicated priority database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	assertPriorityDatabase(t, ctx, conn, name)
	t.Setenv("UZI_TEST_DATABASE_URL", dedicatedDSN)
	return name
}

func priorityFixture(t *testing.T, name string) *ephemeralFixture {
	t.Helper()
	fx := newEphemeralFixture(t, true)
	// Verify the actual connection before resetting candidates.
	assertPriorityDatabase(t, fx.ctx, fx.pool, name)
	if _, err := fx.pool.Exec(fx.ctx, "TRUNCATE workers, runs CASCADE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		assertPriorityDatabase(t, fx.ctx, fx.pool, name)
		if _, err := fx.pool.Exec(fx.ctx, "TRUNCATE workers, runs CASCADE"); err != nil {
			t.Errorf("reset priority candidates: %v", err)
		}
	})
	return fx
}

func priorityTrigger(fx *ephemeralFixture, trigger string) uuid.UUID {
	fx.t.Helper()
	switch trigger {
	case "lane":
		return fx.boundProfile()
	case "saturation":
		fx.activeRunOn(fx.onlineWorkerWithCap("saturated", false, 1))
	}
	return uuid.Nil
}

func priorityRun(fx *ephemeralFixture, trigger string, profile uuid.UUID, kind string, priority *int16, created, queued time.Time) uuid.UUID {
	fx.t.Helper()
	var id uuid.UUID
	switch trigger {
	case "lane":
		id = fx.boundQueuedRun(profile, nil)
	case "gap":
		id = fx.queuedRun([]string{"jvm"})
	default:
		id = fx.queuedRun([]string{})
	}
	var target any
	if kind == "judge" {
		target = fx.queuedRun([]string{})
		if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET status = 'completed' WHERE id = $1", target); err != nil {
			fx.t.Fatal(err)
		}
	}
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET kind = $2, priority = $3, created_at = $4, status_since = $5,
		target_run_id = $6, repo_id = CASE WHEN $2 = 'judge' THEN NULL ELSE repo_id END,
		issue_iid = CASE WHEN $2 = 'judge' THEN NULL ELSE issue_iid END WHERE id = $1`,
		id, kind, priority, created, queued, target); err != nil {
		fx.t.Fatal(err)
	}
	return id
}

func priorityQuery(t *testing.T, fx *ephemeralFixture, q *store.Queries, trigger string, cutoff time.Time, limit int32, delay time.Duration) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	switch trigger {
	case "gap":
		rows, err := q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
			BackgroundGraceCutoff: pgconv.Time(cutoff), MaxRows: limit, MaxPerUser: 10,
			EphemeralLease: workersvc.LeaseInterval(0), CodexCuratedModels: workersvc.CodexCuratedModels(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
	case "saturation":
		rows, err := q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
			BackgroundGraceCutoff: pgconv.Time(cutoff), MaxRows: limit, MaxPerUser: 10,
			SaturationDelay: workersvc.LeaseInterval(delay), EphemeralLease: workersvc.LeaseInterval(0),
			CodexCuratedModels: workersvc.CodexCuratedModels(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
	case "lane":
		rows, err := q.ListIsolatedQueuedRunsForEphemeral(fx.ctx, store.ListIsolatedQueuedRunsForEphemeralParams{
			BackgroundGraceCutoff: pgconv.Time(cutoff), MaxRows: limit, MaxPerUser: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
	default:
		t.Fatalf("unknown trigger %q", trigger)
	}
	return ids
}

func assertPriorityBinding(t *testing.T, fx *ephemeralFixture, want uuid.UUID) {
	t.Helper()
	if _, err := fx.provisionerWithDelay(true, 1, time.Minute).ProvisionPass(fx.ctx); err != nil {
		t.Fatal(err)
	}
	rows := fx.ephemeralRows()
	if len(rows) != 1 || rows[0].runID != want {
		t.Fatalf("owner bindings = %+v, want exactly one worker bound to %s", rows, want)
	}
}

func TestEphemeralPriorityDSN(t *testing.T) {
	const name = "eph_priority_unique"
	for _, dsn := range []string{
		"postgres://user:pass@localhost:5432/shared?sslmode=disable",
		"postgresql://user:pass@localhost:5432/shared?dbname=override&application_name=priority",
		"host=localhost port=5432 user=user password='pass word' dbname=shared sslmode=disable",
		"host=localhost dbname=shared dbname=override application_name=priority",
	} {
		t.Run(dsn, func(t *testing.T) {
			before, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			after, err := pgxpool.ParseConfig(ephemeralPriorityDSN(t, dsn, name))
			if err != nil {
				t.Fatal(err)
			}
			if after.ConnConfig.Database != name {
				t.Fatalf("serialized database = %q, want %q", after.ConnConfig.Database, name)
			}
			if before.ConnConfig.Host != after.ConnConfig.Host ||
				before.ConnConfig.Port != after.ConnConfig.Port ||
				before.ConnConfig.User != after.ConnConfig.User ||
				before.ConnConfig.Password != after.ConnConfig.Password ||
				!reflect.DeepEqual(before.ConnConfig.RuntimeParams, after.ConnConfig.RuntimeParams) {
				t.Fatal("database override changed other connection settings")
			}
		})
	}
}

func TestEphemeralPriorityLiveDB(t *testing.T) {
	name := ephemeralPriorityDatabase(t)
	normal, expedited := int16(1), int16(2)
	for _, trigger := range []string{"gap", "saturation", "lane"} {
		t.Run(trigger+"/query_order_and_limit", func(t *testing.T) {
			fx := priorityFixture(t, name)
			profile := priorityTrigger(fx, trigger)
			now := time.Now().UTC()
			cutoff := now.Add(-5 * time.Minute)
			// Equal rank uses created_at for gap/lane, but status_since for saturation.
			oldCreated, newCreated := now.Add(-40*time.Minute), now.Add(-30*time.Minute)
			oldQueued, newQueued := now.Add(-4*time.Minute), now.Add(-3*time.Minute)
			if trigger == "saturation" {
				oldCreated, newCreated = newCreated, oldCreated
			}
			old := priorityRun(fx, trigger, profile, "issue", &normal, oldCreated, oldQueued)
			next := priorityRun(fx, trigger, profile, "issue", &normal, newCreated, newQueued)
			fast := priorityRun(fx, trigger, profile, "issue", &expedited, now.Add(-time.Minute), now.Add(-2*time.Minute))
			want := []uuid.UUID{fast, old, next}
			if got := priorityQuery(t, fx, fx.q, trigger, cutoff, 100, time.Minute); !reflect.DeepEqual(got, want) {
				t.Fatalf("ordered IDs = %v, want %v", got, want)
			}
			if got := priorityQuery(t, fx, fx.q, trigger, cutoff, 1, time.Minute); !reflect.DeepEqual(got, want[:1]) {
				t.Fatalf("MaxRows=1 IDs = %v, want %v", got, want[:1])
			}
			assertPriorityBinding(t, fx, fast)
		})
		for _, kind := range []string{"judge", "self_improve"} {
			t.Run(trigger+"/"+kind+"/grace", func(t *testing.T) {
				fx := priorityFixture(t, name)
				profile := priorityTrigger(fx, trigger)
				now := time.Now().UTC()
				fresh := priorityRun(fx, trigger, profile, kind, nil, now.Add(-2*time.Minute), now.Add(-4*time.Minute))
				interactive := priorityRun(fx, trigger, profile, "issue", nil, now.Add(-time.Minute), now.Add(-2*time.Minute))
				want := []uuid.UUID{interactive, fresh}
				if got := priorityQuery(t, fx, fx.q, trigger, now.Add(-5*time.Minute), 100, time.Minute); !reflect.DeepEqual(got, want) {
					t.Fatalf("fresh background IDs = %v, want %v", got, want)
				}
				if got := priorityQuery(t, fx, fx.q, trigger, now.Add(-5*time.Minute), 1, time.Minute); !reflect.DeepEqual(got, want[:1]) {
					t.Fatalf("fresh MaxRows=1 = %v, want %v", got, want[:1])
				}
				assertPriorityBinding(t, fx, interactive)
			})
			t.Run(trigger+"/"+kind+"/stale", func(t *testing.T) {
				fx := priorityFixture(t, name)
				profile := priorityTrigger(fx, trigger)
				now := time.Now().UTC()
				stale := priorityRun(fx, trigger, profile, kind, nil, now.Add(-10*time.Minute), now.Add(-4*time.Minute))
				interactive := priorityRun(fx, trigger, profile, "issue", nil, now.Add(-time.Minute), now.Add(-2*time.Minute))
				want := []uuid.UUID{stale, interactive}
				if got := priorityQuery(t, fx, fx.q, trigger, now.Add(-5*time.Minute), 100, time.Minute); !reflect.DeepEqual(got, want) {
					t.Fatalf("restored background IDs = %v, want %v", got, want)
				}
				if got := priorityQuery(t, fx, fx.q, trigger, now.Add(-5*time.Minute), 1, time.Minute); !reflect.DeepEqual(got, want[:1]) {
					t.Fatalf("restored MaxRows=1 = %v, want %v", got, want[:1])
				}
				assertPriorityBinding(t, fx, stale)
			})
		}
	}
	t.Run("trigger_precedence", func(t *testing.T) {
		for _, winner := range []string{"lane", "gap"} {
			t.Run(winner, func(t *testing.T) {
				fx := priorityFixture(t, name)
				priorityTrigger(fx, "saturation")
				now := time.Now().UTC()
				priorityRun(fx, "saturation", uuid.Nil, "issue", &expedited, now.Add(-20*time.Minute), now.Add(-20*time.Minute))
				var want uuid.UUID
				if winner == "lane" {
					priorityRun(fx, "gap", uuid.Nil, "issue", &expedited, now.Add(-15*time.Minute), now.Add(-15*time.Minute))
					want = priorityRun(fx, "lane", fx.boundProfile(), "issue", &normal, now.Add(-2*time.Minute), now.Add(-2*time.Minute))
				} else {
					want = priorityRun(fx, "gap", uuid.Nil, "issue", &normal, now.Add(-2*time.Minute), now.Add(-2*time.Minute))
				}
				assertPriorityBinding(t, fx, want)
			})
		}
	})
	t.Run("expedited_debounce", func(t *testing.T) {
		fx := priorityFixture(t, name)
		priorityTrigger(fx, "saturation")
		now := time.Now().UTC()
		id := priorityRun(fx, "saturation", uuid.Nil, "issue", &expedited, now.Add(-time.Hour), now)
		prov := fx.provisionerWithDelay(true, 1, time.Minute)
		if _, err := prov.ProvisionPass(fx.ctx); err != nil {
			t.Fatal(err)
		}
		if rows := fx.ephemeralRows(); len(rows) != 0 {
			t.Fatalf("expedited before debounce provisioned: %+v", rows)
		}
		// PostgreSQL now() is fixed within a transaction, proving strict equality
		// without any wall-clock tolerance or sleep.
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		if _, err := tx.Exec(fx.ctx, "UPDATE runs SET status_since = now() - interval '1 minute' WHERE id = $1", id); err != nil {
			t.Fatal(err)
		}
		if got := priorityQuery(t, fx, fx.q.WithTx(tx), "saturation", now.Add(-5*time.Minute), 1, time.Minute); len(got) != 0 {
			t.Fatalf("exact debounce equality admitted %v", got)
		}
		if _, err := tx.Exec(fx.ctx, "UPDATE runs SET status_since = now() - interval '2 minutes' WHERE id = $1", id); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(fx.ctx); err != nil {
			t.Fatal(err)
		}
		assertPriorityBinding(t, fx, id)
	})
}
