package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4007 slice A, M3: the terminal-preservation route's service against a REAL Postgres, with
// the broker (pack preparation, the push, the listing) replaced by a counting fake remote and the
// forge's issue / merge request reads by a recording fake. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres (./e2e/run-store-it.sh).

const (
	termTip1    = "1111111111111111111111111111111111111111"
	termTip2    = "2222222222222222222222222222222222222222"
	termTip3    = "3333333333333333333333333333333333333333"
	termCommitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	termTreeA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// termFake is the counting fake remote behind the broker seams.
type termFake struct {
	mu       sync.Mutex
	prepares int
	creates  int
	updates  int
	lists    int
	opts     []pushbroker.TerminalPackOptions
	refs     map[uuid.UUID]string
	listErr  error
	// byPrepared maps a prepared pack back to the options it was built from.
	byPrepared map[*pushbroker.PreparedTerminalPack]pushbroker.TerminalPackOptions

	prepareFn func(o pushbroker.TerminalPackOptions) error
	createFn  func(ctx context.Context, o pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error)
	updateFn  func(ctx context.Context, o pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error)
	delay     time.Duration
}

func (f *termFake) counts() (prepares, sends, lists int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.creates + f.updates, f.lists
}

func (f *termFake) setRef(run uuid.UUID, tip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[run] = tip
}

func (f *termFake) prepare(_ context.Context, o pushbroker.TerminalPackOptions) (*pushbroker.PreparedTerminalPack, error) {
	f.mu.Lock()
	f.prepares++
	f.opts = append(f.opts, o)
	fn := f.prepareFn
	f.mu.Unlock()
	if fn != nil {
		if err := fn(o); err != nil {
			return nil, err
		}
	}
	p := &pushbroker.PreparedTerminalPack{}
	f.mu.Lock()
	f.byPrepared[p] = o
	f.mu.Unlock()
	return p, nil
}

func (f *termFake) create(ctx context.Context, p *pushbroker.PreparedTerminalPack) (pushbroker.TerminalOutcome, error) {
	f.mu.Lock()
	f.creates++
	o := f.byPrepared[p]
	fn := f.createFn
	delay := f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	if fn != nil {
		return fn(ctx, o)
	}
	f.setRef(o.RunID, o.Tip)
	return pushbroker.TerminalApplied, nil
}

func (f *termFake) update(ctx context.Context, p *pushbroker.PreparedTerminalPack) (pushbroker.TerminalOutcome, error) {
	f.mu.Lock()
	f.updates++
	o := f.byPrepared[p]
	fn := f.updateFn
	cur := f.refs[o.RunID]
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, o)
	}
	if cur != o.PriorTip {
		return pushbroker.TerminalRefused, fmt.Errorf("%w: ref moved", pushbroker.ErrTerminalRefused)
	}
	f.setRef(o.RunID, o.Tip)
	return pushbroker.TerminalApplied, nil
}

func (f *termFake) list(_ context.Context, _ pushbroker.ListRefsOptions, run uuid.UUID) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return "", false, f.listErr
	}
	tip, ok := f.refs[run]
	return tip, ok, nil
}

// termForges records which forge-side the lifetime read used. ForgeForConnection satisfies
// ForgeBuilder and fails if the lifetime read ever falls back to it.
type termForges struct {
	mu         sync.Mutex
	issueCalls int
	hostCalls  int
	fallback   int
	issue      forge.Issue
	issueErr   error
	mr         forge.MergeRequestSummary
	mrErr      error
	getIssues  int
	getMRs     int
}

func (f *termForges) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	f.mu.Lock()
	f.fallback++
	f.mu.Unlock()
	return nil, errors.New("ForgeForConnection must not serve the lifetime read")
}

func (f *termForges) IssueTrackerForRepo(_ context.Context, _ uuid.UUID, _, _ string, _ []byte) (forge.IssueTracker, error) {
	f.mu.Lock()
	f.issueCalls++
	f.mu.Unlock()
	return termIssueTracker{f: f}, nil
}

func (f *termForges) CodeHostForRepo(_ context.Context, _ uuid.UUID, _, _ string, _ []byte) (forge.CodeHost, error) {
	f.mu.Lock()
	f.hostCalls++
	f.mu.Unlock()
	return termCodeHost{f: f}, nil
}

type termIssueTracker struct {
	forge.IssueTracker
	f *termForges
}

func (t termIssueTracker) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.getIssues++
	return t.f.issue, t.f.issueErr
}

type termCodeHost struct {
	forge.CodeHost
	f *termForges
}

func (c termCodeHost) GetMergeRequestSummary(context.Context, int64, int64) (forge.MergeRequestSummary, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.getMRs++
	return c.f.mr, c.f.mrErr
}

type termFix struct {
	t      *testing.T
	e      interlockLiveDB
	svc    *Service
	fake   *termFake
	forges *termForges
	pat    string
	worker store.Worker
	run    uuid.UUID
	hold   uuid.UUID
}

