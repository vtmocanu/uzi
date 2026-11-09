package forgesvc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type okLabels struct{}

func (okLabels) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }

// A failure part way through the column insert must not leave a partial board
// that a retry then reads as complete (PR #3956 review, item 1). A trigger makes
// the insert of the SECOND default column fail for this test's repo only; the
// first EnsureBoardColumns call must fail and leave 0 columns, and the retry
// (trigger gone) must give the repo all four, in DefaultColumns order.
//
// Under the old one-row-at-a-time insert the first call kept column 1, the
// retry saw count = 1, returned nil, and the repo stayed at 1 of 4 columns.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.
func TestEnsureBoardColumnsRetryAfterPartialFailureLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("board-cols-%s@e2e", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, userID, []byte{0x1}); err != nil {
		t.Fatalf("seed forge_connection: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 100, $3, 'https://forge.e2e/g/r', false)`,
		repoID, connID, "g/"+repoID.String()); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// The trigger is scoped to this repo id, so other packages sharing the
	// database never see it.
	suffix := strings.ReplaceAll(repoID.String(), "-", "")
	fn, trg := "fail_second_col_"+suffix, "fail_second_col_"+suffix
	second := DefaultColumns[1].Name
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.repo_id = '%s'::uuid AND NEW.label_name = '%s' THEN
				RAISE EXCEPTION 'injected failure on the second column';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER %s BEFORE INSERT ON board_columns
			FOR EACH ROW EXECUTE FUNCTION %s();`,
		fn, repoID, strings.ReplaceAll(second, "'", "''"), trg, fn)); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	dropTrigger := func() {
		_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON board_columns; DROP FUNCTION IF EXISTS %s();`, trg, fn))
	}
	t.Cleanup(dropTrigger)

	if err := EnsureBoardColumns(ctx, q, okLabels{}, repoID, 100); err == nil {
		t.Fatalf("EnsureBoardColumns with the second insert failing returned nil, want an error")
	}
	if n, _ := q.CountBoardColumns(ctx, repoID); n != 0 {
		t.Fatalf("after a failed insert the repo has %d columns, want 0 (no partial board)", n)
	}

	dropTrigger()
	if err := EnsureBoardColumns(ctx, q, okLabels{}, repoID, 100); err != nil {
		t.Fatalf("retry: %v", err)
	}
	cols, err := q.ListBoardColumns(ctx, repoID)
	if err != nil {
		t.Fatalf("list columns: %v", err)
	}
	if len(cols) != len(DefaultColumns) {
		t.Fatalf("after the retry the repo has %d columns, want %d", len(cols), len(DefaultColumns))
	}
	for i, c := range cols {
		if c.LabelName != DefaultColumns[i].Name || int(c.Position) != i {
			t.Errorf("column %d = %q at %d, want %q at %d", i, c.LabelName, c.Position, DefaultColumns[i].Name, i)
		}
	}

	// A repo with its own columns is left alone.
	other := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 101, $3, 'https://forge.e2e/g/o', false)`,
		other, connID, "g/"+other.String()); err != nil {
		t.Fatalf("seed second repo: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO board_columns (repo_id, label_name, position) VALUES ($1, 'Custom', 0)`, other); err != nil {
		t.Fatalf("seed custom column: %v", err)
	}
	if err := EnsureBoardColumns(ctx, q, okLabels{}, other, 101); err != nil {
		t.Fatalf("EnsureBoardColumns on a custom board: %v", err)
	}
	if n, _ := q.CountBoardColumns(ctx, other); n != 1 {
		t.Errorf("a repo with its own column now has %d columns, want 1 (left alone)", n)
	}
}
