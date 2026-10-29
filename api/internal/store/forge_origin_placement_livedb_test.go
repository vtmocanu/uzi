package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// These are storage proofs, not permission to adopt source or release custody.
// Every FINAL is produced with the installed triggers enabled.
type forgePlacementFixture struct {
	*rollAffinityFixture
	owner, peer, run uuid.UUID
	holds            []uuid.UUID
}

func newForgePlacementFixture(t *testing.T) *forgePlacementFixture {
	fx := newRollAffinityFixture(t)
	f := &forgePlacementFixture{rollAffinityFixture: fx}
	f.owner = fx.worker("cooled-owner", 0, false)
	f.peer = fx.worker("healthy-peer", 0, false)
	f.run = fx.pinnedRun(f.owner, 0)
	f.exec("UPDATE workers SET protocol_capabilities=ARRAY['recovery_completed_publication_v1'] WHERE id=$1", f.owner)
	forgeAdmissionCooldown(fx, f.owner, false)
	return f
}

func (f *forgePlacementFixture) exec(sql string, args ...any) {
	f.t.Helper()
	tag, err := f.pool.Exec(f.ctx, sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		f.t.Fatalf("fixture transition rows=%d err=%v SQL=%s", tag.RowsAffected(), err, sql)
	}
}

func (f *forgePlacementFixture) open(generation int, guarded bool) uuid.UUID {
	f.t.Helper()
	f.exec("UPDATE runs SET status='claimed',claim_released_at=NULL,claim_generation=$2 WHERE id=$1", f.run, generation)
	hold := uuid.New()
	f.exec(`INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
		 live_worker_id,live_run_id,inventory_guarded)
		VALUES($1,$2,$3,$4,$5,'open',$6,'placement-owner',$6,$4,$7)`,
		hold, f.userID, f.repoID, f.run, generation, f.owner, guarded)
	f.holds = append(f.holds, hold)
	return hold
}

func (f *forgePlacementFixture) finish(hold uuid.UUID, disposition, evidence string) {
	f.t.Helper()
	f.exec(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
		final_disposition=$2,release_evidence=$3,released_at=now(),
		final_coverage_digest=CASE WHEN $2='settled' THEN
		'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' ELSE NULL END
		WHERE id=$1`, hold, disposition, evidence)
}

func (f *forgePlacementFixture) capture(hold uuid.UUID, source string, prerequisites []string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO recovery_captures
		(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,
		 state,manifest_bound,coverage_digest,local_replica_worker_id,ready_retention_seconds,
		 expires_at,prerequisite_shas)
		VALUES($1::uuid,$2,$3,$4,$5,'placement-owner',$6,($1::uuid)::text,'available',true,
		 repeat('d',64),$5,3600,now()+interval '1 day',$7)`,
		id, hold, f.run, f.userID, f.owner, source, prerequisites)
	return id
}