// newTermFix seeds a run for the capable worker. "flight" is the worker's own running run at claim
// generation 1; "hold" is the same run failed, with an open inventory-guarded hold of generation 1.
func newTermFix(t *testing.T, mode string) *termFix {
	t.Helper()
	e := setupInterlockLiveDB(t)
	box := newBox(t)
	pat := strings.Join([]string{"bot", "pat", "term", uuid.NewString()}, "-")
	sealed, err := box.Seal([]byte(pat))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	e.exec(t, `UPDATE forge_connections SET token_ciphertext = $1
	           WHERE id = (SELECT connection_id FROM repos WHERE id = $2)`, sealed, e.repoID)

	p := testParams()
	svc := New(e.q, box, p)
	svc.SetTxBeginner(e.pool)
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	fake := &termFake{refs: map[uuid.UUID]string{}, byPrepared: map[*pushbroker.PreparedTerminalPack]pushbroker.TerminalPackOptions{}}
	svc.terminal = terminalHooks{prepare: fake.prepare, sendCreate: fake.create, sendUpdate: fake.update, list: fake.list}
	forges := &termForges{issue: forge.Issue{State: "opened"}, mr: forge.MergeRequestSummary{State: forge.MRStateOpened}}
	svc.SetForges(forges)

	caps := []string{capability.RecoveryTerminalPreservationV1}
	wid := e.seedWorker(t, caps)
	f := &termFix{t: t, e: e, svc: svc, fake: fake, forges: forges, pat: pat, worker: store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}}
	f.run = e.seedLegacyRunningRun(t, wid)
	e.exec(t, `UPDATE runs SET claim_generation=1 WHERE id=$1`, f.run)
	if mode == "hold" {
		f.hold = uuid.New()
		e.exec(t, `INSERT INTO recovery_custody_holds
			(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
			VALUES ($1,$2,$3,$4,1,'open',$5,'term-ident',$5,$4,true)`, f.hold, e.userID, e.repoID, f.run, wid)
		e.exec(t, `UPDATE runs SET status='failed', fail_origin='agent_failure' WHERE id=$1`, f.run)
	}
	return f
}

func (f *termFix) manifest(gen int64) apitypes.TerminalManifest {
	return apitypes.TerminalManifest{
		RunID: f.run.String(), Generation: gen, WorkerID: f.worker.ID.String(),
		Roots:     []apitypes.TerminalManifestRoot{{SourceID: "src-1", AttemptID: "att-1", Commit: termCommitA, Tree: termTreeA}},
		Abandoned: []apitypes.TerminalManifestAbandoned{{Class: "ignored", SourceID: "src-1", Extents: apitypes.TerminalManifestExtents{Files: 2, Bytes: 10}}},
	}
}

func (f *termFix) req(gen int64, tip, prior string) TerminalPublishRequest {
	return TerminalPublishRequest{Generation: gen, Tip: tip, PriorTip: prior, Manifest: f.manifest(gen), Pack: []byte("pack")}
}

func (f *termFix) publish(req TerminalPublishRequest) (TerminalPublishResult, error) {
	return f.svc.PublishTerminal(f.e.ctx, f.worker, f.run, req)
}

func (f *termFix) pres() (store.RunTerminalPreservation, bool) {
	f.t.Helper()
	p, err := f.e.q.GetTerminalPreservationByRun(f.e.ctx, f.run)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.RunTerminalPreservation{}, false
	}
	if err != nil {
		f.t.Fatalf("read preservation: %v", err)
	}
	return p, true
}

func (f *termFix) con(gen int64) (store.RunTerminalContribution, bool) {
	f.t.Helper()
	c, err := f.e.q.GetTerminalContributionByRunGeneration(f.e.ctx, store.GetTerminalContributionByRunGenerationParams{RunID: f.run, Generation: gen})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.RunTerminalContribution{}, false
	}
	if err != nil {
		f.t.Fatalf("read contribution: %v", err)
	}
	return c, true
}

