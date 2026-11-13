package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestCustodyUnitsEpisodeQueriesLiveDB (issue #4162): the owner episode scans read admission UNITS.
// One failed run with five open holds on one worker plus three other holds is eight raw open holds
// but four units, so ListOwnersOverCustodyLimit lists the owner only at limits 4 and below, and
// ListOwnersWithClearedCustodyEpisode treats a notified owner as cleared only above that. Counting
// raw holds would list the owner at limit 8.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestCustodyUnitsEpisodeQueriesLiveDB(t *testing.T) {
	ctx, pool, q := openCustodyEpisodeLiveDB(t)
	s := &custodyEpisodeSeeder{ctx: ctx, t: t, pool: pool}
	owner := s.user()
	t.Cleanup(func() {
		// Plain custody owner columns need explicit cleanup before cascading the user, and the
		// owner scans are instance-wide, so a leftover owner would leak into sibling tests.
		s.exec(`DELETE FROM recovery_captures WHERE user_id=$1`, owner)
		s.exec(`DELETE FROM recovery_custody_holds WHERE user_id=$1`, owner)
		s.exec(`DELETE FROM users WHERE id=$1`, owner)
	})

	conn, repo, worker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	s.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
	  VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, owner, []byte{1})
	s.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
	  VALUES ($1,$2,1,'g/units-episode','https://forge.e2e/g/units-episode','main',true)`, repo, conn)
	s.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at)
	  VALUES ($1,$2,'units-episode',$3,'offline',$4)`, worker, owner, worker[:], time.Now().Add(-24*time.Hour))
	s.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,finished_at)
	  VALUES ($1,$2,$3,'issue',1,'units','d','failed',5,now())`, run, owner, repo)
	for gen := int64(1); gen <= 5; gen++ {
		s.exec(`INSERT INTO recovery_custody_holds
		  (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
		  VALUES ($1,$2,$3,$4,$5,'open',$6,'units-episode',$6,$4)`, uuid.New(), owner, repo, run, gen, worker)
	}
	s.openHolds(owner, 3) // three singleton holds on other runs

	var units int64
	if err := pool.QueryRow(ctx, `SELECT fn_custody_admission_count($1,$2)`, owner, time.Now().Add(-45*time.Second)).Scan(&units); err != nil || units != 4 {
		t.Fatalf("precondition: owner units = %d (%v), want 4 (raw open holds are 8)", units, err)
	}
	if got, err := q.ClaimCustodyEpisodeNotice(ctx, owner); err != nil || got != owner {
		t.Fatalf("claim notice = (%v, %v), want (%v, nil)", got, err, owner)
	}

	contains := func(ids []uuid.UUID) bool {
		for _, id := range ids {
			if id == owner {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		limit       int32
		listed      bool
		clearedWant bool
	}{
		{limit: 8, listed: false, clearedWant: true}, // eight raw holds, four units: below the limit
		{limit: 5, listed: false, clearedWant: true}, // one unit short of the limit
		{limit: 4, listed: true, clearedWant: false}, // at the limit in units
		{limit: 3, listed: true, clearedWant: false}, // over the limit in units
	} {
		if got := contains(s.listOver(tc.limit)); got != tc.listed {
			t.Fatalf("limit %d: ListOwnersOverCustodyLimit lists the owner = %v, want %v (units 4)", tc.limit, got, tc.listed)
		}
		if got := contains(s.listCleared(tc.limit)); got != tc.clearedWant {
			t.Fatalf("limit %d: ListOwnersWithClearedCustodyEpisode lists the owner = %v, want %v (units 4)", tc.limit, got, tc.clearedWant)
		}
	}
}