func (f *forgePlacementFixture) history(kind string) {
	f.t.Helper()
	hold := f.open(1, true)
	switch kind {
	case "no_adopted_source":
		f.finish(hold, kind, kind)
	case "forge_no_output", "publication":
		f.exec("UPDATE runs SET status='completed' WHERE id=$1", f.run)
		f.finish(hold, "settled", kind)
	case "owner_discard":
		f.exec(`UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',
			live_worker_id=NULL,live_run_id=NULL WHERE id=$1`, hold)
	case "completed_publication":
		f.exec("UPDATE runs SET mr_iid=42,status='completed',completion_final_head=repeat('a',40) WHERE id=$1", f.run)
		f.exec(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
			final_disposition='completed_publication',release_evidence='completed_publication',
			completed_publication_receipt=completion_identity ||
				jsonb_build_object('observed_branch_head',repeat('b',40)),released_at=now() WHERE id=$1`, hold)
	case "held_publication", "held_created":
		f.exec("UPDATE runs SET status='failed' WHERE id=$1", f.run)
		// Reuse the exact receipt builder, but walk states through the actual guards.
		h := heldFixture{pool: f.pool, user: f.userID, repo: f.repoID, worker: f.owner, run: f.run, hold: hold, pub: uuid.New()}
		h.insertPreparedAt(f.ctx, f.t, h.pub, 1, f.owner)
		f.exec("UPDATE run_held_publications SET state='invoked',create_invoked_at=now() WHERE id=$1", h.pub)
		f.exec("UPDATE run_held_publications SET state='created',expires_at=now()+interval '1 day' WHERE id=$1", h.pub)
		if n, err := h.release(f.ctx, f.t, h.pub, heldDigest, heldTip); err != nil || n != 1 {
			f.t.Fatalf("held FINAL rows=%d err=%v", n, err)
		}
		if kind == "held_publication" {
			f.exec("UPDATE run_held_publications SET state='acknowledged',acknowledged_at=now() WHERE id=$1", h.pub)
		}
	case "archive":
		f.exec("UPDATE runs SET status='failed' WHERE id=$1", f.run)
		capture := f.capture(hold, heldTip, []string{})
		f.exec(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
			final_disposition='archive',release_evidence='archive',final_capture_id=$2,
			final_source_sha=$3,final_coverage_digest=repeat('d',64),released_at=now() WHERE id=$1`,
			hold, capture, heldTip)
	case "merged_predecessor_discard":
		second := f.open(2, true)
		f.exec("UPDATE runs SET mr_iid=42,status='completed' WHERE id=$1", f.run)
		f.exec(`UPDATE recovery_custody_holds h SET state='discarded',live_worker_id=NULL,live_run_id=NULL,
			release_evidence='merged_predecessor_discard',
			merged_predecessor_discard_binding=jsonb_build_object(
				'hold_id',h.id,'generation',h.generation,'run_id',h.run_id,
				'owner_id',h.user_id,'repo_id',h.repo_id,'connection_id',c.id,
				'forge_type',c.forge_type,'base_url',c.base_url,'project_id',p.forge_project_id,
				'mr_iid',r.mr_iid,'completed_generation',r.claim_generation,
				'verified_at',to_char(statement_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
			FROM runs r JOIN repos p ON p.id=r.repo_id JOIN forge_connections c ON c.id=p.connection_id
			WHERE h.id=$1 AND r.id=h.run_id`, hold)
		f.finish(second, "settled", "publication")
	default:
		f.t.Fatalf("unknown history %q", kind)
	}
	f.queue()
}

func (f *forgePlacementFixture) queue() {
	f.t.Helper()
	// Refresh the affinity episode: passing cannot be explained by ceiling expiry.
	f.exec("UPDATE runs SET status='queued',updated_at=now(),status_since=now() WHERE id=$1", f.run)
}

func (f *forgePlacementFixture) safe(want bool) {
	f.t.Helper()
	var got bool
	if err := f.pool.QueryRow(f.ctx, "SELECT fn_forge_source_history_safe(r) FROM runs r WHERE id=$1", f.run).Scan(&got); err != nil || got != want {
		f.t.Fatalf("complete positive-source predicate=%v want=%v err=%v", got, want, err)
	}
}

func (f *forgePlacementFixture) peerClaim(want bool) {
	f.t.Helper()
	var before []byte
	if want {
		if err := f.pool.QueryRow(f.ctx, "SELECT jsonb_agg(to_jsonb(h) ORDER BY generation) FROM recovery_custody_holds h WHERE id=ANY($1)", f.holds).Scan(&before); err != nil {
			f.t.Fatal(err)
		}
	}
	got, err := forgeAdmissionClaim(f.rollAffinityFixture, f.q, f.peer)
	if !want {
		if !errors.Is(err, pgx.ErrNoRows) {
			f.t.Fatalf("protected run claimed: %s err=%v", got.ID, err)
		}
		return
	}
	if err != nil || got.ID != f.run || got.ClaimGeneration != int64(len(f.holds)+1) {
		f.t.Fatalf("immediate peer claim run=%s G=%d err=%v", got.ID, got.ClaimGeneration, err)
	}
	var after []byte
	if err := f.pool.QueryRow(f.ctx, "SELECT jsonb_agg(to_jsonb(h) ORDER BY generation) FROM recovery_custody_holds h WHERE id=ANY($1)", f.holds).Scan(&after); err != nil {
		f.t.Fatal(err)
	}
	if string(before) != string(after) {
		f.t.Fatal("peer claim rewrote lifetime custody evidence")
	}
	var exact bool
	if err := f.pool.QueryRow(f.ctx, `SELECT
		(SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1)=$3
		AND (SELECT count(*) FROM recovery_custody_holds WHERE id=ANY($2) AND state IN ('released','discarded')
			AND live_worker_id IS NULL AND live_run_id IS NULL)=$4
		AND EXISTS (SELECT 1 FROM recovery_custody_holds WHERE run_id=$1 AND generation=$3
			AND state='open' AND original_worker_id=$5 AND live_worker_id=$5 AND live_run_id=$1)`,
		f.run, f.holds, len(f.holds)+1, len(f.holds), f.peer).Scan(&exact); err != nil || !exact {
		f.t.Fatalf("lifetime ledger lost history or new exact custody: %v err=%v", exact, err)
	}
}