// assertNothing asserts a refused request left no row in either table and reached neither the
// forge connection nor the broker.
func (f *termFix) assertNothing(why string) {
	f.t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT (SELECT count(*) FROM run_terminal_preservations WHERE run_id=$1)
		+ (SELECT count(*) FROM run_terminal_contributions WHERE run_id=$1)`, f.run).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	pre, sends, lists := f.fake.counts()
	if n != 0 || pre != 0 || sends != 0 || lists != 0 {
		f.t.Fatalf("%s: rows=%d prepares=%d sends=%d lists=%d, want all zero", why, n, pre, sends, lists)
	}
}

func termReason(t *testing.T, err error) string {
	t.Helper()
	var r *TerminalRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *TerminalRefusal", err)
	}
	return r.Reason
}

// A current-flight create: the row identities are the SERVER's, the credential reached the broker
// only from the sealed connection, the ci-config guard came from the run row, and the pointer and
// tip are set once the ref is read back.
func TestTerminalPublishCurrentFlightCreateLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	f.e.exec(t, `UPDATE runs SET ci_config_guard=true, ci_config_paths=ARRAY['.github/workflows']::text[] WHERE id=$1`, f.run)
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "published" || res.PreservationState != "live" || res.Tip != termTip1 ||
		res.Ref != "refs/uzi-terminal/"+f.run.String() || res.Disposition != "preserved" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	p, _ := f.pres()
	c, _ := f.con(1)
	if p.State != "live" || p.Tip.String != termTip1 || !p.FirstPublishedAt.Valid || uuid.UUID(p.LiveRunID.Bytes) != f.run ||
		p.UserID != f.e.userID || p.RepoID != f.e.repoID || p.LifetimeAtBind != "issue" {
		t.Fatalf("preservation = %+v", p)
	}
	if c.State != "published" || c.Authority != "current_flight" || c.HoldID.Valid || !c.CreateInvokedAt.Valid || !c.PublishedAt.Valid ||
		c.WorkerID != f.worker.ID || c.Disposition != "preserved" || c.PriorTip.Valid {
		t.Fatalf("contribution = %+v", c)
	}
	// An independent digest: the sha256 of the canonical manifest written out by hand (sorted,
	// field order of the DTO, no whitespace).
	canonical := fmt.Sprintf(`{"run_id":%q,"generation":1,"worker_id":%q,"roots":[{"source_id":"src-1","attempt_id":"att-1","commit":%q,"tree":%q}],"abandoned":[{"class":"ignored","source_id":"src-1","extents":{"files":2,"bytes":10}}]}`,
		f.run, f.worker.ID, termCommitA, termTreeA)
	sum := sha256.Sum256([]byte(canonical))
	if c.CoverageDigest != hex.EncodeToString(sum[:]) || !termJSONEqual(t, c.Manifest, canonical) {
		t.Fatalf("digest %s manifest %s, want digest of %s", c.CoverageDigest, c.Manifest, canonical)
	}
	pre, sends, _ := f.fake.counts()
	if pre != 1 || sends != 1 || f.fake.creates != 1 {
		t.Fatalf("prepares=%d sends=%d creates=%d, want 1/1/1", pre, sends, f.fake.creates)
	}
	o := f.fake.opts[0]
	if o.PAT != f.pat || o.Username != "bot" || o.CloneURL != "https://forge.e2e/g/interlock.git" || o.RunID != f.run ||
		o.Tip != termTip1 || o.PriorTip != "" || !o.CIConfigGuard || len(o.CIConfigPaths) != 1 || o.CIConfigPaths[0] != ".github/workflows" ||
		len(o.Roots) != 1 || o.Roots[0].Commit != termCommitA || o.Roots[0].Tree != termTreeA || o.DefaultBranch != "main" {
		t.Fatalf("broker options = %+v (PAT compared, not printed)", o)
	}
}

// termJSONEqual compares two JSON documents by value: jsonb does not keep the text it was given.
func termJSONEqual(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// A second generation fast-forwards the ref with exactly one update; first_published_at is kept.
func TestTerminalPublishUpdateAdvancesTipLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	if _, err := f.publish(f.req(1, termTip1, "")); err != nil {
		t.Fatal(err)
	}
	first, _ := f.pres()
	f.e.exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run)
	res, err := f.publish(f.req(2, termTip2, termTip1))
	if err != nil || res.State != "published" || res.Tip != termTip2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	p, _ := f.pres()
	c2, _ := f.con(2)
	if p.Tip.String != termTip2 || p.State != "live" || p.FirstPublishedAt.Time != first.FirstPublishedAt.Time || c2.PriorTip.String != termTip1 {
		t.Fatalf("preservation=%+v contribution=%+v", p, c2)
	}
	if f.fake.creates != 1 || f.fake.updates != 1 {
		t.Fatalf("creates=%d updates=%d, want 1 and 1", f.fake.creates, f.fake.updates)
	}
	// A prior tip that is not the preservation's tip is refused before anything is written.
	f.e.exec(t, `UPDATE runs SET claim_generation=3 WHERE id=$1`, f.run)
	_, err = f.publish(f.req(3, termTip3, termTip1))
	if r := termReason(t, err); r != apitypes.TerminalReasonPriorTipMismatch {
		t.Fatalf("reason = %s", r)
	}
	if _, ok := f.con(3); ok {
		t.Fatal("a refused prior tip left a contribution row")
	}
}

// Every current_flight refusal leaves no row and makes no forge call.
func TestTerminalCurrentFlightRefusalsLiveDB(t *testing.T) {
	cases := []struct {
		name    string
		gen     int64
		mutate  string
		wantErr error
		reason  string
	}{
		{"zero generation", 0, "", ErrMissingClaimGeneration, ""},
		{"stale generation", 1, `UPDATE runs SET claim_generation=2 WHERE id=$1`, ErrStaleClaim, ""},
		{"future generation", 2, "", ErrStaleClaim, ""},
		{"released claim", 1, `UPDATE runs SET claim_released_at=now() WHERE id=$1`, ErrStaleClaim, ""},
		{"completed run", 1, `UPDATE runs SET status='completed' WHERE id=$1`, nil, apitypes.TerminalReasonUnsupported},
		{"failed run", 1, `UPDATE runs SET status='failed' WHERE id=$1`, nil, apitypes.TerminalReasonUnsupported},
		{"cancelled run", 1, `UPDATE runs SET status='cancelled' WHERE id=$1`, nil, apitypes.TerminalReasonUnsupported},
		{"paused run", 1, `UPDATE runs SET status='paused' WHERE id=$1`, nil, apitypes.TerminalReasonUnsupported},
		{"push_secret_blocked", 1, `UPDATE runs SET fail_origin='push_secret_blocked' WHERE id=$1`, nil, apitypes.TerminalReasonExcludedOrigin},
		{"another worker's run", 1, "other", nil, apitypes.TerminalReasonHoldMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTermFix(t, "flight")
			switch tc.mutate {
			case "":
			case "other":
				other := f.e.seedWorker(t, f.worker.ProtocolCapabilities)
				f.e.exec(t, `UPDATE runs SET worker_id=$2 WHERE id=$1`, f.run, other)
			default:
				f.e.exec(t, tc.mutate, f.run)
			}
			_, err := f.publish(f.req(tc.gen, termTip1, ""))
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			default:
				if r := termReason(t, err); r != tc.reason {
					t.Fatalf("reason = %s, want %s", r, tc.reason)
				}
			}
			f.assertNothing(tc.name)
			if f.forges.getIssues+f.forges.getMRs != 0 {
				t.Fatalf("a refused request read the forge (%d issue, %d mr reads)", f.forges.getIssues, f.forges.getMRs)
			}
		})
	}
}

// open_guarded_hold: a terminal run's exact open hold authorises; every other shape is refused
// with no write.
func TestTerminalOpenGuardedHoldLiveDB(t *testing.T) {
	t.Run("authorised", func(t *testing.T) {
		f := newTermFix(t, "hold")
		res, err := f.publish(f.req(1, termTip1, ""))
		if err != nil || res.State != "published" {
			t.Fatalf("result = %+v, %v", res, err)
		}
		c, _ := f.con(1)
		if c.Authority != "open_guarded_hold" || !c.HoldID.Valid || uuid.UUID(c.HoldID.Bytes) != f.hold {
			t.Fatalf("contribution = %+v", c)
		}
	})
	cases := []struct {
		name   string
		mutate func(f *termFix)
		gen    int64
		reason string
	}{
		{"other generation", func(*termFix) {}, 2, "stale_claim"},
		{"other worker", func(f *termFix) {
			other := f.e.seedWorker(t, f.worker.ProtocolCapabilities)
			f.worker = store.Worker{ID: other, UserID: f.e.userID, ProtocolCapabilities: f.worker.ProtocolCapabilities}
		}, 1, apitypes.TerminalReasonHoldMissing},
		{"closed hold", func(f *termFix) {
			f.rawHold(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL,
				released_at=now() WHERE id=$1`)
		}, 1, apitypes.TerminalReasonHoldMissing},
		{"discarded hold", func(f *termFix) {
			f.rawHold(`UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL,
				release_evidence='owner_discard', released_at=now() WHERE id=$1`)
		}, 1, apitypes.TerminalReasonHoldMissing},
		// The hold lookup only finds guarded holds, so an unguarded one is no hold at all: the
		// terminal run then has no authority, the current_flight status refusal.
		{"hold not guarded", func(f *termFix) {
			f.rawHold(`UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1`)
		}, 1, apitypes.TerminalReasonUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTermFix(t, "hold")
			tc.mutate(f)
			_, err := f.publish(f.req(tc.gen, termTip1, ""))
			if tc.reason == "stale_claim" {
				// The run is this worker's at claim generation 1: generation 2 is a stale claim, and
				// no hold of generation 2 exists to authorise it instead.
				if !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("err = %v, want ErrStaleClaim", err)
				}
			} else if r := termReason(t, err); r != tc.reason {
				t.Fatalf("reason = %s, want %s", r, tc.reason)
			}
			f.assertNothing(tc.name)
		})
	}
}

