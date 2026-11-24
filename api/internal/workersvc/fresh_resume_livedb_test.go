package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Fresh resume (issue #4628) service proofs against a real Postgres: evidence selection and its token,
// the R5 start rules, owner Resume on a pending row, and RefuseFreshResumeInTx. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

func freshSha(c byte) string { return strings.Repeat(string(c), 40) }

// freshFakeForge is the CodeHost the selection reads: a default branch head, per-ref heads and a
// per-pair ancestry table. Anything unlisted is "not an ancestor" / "ref not found".
type freshFakeForge struct {
	forgetest.BaseFake
	mu      sync.Mutex
	head    string
	refs    map[string]string
	anc     map[[2]string]forge.Ancestry
	refErr  error
	compare int
}

func (f *freshFakeForge) BranchHead(context.Context, int64, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, nil
}

func (f *freshFakeForge) RefHead(_ context.Context, _ int64, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refErr != nil {
		return "", f.refErr
	}
	if h, ok := f.refs[ref]; ok {
		return h, nil
	}
	return "", forge.ErrRefNotFound
}

func (f *freshFakeForge) CompareAncestry(_ context.Context, _ int64, head, candidate string) (forge.Ancestry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compare++
	if a, ok := f.anc[[2]string{head, candidate}]; ok {
		return a, nil
	}
	return forge.AncestryNotAncestor, nil
}

type freshForgeBuilder struct{ f *freshFakeForge }

func (b freshForgeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

type freshFix struct {
	t     *testing.T
	env   codexTestEnv
	svc   *Service
	fg    *freshFakeForge
	user  uuid.UUID
	repo  uuid.UUID
	w1    uuid.UUID
	w2    uuid.UUID
	run   uuid.UUID
	iid   int64
	limit *fakeCustodyLimit
}

// newFreshFix seeds an owner with a held run (worker_requeue_exhausted recovery_wait at generation gen,
// bound to W1 with an open custody hold at gen) and one eligible destination worker W2.
func newFreshFix(t *testing.T, gate bool, gen int64) *freshFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	fg := &freshFakeForge{head: freshSha('d'), refs: map[string]string{}, anc: map[[2]string]forge.Ancestry{}}
	limit := &fakeCustodyLimit{n: testCustodyLimit}
	params := testParams()
	params.FreshResume = gate
	params.CustodyHoldLimit = limit
	svc := snapshotSvc(env, params)
	svc.SetForges(freshForgeBuilder{f: fg})
	f := &freshFix{t: t, env: env, svc: svc, fg: fg, user: user, repo: repo, limit: limit}
	f.w1 = seedSnapshotWorker(t, env, user, "fresh-w1")
	f.run = seedOutageRun(t, env, user, repo, f.w1, "recovery_wait", "issue", gen, 0)
	env.exec(`UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted', claim_released_at=now(), released_worker_id=$2 WHERE id=$1`, f.run, f.w1)
	if err := env.pool.QueryRow(env.ctx, `SELECT issue_iid FROM runs WHERE id=$1`, f.run).Scan(&f.iid); err != nil {
		t.Fatal(err)
	}
	f.w2 = f.destination(user, capability.FreshResumeV1, capability.RecoveryInventoryV1)
	// run_fresh_resumes.capture_id is ON DELETE RESTRICT: rows left behind would fail a later package
	// test's unscoped DELETE FROM recovery_captures on the shared database. Several tests seed more
	// than one run, so the cleanup is by owner.
	t.Cleanup(func() {
		if _, err := env.pool.Exec(context.Background(), `DELETE FROM run_fresh_resumes WHERE user_id=$1`, user); err != nil {
			t.Errorf("cleanup run_fresh_resumes: %v", err)
		}
	})
	return f
}

// destination inserts an online hosted worker of owner with the given protocol capabilities.
func (f *freshFix) destination(owner uuid.UUID, caps ...string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		id, owner, "dest-"+id.String(), id[:])
	f.env.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', protocol_capabilities=$2, last_heartbeat_at=now() WHERE id=$1`, id, caps)
	return id
}

// hold opens a custody hold of the run at generation gen held by worker.
func (f *freshFix) hold(gen int64, worker uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
	            VALUES($1,$2,$3,$4,$5,'open',$6,'fresh-fixture',$6,$4)`, id, f.user, f.repo, f.run, gen, worker)
	return id
}

// capture inserts a capture of the run under a new hold at generation gen, in the given state, with the
// given source sha, prerequisites and retention left.
func (f *freshFix) capture(gen int64, state, sha string, prereqs []string, ttl time.Duration) uuid.UUID {
	f.t.Helper()
	hold := f.hold(gen, f.w1)
	id := uuid.New()
	if prereqs == nil {
		prereqs = []string{}
	}
	if state == "available" {
		f.env.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,prerequisite_shas,expires_at)
		            VALUES($1,$2,$3,$4,$5,'fresh-fixture',$6,$7,'available',true,3,'deadbeef',$8,now()+make_interval(secs=>$9))`,
			id, hold, f.run, f.user, f.w1, sha, "k-"+id.String(), prereqs, ttl.Seconds())
	} else {
		f.env.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,prerequisite_shas)
		            VALUES($1,$2,$3,$4,$5,'fresh-fixture',$6,$7,$8,$9)`,
			id, hold, f.run, f.user, f.w1, sha, "k-"+id.String(), state, prereqs)
	}
	return id
}

// tip stamps the run's checkpoint tip; gen < 0 leaves the generation NULL (an unbound tip).
func (f *freshFix) tip(sha string, gen int64) {
	f.t.Helper()
	if gen < 0 {
		f.env.exec(`UPDATE runs SET checkpoint_tip=$2, checkpoint_tip_generation=NULL, checkpoint_tip_generation_tip=NULL WHERE id=$1`, f.run, sha)
		return
	}
	f.env.exec(`UPDATE runs SET checkpoint_tip=$2, checkpoint_tip_generation=$3, checkpoint_tip_generation_tip=$2 WHERE id=$1`, f.run, sha, gen)
}

func (f *freshFix) ref() string { return fmt.Sprintf("refs/uzi-checkpoints/agent/issue-%d", f.iid) }

func (f *freshFix) preview() apitypes.FreshResumePreviewDTO {
	f.t.Helper()
	dto, err := f.svc.PreviewFreshResume(f.env.ctx, f.user, f.run)
	if err != nil {
		f.t.Fatalf("PreviewFreshResume: %v", err)
	}
	return dto
}

func (f *freshFix) start(token string, request uuid.UUID) (FreshResumeStartResult, error) {
	return f.svc.StartFreshResume(f.env.ctx, f.user, f.run, token, request)
}

func (f *freshFix) rows() int {
	f.t.Helper()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_fresh_resumes WHERE run_id=$1`, f.run).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *freshFix) runJSON() string {
	f.t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT row_to_json(r)::text FROM runs r WHERE r.id=$1`, f.run).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *freshFix) version() int64 {
	f.t.Helper()
	var v int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT fresh_hold_version FROM runs WHERE id=$1`, f.run).Scan(&v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

func wantRefusal(t *testing.T, what string, err error, reason string) {
	t.Helper()
	var refused *FreshResumeRefused
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("%s: err = %v, want refusal %q", what, err, reason)
	}
}

