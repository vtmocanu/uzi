package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const codexPlacementMigration = "00521_placement_codex_provider_capability.sql"

func placementMigrationFunction(t *testing.T, section string) string {
	t.Helper()
	loc := placementFunctionHeader.FindStringIndex(section)
	if loc == nil {
		t.Fatal("missing placement function")
	}
	end := strings.Index(section[loc[0]:], "$$;")
	if end < 0 {
		t.Fatal("missing function terminator")
	}
	// The first delimiter opens the body; find its closing delimiter.
	start := strings.Index(section[loc[0]:], "$$")
	end = strings.Index(section[loc[0]+start+2:], "$$;")
	if end < 0 {
		t.Fatal("missing function terminator")
	}
	return section[loc[0] : loc[0]+start+2+end+3]
}

func codexPlacementSections(t *testing.T) (string, string) {
	t.Helper()
	raw, err := os.ReadFile("migrations/" + codexPlacementMigration)
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("missing Down")
	}
	return up, down
}

func TestCodexProviderPlacementRestoresOriginalFunction(t *testing.T) {
	raw, err := os.ReadFile("migrations/00507_placement_codex_phase_models.sql")
	if err != nil {
		t.Fatal(err)
	}
	original, _, _ := strings.Cut(string(raw), "-- +goose Down")
	up, down := codexPlacementSections(t)
	prior := placementMigrationFunction(t, original)
	if placementMigrationFunction(t, down) != prior {
		t.Fatal("Down does not restore exact 00507 Up function")
	}
	// Removing just the replacement provider clause must recover every other byte.
	oldClause := "AND (r.model_provider_key_mode IS NULL OR 'claude_provider_v1' = ANY(w.protocol_capabilities))"
	newClause := "AND (r.model_provider_key_mode IS NULL\n                      OR (r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities))\n                      OR (r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities)))"
	current := stripSQLCommentLines(placementMigrationFunction(t, up))
	previous := stripSQLCommentLines(prior)
	if !strings.Contains(current, newClause) || strings.Replace(current, newClause, oldClause, 1) != previous {
		t.Fatal("Up changes more than the provider clause")
	}
}

func bindCodexPlacementRun(fx *fleetFixture, run uuid.UUID) {
	fx.t.Helper()
	p, err := fx.q.InsertModelProvider(fx.ctx, store.InsertModelProviderParams{OwnerUserID: fx.userID, Name: uuid.NewString(), Engine: "codex"})
	if err != nil {
		fx.t.Fatal(err)
	}
	if err := fx.q.InsertModelProviderRevision(fx.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: "https://example.test/v1",
		AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`),
	}); err != nil {
		fx.t.Fatal(err)
	}
	key, err := insertProviderKey(fx.ctx, fx.q, fx.userID, p.ID)
	if err != nil {
		fx.t.Fatal(err)
	}
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET harness='codex',
 anthropic_secret_id=NULL, codex_secret_id=NULL, codex_auth_mode=NULL, credential_override_secret_id=NULL,
 model_provider_id=$2, model_provider_revision=1, model_provider_key_mode='user_key',
 model_provider_key_secret_id=$3, model_provider_key_rev=(SELECT enablement_rev FROM user_secrets WHERE id=$3)
 WHERE id=$1`, run, p.ID, key.ID)
}

func TestCodexProviderPlacementMatrixLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	workers := map[string]uuid.UUID{
		"legacy":        fx.jobWorker("legacy", capOf(1), false),
		"claude":        fx.jobWorker("claude", capOf(1), false, claudeProviderCap),
		"native":        fx.jobWorker("native", capOf(1), false, "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"),
		"native-v2":     fx.jobWorker("native-v2", capOf(1), false, "codex_harness_v1", "codex_runtime_v2"),
		"codex":         fx.jobWorker("codex", capOf(1), false, "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_provider_v1"),
		"codex-v2":      fx.jobWorker("codex-v2", capOf(1), false, "codex_harness_v1", "codex_runtime_v2", "codex_provider_v1"),
		"provider-only": fx.jobWorker("provider-only", capOf(1), false, "codex_provider_v1"),
	}
	code := queuedRunWithCaps(fx, []string{})
	bindCodexPlacementRun(fx, code)
	claude := queuedRunWithCaps(fx, []string{})
	bindRunToProvider(fx, claude, "claude-placement")
	vendorClaude := queuedRunWithCaps(fx, []string{})
	vendorCode := queuedRunWithCaps(fx, []string{})
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET harness='codex' WHERE id=$1", vendorCode)
	for _, tc := range []struct {
		name    string
		harness string
		run     uuid.UUID
		want    map[string]bool
	}{
		{"codex-provider", "codex", code, map[string]bool{"codex": true, "codex-v2": false, "native-v2": false}},
		{"unknown-provider", "unknown", code, map[string]bool{}},
		{"claude-provider", "claude", claude, map[string]bool{"claude": true}},
		{"vendor-claude", "claude", vendorClaude, map[string]bool{"legacy": true, "claude": true, "native": true, "codex": true, "provider-only": true, "native-v2": true, "codex-v2": true}},
		{"vendor-codex", "codex", vendorCode, map[string]bool{"native": true, "codex": true, "native-v2": false, "codex-v2": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := fx.pool.Query(fx.ctx, `SELECT p.worker_id FROM runs r
 CROSS JOIN LATERAL fn_placement_admitting_workers(jsonb_populate_record(r, jsonb_build_object('harness', $2::text)), 'gpt-5', now(), now()-interval '2 minutes', interval '1 hour', false, ARRAY['gpt-5']::text[]) p
 WHERE r.id=$1`, tc.run, tc.harness)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got := map[uuid.UUID]bool{}
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got[id] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			for name, id := range workers {
				if got[id] != tc.want[name] {
					t.Errorf("%s admitted=%t want %t", name, got[id], tc.want[name])
				}
			}
		})
	}
}

func TestCodexProviderPlacementRollbackLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "codex_placement_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	owner, _ := newProductUser(ctx, t, pool)
	fx := &fleetFixture{t: t, ctx: ctx, pool: pool, q: q, userID: owner}
	run := fx.queuedJob()
	bindCodexPlacementRun(fx, run)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	const definition = `SELECT pg_get_functiondef('fn_placement_admitting_workers(runs,text,timestamptz,timestamptz,interval,boolean,text[])'::regprocedure)`
	var latest string
	if err := tx.QueryRow(ctx, definition).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range migrationDownStatements(t, "00523_codex_runtime_v3.sql") {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var current string
	if err := tx.QueryRow(ctx, definition).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT guarded_down"); err != nil {
		t.Fatal(err)
	}
	var refusal error
	for _, stmt := range migrationDownStatements(t, codexPlacementMigration) {
		if _, refusal = tx.Exec(ctx, stmt); refusal != nil {
			break
		}
	}
	if refusal == nil || !strings.Contains(refusal.Error(), "refusing to roll back 00521") {
		t.Fatalf("Down refusal=%v", refusal)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT guarded_down"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM runs WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range migrationDownStatements(t, codexPlacementMigration) {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("migrations/00507_placement_codex_phase_models.sql")
	if err != nil {
		t.Fatal(err)
	}
	prior, _, _ := strings.Cut(string(raw), "-- +goose Down")
	var restored string
	if err := tx.QueryRow(ctx, definition).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, placementMigrationFunction(t, prior)); err != nil {
		t.Fatal(err)
	}
	var expected string
	if err := tx.QueryRow(ctx, definition).Scan(&expected); err != nil {
		t.Fatal(err)
	}
	if restored != expected {
		t.Fatal("Down definition differs from original Up")
	}
	for _, stmt := range migrationUpStatements(t, codexPlacementMigration) {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.QueryRow(ctx, definition).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored != current {
		t.Fatal("00521 Down/Up roundtrip changed boundary definition")
	}
	for _, stmt := range migrationUpStatements(t, "00523_codex_runtime_v3.sql") {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.QueryRow(ctx, definition).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	// 00523 Up is the immediate predecessor of 00534: the live head minus 00534's judge clause.
	if want := strings.Replace(latest, " AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities))", "", 1); restored != want || want == latest {
		t.Fatal("00523 Up did not restore the exact pre-00534 definition")
	}
	// Each later placement migration is replayed in order; the last one must land the live head.
	for _, stmt := range migrationUpStatements(t, "00534_placement_provider_judge_capability.sql") {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.QueryRow(ctx, definition).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored != latest {
		t.Fatal("00534 Up did not restore exact latest definition")
	}
}