func TestForgeOriginPlacementPositiveHistoryLiveDB(t *testing.T) {
	for _, kind := range []string{"no_adopted_source", "forge_no_output", "publication", "owner_discard",
		"merged_predecessor_discard", "completed_publication", "held_publication", "archive"} {
		t.Run(kind, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			f.history(kind)
			f.safe(true)
			f.peerClaim(true)
			// The new open generation makes a second cold placement unsafe.
			f.safe(false)
		})
	}
}

func TestForgeOriginPlacementHistoryFencesLiveDB(t *testing.T) {
	for _, name := range []string{"missing_middle", "extra_generation", "legacy", "unknown_inventory",
		"current_worker_mismatch", "repo_mismatch", "owner_mismatch", "open_original", "open_live",
		"bare_release", "warm_prior_work", "prerequisite", "conflicting_captures", "expired_archive",
		"other_origin", "other_owner_origin", "manual_cordon", "ineligible_peer"} {
		t.Run(name, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			switch name {
			case "legacy", "bare_release":
				h := f.open(1, false)
				if name == "legacy" {
					f.finish(h, "no_adopted_source", "no_adopted_source")
				} else {
					f.exec(`UPDATE recovery_custody_holds SET state='released',release_evidence='forge_no_output',
						live_worker_id=NULL,live_run_id=NULL,released_at=now() WHERE id=$1`, h)
				}
				f.queue()
			case "unknown_inventory":
				f.open(1, true)
				f.queue()
			case "warm_prior_work", "prerequisite", "conflicting_captures":
				h := f.open(1, true)
				prereqs := []string{}
				if name == "prerequisite" {
					prereqs = []string{heldTip}
				}
				f.capture(h, heldTip, prereqs)
				if name == "conflicting_captures" {
					f.capture(h, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{})
				}
				f.exec("UPDATE runs SET status='failed' WHERE id=$1", f.run)
				// Discard is valid custody evidence, but does not erase dependencies.
				// A bare capture alone is not FINAL inventory.
				if name != "warm_prior_work" {
					f.exec(`UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',
						live_worker_id=NULL,live_run_id=NULL WHERE id=$1`, h)
				}
				f.queue()
			default:
				f.history("no_adopted_source")
				switch name {
				case "missing_middle":
					h := f.open(3, true)
					f.finish(h, "no_adopted_source", "no_adopted_source")
					f.queue()
				case "extra_generation":
					h := f.open(2, true)
					f.finish(h, "no_adopted_source", "no_adopted_source")
					f.exec("UPDATE runs SET claim_generation=1 WHERE id=$1", f.run)
					f.queue()
				case "current_worker_mismatch":
					f.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", f.run, f.peer)
					f.peer = f.worker("third-peer", 0, false)
				case "repo_mismatch":
					repo := uuid.New()
					f.exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url)
					SELECT $1,connection_id,forge_project_id+1000,'g/other',web_url FROM repos WHERE id=$2`, repo, f.repoID)
					f.exec("UPDATE runs SET repo_id=$2 WHERE id=$1", f.run, repo)
				case "owner_mismatch":
					other := newRollAffinityFixture(t)
					f.exec("UPDATE runs SET user_id=$2 WHERE id=$1", f.run, other.userID)
				case "open_original", "open_live":
					h := f.open(2, true)
					// Isolate each side of the original OR live custodian check.
					original, live := f.owner, f.peer
					if name == "open_live" {
						original, live = f.peer, f.owner
					}
					f.exec("DELETE FROM recovery_custody_holds WHERE id=$1", h)
					f.exec(`INSERT INTO recovery_custody_holds
						(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
						live_worker_id,live_run_id,inventory_guarded)
						VALUES($1,$2,$3,$4,2,'open',$5,'older-generation',$6,$4,true)`,
						h, f.userID, f.repoID, f.run, original, live)
					// It is an older-generation hold, not merely the current generation.
					current := f.open(3, true)
					f.finish(current, "no_adopted_source", "no_adopted_source")
					f.queue()
				case "expired_archive":
					// Build a separate valid FINAL, then expire its available archive.
					f2 := newForgePlacementFixture(t)
					f = f2
					f.history("archive")
					f.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 day' WHERE hold_id=$1", f.holds[0])
				case "other_origin":
					f.exec("UPDATE repos SET web_url='https://other.example/g/r' WHERE id=$1", f.repoID)
				case "other_owner_origin":
					other := newRollAffinityFixture(t)
					w := other.worker("other-owner-cooled", 0, false)
					forgeAdmissionCooldown(other, w, false)
					f.exec("DELETE FROM forge_origin_cooldowns WHERE worker_id=$1", f.owner)
				case "manual_cordon":
					manualChange(t, f.rollAffinityFixture, f.peer, true)
				case "ineligible_peer":
					forgeAdmissionCooldown(f.rollAffinityFixture, f.peer, false)
				}
			}
			safe := name == "other_origin" || name == "other_owner_origin" || name == "manual_cordon" || name == "ineligible_peer"
			f.safe(safe)
			// Owner mismatch is checked on the real row; the claimant still belongs to
			// the original owner and cannot steal it across the user boundary.
			f.peerClaim(false)
		})
	}
}

func TestForgeOriginPlacementFinalBindingLiveDB(t *testing.T) {
	for _, name := range []string{"capture", "source", "digest"} {
		t.Run(name, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			hold := f.open(1, true)
			f.exec("UPDATE runs SET status='failed' WHERE id=$1", f.run)
			capture := f.capture(hold, heldTip, []string{})
			source, digest := heldTip, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			switch name {
			case "capture":
				capture = f.capture(hold, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{})
			case "source":
				source = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			case "digest":
				digest = heldDigest
			}
			tag, err := f.pool.Exec(f.ctx, `UPDATE recovery_custody_holds SET state='released',
				live_worker_id=NULL,live_run_id=NULL,final_disposition='archive',release_evidence='archive',
				final_capture_id=$2,final_source_sha=$3,final_coverage_digest=$4,released_at=now() WHERE id=$1`,
				hold, capture, source, digest)
			if err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("mismatched FINAL must be refused: rows=%d err=%v", tag.RowsAffected(), err)
			}
			f.queue()
			f.safe(false)
			f.peerClaim(false)
		})
	}
	for _, name := range []string{"not_acknowledged", "expired", "owner_expired"} {
		t.Run(name, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			if name == "not_acknowledged" {
				f.history("held_created")
			} else {
				f.history("held_publication")
			}
			switch name {
			case "not_acknowledged": // Leave the exact FINAL receipt at created.
			case "expired":
				f.exec("UPDATE run_held_publications SET expires_at=now()-interval '1 day' WHERE hold_id=$1", f.holds[0])
			case "owner_expired":
				f.exec("UPDATE run_held_publications SET owner_expired_at=now() WHERE hold_id=$1", f.holds[0])
			}
			f.safe(false)
			f.peerClaim(false)
		})
	}
}

func TestForgeOriginPlacementCompletedReceiptExactLiveDB(t *testing.T) {
	f := newForgePlacementFixture(t)
	h := f.open(1, true)
	f.exec("UPDATE runs SET mr_iid=42,status='completed',completion_final_head=repeat('a',40) WHERE id=$1", f.run)
	_, err := f.pool.Exec(f.ctx, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
		final_disposition='completed_publication',release_evidence='completed_publication',
		completed_publication_receipt=completion_identity ||
			jsonb_build_object('observed_branch_head',repeat('b',40),'worker_id',$2::uuid),
		released_at=now() WHERE id=$1`, h, f.peer)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("receipt with substituted worker must violate exact receipt CHECK: %v", err)
	}
	f.queue()
	f.safe(false)
	f.peerClaim(false)
}

func TestForgeOriginPlacementSuccessorColumnsLiveDB(t *testing.T) {
	for _, assignment := range []string{
		"release_successor_generation=2", "release_pushed_sha=repeat('a',40)",
		"release_source_sha=repeat('b',40)", "release_adopted_sha=repeat('c',40)",
		"release_final_head_sha=repeat('d',40)", "release_branch='agent/issue-1'",
	} {
		t.Run(assignment, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			f.history("forge_no_output")
			// These historical audit columns are schema-valid on a settled release;
			// placement must check each one rather than trust the evidence label.
			f.exec("UPDATE recovery_custody_holds SET "+assignment+" WHERE id=$1", f.holds[0])
			f.safe(false)
			f.peerClaim(false)
		})
	}
}