func TestFreshResumeSelectionClassesLiveDB(t *testing.T) {
	t.Run("capture", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		p1 := freshSha('1')
		f.fg.anc[[2]string{f.fg.head, p1}] = forge.AncestryAncestor
		cap := f.capture(2, "available", freshSha('a'), []string{p1}, 2*time.Hour)
		dto := f.preview()
		if dto.Status != apitypes.FreshResumeStatusAvailable || dto.Kind != apitypes.FreshResumeKindCapture ||
			dto.CaptureID != cap.String() || dto.Sha != freshSha('a') || dto.EvidenceGeneration != 2 || dto.ClaimGeneration != 2 ||
			dto.SelectionToken == "" || len(dto.UncoveredGenerations) != 0 || len(dto.Notes) < 5 {
			t.Fatalf("preview = %+v", dto)
		}
	})
	t.Run("checkpoint ref", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.hold(2, f.w1)
		f.tip(freshSha('c'), 2)
		f.fg.refs[f.ref()] = freshSha('c')
		dto := f.preview()
		if dto.Status != apitypes.FreshResumeStatusAvailable || dto.Kind != apitypes.FreshResumeKindCheckpointRef ||
			dto.CheckpointRef != f.ref() || dto.Sha != freshSha('c') || dto.EvidenceGeneration != 2 {
			t.Fatalf("preview = %+v", dto)
		}
		// The ref moved on: the evidence as stamped is gone.
		f.fg.refs[f.ref()] = freshSha('e')
		if got := f.preview().Status; got != apitypes.FreshResumeStatusExpired {
			t.Fatalf("moved ref status = %q, want expired", got)
		}
		delete(f.fg.refs, f.ref())
		if got := f.preview().Status; got != apitypes.FreshResumeStatusExpired {
			t.Fatalf("missing ref status = %q, want expired", got)
		}
		f.fg.refErr = errors.New("forge down")
		if got := f.preview().Status; got != apitypes.FreshResumeStatusPrereqUnproven {
			t.Fatalf("unreadable ref status = %q, want prereq_unproven", got)
		}
	})
	t.Run("ref needs an open hold", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.tip(freshSha('c'), 2)
		f.fg.refs[f.ref()] = freshSha('c')
		if got := f.preview().Status; got != apitypes.FreshResumeStatusNoEvidence {
			t.Fatalf("holdless ref status = %q, want no_evidence", got)
		}
	})
	t.Run("unbound tip", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.hold(2, f.w1)
		f.tip(freshSha('c'), -1)
		f.fg.refs[f.ref()] = freshSha('c')
		if got := f.preview().Status; got != apitypes.FreshResumeStatusUnbound {
			t.Fatalf("lone unbound tip status = %q, want unbound", got)
		}
		// A capture that does not contain the tip leaves it unbound.
		f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusUnbound {
			t.Fatalf("capture without the tip status = %q, want unbound", got)
		}
		// A capture that lists the tip as a prerequisite dominates it.
		f.fg.anc[[2]string{f.fg.head, freshSha('c')}] = forge.AncestryAncestor // an unbound tip is not a proof by equality
		f.env.exec(`UPDATE recovery_captures SET prerequisite_shas=$2 WHERE run_id=$1`, f.run, []string{freshSha('c')})
		dto := f.preview()
		if dto.Status != apitypes.FreshResumeStatusAvailable || dto.Kind != apitypes.FreshResumeKindCapture {
			t.Fatalf("dominating capture preview = %+v", dto)
		}
		// A stamped tip whose stamp names another tip is unbound too.
		f.env.exec(`UPDATE runs SET checkpoint_tip_generation=2, checkpoint_tip_generation_tip=$2 WHERE id=$1`, f.run, freshSha('f'))
		f.env.exec(`UPDATE recovery_captures SET prerequisite_shas='{}' WHERE run_id=$1`, f.run)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusUnbound {
			t.Fatalf("restamped tip status = %q, want unbound", got)
		}
	})
	t.Run("expired", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.capture(2, "expired", freshSha('a'), nil, 0)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusExpired {
			t.Fatalf("expired capture status = %q", got)
		}
		// Inside the 30 minute admission window counts as expired too.
		f.capture(2, "available", freshSha('b'), nil, 10*time.Minute)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusExpired {
			t.Fatalf("capture with 10 minutes left status = %q, want expired", got)
		}
	})
	t.Run("no evidence", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusNoEvidence {
			t.Fatalf("status = %q, want no_evidence", got)
		}
		f.capture(2, "discarded", freshSha('a'), nil, 0)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusNoEvidence {
			t.Fatalf("discarded-only status = %q, want no_evidence", got)
		}
	})
	t.Run("prerequisites unproven", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.capture(2, "available", freshSha('a'), []string{freshSha('1')}, 2*time.Hour)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusPrereqUnproven {
			t.Fatalf("status = %q, want prereq_unproven", got)
		}
		// The stamped checkpoint tip proves a prerequisite by equality, with no forge question.
		f.tip(freshSha('1'), 1)
		f.fg.compare = 0
		if got := f.preview().Status; got != apitypes.FreshResumeStatusAvailable {
			t.Fatalf("tip-equal prerequisite status = %q, want available", got)
		}
		if f.fg.compare != 0 {
			t.Fatalf("ancestry asked %d times for an equal prerequisite", f.fg.compare)
		}
	})
	t.Run("dominance", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		sa, sb := freshSha('a'), freshSha('b')
		f.capture(2, "available", sa, nil, 2*time.Hour)
		// Two unrelated candidates: nothing contains the other.
		capB := f.capture(2, "available", sb, nil, 2*time.Hour)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusAmbiguous {
			t.Fatalf("unrelated captures status = %q, want ambiguous", got)
		}
		// B lists A as a prerequisite: B dominates.
		f.env.exec(`UPDATE recovery_captures SET prerequisite_shas=$2 WHERE id=$1`, capB, []string{sa})
		f.fg.anc[[2]string{f.fg.head, sa}] = forge.AncestryAncestor
		if dto := f.preview(); dto.Status != apitypes.FreshResumeStatusAvailable || dto.CaptureID != capB.String() {
			t.Fatalf("dominating capture preview = %+v, want capture %s", dto, capB)
		}
		// The forge proving A an ancestor of one of B's prerequisites also dominates.
		f.env.exec(`UPDATE recovery_captures SET prerequisite_shas=$2 WHERE id=$1`, capB, []string{freshSha('9')})
		f.fg.anc[[2]string{f.fg.head, freshSha('9')}] = forge.AncestryAncestor
		f.fg.anc[[2]string{freshSha('9'), sa}] = forge.AncestryAncestor
		if dto := f.preview(); dto.Status != apitypes.FreshResumeStatusAvailable || dto.CaptureID != capB.String() {
			t.Fatalf("ancestry-dominated preview = %+v, want capture %s", dto, capB)
		}
	})
	t.Run("equal evidence ties to the ref then the lowest capture", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.hold(2, f.w1)
		f.tip(freshSha('a'), 2)
		f.fg.refs[f.ref()] = freshSha('a')
		c1 := f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		c2 := f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		if dto := f.preview(); dto.Kind != apitypes.FreshResumeKindCheckpointRef {
			t.Fatalf("tie preview = %+v, want the checkpoint ref", dto)
		}
		f.env.exec(`UPDATE runs SET checkpoint_tip=NULL, checkpoint_tip_generation=NULL, checkpoint_tip_generation_tip=NULL WHERE id=$1`, f.run)
		want := c1
		if c2.String() < c1.String() {
			want = c2
		}
		if dto := f.preview(); dto.CaptureID != want.String() {
			t.Fatalf("tie preview = %+v, want the lowest capture %s", dto, want)
		}
	})
	t.Run("older generation disclosed", func(t *testing.T) {
		f := newFreshFix(t, true, 4)
		f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		dto := f.preview()
		if dto.Status != apitypes.FreshResumeStatusAvailable || dto.EvidenceGeneration != 2 || dto.ClaimGeneration != 4 ||
			len(dto.UncoveredGenerations) != 2 || dto.UncoveredGenerations[0] != 3 || dto.UncoveredGenerations[1] != 4 {
			t.Fatalf("preview = %+v, want evidence generation 2 with 3 and 4 uncovered", dto)
		}
		joined := strings.Join(dto.Notes, "\n")
		for _, want := range []string{"3, 4", "not included", "volume is gone", "extra admission unit", "third eligible hosted worker", "add a worker or cancel"} {
			if !strings.Contains(joined, want) {
				t.Errorf("notes lack %q:\n%s", want, joined)
			}
		}
	})
	t.Run("no fallback past a refused generation", func(t *testing.T) {
		f := newFreshFix(t, true, 3)
		f.capture(1, "available", freshSha('a'), nil, 2*time.Hour)
		f.capture(3, "expired", freshSha('b'), nil, 0)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusExpired {
			t.Fatalf("status = %q, want expired (generation 3 has evidence; generation 1 is not used)", got)
		}
	})
}

func TestFreshResumeDestinationLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	if got := f.preview().Status; got != apitypes.FreshResumeStatusAvailable {
		t.Fatalf("baseline status = %q", got)
	}
	setW2 := func(sql string) {
		f.t.Helper()
		f.env.exec(`UPDATE workers SET `+sql+` WHERE id=$1`, f.w2)
	}
	for _, c := range []struct{ name, set string }{
		{"missing fresh_resume_v1", `protocol_capabilities='{recovery_inventory_v1}'`},
		{"missing recovery_inventory_v1", `protocol_capabilities='{fresh_resume_v1}'`},
		{"not hosted", `kind='external', hosted_size=NULL, template_declared=NULL`},
		{"ephemeral", `ephemeral=true`},
		{"offline", `status='offline'`},
		{"stale heartbeat", `last_heartbeat_at=now()-interval '1 hour'`},
		{"draining", `draining_since=now()`},
	} {
		t.Run(c.name, func(t *testing.T) {
			setW2(c.set)
			dto := f.preview()
			if dto.Status != apitypes.FreshResumeStatusNoDestination || dto.SelectionToken != "" {
				t.Fatalf("preview = %+v, want no_destination with no token", dto)
			}
			_, err := f.start("anything", uuid.New())
			wantRefusal(t, "start", err, apitypes.FreshResumeStatusNoDestination)
			if f.rows() != 0 {
				t.Fatal("a refused start left a row")
			}
			f.env.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', ephemeral=false, status='online',
			            last_heartbeat_at=now(), draining_since=NULL, protocol_capabilities=$2 WHERE id=$1`,
				f.w2, []string{capability.FreshResumeV1, capability.RecoveryInventoryV1})
		})
	}
	t.Run("W1 and open-hold holders are excluded", func(t *testing.T) {
		// W2 itself holds an open custody hold of the run: it is a predecessor, not a destination.
		f.hold(2, f.w2)
		if got := f.preview().Status; got != apitypes.FreshResumeStatusNoDestination {
			t.Fatalf("holder status = %q, want no_destination", got)
		}
		// The only fresh-capable worker being W1 itself is no destination either.
		f.env.exec(`DELETE FROM recovery_custody_holds WHERE run_id=$1 AND live_worker_id=$2`, f.run, f.w2)
		f.env.exec(`UPDATE workers SET protocol_capabilities='{}' WHERE id=$1`, f.w2)
		f.env.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', protocol_capabilities=$2 WHERE id=$1`,
			f.w1, []string{capability.FreshResumeV1, capability.RecoveryInventoryV1})
		if got := f.preview().Status; got != apitypes.FreshResumeStatusNoDestination {
			t.Fatalf("W1-only status = %q, want no_destination", got)
		}
	})
}

func TestFreshResumeCustodyLimitLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour) // one open hold of the run
	// The new attempt adds a second hold of the run and a pending reservation: both must fit.
	f.limit.n = 2
	dto := f.preview()
	if dto.Status != apitypes.FreshResumeStatusCustodyLimit || dto.SelectionToken != "" {
		t.Fatalf("limit 2 preview = %+v, want custody_limit", dto)
	}
	_, err := f.start("anything", uuid.New())
	wantRefusal(t, "start at limit 2", err, apitypes.FreshResumeStatusCustodyLimit)
	f.limit.n = 3
	if got := f.preview().Status; got != apitypes.FreshResumeStatusAvailable {
		t.Fatalf("limit 3 status = %q, want available", got)
	}
	f.limit.n = 0
	if got := f.preview().Status; got != apitypes.FreshResumeStatusAvailable {
		t.Fatalf("limit 0 status = %q, want available (a non-positive limit admits)", got)
	}
}

func TestFreshResumeGateOffLiveDB(t *testing.T) {
	f := newFreshFix(t, false, 2)
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	dto := f.preview()
	if dto.Status != apitypes.FreshResumeStatusDisabled || dto.SelectionToken != "" {
		t.Fatalf("preview = %+v, want disabled", dto)
	}
	_, err := f.start("anything", uuid.New())
	wantRefusal(t, "start", err, apitypes.FreshResumeStatusDisabled)
	if f.rows() != 0 {
		t.Fatal("a disabled start left a row")
	}
	// With the gate off and no transfer rows, an owner Resume takes its unchanged autocommit path.
	run, err := f.env.q.GetRunByIDForUser(f.env.ctx, store.GetRunByIDForUserParams{ID: f.run, UserID: f.user})
	if err != nil {
		t.Fatal(err)
	}
	handled, err := f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, run, 36000)
	if handled || err != nil {
		t.Fatalf("gate off, no rows: handled=%v err=%v, want the unchanged path", handled, err)
	}
	// An outstanding transfer row still routes it through the locked path.
	f.env.exec(`INSERT INTO run_fresh_resumes (run_id,user_id,request_id,evidence_generation,evidence_kind,checkpoint_ref,sha,candidate_token)
	            VALUES ($1,$2,$3,2,'checkpoint_ref','refs/uzi-checkpoints/x',$4,'tok')`, f.run, f.user, uuid.New(), freshSha('a'))
	handled, err = f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, run, 36000)
	if !handled || err != nil {
		t.Fatalf("gate off with a pending row: handled=%v err=%v, want the locked path", handled, err)
	}
}

func TestFreshResumeStartR5LiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	capID := f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	tok1 := f.preview().SelectionToken
	if tok1 == "" {
		t.Fatal("no selection token")
	}

	// A start writes no runs column and one pending row, with user_id from the locked run row.
	before := f.runJSON()
	req1 := uuid.New()
	res, err := f.start(tok1, req1)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Replay || res.Row.State != "pending" || res.Row.UserID != f.user || res.Row.RunID != f.run ||
		res.Row.EvidenceKind != "capture" || res.Row.CaptureID.Bytes != capID || res.Row.Sha != freshSha('a') ||
		res.Row.EvidenceGeneration != 2 || res.Row.RequestID != req1 {
		t.Fatalf("start result = %+v", res)
	}
	if after := f.runJSON(); after != before {
		t.Fatalf("start wrote the run:\nbefore %s\nafter  %s", before, after)
	}
	if f.rows() != 1 {
		t.Fatalf("rows = %d, want 1", f.rows())
	}

	// A foreign caller is a 404 and creates nothing, whatever the request says.
	if _, err := f.svc.StartFreshResume(f.env.ctx, uuid.New(), f.run, tok1, uuid.New()); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign start err = %v, want ErrRunNotFound", err)
	}

	// An active request id replays to its row, creating nothing, even once the run would no longer qualify.
	f.env.exec(`UPDATE runs SET recovery_wait_cause='codex_idle_stall' WHERE id=$1`, f.run)
	replay, err := f.start("a-different-token", req1)
	if err != nil || !replay.Replay || replay.Row.ID != res.Row.ID {
		t.Fatalf("replay = %+v err=%v, want the existing row replayed before eligibility", replay, err)
	}
	// A second request while one is active is refused, whatever its token.
	_, err = f.start(tok1, uuid.New())
	wantRefusal(t, "second request while one is active", err, apitypes.FreshResumeStatusInProgress)
	f.env.exec(`UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1`, f.run)
	if f.rows() != 1 {
		t.Fatalf("rows = %d after a double start, want 1", f.rows())
	}

	// The preview of a run with an active transfer says so.
	if got := f.preview().Status; got != apitypes.FreshResumeStatusInProgress {
		t.Fatalf("preview with an active row = %q, want in_progress", got)
	}

	// The pending row expires (the sweep arm's refusal): a delayed replay of the same request is
	// request_completed and creates no row.
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	vBefore := f.version()
	out, err := RefuseFreshResumeInTx(f.env.ctx, tx, res.Row.ID, FreshRefusalAdmissionDeadline)
	if err != nil || out != FreshRefusedPending {
		t.Fatalf("refuse pending: %v %v", out, err)
	}
	if err := tx.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.version(); got != vBefore+1 {
		t.Fatalf("hold version = %d, want %d after leaving pending", got, vBefore+1)
	}
	_, err = f.start(tok1, req1)
	wantRefusal(t, "delayed replay after expiry", err, apitypes.FreshResumeReasonRequestCompleted)
	// The old token with a new request id is stale: the version moved.
	_, err = f.start(tok1, uuid.New())
	wantRefusal(t, "old token, new request", err, apitypes.FreshResumeReasonStaleSelection)
	if f.rows() != 1 {
		t.Fatalf("rows = %d after delayed replays, want still 1", f.rows())
	}

	// The same artifact with a new token and request id is admitted.
	tok2 := f.preview().SelectionToken
	if tok2 == "" || tok2 == tok1 {
		t.Fatalf("new token %q must differ from %q", tok2, tok1)
	}
	res2, err := f.start(tok2, uuid.New())
	if err != nil || res2.Replay || res2.Row.CaptureID.Bytes != capID {
		t.Fatalf("restart on the same artifact: %+v %v", res2, err)
	}

	// A garbage token is stale, and a capture that stopped being usable is refused, with no row either way.
	f.env.exec(`DELETE FROM run_fresh_resumes WHERE id=$1 OR id=$2`, res.Row.ID, res2.Row.ID)
	_, err = f.start("not-a-token", uuid.New())
	wantRefusal(t, "garbage token", err, apitypes.FreshResumeReasonStaleSelection)
	tok3 := f.preview().SelectionToken
	f.env.exec(`UPDATE recovery_captures SET state='discarded' WHERE id=$1`, capID)
	_, err = f.start(tok3, uuid.New())
	if err == nil {
		t.Fatal("start on a discarded capture succeeded")
	}
	if f.rows() != 0 {
		t.Fatalf("rows = %d after refused starts, want 0", f.rows())
	}
}

func TestFreshResumeStartLocksTheCaptureLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	capID := f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	tok := f.preview().SelectionToken

	// A concurrent expiry that commits while the start waits on the capture is re-evaluated on the new
	// row version: the start must not create a row on a capture that stopped being available.
	blocker, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(f.env.ctx) }()
	if _, err := blocker.Exec(f.env.ctx, `SELECT 1 FROM recovery_captures WHERE id=$1 FOR UPDATE`, capID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		res FreshResumeStartResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := f.start(tok, uuid.New())
		done <- result{res, err}
	}()
	// Wait for the start to be blocked on the capture lock.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM pg_stat_activity
		    WHERE datname=current_database() AND pid <> pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%FOR SHARE OF c%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start never blocked on the capture lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := blocker.Exec(f.env.ctx, `UPDATE recovery_captures SET state='expired' WHERE id=$1`, capID); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	wantRefusal(t, "start behind an expiry", r.err, apitypes.FreshResumeStatusExpired)
	if f.rows() != 0 {
		t.Fatalf("rows = %d, want 0", f.rows())
	}
}

func TestFreshResumeDoubleStartLeavesOneRowLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	tok := f.preview().SelectionToken
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.start(tok, uuid.New())
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		wantRefusal(t, "racing start", err, apitypes.FreshResumeStatusInProgress)
	}
	if ok != 1 || f.rows() != 1 {
		t.Fatalf("%d starts succeeded and %d rows exist, want exactly one of each", ok, f.rows())
	}
}

func TestFreshResumeOwnerResumeRefusesPendingFirstLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	res, err := f.start(f.preview().SelectionToken, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	// While the row is pending, the run is untouched by the start, but an unlocked autocommit write is
	// refused by the guard: the Resume path must refuse the row itself.
	if _, err := f.env.pool.Exec(f.env.ctx, `UPDATE runs SET status='queued' WHERE id=$1`, f.run); !store.IsFreshResumeInProgress(err) {
		t.Fatalf("autocommit status write err = %v, want the in-progress guard", err)
	}
	run, err := f.env.q.GetRunByIDForUser(f.env.ctx, store.GetRunByIDForUserParams{ID: f.run, UserID: f.user})
	if err != nil {
		t.Fatal(err)
	}
	vBefore := f.version()
	handled, err := f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, run, 36000)
	if !handled || err != nil {
		t.Fatalf("owner resume: handled=%v err=%v", handled, err)
	}
	var state, reason string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT state, refusal_reason FROM run_fresh_resumes WHERE id=$1`, res.Row.ID).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "refused" || reason != FreshRefusalOwnerResume {
		t.Fatalf("transfer = %s/%s, want refused/owner_resume", state, reason)
	}
	var status string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status FROM runs WHERE id=$1`, f.run).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("run status = %q err=%v, want queued after the resume", status, err)
	}
	if got := f.version(); got != vBefore+1 {
		t.Fatalf("hold version = %d, want %d", got, vBefore+1)
	}

	// A claimed transfer is not refused by Resume: it is in progress.
	f.env.exec(`UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted', status_since=now() WHERE id=$1`, f.run)
	res2, err := f.start(f.preview().SelectionToken, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	f.claimTo(res2.Row.ID, 3)
	run, _ = f.env.q.GetRunByIDForUser(f.env.ctx, store.GetRunByIDForUserParams{ID: f.run, UserID: f.user})
	_, err = f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, run, 36000)
	wantRefusal(t, "resume of a claimed transfer", err, apitypes.FreshResumeStatusInProgress)
}