func (f *termFix) rawHold(sql string) {
	f.t.Helper()
	tx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.e.ctx) }()
	for _, q := range []string{`ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard`, sql,
		`ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard`} {
		var args []any
		if q == sql {
			args = []any{f.hold}
		}
		if _, err := tx.Exec(f.e.ctx, q, args...); err != nil {
			f.t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(f.e.ctx); err != nil {
		f.t.Fatal(err)
	}
}

// Without the capability the route is unsupported: no row, no forge call.
func TestTerminalUnsupportedWithoutCapabilityLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	f.worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV2}
	f.e.exec(t, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.worker.ID, f.worker.ProtocolCapabilities)
	_, err := f.publish(f.req(1, termTip1, ""))
	if r := termReason(t, err); r != apitypes.TerminalReasonUnsupported {
		t.Fatalf("reason = %s", r)
	}
	f.assertNothing("no capability")
	if err := f.svc.TerminalPreservationAdmit(f.worker); termReason(t, err) != apitypes.TerminalReasonUnsupported {
		t.Fatalf("admit = %v", err)
	}
	// The worker row is the server's record: a payload-side claim of the capability changes nothing,
	// but a worker row that lost it between the pre-check and tx1 is refused under the lock.
	f2 := newTermFix(t, "flight")
	f2.e.exec(t, `UPDATE workers SET protocol_capabilities='{}' WHERE id=$1`, f2.worker.ID)
	_, err = f2.publish(f2.req(1, termTip1, ""))
	if r := termReason(t, err); r != apitypes.TerminalReasonUnsupported {
		t.Fatalf("locked capability reason = %s", r)
	}
	var rows int
	if err := f2.e.pool.QueryRow(f2.e.ctx, `SELECT (SELECT count(*) FROM run_terminal_preservations WHERE run_id=$1)
		+ (SELECT count(*) FROM run_terminal_contributions WHERE run_id=$1)`, f2.run).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows = %d err=%v, want none", rows, err)
	}
	if _, sends, _ := f2.fake.counts(); sends != 0 {
		t.Fatalf("sends = %d", sends)
	}
}

// A manifest that does not name the authenticated run, generation and worker is refused with no write.
func TestTerminalManifestMismatchLiveDB(t *testing.T) {
	for name, mutate := range map[string]func(m *apitypes.TerminalManifest){
		"run":        func(m *apitypes.TerminalManifest) { m.RunID = uuid.NewString() },
		"generation": func(m *apitypes.TerminalManifest) { m.Generation = 9 },
		"worker":     func(m *apitypes.TerminalManifest) { m.WorkerID = uuid.NewString() },
	} {
		t.Run(name, func(t *testing.T) {
			f := newTermFix(t, "flight")
			req := f.req(1, termTip1, "")
			mutate(&req.Manifest)
			_, err := f.publish(req)
			if r := termReason(t, err); r != apitypes.TerminalReasonManifestMismatch {
				t.Fatalf("reason = %s", r)
			}
			f.assertNothing(name)
		})
	}
	t.Run("invalid entry", func(t *testing.T) {
		f := newTermFix(t, "flight")
		req := f.req(1, termTip1, "")
		req.Manifest.Roots[0].Commit = "nothex"
		_, err := f.publish(req)
		if r := termReason(t, err); r != apitypes.TerminalReasonManifestInvalid {
			t.Fatalf("reason = %s", r)
		}
		f.assertNothing("invalid manifest")
	})
}

// Exactly one send under N concurrent identical requests; the losers list and reconcile.
func TestTerminalConcurrentIdenticalRequestsSendOnceLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	f.fake.delay = 150 * time.Millisecond
	const n = 8
	results := make([]TerminalPublishResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.publish(f.req(1, termTip1, ""))
		}()
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if s := results[i].State; s != "published" && s != "invoked" {
			t.Fatalf("request %d state = %s", i, s)
		}
	}
	if _, sends, _ := f.fake.counts(); sends != 1 {
		t.Fatalf("sends = %d, want exactly 1", sends)
	}
	if c, _ := f.con(1); c.State != "published" {
		t.Fatalf("final contribution = %s", c.State)
	}
}