func TestForgeOriginPlacementDuplicateGenerationLiveDB(t *testing.T) {
	f := newForgePlacementFixture(t)
	f.history("owner_discard")
	duplicate := uuid.New()
	_, err := f.pool.Exec(f.ctx, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded,live_worker_id,live_run_id)
		VALUES($1,$2,$3,$4,1,'open',$5,'duplicate',true,$5,$4)`, duplicate, f.userID, f.repoID, f.run, f.owner)
	if err != nil {
		t.Fatalf("schema-valid duplicate fixture: %v", err)
	}
	f.exec(`UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',
		live_worker_id=NULL,live_run_id=NULL WHERE id=$1`, duplicate)
	third := f.open(3, true)
	f.exec(`UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',
		live_worker_id=NULL,live_run_id=NULL WHERE id=$1`, third)
	f.queue()
	// Exactly three positive, guarded, closed holds at [1,1,3] satisfy total
	// count and range at G=3. Only DISTINCT detects the missing generation.
	f.safe(false)
	f.peerClaim(false)
}

func TestForgeOriginPlacementIncomingCustodyLiveDB(t *testing.T) {
	f := newForgePlacementFixture(t)
	f.history("no_adopted_source")
	predecessor := f.pinnedRun(f.owner, 0)
	f.exec("UPDATE runs SET claim_generation=1,status='failed' WHERE id=$1", predecessor)
	// Legacy live custody can retain another run's immutable provenance while
	// pointing at this run. Its absence from this run's generation ledger is
	// not evidence that the retained source is gone.
	f.exec(`INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
		 live_worker_id,live_run_id,inventory_guarded)
		VALUES($1,$2,$3,$4,1,'open',$5,'predecessor',$5,$6,false)`,
		uuid.New(), f.userID, f.repoID, predecessor, f.owner, f.run)
	f.peerClaim(false)
	f.safe(false)
}

// No sleeps: all clock boundaries use SQL timestamps, all concurrency is joined.
func TestForgeOriginPlacementClaimTransactionsLiveDB(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			f.history("archive")
			other := f.worker("concurrent-peer", 0, false)
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(f.ctx) }()
			got, err := forgeAdmissionClaim(f.rollAffinityFixture, store.New(tx), f.peer)
			if err != nil || got.ID != f.run {
				t.Fatalf("transaction claim: %s %v", got.ID, err)
			}
			result := make(chan error, 1)
			go func() { _, err := forgeAdmissionClaim(f.rollAffinityFixture, f.q, other); result <- err }()
			if err := <-result; !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("concurrent locked claim: %v", err)
			}
			if commit {
				if err := tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
				var retained bool
				if err := f.pool.QueryRow(f.ctx, `SELECT status='claimed' AND worker_id=$2 AND claim_generation=2
					AND (SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1)=2 FROM runs WHERE id=$1`,
					f.run, f.peer).Scan(&retained); err != nil || !retained {
					t.Fatalf("commit custody: %v %v", retained, err)
				}
			} else {
				if err := tx.Rollback(f.ctx); err != nil {
					t.Fatal(err)
				}
				f.safe(true)
				f.peerClaim(true)
			}
		})
	}
}
