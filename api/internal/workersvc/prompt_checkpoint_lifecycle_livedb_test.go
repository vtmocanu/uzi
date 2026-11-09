package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4116: a prompt run is checkpoint-eligible under the key uzi/prompt-<run id>. The publish
// side is pinned through the worker routes in the handler package; these tests pin the two
// lifecycle consumers that read the same key after the run ends: the terminal retention record
// (the migration 00354 trigger through fn_checkpoint_coordinates) and the failed-run salvage
// sweep (SweepSalvage). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway database;
// run via ./e2e/run-store-it.sh.

const (
	promptLifecycleTipA = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	promptLifecycleTipB = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

type promptLifecycleFix struct {
	env  codexTestEnv
	user uuid.UUID
	repo uuid.UUID
	iid  int64
}

// newPromptLifecycleFix seeds one fresh owner with a github connection (its bot token sealed so
// the salvage sweep can open it) and one repo. Cleanup removes the salvage and retention rows
// first, because a promoted salvage row's RESTRICT pointer would otherwise block the owner delete.
func newPromptLifecycleFix(t *testing.T) *promptLifecycleFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	f := &promptLifecycleFix{env: env, user: env.seedUser(t), repo: uuid.New()}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, q := range []string{
			"DELETE FROM run_salvage WHERE user_id = $1",
			"DELETE FROM checkpoint_retentions WHERE user_id = $1",
			"DELETE FROM users WHERE id = $1",
		} {
			if _, err := env.pool.Exec(ctx, q, f.user); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	sealed, err := env.box.Seal([]byte("prompt-lifecycle-bot-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	conn := uuid.New()
	env.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, f.user, sealed)
	env.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,9,$3,$4,'main',true)`, f.repo, conn, "g/pl-"+f.repo.String(), "https://forge.e2e/g/pl-"+f.repo.String())
	return f
}

// seedRun inserts a running run of kind that already published tip, and returns its id and the
// checkpoint key the api must use for it. Issue and self_improve runs carry a (distinct) issue iid.
func (f *promptLifecycleFix) seedRun(t *testing.T, kind, tip string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	key := "uzi/prompt-" + id.String()
	if kind == "prompt" {
		f.env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_title,issue_description,status)
 VALUES ($1,$2,$3,'prompt','t','d','running')`, id, f.user, f.repo)
	} else {
		f.iid++
		iid := int64(id[0]) + int64(id[1])*256 + int64(id[2])*65536 + f.iid
		f.env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status)
 VALUES ($1,$2,$3,$4,$5,'t','d','running')`, id, f.user, f.repo, kind, iid)
		key = "uzi/self-improve/" + id.String()
		if kind == "issue" {
			key = fmt.Sprintf("agent/issue-%d", iid)
		}
	}
	f.env.exec("UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1", id, tip)
	return id, key
}

// TestPromptCheckpointRetentionLiveDB: a prompt run with a published checkpoint that turns
// terminal gets a retention record on its prompt key, exactly as an issue or self_improve run does
// on theirs (the controls). The record is written by the status trigger in the terminal
// transaction itself, with no Go retention call.
func TestPromptCheckpointRetentionLiveDB(t *testing.T) {
	cases := []struct{ kind, terminal string }{
		{"prompt", "failed"},
		{"prompt", "cancelled"},
		{"prompt", "completed"},
		{"issue", "failed"},
		{"self_improve", "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+"/"+tc.terminal, func(t *testing.T) {
			f := newPromptLifecycleFix(t)
			id, key := f.seedRun(t, tc.kind, promptLifecycleTipA)
			if _, err := f.env.q.GetCheckpointRetention(f.env.ctx, id); err == nil {
				t.Fatal("a running run already has a retention record")
			}
			f.env.exec("UPDATE runs SET status=$2, finished_at=now() WHERE id=$1", id, tc.terminal)
			row, err := f.env.q.GetCheckpointRetention(f.env.ctx, id)
			if err != nil {
				t.Fatalf("terminal %s %s run has no retention record: %v", tc.terminal, tc.kind, err)
			}
			if row.Branch != key || row.Ref != "refs/uzi-checkpoints/"+key || row.Tip != promptLifecycleTipA ||
				row.UserID != f.user || row.RepoID != f.repo {
				t.Fatalf("retention = {branch %q ref %q tip %q}; want {%q %q %q}",
					row.Branch, row.Ref, row.Tip, key, "refs/uzi-checkpoints/"+key, promptLifecycleTipA)
			}
		})
	}
}

// TestPromptCheckpointSalvageLiveDB: one SweepSalvage enqueues both a failed prompt run and a
// LATER (newer) failed issue run on a salvage-enabled forge, each under its own checkpoint key, and
// then copies each tip from its own checkpoint ref into refs/uzi-salvage/<run id>. The prompt run
// being first in the oldest-first candidate order must not stop the sweep before the issue run.
func TestPromptCheckpointSalvageLiveDB(t *testing.T) {
	f := newPromptLifecycleFix(t)
	env := f.env

	var now time.Time
	if err := env.pool.QueryRow(env.ctx, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	// Keep every other test's historical failures outside this sweep's enqueue window: the clock
	// and both finish times sit 30 days ahead of them. Nothing waits.
	now = now.Add(30 * 24 * time.Hour)

	prompt, promptKey := f.seedRun(t, "prompt", promptLifecycleTipA)
	issue, issueKey := f.seedRun(t, "issue", promptLifecycleTipB)
	env.exec("UPDATE runs SET status='failed', finished_at=$2 WHERE id=$1", prompt, now.Add(-2*time.Hour))
	env.exec("UPDATE runs SET status='failed', finished_at=$2 WHERE id=$1", issue, now.Add(-time.Hour))

	origin := newMemForge()
	origin.set(checkpointRefPrefix+promptKey, promptLifecycleTipA)
	origin.set(checkpointRefPrefix+issueKey, promptLifecycleTipB)
	sources := map[uuid.UUID]string{}
	p := testParams()
	p.SalvageForges = []string{"github"}
	p.RecoveryReadyRetention = 24 * time.Hour
	svc := New(env.q, env.box, p)
	svc.SetClock(func() time.Time { return now })
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	svc.salvageListRefTipsFn = origin.listRefTips
	svc.salvageDeleteRefFn = origin.deleteRef
	svc.salvageCreateRefFn = func(_ context.Context, o pushbroker.CreateRefOptions) error {
		if src, ok := origin.ref(o.SourceRef); !ok || src != o.Tip {
			return pushbroker.ErrSourceMissing
		}
		if !origin.land(o.Ref, "", o.Tip) {
			return pushbroker.ErrRefExists
		}
		for _, id := range []uuid.UUID{prompt, issue} {
			if o.Ref == pushbroker.SalvageRef(id) {
				sources[id] = o.SourceRef
			}
		}
		return nil
	}

	row := func(id uuid.UUID) (store.RunSalvage, error) { return env.q.GetRunSalvage(env.ctx, id) }
	wantBranch := map[uuid.UUID]string{prompt: promptKey, issue: issueKey}
	wantTip := map[uuid.UUID]string{prompt: promptLifecycleTipA, issue: promptLifecycleTipB}

	if _, err := svc.SweepSalvage(env.ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	for _, id := range []uuid.UUID{prompt, issue} {
		r, err := row(id)
		if err != nil {
			t.Fatalf("run %s not enqueued by the first sweep: %v", id, err)
		}
		if r.Branch != wantBranch[id] || r.Tip != wantTip[id] || r.ForgeType != "github" ||
			r.UserID != f.user || r.RepoID != f.repo || !r.LiveRunID.Valid || uuid.UUID(r.LiveRunID.Bytes) != id {
			t.Fatalf("salvage row for %s = {branch %q tip %q forge %q}; want branch %q tip %q",
				id, r.Branch, r.Tip, r.ForgeType, wantBranch[id], wantTip[id])
		}
	}

	// Each pass advances at most a few rows instance-wide, so allow a bounded number of passes for
	// both of ours to reach promoted. Every pass must succeed.
	for pass := 0; pass < 8; pass++ {
		done := true
		for _, id := range []uuid.UUID{prompt, issue} {
			if r, err := row(id); err != nil || r.State != "promoted" {
				done = false
			}
		}
		if done {
			break
		}
		if _, err := svc.SweepSalvage(env.ctx); err != nil {
			t.Fatalf("sweep %d: %v", pass+2, err)
		}
	}
	for _, id := range []uuid.UUID{prompt, issue} {
		r, err := row(id)
		if err != nil || r.State != "promoted" || r.LastError.Valid {
			t.Fatalf("salvage row for %s = %+v err=%v; want promoted with no error", id, r, err)
		}
		if got, ok := origin.ref(pushbroker.SalvageRef(id)); !ok || got != wantTip[id] {
			t.Fatalf("refs/uzi-salvage/%s = %q (present %v); want %s", id, got, ok, wantTip[id])
		}
		if want := checkpointRefPrefix + wantBranch[id]; sources[id] != want {
			t.Fatalf("salvage of %s copied from %q; want its own checkpoint ref %q", id, sources[id], want)
		}
	}
}

// TestPromptCheckpointRepoLessLiveDB: a repo-less prompt run cannot exist (runs_kind_shape), and
// if one did, fn_checkpoint_coordinates (the retention, salvage and recovery derivation) resolves
// no key for it. Its publication would answer no_ref, but from GetRunClaimContext's repo join, not
// from this function. The NULL comes from the general repo_id guard ahead of every kind arm, so it
// is not specific to the prompt arm; the repo-backed row resolving its prompt key is the control
// that is. The synthetic record is the stored prompt row with its repo cleared.
func TestPromptCheckpointRepoLessLiveDB(t *testing.T) {
	f := newPromptLifecycleFix(t)
	env := f.env
	_, err := env.pool.Exec(env.ctx, `INSERT INTO runs (id,user_id,repo_id,kind,issue_title,issue_description,status)
 VALUES ($1,$2,NULL,'prompt','t','d','running')`, uuid.New(), f.user)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_kind_shape" {
		t.Fatalf("repo-less prompt insert err = %v; want 23514 runs_kind_shape", err)
	}
	id, key := f.seedRun(t, "prompt", promptLifecycleTipA)
	var got *string
	if err := env.pool.QueryRow(env.ctx, `SELECT fn_checkpoint_coordinates(r)->>'checkpoint_key' FROM runs r WHERE id=$1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != key {
		t.Fatalf("repo-backed prompt checkpoint_key = %v; want %s", got, key)
	}
	var isNull bool
	if err := env.pool.QueryRow(env.ctx, `SELECT fn_checkpoint_coordinates(jsonb_populate_record(NULL::runs, to_jsonb(r) || '{"repo_id":null}'::jsonb)) IS NULL FROM runs r WHERE id=$1`, id).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("a repo-less prompt record resolved checkpoint coordinates")
	}
}