// Sequential retries, and a different payload for the same generation.
func TestTerminalSequentialRetriesSendOnceLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	for i := range 3 {
		res, err := f.publish(f.req(1, termTip1, ""))
		if err != nil || res.State != "published" {
			t.Fatalf("retry %d = %+v, %v", i, res, err)
		}
	}
	if pre, sends, _ := f.fake.counts(); sends != 1 || pre != 1 {
		t.Fatalf("prepares=%d sends=%d, want 1/1 (a retry of a decided contribution neither prepares nor sends)", pre, sends)
	}
	_, err := f.publish(f.req(1, termTip2, ""))
	if r := termReason(t, err); r != apitypes.TerminalReasonGenerationBound {
		t.Fatalf("a different tip for the same generation: reason = %s", r)
	}
	other := f.req(1, termTip1, "")
	other.Manifest.Abandoned[0].Extents.Bytes = 99
	_, err = f.publish(other)
	if r := termReason(t, err); r != apitypes.TerminalReasonGenerationBound {
		t.Fatalf("a different manifest for the same generation: reason = %s", r)
	}
}

// An injected tx1 commit failure: zero sends, nothing durable, and a retry then wins normally.
func TestTerminalTx1CommitErrorSendsNothingLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	failing := true
	f.svc.terminal.beforeCommit = func() error {
		if failing {
			return errors.New("injected commit failure")
		}
		return nil
	}
	if _, err := f.publish(f.req(1, termTip1, "")); err == nil {
		t.Fatal("want the injected error")
	}
	if _, sends, _ := f.fake.counts(); sends != 0 {
		t.Fatalf("sends = %d after a failed tx1", sends)
	}
	if _, ok := f.pres(); ok {
		t.Fatal("a failed tx1 left a preservation row")
	}
	failing = false
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "published" {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if _, sends, _ := f.fake.counts(); sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
}

// A crash after tx1 committed: the rows are publishing/invoked and nothing was sent. A retry only
// reconciles (lists), never sends, and records published once the ref is at the tip.
func TestTerminalCrashAfterTx1LiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	f.svc.terminal.beforeSend = func() { panic("crash after tx1") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("want the injected crash")
			}
		}()
		_, _ = f.publish(f.req(1, termTip1, ""))
	}()
	p, _ := f.pres()
	c, _ := f.con(1)
	if p.State != "publishing" || !p.LiveRunID.Valid || c.State != "invoked" || !c.CreateInvokedAt.Valid {
		t.Fatalf("after the crash: preservation=%s pointer=%v contribution=%s", p.State, p.LiveRunID.Valid, c.State)
	}
	f.svc.terminal.beforeSend = nil
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "invoked" {
		t.Fatalf("retry with no ref = %+v, %v", res, err)
	}
	if _, sends, lists := f.fake.counts(); sends != 0 || lists == 0 {
		t.Fatalf("sends=%d lists=%d, want 0 sends and a listing", sends, lists)
	}
	f.fake.setRef(f.run, termTip1)
	res, err = f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "published" || res.PreservationState != "live" {
		t.Fatalf("retry with the ref at the tip = %+v, %v", res, err)
	}
	if _, sends, _ := f.fake.counts(); sends != 0 {
		t.Fatalf("sends = %d, want 0: the retry only reconciles", sends)
	}
	if p, _ := f.pres(); p.Tip.String != termTip1 || !p.FirstPublishedAt.Valid {
		t.Fatalf("preservation = %+v", p)
	}
}

// An unknown outcome is publish_unknown, with the preservation still publishing; a later request
// reconciles from the listing and never resends.
func TestTerminalUnknownOutcomeReconcilesLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	f.fake.createFn = func(context.Context, pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error) {
		return pushbroker.TerminalUnknown, errors.New("connection reset")
	}
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "publish_unknown" || res.PreservationState != "publishing" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if p, _ := f.pres(); !p.LiveRunID.Valid || !p.LastError.Valid {
		t.Fatalf("preservation = %+v", p)
	}
	f.fake.setRef(f.run, termTip1)
	res, err = f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "published" {
		t.Fatalf("reconcile = %+v, %v", res, err)
	}
	if _, sends, _ := f.fake.counts(); sends != 1 {
		t.Fatalf("sends = %d, want the original one only", sends)
	}
}

// The remote's ng text is scrubbed and collapsed before it reaches a row or a log.
func TestTerminalRefusedNgTextIsScrubbedLiveDB(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newTermFix(t, "flight")
	secret := "glpat-" + strings.Repeat("aB3d", 5)
	f.fake.createFn = func(context.Context, pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error) {
		return pushbroker.TerminalRefused, fmt.Errorf("%w: pre-receive hook declined\n\ttoken %s\x1b[31m and pat %s   done", pushbroker.ErrTerminalRefused, secret, f.pat)
	}
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "refused" || res.Reason != apitypes.TerminalReasonCreateRefused || res.PreservationState != "not_published" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	p, _ := f.pres()
	c, _ := f.con(1)
	if p.State != "not_published" || p.LiveRunID.Valid || p.Tip.Valid || c.State != "refused" || c.RefusalReason.String != "create_refused" {
		t.Fatalf("preservation=%+v contribution=%+v", p, c)
	}
	for _, leaky := range []string{secret, f.pat, "\x1b"} {
		if strings.Contains(p.LastError.String, leaky) || strings.Contains(logs.String(), leaky) {
			t.Fatalf("%q survived in last_error=%q or the log", leaky, p.LastError.String)
		}
	}
	if strings.ContainsAny(p.LastError.String, "\n\t") || strings.Contains(p.LastError.String, "  ") {
		t.Fatalf("last_error = %q, want whitespace collapsed", p.LastError.String)
	}
	if !strings.Contains(p.LastError.String, "pre-receive hook declined") || !strings.Contains(p.LastError.String, "[redacted]") {
		t.Fatalf("last_error = %q, want the scrubbed text", p.LastError.String)
	}
}