// claimTo moves a pending row to claimed at generation gen on W2 the way the fresh claim will: opt in,
// prepare the tuple, the claim's own run write, mark claimed.
func (f *freshFix) claimTo(frid uuid.UUID, gen int64) {
	f.t.Helper()
	ctx := f.env.ctx
	tx, err := f.env.pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.OptInFreshResume(ctx, frid.String()); err != nil {
		f.t.Fatal(err)
	}
	if _, err := q.PrepareFreshResumeClaim(ctx, store.PrepareFreshResumeClaimParams{RunID: f.run, ID: frid, WorkerID: pgtype.UUID{Bytes: f.w2, Valid: true}}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status='claimed', claim_generation=$2, claimed_at=now(), claim_released_at=NULL,
	    released_worker_id=NULL, released_worker_nonce=NULL, status_since=now() WHERE id=$1`, f.run, gen); err != nil {
		f.t.Fatal(err)
	}
	if _, err := q.MarkFreshResumeClaimed(ctx, store.MarkFreshResumeClaimedParams{
		ID: frid, WorkerID: pgtype.UUID{Bytes: f.w2, Valid: true}, ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}, TransferSeconds: 7200}); err != nil {
		f.t.Fatal(err)
	}
	if err := q.ClearFreshResumeOptIn(ctx); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *freshFix) refuse(frid uuid.UUID, reason string, opts ...RefuseOption) (FreshRefuseOutcome, error) {
	f.t.Helper()
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.env.ctx) }()
	out, err := RefuseFreshResumeInTx(f.env.ctx, tx, frid, reason, opts...)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(f.env.ctx)
}

func (f *freshFix) transfer(frid uuid.UUID) store.RunFreshResume {
	f.t.Helper()
	row, err := f.env.q.GetFreshResume(f.env.ctx, frid)
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

func TestRefuseFreshResumeInTxLiveDB(t *testing.T) {
	t.Run("pending never touches the run", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		res, err := f.start(f.preview().SelectionToken, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		v := f.version()
		before := f.runJSONWithoutVersion()
		out, err := f.refuse(res.Row.ID, FreshRefusalInsufficientHeadroom)
		if err != nil || out != FreshRefusedPending {
			t.Fatalf("refuse: %v %v", out, err)
		}
		after := f.runJSONWithoutVersion()
		if before != after {
			t.Fatalf("a pending refusal wrote the run:\nbefore %s\nafter  %s", before, after)
		}
		row := f.transfer(res.Row.ID)
		if row.State != "refused" || row.RefusalReason.String != FreshRefusalInsufficientHeadroom || !row.ClosedAt.Valid {
			t.Fatalf("row = %+v", row)
		}
		if got := f.version(); got != v+1 {
			t.Fatalf("version = %d, want %d", got, v+1)
		}
		// Idempotent: a second refusal is a no-op and does not bump again.
		out, err = f.refuse(res.Row.ID, FreshRefusalGateOff)
		if err != nil || out != FreshRefuseNoop || f.version() != v+1 {
			t.Fatalf("second refuse: %v %v version=%d", out, err, f.version())
		}
	})

	t.Run("claimed restores the tuple and keeps the fences", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		f.env.exec(`UPDATE runs SET hold_reason='x', hold_captured_head='deadbeef', released_worker_nonce='n1', budget_paused_seconds=5, requeue_count=2 WHERE id=$1`, f.run)
		res, err := f.start(f.preview().SelectionToken, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		type snap struct {
			Status, HoldReason, Head, Nonce string
			Worker                          pgtype.UUID
			Count, Paused                   int32
			Released                        pgtype.UUID
		}
		read := func() (s snap, gen, epoch, version int64) {
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, COALESCE(hold_reason,''), COALESCE(hold_captured_head,''),
			    COALESCE(released_worker_nonce,''), worker_id, requeue_count, budget_paused_seconds, released_worker_id,
			    claim_generation, codex_claim_epoch, fresh_hold_version FROM runs WHERE id=$1`, f.run).
				Scan(&s.Status, &s.HoldReason, &s.Head, &s.Nonce, &s.Worker, &s.Count, &s.Paused, &s.Released, &gen, &epoch, &version); err != nil {
				t.Fatal(err)
			}
			return
		}
		// Every column but the fences: the restored row must equal the pre-claim row.
		stable := func() string {
			var s string
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (to_jsonb(r) - 'fresh_hold_version' - 'claim_generation' - 'updated_at'
			    - 'codex_claim_epoch' - 'codex_cap_hash')::text FROM runs r WHERE r.id=$1`, f.run).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		preClaim := stable()
		orig, _, _, v0 := read()
		f.claimTo(res.Row.ID, 3)
		if mid, _, _, _ := read(); mid.Status != "claimed" || mid.Worker.Bytes != f.w2 {
			t.Fatalf("claimed run = %+v", mid)
		}
		// The claim advanced the codex epoch; a refusal must not roll it back.
		f.env.exec(`UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 7 WHERE id=$1`, f.run)
		_, _, epochAfterClaim, _ := read()
		out, err := f.refuse(res.Row.ID, "assembly_refused")
		if err != nil || out != FreshRefusedRestored {
			t.Fatalf("refuse: %v %v", out, err)
		}
		got, gen, epoch, v1 := read()
		if post := stable(); post != preClaim {
			t.Fatalf("run row not restored:\npre  %s\npost %s", preClaim, post)
		}
		if epoch != epochAfterClaim {
			t.Fatalf("codex_claim_epoch = %d, want it kept at its post-claim %d", epoch, epochAfterClaim)
		}
		// paused-time credit is the only tuple field the claim legitimately advanced and the restore reverts it.
		if got != orig {
			t.Fatalf("tuple not restored:\norig %+v\ngot  %+v", orig, got)
		}
		if gen != 3 {
			t.Fatalf("claim_generation = %d, want it kept at the claim's 3", gen)
		}
		if v1 != v0+1 {
			t.Fatalf("version = %d, want %d", v1, v0+1)
		}
		row := f.transfer(res.Row.ID)
		if row.State != "refused" || row.RefusalReason.String != "assembly_refused" {
			t.Fatalf("row = %+v", row)
		}
		// The restored run is startable again on a new preview.
		if f.preview().Status == apitypes.FreshResumeStatusInProgress {
			t.Fatal("preview still in_progress after the refusal")
		}
	})

	t.Run("run moved closes the row without writing the run", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		res, err := f.start(f.preview().SelectionToken, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		f.claimTo(res.Row.ID, 3)
		// The run moved on: a newer claim generation by another worker, as a real re-claim would write it.
		moveTx, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.New(moveTx).OptInFreshResume(f.env.ctx, res.Row.ID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := moveTx.Exec(f.env.ctx, `UPDATE runs SET claim_generation=4 WHERE id=$1`, f.run); err != nil {
			t.Fatal(err)
		}
		if err := moveTx.Commit(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		before, v := f.runJSON(), f.version()
		out, err := f.refuse(res.Row.ID, "assembly_refused")
		if err != nil || out != FreshRefusedRunMoved {
			t.Fatalf("refuse: %v %v", out, err)
		}
		if f.runJSON() != before || f.version() != v {
			t.Fatalf("run_moved wrote the run (version %d -> %d)", v, f.version())
		}
		row := f.transfer(res.Row.ID)
		if row.State != "refused" || row.RefusalReason.String != "run_moved" {
			t.Fatalf("row = %+v", row)
		}
	})

	t.Run("run moved to another worker at the same generation", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		res, err := f.start(f.preview().SelectionToken, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		f.claimTo(res.Row.ID, 3)
		// worker_id moves to another worker while claim_generation stays at G'.
		other := f.destination(f.user, capability.FreshResumeV1, capability.RecoveryInventoryV1)
		moveTx, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.New(moveTx).OptInFreshResume(f.env.ctx, res.Row.ID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := moveTx.Exec(f.env.ctx, `UPDATE runs SET worker_id=$2 WHERE id=$1`, f.run, other); err != nil {
			t.Fatal(err)
		}
		if err := moveTx.Commit(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		before, v := f.runJSON(), f.version()
		out, err := f.refuse(res.Row.ID, "assembly_refused")
		if err != nil || out != FreshRefusedRunMoved {
			t.Fatalf("refuse: %v %v, want run_moved", out, err)
		}
		if f.runJSON() != before || f.version() != v {
			t.Fatalf("run_moved wrote the run (version %d -> %d)", v, f.version())
		}
		if row := f.transfer(res.Row.ID); row.State != "refused" || row.RefusalReason.String != "run_moved" {
			t.Fatalf("row = %+v", row)
		}
	})

	t.Run("deleted predecessor restores worker NULL", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
		res, err := f.start(f.preview().SelectionToken, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		f.claimTo(res.Row.ID, 3)
		// The predecessor row is deleted (holds released first so the RESTRICT FKs allow it).
		f.env.exec(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now() WHERE run_id=$1`, f.run)
		f.env.exec(`DELETE FROM workers WHERE id=$1`, f.w1)
		out, err := f.refuse(res.Row.ID, "assembly_refused", RefuseNoWait())
		if err != nil || out != FreshRefusedRestored {
			t.Fatalf("refuse: %v %v", out, err)
		}
		var status string
		var worker pgtype.UUID
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, worker_id FROM runs WHERE id=$1`, f.run).Scan(&status, &worker); err != nil {
			t.Fatal(err)
		}
		if status != "recovery_wait" || worker.Valid {
			t.Fatalf("restored run = %s worker=%+v, want recovery_wait with no worker", status, worker)
		}
	})

	t.Run("missing row is a no-op", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		out, err := f.refuse(uuid.New(), "gate_off")
		if err != nil || out != FreshRefuseNoop {
			t.Fatalf("refuse of an unknown row: %v %v", out, err)
		}
	})
}

// A Resume on the unlocked autocommit path (another replica, or the gate off) racing a start that has
// not committed waits on the run lock, then hits the guard's rule 1 and fails closed: the run is not
// resumed under the new pending row.
func TestFreshResumeAutocommitResumeRacesUncommittedStartLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	ctx := f.env.ctx
	startTx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = startTx.Rollback(ctx) }()
	q := store.New(startTx)
	if _, err := q.GetRunByIDForUpdate(ctx, f.run); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertFreshResumePending(ctx, store.InsertFreshResumePendingParams{
		RunID: f.run, UserID: f.user, RequestID: uuid.New(), EvidenceGeneration: 2, EvidenceKind: "checkpoint_ref",
		CheckpointRef: pgtype.Text{String: f.ref(), Valid: true}, Sha: freshSha('a'), CandidateToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.env.q.ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: f.run, UserID: f.user, GlobalTimeoutSeconds: 36000})
		done <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := f.env.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		    WHERE datname=current_database() AND pid <> pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%locked_source_workers%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the autocommit resume did not wait on the uncommitted start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the autocommit resume never blocked on the run lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := startTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !store.IsFreshResumeInProgress(err) {
		t.Fatalf("resume behind a committed start err = %v, want the in-progress guard", err)
	}
	var status string
	if err := f.env.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, f.run).Scan(&status); err != nil || status != "recovery_wait" {
		t.Fatalf("run status = %q err=%v, want it still held", status, err)
	}
}

// A chained run's only other eligible worker being the predecessor of its seeded transfer is no
// destination: the guard refuses that worker as W2, so the count must not offer it.
func TestFreshResumeDestinationExcludesSeededPredecessorLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	res, err := f.start(f.preview().SelectionToken, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	f.claimTo(res.Row.ID, 3) // W1 is the saved predecessor; the run is now W2's claim at generation 3
	if _, err := f.env.q.MarkFreshResumeSeeded(f.env.ctx, store.MarkFreshResumeSeededParams{
		ID: res.Row.ID, AttemptID: "20260101T000000Z-g3-0123456789abcdef",
		WorkerID: pgtype.UUID{Bytes: f.w2, Valid: true}, ClaimGeneration: pgtype.Int8{Int64: 3, Valid: true}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// W1 is otherwise fully eligible: hosted, capable, online, no open hold of the run.
	f.env.exec(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now() WHERE run_id=$1`, f.run)
	f.env.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', protocol_capabilities=$2, last_heartbeat_at=now() WHERE id=$1`,
		f.w1, []string{capability.FreshResumeV1, capability.RecoveryInventoryV1})
	// The run is held again, bound to W2, so W2 is excluded as the current worker.
	count := func() int64 {
		n, err := f.env.q.CountFreshResumeDestinations(f.env.ctx, store.CountFreshResumeDestinationsParams{
			RunID: f.run, UserID: f.user, HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("destinations = %d, want 0: the seeded row's predecessor must not be offered", n)
	}
	// Control: a worker that is not the predecessor is counted alone.
	f.destination(f.user, capability.FreshResumeV1, capability.RecoveryInventoryV1)
	if n := count(); n != 1 {
		t.Fatalf("destinations = %d, want exactly the unrelated worker", n)
	}
}

// The headroom arm AdmissionCountedHolds+1 > limit decides alone: the run's own holds are few, but the
// owner's counted units are at the limit.
func TestFreshResumeCustodyLimitOwnerWideArmLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour) // one own hold
	for i := 0; i < 2; i++ {
		other := seedOutageRun(t, f.env, f.user, f.repo, f.w1, "recovery_wait", "issue", 1, 0)
		f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
		            VALUES($1,$2,$3,$4,1,'open',$5,'fresh-other',$5,$4)`, uuid.New(), f.user, f.repo, other, f.w1)
	}
	f.limit.n = 3
	adm, err := f.env.q.GetCustodyAdmissionForRun(f.env.ctx, store.GetCustodyAdmissionForRunParams{
		UserID: f.user, RunID: f.run, CustodyHoldLimit: 3, HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	if adm.AdmissionCountedHolds != 3 || adm.OwnOpenHolds+2 > 3 {
		t.Fatalf("fixture: counted=%d own=%d, want counted 3 at the limit and the own-holds arm quiet", adm.AdmissionCountedHolds, adm.OwnOpenHolds)
	}
	dto := f.preview()
	if dto.Status != apitypes.FreshResumeStatusCustodyLimit {
		t.Fatalf("preview = %+v, want custody_limit from the owner-wide arm", dto)
	}
	_, err = f.start("anything", uuid.New())
	wantRefusal(t, "start", err, apitypes.FreshResumeStatusCustodyLimit)
	// One unit of room and the same run is admitted.
	f.limit.n = 4
	if got := f.preview().Status; got != apitypes.FreshResumeStatusAvailable {
		t.Fatalf("limit 4 status = %q, want available", got)
	}
}

// Two held runs of one owner with room for one more start: concurrent starts serialize on the owner
// advisory lock, so exactly one succeeds and the other sees the first one's reservation. A blocker holds
// both captures so each start has passed its headroom read before either can insert.
func TestFreshResumeOwnerLockSerializesStartsLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	capA := f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	runA := f.run
	runB := seedOutageRun(t, f.env, f.user, f.repo, f.w1, "recovery_wait", "issue", 2, 0)
	f.env.exec(`UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted', claim_released_at=now(), released_worker_id=$2 WHERE id=$1`, runB, f.w1)
	f.run = runB
	capB := f.capture(2, "available", freshSha('b'), nil, 2*time.Hour)
	tokB := f.preview().SelectionToken
	f.run = runA
	tokA := f.preview().SelectionToken
	// Two own-run holds plus the two units: counted 2, so a limit of 3 admits one reservation, not two.
	f.limit.n = 3
	if tokA == "" || tokB == "" {
		t.Fatal("no selection tokens")
	}

	ctx := f.env.ctx
	blocker, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM recovery_captures WHERE id = ANY($1) FOR UPDATE`, []uuid.UUID{capA, capB}); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, c := range []struct {
		run uuid.UUID
		tok string
	}{{runA, tokA}, {runB, tokB}} {
		go func() {
			_, err := f.svc.StartFreshResume(ctx, f.user, c.run, c.tok, uuid.New())
			errs <- err
		}()
	}
	// Both starts are parked on a lock: one on the capture, the other on the capture (no owner lock) or
	// on the owner advisory lock.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := f.env.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		    WHERE datname=current_database() AND pid <> pg_backend_pid() AND wait_event_type='Lock'
		      AND (query LIKE '%FOR SHARE OF c%' OR query LIKE '%pg_advisory_xact_lock%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d starts parked on a lock", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	ok, limited := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		var refused *FreshResumeRefused
		switch {
		case err == nil:
			ok++
		case errors.As(err, &refused) && refused.Reason == apitypes.FreshResumeStatusCustodyLimit:
			limited++
		default:
			t.Fatalf("start err = %v", err)
		}
	}
	if ok != 1 || limited != 1 {
		t.Fatalf("%d succeeded and %d were custody_limit, want exactly one of each", ok, limited)
	}
}

type freshFailingBeginner struct{}

func (freshFailingBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("begin refused")
}

// ResumeWorkerRecoveryFresh declines every run kind a transfer row can never carry, so a cross_check
// checker is never locked ahead of its parent lead; and with the gate off an unreadable outstanding-
// transfer probe fails closed rather than taking the plain path past a possible pending row.
func TestResumeWorkerRecoveryFreshKindAndFailClosedLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	run, err := f.env.q.GetRunByIDForUser(f.env.ctx, store.GetRunByIDForUserParams{ID: f.run, UserID: f.user})
	if err != nil {
		t.Fatal(err)
	}
	checker := run
	checker.Kind = "cross_check"
	if handled, err := f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, checker, 36000); handled || err != nil {
		t.Fatalf("cross_check with the gate on: handled=%v err=%v, want the plain path", handled, err)
	}
	var status string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status FROM runs WHERE id=$1`, f.run).Scan(&status); err != nil || status != "recovery_wait" {
		t.Fatalf("the declined kind wrote the run: status=%q err=%v", status, err)
	}
	if handled, err := f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, run, 36000); !handled || err != nil {
		t.Fatalf("issue run with the gate on: handled=%v err=%v, want the locked path", handled, err)
	}

	off := newFreshFix(t, false, 2)
	offRun, err := off.env.q.GetRunByIDForUser(off.env.ctx, store.GetRunByIDForUserParams{ID: off.run, UserID: off.user})
	if err != nil {
		t.Fatal(err)
	}
	off.svc.SetTxBeginner(freshFailingBeginner{})
	handled, err := off.svc.ResumeWorkerRecoveryFresh(off.env.ctx, off.user, offRun, 36000)
	if !handled || err == nil {
		t.Fatalf("gate off, probe failed: handled=%v err=%v, want handled with the error", handled, err)
	}
}

