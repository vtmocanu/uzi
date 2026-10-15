package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMergedPredecessorRollbackRefusesAuditLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "merged_rollback_")
	if err := store.MigrateTo(ctx, dsn, 338); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	f := heldNewHoldFixture(ctx, t, pool, true, "claimed", nil)
	sibling := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
		 live_worker_id,live_run_id,inventory_guarded)
		VALUES($1,$2,$3,$4,2,'open',$5,'rollback-worker',$5,$4,true)`,
		sibling, f.user, f.repo, f.run, f.worker)
	mustExec(ctx, t, pool, `UPDATE runs SET status='completed',claim_generation=3,mr_iid=7 WHERE id=$1`, f.run)
	var binding recovery.MergeBinding
	if err := pool.QueryRow(ctx, `SELECT r.id,r.user_id,r.repo_id,p.connection_id,
		p.forge_project_id,c.forge_type,c.base_url,r.mr_iid,r.claim_generation
		FROM runs r JOIN repos p ON p.id=r.repo_id
		JOIN forge_connections c ON c.id=p.connection_id WHERE r.id=$1`, f.run).
		Scan(&binding.RunID, &binding.OwnerID, &binding.RepoID, &binding.ConnectionID,
			&binding.ProjectID, &binding.ForgeType, &binding.BaseURL, &binding.MRIID, &binding.CompletedGeneration); err != nil {
		t.Fatal(err)
	}
	snapshot, err := recovery.NewVerifiedMergeSnapshot(binding, time.Now().UTC().Add(-time.Second),
		&recovery.MergeObservation{IID: binding.MRIID, State: "merged"})
	if err != nil {
		t.Fatal(err)
	}
	svc := recovery.New(store.New(pool), pool, nil, recovery.Limits{}, nil)
	if n, err := svc.DiscardMergedPredecessorHolds(ctx, snapshot); err != nil || n != 2 {
		t.Fatalf("commit merged predecessor discard: holds=%d err=%v", n, err)
	}
	// Independent pool reads prove the service committed both real guarded transitions.
	for _, hold := range []uuid.UUID{f.hold, sibling} {
		var valid bool
		if err := pool.QueryRow(ctx, `SELECT state='discarded'
			AND release_evidence='merged_predecessor_discard'
			AND merged_predecessor_discard_binding->>'hold_id'=id::text
			AND merged_predecessor_discard_binding->>'generation'=generation::text
			FROM recovery_custody_holds WHERE id=$1`, hold).Scan(&valid); err != nil || !valid {
			t.Fatalf("committed audit for hold %s: valid=%v err=%v", hold, valid, err)
		}
	}
	version := func() int64 {
		t.Helper()
		var applied int64
		if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		return applied
	}
	if applied := version(); applied != 338 {
		t.Fatalf("fixture schema version=%d, want 338", applied)
	}
	retained := func() string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
			'holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.generation,h.id)
				FROM recovery_custody_holds h WHERE h.run_id=$1),
			'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY v.id) FROM goose_db_version v),
			'audit_column',(SELECT data_type FROM information_schema.columns
				WHERE table_schema='public' AND table_name='recovery_custody_holds'
				AND column_name='merged_predecessor_discard_binding'),
			'audit_trigger',(SELECT pg_get_triggerdef(oid) FROM pg_trigger
				WHERE tgrelid='recovery_custody_holds'::regclass
				AND tgname='merged_predecessor_discard_guard'),
			'audit_guard',pg_get_functiondef('merged_predecessor_discard_guard()'::regprocedure),
			'inventory_guard',pg_get_functiondef('recovery_inventory_hold_guard()'::regprocedure)
		)::text`, f.run).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := retained()
	err = store.MigrateDownTo(ctx, dsn, 337)
	if err == nil {
		t.Fatal("Down accepted committed merged predecessor audit")
	}
	// Without the explicit refusal, the old evidence CHECK can also reject Down.
	// Require the intentional audit refusal, rather than accepting that incidental error.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" ||
		pgErr.Message != "merged predecessor audit exists; refusing destructive rollback" {
		t.Errorf("Down refused for the wrong reason: %v", err)
	}
	if applied := version(); applied != 338 {
		t.Errorf("refused Down changed schema version to %d, want 338", applied)
	}
	if after := retained(); after != before {
		t.Errorf("refused Down changed committed audit or schema:\nbefore=%s\nafter=%s", before, after)
	}
}