// A refused update goes back to live at the old tip with update_refused.
func TestTerminalRefusedUpdateBackToLiveLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	if _, err := f.publish(f.req(1, termTip1, "")); err != nil {
		t.Fatal(err)
	}
	f.e.exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run)
	f.fake.updateFn = func(context.Context, pushbroker.TerminalPackOptions) (pushbroker.TerminalOutcome, error) {
		return pushbroker.TerminalRefused, fmt.Errorf("%w: stale info", pushbroker.ErrTerminalRefused)
	}
	res, err := f.publish(f.req(2, termTip2, termTip1))
	if err != nil || res.State != "refused" || res.Reason != apitypes.TerminalReasonUpdateRefused || res.PreservationState != "live" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if p, _ := f.pres(); p.State != "live" || p.Tip.String != termTip1 || !p.LiveRunID.Valid {
		t.Fatalf("preservation = %+v", p)
	}
}

// Prepare-time refusals are typed wire refusals and write NO rows.
func TestTerminalPrepareRefusalsWriteNothingLiveDB(t *testing.T) {
	cases := map[string]struct {
		err    error
		reason string
	}{
		"pack too large":  {pushbroker.ErrPackTooLarge, apitypes.TerminalReasonPackTooLarge},
		"pack invalid":    {fmt.Errorf("%w: x", pushbroker.ErrPackInvalid), apitypes.TerminalReasonPackInvalid},
		"invalid ref":     {pushbroker.ErrInvalidRef, apitypes.TerminalReasonPackInvalid},
		"tip missing":     {pushbroker.ErrTipMissing, apitypes.TerminalReasonTipMissing},
		"ci config guard": {pushbroker.ErrCIConfigGuard, apitypes.TerminalReasonCIConfigGuard},
		"not descendant":  {pushbroker.ErrNotDescendant, apitypes.TerminalReasonNotDescendant},
		"root":            {pushbroker.ErrTerminalRootUnreachable, apitypes.TerminalReasonRootUnreachable},
		"tree":            {pushbroker.ErrTerminalTreeMismatch, apitypes.TerminalReasonTreeMismatch},
		"walk":            {pushbroker.ErrTerminalWalkTooLarge, apitypes.TerminalReasonWalkTooLarge},
		"prior moved":     {pushbroker.ErrTerminalPriorMoved, apitypes.TerminalReasonForeignTip},
		"transport":       {errors.New("dial tcp: i/o timeout"), apitypes.TerminalReasonForgeUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTermFix(t, "flight")
			f.fake.prepareFn = func(pushbroker.TerminalPackOptions) error { return tc.err }
			_, err := f.publish(f.req(1, termTip1, ""))
			if r := termReason(t, err); r != tc.reason {
				t.Fatalf("reason = %s, want %s", r, tc.reason)
			}
			var n int
			if err := f.e.pool.QueryRow(f.e.ctx, `SELECT (SELECT count(*) FROM run_terminal_preservations WHERE run_id=$1)
				+ (SELECT count(*) FROM run_terminal_contributions WHERE run_id=$1)`, f.run).Scan(&n); err != nil || n != 0 {
				t.Fatalf("rows = %d err=%v, want none", n, err)
			}
			if _, sends, _ := f.fake.counts(); sends != 0 {
				t.Fatalf("sends = %d", sends)
			}
		})
	}
}

// A retiring, deleted or abandoned preservation refuses preservation_retired; another generation's
// publication in flight refuses publication_in_flight.
func TestTerminalRetiredAndInFlightRefusalsLiveDB(t *testing.T) {
	for _, state := range []string{"retiring", "deleted", "abandoned"} {
		t.Run(state, func(t *testing.T) {
			f := newTermFix(t, "flight")
			if _, err := f.publish(f.req(1, termTip1, "")); err != nil {
				t.Fatal(err)
			}
			p, _ := f.pres()
			f.e.exec(t, `UPDATE run_terminal_preservations SET state='retiring', retire_tip=$2, retire_reason='issue_closed',
				retire_observed_at=now(), retire_decided_at=now() WHERE id=$1`, p.ID, termTip1)
			switch state {
			case "deleted":
				f.e.exec(t, `UPDATE run_terminal_preservations SET state='deleted', live_run_id=NULL, deleted_at=now() WHERE id=$1`, p.ID)
			case "abandoned":
				f.e.exec(t, `UPDATE run_terminal_preservations SET state='abandoned', live_run_id=NULL WHERE id=$1`, p.ID)
			}
			f.e.exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run)
			before, _, _ := f.fake.counts()
			_, err := f.publish(f.req(2, termTip2, termTip1))
			if r := termReason(t, err); r != apitypes.TerminalReasonPreservationRetired {
				t.Fatalf("reason = %s", r)
			}
			if _, ok := f.con(2); ok {
				t.Fatal("a retired preservation accepted a contribution")
			}
			if after, sends, _ := f.fake.counts(); after != before || sends != 1 {
				t.Fatalf("prepares %d -> %d, sends %d", before, after, sends)
			}
		})
	}
	t.Run("in flight", func(t *testing.T) {
		f := newTermFix(t, "flight")
		f.svc.terminal.beforeSend = func() { panic("crash after tx1") }
		func() {
			defer func() { _ = recover() }()
			_, _ = f.publish(f.req(1, termTip1, ""))
		}()
		f.svc.terminal.beforeSend = nil
		f.e.exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run)
		_, err := f.publish(f.req(2, termTip2, ""))
		if r := termReason(t, err); r != apitypes.TerminalReasonPublicationInFlight {
			t.Fatalf("reason = %s", r)
		}
	})
}