// Preview and start name the same status for a stamped tip at Ge on a run with no open hold: no_evidence.
func TestFreshResumeHoldlessStampedTipStatusLiveDB(t *testing.T) {
	f := newFreshFix(t, true, 2)
	f.tip(freshSha('c'), 2)
	f.fg.refs[f.ref()] = freshSha('c')
	holdID := f.hold(2, f.w1)
	dto := f.preview()
	if dto.Status != apitypes.FreshResumeStatusAvailable || dto.Kind != apitypes.FreshResumeKindCheckpointRef {
		t.Fatalf("baseline preview = %+v", dto)
	}
	f.env.exec(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now() WHERE id=$1`, holdID)
	f.env.exec(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now() WHERE run_id=$1 AND state='open'`, f.run)
	if got := f.preview().Status; got != apitypes.FreshResumeStatusNoEvidence {
		t.Fatalf("preview = %q, want no_evidence", got)
	}
	_, err := f.start(dto.SelectionToken, uuid.New())
	wantRefusal(t, "start", err, apitypes.FreshResumeStatusNoEvidence)
	// The start's own re-check under the run lock (a hold that closes after the facts were read) agrees.
	run, err := f.env.q.GetRunByIDForUser(f.env.ctx, store.GetRunByIDForUserParams{ID: f.run, UserID: f.user})
	if err != nil {
		t.Fatal(err)
	}
	reason, err := f.svc.freshLockCandidate(f.env.ctx, f.env.q, run, freshSelection{
		Ge: 2, Chosen: freshCandidate{kind: apitypes.FreshResumeKindCheckpointRef, ref: f.ref(), sha: freshSha('c')}})
	if err != nil || reason != apitypes.FreshResumeStatusNoEvidence {
		t.Fatalf("freshLockCandidate = %q, %v, want no_evidence", reason, err)
	}
}

// runJSONWithoutVersion is the run row less the hold version, which a pending refusal bumps by design.
func (f *freshFix) runJSONWithoutVersion() string {
	f.t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (to_jsonb(r) - 'fresh_hold_version')::text FROM runs r WHERE r.id=$1`, f.run).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}