// The lifetime classification, end to end: precedence, abandonment, and the not-proven cases.
func TestTerminalLifetimePrecedenceLiveDB(t *testing.T) {
	type tc struct {
		name        string
		issueless   bool // the run has no issue_iid but an mr_iid
		issue       forge.Issue
		issueErr    error
		mr          forge.MergeRequestSummary
		mrErr       error
		wantSend    bool
		wantDisp    string
		wantClass   string
		wantIssueGT int // expected GetIssue calls
		wantMRGT    int // expected GetMergeRequestSummary calls
	}
	closedIssue := forge.Issue{State: "closed"}
	for _, c := range []tc{
		{name: "open issue and closed PR preserves", issue: forge.Issue{State: "opened"}, mr: forge.MergeRequestSummary{State: forge.MRStateMerged},
			wantSend: true, wantDisp: "preserved", wantClass: "issue", wantIssueGT: 1},
		{name: "closed issue abandons", issue: closedIssue, wantDisp: "closed_issue_abandoned", wantClass: "issue", wantIssueGT: 1},
		{name: "closed issue wins over an open PR", issue: closedIssue, mr: forge.MergeRequestSummary{State: forge.MRStateOpened},
			wantDisp: "closed_issue_abandoned", wantClass: "issue", wantIssueGT: 1},
		{name: "issue-less merged PR abandons", issueless: true, mr: forge.MergeRequestSummary{State: forge.MRStateMerged},
			wantDisp: "closed_pr_abandoned", wantClass: "pr", wantMRGT: 1},
		{name: "issue-less closed PR abandons", issueless: true, mr: forge.MergeRequestSummary{State: forge.MRStateClosed},
			wantDisp: "closed_pr_abandoned", wantClass: "pr", wantMRGT: 1},
		{name: "issue-less open PR preserves", issueless: true, mr: forge.MergeRequestSummary{State: forge.MRStateOpened},
			wantSend: true, wantDisp: "preserved", wantClass: "pr", wantMRGT: 1},
		{name: "issue-less locked PR is not proven", issueless: true, mr: forge.MergeRequestSummary{State: forge.MRStateLocked},
			wantSend: true, wantDisp: "preserved", wantClass: "pr", wantMRGT: 1},
		{name: "issue lookup error preserves", issueErr: errors.New("forge down"),
			wantSend: true, wantDisp: "preserved", wantClass: "issue", wantIssueGT: 1},
		{name: "PR lookup error preserves", issueless: true, mrErr: errors.New("forge down"),
			wantSend: true, wantDisp: "preserved", wantClass: "pr", wantMRGT: 1},
		{name: "a closed pull request answered as an issue is not proven", issue: forge.Issue{State: "closed", IsPullRequest: true},
			wantSend: true, wantDisp: "preserved", wantClass: "issue", wantIssueGT: 1},
		{name: "an unknown issue state is not proven", issue: forge.Issue{State: "archived"},
			wantSend: true, wantDisp: "preserved", wantClass: "issue", wantIssueGT: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTermFix(t, "flight")
			f.forges.issue, f.forges.issueErr, f.forges.mr, f.forges.mrErr = c.issue, c.issueErr, c.mr, c.mrErr
			if c.issueless {
				f.issueless(77)
			} else {
				f.e.exec(t, `UPDATE runs SET mr_iid=77 WHERE id=$1`, f.run)
			}
			res, err := f.publish(f.req(1, termTip1, ""))
			if err != nil {
				t.Fatal(err)
			}
			pre, sends, _ := f.fake.counts()
			if (sends == 1) != c.wantSend || (pre == 1) != c.wantSend {
				t.Fatalf("prepares=%d sends=%d, want a push: %v", pre, sends, c.wantSend)
			}
			p, _ := f.pres()
			con, _ := f.con(1)
			if con.Disposition != c.wantDisp || p.LifetimeAtBind != c.wantClass || res.Disposition != c.wantDisp {
				t.Fatalf("disposition=%s class=%s result=%+v, want %s/%s", con.Disposition, p.LifetimeAtBind, res, c.wantDisp, c.wantClass)
			}
			if c.wantSend {
				if con.State != "published" || p.State != "live" {
					t.Fatalf("contribution=%s preservation=%s", con.State, p.State)
				}
			} else if con.State != "recorded" || p.State != "not_published" || p.LiveRunID.Valid || con.CreateInvokedAt.Valid {
				t.Fatalf("an abandonment: contribution=%+v preservation=%+v", con, p)
			}
			f.forges.mu.Lock()
			defer f.forges.mu.Unlock()
			if f.forges.getIssues != c.wantIssueGT || f.forges.getMRs != c.wantMRGT {
				t.Fatalf("GetIssue calls=%d GetMergeRequestSummary calls=%d, want %d/%d", f.forges.getIssues, f.forges.getMRs, c.wantIssueGT, c.wantMRGT)
			}
			// The issue side comes from IssueTrackerForRepo, the PR side from CodeHostForRepo, and
			// the generic ForgeForConnection builder is never used for the read.
			if f.forges.fallback != 0 || (c.wantIssueGT > 0 && f.forges.issueCalls != 1) || (c.wantMRGT > 0 && f.forges.hostCalls != 1) ||
				(c.wantIssueGT > 0 && f.forges.hostCalls != 0) || (c.wantMRGT > 0 && f.forges.issueCalls != 0) {
				t.Fatalf("issueTrackerForRepo=%d codeHostForRepo=%d fallback=%d", f.forges.issueCalls, f.forges.hostCalls, f.forges.fallback)
			}
		})
	}

	t.Run("no PR at all is no_pr and never reads the forge", func(t *testing.T) {
		f := newTermFix(t, "flight")
		f.issueless(0)
		if _, err := f.publish(f.req(1, termTip1, "")); err != nil {
			t.Fatal(err)
		}
		if p, _ := f.pres(); p.LifetimeAtBind != "no_pr" || p.State != "live" {
			t.Fatalf("preservation = %+v", p)
		}
		if f.forges.issueCalls+f.forges.hostCalls != 0 {
			t.Fatal("a no_pr run read the forge")
		}
	})

	t.Run("a closed lifetime with a ref already live stays due and sends nothing", func(t *testing.T) {
		f := newTermFix(t, "flight")
		if _, err := f.publish(f.req(1, termTip1, "")); err != nil {
			t.Fatal(err)
		}
		f.e.exec(t, `UPDATE run_terminal_preservations SET next_check_at = now() + interval '1 day' WHERE run_id=$1`, f.run)
		f.e.exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run)
		f.forges.issue = forge.Issue{State: "closed"}
		res, err := f.publish(f.req(2, termTip2, termTip1))
		if err != nil || res.State != "recorded" || res.Disposition != "closed_issue_abandoned" {
			t.Fatalf("result = %+v, %v", res, err)
		}
		p, _ := f.pres()
		if p.State != "live" || p.Tip.String != termTip1 || p.NextCheckAt.Time.After(time.Now().Add(time.Minute)) {
			t.Fatalf("preservation = %+v, want live, unchanged tip and due now", p)
		}
		if f.fake.creates != 1 || f.fake.updates != 0 {
			t.Fatalf("creates=%d updates=%d", f.fake.creates, f.fake.updates)
		}
	})
}

// The completed-release gate and the sweep are untouched by a recorded abandonment's guard: a
// recorded contribution still obeys the frozen-row guard.
func TestTerminalRecordedContributionIsFrozenLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	f.forges.issue = forge.Issue{State: "closed"}
	if _, err := f.publish(f.req(1, termTip1, "")); err != nil {
		t.Fatal(err)
	}
	c, _ := f.con(1)
	if tag, err := f.e.pool.Exec(f.e.ctx, `UPDATE run_terminal_contributions SET state='invoked' WHERE id=$1`, c.ID); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("a recorded contribution moved: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

// issueless turns the fixture's run into a ci_fix run, which has no issue (runs_kind_shape), with
// mr as its merge request (0: none).
func (f *termFix) issueless(mr int64) {
	f.t.Helper()
	if mr == 0 {
		f.e.exec(f.t, `UPDATE runs SET kind='ci_fix', issue_iid=NULL, mr_iid=NULL, pipeline_id=1, pipeline_ref='refs/heads/x' WHERE id=$1`, f.run)
		return
	}
	f.e.exec(f.t, `UPDATE runs SET kind='ci_fix', issue_iid=NULL, mr_iid=$2, pipeline_id=1, pipeline_ref='refs/heads/x' WHERE id=$1`, f.run, mr)
}

// Unit: the manifest canonicalisation is order-independent, bounded and strict.
func TestCanonicalTerminalManifest(t *testing.T) {
	run, worker := uuid.New(), uuid.New()
	base := apitypes.TerminalManifest{
		RunID: run.String(), Generation: 3, WorkerID: worker.String(),
		Roots: []apitypes.TerminalManifestRoot{
			{SourceID: "b", AttemptID: "1", Commit: termCommitA, Tree: termTreeA},
			{SourceID: "a", AttemptID: "1", Commit: termCommitA, Tree: termTreeA},
		},
		Abandoned: []apitypes.TerminalManifestAbandoned{
			{Class: "ignored", SourceID: "z", Extents: apitypes.TerminalManifestExtents{Files: 1}},
			{Class: "cold_dirty", SourceID: "a", Extents: apitypes.TerminalManifestExtents{Bytes: 5}},
		},
	}
	want := fmt.Sprintf(`{"run_id":%q,"generation":3,"worker_id":%q,"roots":[{"source_id":"a","attempt_id":"1","commit":%q,"tree":%q},{"source_id":"b","attempt_id":"1","commit":%q,"tree":%q}],"abandoned":[{"class":"cold_dirty","source_id":"a","extents":{"files":0,"bytes":5}},{"class":"ignored","source_id":"z","extents":{"files":1,"bytes":0}}]}`,
		run, worker, termCommitA, termTreeA, termCommitA, termTreeA)
	canon, digest, roots, ref := canonicalTerminalManifest(base, run, worker, 3)
	sum := sha256.Sum256([]byte(want))
	if ref != nil || string(canon) != want || digest != hex.EncodeToString(sum[:]) || len(roots) != 2 {
		t.Fatalf("canon=%s digest=%s roots=%v ref=%v", canon, digest, roots, ref)
	}
	swapped := base
	swapped.Roots = []apitypes.TerminalManifestRoot{base.Roots[1], base.Roots[0]}
	if _, d2, _, _ := canonicalTerminalManifest(swapped, run, worker, 3); d2 != digest {
		t.Fatal("the digest depends on the order the worker listed the roots in")
	}

	bad := map[string]func(m *apitypes.TerminalManifest){
		"duplicate root":      func(m *apitypes.TerminalManifest) { m.Roots = append(m.Roots, m.Roots[0]) },
		"bad class":           func(m *apitypes.TerminalManifest) { m.Abandoned[0].Class = "tracked" },
		"negative extents":    func(m *apitypes.TerminalManifest) { m.Abandoned[0].Extents.Files = -1 },
		"duplicate abandoned": func(m *apitypes.TerminalManifest) { m.Abandoned = append(m.Abandoned, m.Abandoned[0]) },
		"bad source id":       func(m *apitypes.TerminalManifest) { m.Roots[0].SourceID = "has space" },
		"short commit":        func(m *apitypes.TerminalManifest) { m.Roots[0].Commit = "abc" },
		"too many roots": func(m *apitypes.TerminalManifest) {
			for i := 0; len(m.Roots) <= apitypes.TerminalManifestMaxRoots; i++ {
				m.Roots = append(m.Roots, apitypes.TerminalManifestRoot{SourceID: fmt.Sprintf("s%d", i), AttemptID: "1", Commit: termCommitA, Tree: termTreeA})
			}
		},
	}
	for name, mutate := range bad {
		m := base
		m.Roots = append([]apitypes.TerminalManifestRoot(nil), base.Roots...)
		m.Abandoned = append([]apitypes.TerminalManifestAbandoned(nil), base.Abandoned...)
		mutate(&m)
		if _, _, _, ref := canonicalTerminalManifest(m, run, worker, 3); ref == nil || ref.Reason != apitypes.TerminalReasonManifestInvalid {
			t.Fatalf("%s: refusal = %v, want manifest_invalid", name, ref)
		}
	}
}
