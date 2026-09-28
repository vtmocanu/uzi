package workersvc

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1867 M3: SweepSalvage against a fake store and a fake broker. The SQL filters
// (plan_rejected, eligible kinds, window, forge kind) are the store's and are covered by
// its LiveDB tests; here the pass is pinned on the params it passes and on every
// transition it drives.

var salvageNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

const (
	salvageTip   = "2222222222222222222222222222222222222222"
	salvagePATok = "bot-pat-SALVAGE-abcdef1234567890"
)

// salvageStore is the run_salvage slice of Store, recording every call. Writes return
// one row and fail when their context is already done, so a test can tell whether an
// outcome write survived an expired pass budget.
type salvageStore struct {
	Store

	mu sync.Mutex

	candidates    []store.ListSalvageCandidatesRow
	candidatesErr error
	candParams    []store.ListSalvageCandidatesParams
	insertErr     map[uuid.UUID]error
	inserts       []store.InsertRunSalvageParams

	dueExpiry     []store.RunSalvage
	duePending    []store.RunSalvage
	dueErr        error
	expiryParams  []store.ListSalvageDueExpiryParams
	pendingParams []store.ListSalvageDuePendingParams

	createdErr   error // RecordSalvageCreated fails with this (the ref landed unrecorded)
	created      []store.RecordSalvageCreatedParams
	promoted     []store.MarkSalvagePromotedParams
	attempts     []store.RecordSalvageAttemptFailedParams
	settled      []store.SettleSalvageParams
	expireFailed []store.RecordSalvageExpireFailedParams

	claimCtx   store.GetRunClaimContextRow
	claimErr   error
	claimCalls int
}

func (f *salvageStore) ListSalvageCandidates(_ context.Context, arg store.ListSalvageCandidatesParams) ([]store.ListSalvageCandidatesRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candParams = append(f.candParams, arg)
	return f.candidates, f.candidatesErr
}

func (f *salvageStore) InsertRunSalvage(_ context.Context, arg store.InsertRunSalvageParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.insertErr[arg.RunID]; err != nil {
		return 0, err
	}
	f.inserts = append(f.inserts, arg)
	return 1, nil
}

func (f *salvageStore) ListSalvageDueExpiry(_ context.Context, arg store.ListSalvageDueExpiryParams) ([]store.RunSalvage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expiryParams = append(f.expiryParams, arg)
	return f.dueExpiry, f.dueErr
}

func (f *salvageStore) ListSalvageDuePending(_ context.Context, arg store.ListSalvageDuePendingParams) ([]store.RunSalvage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingParams = append(f.pendingParams, arg)
	return f.duePending, f.dueErr
}

func (f *salvageStore) RecordSalvageCreated(ctx context.Context, arg store.RecordSalvageCreatedParams) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createdErr != nil {
		return 0, f.createdErr
	}
	f.created = append(f.created, arg)
	return 1, nil
}

func (f *salvageStore) MarkSalvagePromoted(ctx context.Context, arg store.MarkSalvagePromotedParams) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoted = append(f.promoted, arg)
	return 1, nil
}

func (f *salvageStore) RecordSalvageAttemptFailed(ctx context.Context, arg store.RecordSalvageAttemptFailedParams) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, arg)
	return 1, nil
}

func (f *salvageStore) SettleSalvage(ctx context.Context, arg store.SettleSalvageParams) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, arg)
	return 1, nil
}

func (f *salvageStore) RecordSalvageExpireFailed(ctx context.Context, arg store.RecordSalvageExpireFailedParams) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireFailed = append(f.expireFailed, arg)
	return 1, nil
}

func (f *salvageStore) GetRunClaimContext(_ context.Context, _ uuid.UUID) (store.GetRunClaimContextRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls++
	return f.claimCtx, f.claimErr
}

// salvageBroker is a fake forge behind the three salvage seams (salvageListRefTipsFn,
// salvageCreateRefFn, salvageDeleteRefFn), modelling #1810's primitives. Every row in
// these tests records tip salvageTip. Per run, result picks the forge state the next
// attempt sees:
//
//   - unset, salvageCreated or salvageFailed: the branch checkpoint ref (or, with
//     fromRecovery, refs/uzi-recovery/<run-id>) is at the tip, so a create lands, unless
//     result is salvageFailed or createErr is set, when CreateRef fails without writing;
//   - salvageUnavailable: neither source is at the tip;
//   - salvageRefused: refs/uzi-salvage/<run-id> is already at salvageOtherTip.
//
// order logs every call as "list:<run>", "create:<run>" or "delete:<run>".
type salvageBroker struct {
	t *testing.T

	mu           sync.Mutex
	result       map[uuid.UUID]salvageResult
	createErr    map[uuid.UUID]error
	fromRecovery map[uuid.UUID]bool
	panicOn      map[uuid.UUID]bool
	// salvage is the forge's refs/uzi-salvage/<run-id> tips.
	salvage map[uuid.UUID]string
	listErr error
	block   bool
	// blockRes: a blocked create applies once ctx is done (the forge applied it but the
	// reply arrived after the pass budget) and returns nil, not a failure.
	blockRes bool
	// blockDelete: every Delete hangs until ctx is done (a remote that stalls rather
	// than failing fast).
	blockDelete bool
	deleteErr   error
	// deleteNoop: Delete returns nil but leaves the ref in place, as casDelete does for a
	// lock-failure refusal it classifies benign.
	deleteNoop bool
	lists      []pushbroker.ListRefsOptions
	creates    []pushbroker.CreateRefOptions
	deletes    []pushbroker.DeleteOptions
	order      []string
}

const salvageOtherTip = "3333333333333333333333333333333333333333"

// runOf is the run id a salvage-seam call is about, read from its refs/uzi-salvage/ ref.
func (b *salvageBroker) runOf(refs ...string) uuid.UUID {
	for _, r := range refs {
		if id, ok := strings.CutPrefix(r, pushbroker.SalvageRefPrefix); ok {
			return uuid.MustParse(id)
		}
	}
	b.t.Fatalf("salvage call without a refs/uzi-salvage/ ref: %v", refs)
	return uuid.Nil
}

// sourceTip is where the fake forge advertises ref for run id: a source ref at the tip
// unless the run is unavailable, and only the source the run is configured for.
func (b *salvageBroker) sourceTip(id uuid.UUID, ref string) (string, bool) {
	if b.result[id] == salvageUnavailable {
		return "", false
	}
	switch {
	case strings.HasPrefix(ref, pushbroker.RecoveryRefPrefix):
		return salvageTip, b.fromRecovery[id]
	case strings.HasPrefix(ref, checkpointRefPrefix):
		return salvageTip, !b.fromRecovery[id]
	}
	return "", false
}

func (b *salvageBroker) salvageTipOf(id uuid.UUID) (string, bool) {
	if b.result[id] == salvageRefused {
		return salvageOtherTip, true
	}
	tip, ok := b.salvage[id]
	return tip, ok
}

func (b *salvageBroker) list(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.runOf(refs...)
	b.lists = append(b.lists, o)
	b.order = append(b.order, "list:"+id.String())
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.listErr != nil {
		return nil, b.listErr
	}
	out := map[string]string{}
	for _, r := range refs {
		var tip string
		var ok bool
		if strings.HasPrefix(r, pushbroker.SalvageRefPrefix) {
			tip, ok = b.salvageTipOf(id)
		} else {
			tip, ok = b.sourceTip(id, r)
		}
		if ok {
			out[r] = tip
		}
	}
	return out, nil
}

func (b *salvageBroker) create(ctx context.Context, o pushbroker.CreateRefOptions) error {
	b.mu.Lock()
	id := b.runOf(o.Ref)
	if o.Ref != pushbroker.SalvageRef(id) {
		b.t.Errorf("CreateRef called with %q; salvage may only create refs/uzi-salvage/<run-id>", o.Ref)
	}
	b.creates = append(b.creates, o)
	b.order = append(b.order, "create:"+id.String())
	err, boom, block, blockRes := b.createErr[id], b.panicOn[id], b.block, b.blockRes
	b.mu.Unlock()
	if boom {
		panic("go-git nil deref for " + id.String() + " via " + o.PAT)
	}
	if block {
		<-ctx.Done()
		if !blockRes {
			return ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		return err
	}
	if r, set := b.result[id]; set && r == salvageFailed && !block {
		return errors.New("pushbroker: create ref: ng " + o.Ref + " refused")
	}
	if tip, ok := b.salvageTipOf(id); ok {
		if tip == o.Tip {
			return pushbroker.ErrRefExistsAtTip
		}
		return pushbroker.ErrRefExists
	}
	if tip, ok := b.sourceTip(id, o.SourceRef); !ok || tip != o.Tip {
		return pushbroker.ErrSourceMissing
	}
	b.salvage[id] = o.Tip
	return nil
}

// delete fails with ctx's error once ctx is done (as a real network call would), and
// panics for a run in panicOn.
func (b *salvageBroker) delete(ctx context.Context, o pushbroker.DeleteOptions) error {
	b.mu.Lock()
	if !strings.HasPrefix(o.Ref, pushbroker.SalvageRefPrefix) || o.ExpectedOldTip == "" || o.Branch != "" {
		b.t.Errorf("Delete called with %+v; salvage may only CAS-delete refs/uzi-salvage/*", o)
	}
	id := b.runOf(o.Ref)
	b.deletes = append(b.deletes, o)
	b.order = append(b.order, "delete:"+id.String())
	boom := b.panicOn[id]
	err, block := b.deleteErr, b.blockDelete
	b.mu.Unlock()
	if boom {
		panic("go-git nil deref deleting " + id.String() + " via " + o.PAT)
	}
	if block {
		<-ctx.Done()
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.deleteNoop && b.salvage[id] == o.ExpectedOldTip {
		delete(b.salvage, id)
	}
	return nil
}

// orderOf builds an expected broker order from kind:run pairs.
func orderOf(parts ...string) string { return strings.Join(parts, " ") }

// newSalvageSvc wires a Service for SweepSalvage: an open SSRF gate for the fake forge, a
// sealed bot PAT, the fake broker, a fixed clock, and a deleteCheckpointFn that fails the
// test if salvage ever reaches it.
func newSalvageSvc(t *testing.T, fs *salvageStore, forges []string, retention time.Duration) (*Service, *salvageBroker) {
	t.Helper()
	box := newBox(t)
	sealed, err := box.Seal([]byte(salvagePATok))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if fs.claimCtx.RepoWebUrl == "" {
		fs.claimCtx = store.GetRunClaimContextRow{
			RepoWebUrl:      "https://github.example.com/team/repo",
			BaseUrl:         "https://github.example.com",
			BotUsername:     "uzi-bot",
			TokenCiphertext: sealed,
		}
	}
	p := testParams()
	p.SalvageForges = forges
	p.RecoveryReadyRetention = retention
	svc := New(fs, box, p)
	svc.now = func() time.Time { return salvageNow }
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://github.example.com" })
	b := &salvageBroker{
		t:            t,
		result:       map[uuid.UUID]salvageResult{},
		createErr:    map[uuid.UUID]error{},
		fromRecovery: map[uuid.UUID]bool{},
		panicOn:      map[uuid.UUID]bool{},
		salvage:      map[uuid.UUID]string{},
	}
	svc.salvageListRefTipsFn = b.list
	svc.salvageCreateRefFn = b.create
	svc.salvageDeleteRefFn = b.delete
	failShared := func(name string) { t.Errorf("salvage reached %s; it must use its own seams", name) }
	svc.SetCreateRefFn(func(context.Context, pushbroker.CreateRefOptions) error { failShared("createRefFn"); return nil })
	svc.SetListRefTipsFn(func(context.Context, pushbroker.ListRefsOptions, ...string) (map[string]string, error) {
		failShared("listRefTipsFn")
		return nil, nil
	})
	svc.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error {
		t.Errorf("salvage reached deleteCheckpointFn; it must never delete the branch checkpoint ref")
		return nil
	})
	return svc, b
}

func pendingSalvageRow(forge string) store.RunSalvage {
	id := uuid.New()
	return store.RunSalvage{
		RunID: id, UserID: uuid.New(), RepoID: uuid.New(), ForgeType: forge,
		Branch: "agent/issue-7", Tip: salvageTip, LiveRunID: pgtype.UUID{Bytes: id, Valid: true}, State: "pending",
	}
}

func promotedSalvageRow(forge string) store.RunSalvage {
	r := pendingSalvageRow(forge)
	r.State = "promoted"
	r.SalvageCreatedAt = pgtype.Timestamptz{Time: salvageNow.Add(-200 * time.Hour), Valid: true}
	r.ExpiresAt = pgtype.Timestamptz{Time: salvageNow.Add(-time.Hour), Valid: true}
	return r
}

func TestSweepSalvageEnqueuePassesParamsAndRecordsStates(t *testing.T) {
	issueRun, selfRun, secretRun, taskRun, goneRun := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fs := &salvageStore{
		candidates: []store.ListSalvageCandidatesRow{
			{ID: issueRun, Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: 7, Valid: true}, CheckpointTip: salvageTip, FailOrigin: pgtype.Text{String: "agent_failure", Valid: true}, ForgeType: "github"},
			{ID: selfRun, Kind: runkind.SelfImprove, IssueIid: pgtype.Int8{Int64: 3, Valid: true}, CheckpointTip: salvageTip, ForgeType: "gitlab"},
			{ID: secretRun, Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: 8, Valid: true}, CheckpointTip: salvageTip, FailOrigin: pgtype.Text{String: "push_secret_blocked", Valid: true}, ForgeType: "github"},
			{ID: taskRun, Kind: "task", CheckpointTip: salvageTip, ForgeType: "github"}, // not checkpoint-eligible
			{ID: goneRun, Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: 9, Valid: true}, CheckpointTip: salvageTip, ForgeType: "github"},
		},
		insertErr: map[uuid.UUID]error{goneRun: &pgconn.PgError{Code: "23503", ConstraintName: "run_salvage_live_run_id_fkey"}},
	}
	svc, b := newSalvageSvc(t, fs, []string{"github", "gitlab"}, 168*time.Hour)

	n, err := svc.SweepSalvage(context.Background())
	if err != nil {
		t.Fatalf("SweepSalvage: %v (a 23503 on the live pointer is a benign skip)", err)
	}
	if n != 3 {
		t.Fatalf("touched = %d, want 3 inserts", n)
	}
	if len(fs.candParams) != 1 {
		t.Fatalf("ListSalvageCandidates calls = %d, want 1", len(fs.candParams))
	}
	cp := fs.candParams[0]
	if strings.Join(cp.Forges, ",") != "github,gitlab" || !cp.Since.Valid || !cp.Since.Time.Equal(salvageNow.Add(-168*time.Hour)) || cp.Lim != salvageEnqueueLimit {
		t.Fatalf("candidate params = %+v, want forges github,gitlab, since now-168h, lim %d", cp, salvageEnqueueLimit)
	}

	byRun := map[uuid.UUID]store.InsertRunSalvageParams{}
	for _, in := range fs.inserts {
		byRun[in.RunID] = in
	}
	if in := byRun[issueRun]; in.State != "pending" || in.Branch != "agent/issue-7" || in.Tip != salvageTip || in.ForgeType != "github" ||
		!in.LiveRunID.Valid || uuid.UUID(in.LiveRunID.Bytes) != issueRun {
		t.Errorf("issue run insert = %+v, want pending on agent/issue-7 with its own live pointer", in)
	}
	if in := byRun[selfRun]; in.State != "pending" || in.Branch != "uzi/self-improve/"+selfRun.String() {
		t.Errorf("self_improve insert = %+v, want pending on uzi/self-improve/<run-id>", in)
	}
	if in := byRun[secretRun]; in.State != "skipped_secret" || in.LiveRunID.Valid {
		t.Errorf("secret-blocked insert = %+v, want skipped_secret with no live pointer", in)
	}
	if _, ok := byRun[taskRun]; ok {
		t.Errorf("a non-checkpoint-eligible run was enqueued")
	}
	if len(b.creates)+len(b.deletes) != 0 {
		t.Errorf("enqueue made broker calls: creates=%d deletes=%d", len(b.creates), len(b.deletes))
	}
}

// TestSweepSalvageEnqueueWindowFloor: the look-back is max(retention, 24h).
func TestSweepSalvageEnqueueWindowFloor(t *testing.T) {
	for _, retention := range []time.Duration{time.Hour, 0, -time.Hour} {
		fs := &salvageStore{}
		svc, _ := newSalvageSvc(t, fs, []string{"forgejo"}, retention)
		if _, err := svc.SweepSalvage(context.Background()); err != nil {
			t.Fatalf("SweepSalvage: %v", err)
		}
		if got := fs.candParams[0].Since.Time; !got.Equal(salvageNow.Add(-24 * time.Hour)) {
			t.Errorf("retention %v: since = %v, want now-24h", retention, got)
		}
	}
}

// TestSweepSalvageEnqueueOtherFKViolationIsReturned: only a 23503 on the live-run pointer
// constraint is the benign "run deleted first" skip; a 23503 on any other constraint is a
// real error and is returned.
func TestSweepSalvageEnqueueOtherFKViolationIsReturned(t *testing.T) {
	run := uuid.New()
	fs := &salvageStore{
		candidates: []store.ListSalvageCandidatesRow{{ID: run, Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: 1, Valid: true}, CheckpointTip: salvageTip, ForgeType: "github"}},
		insertErr:  map[uuid.UUID]error{run: &pgconn.PgError{Code: "23503", ConstraintName: "run_salvage_repo_id_fkey"}},
	}
	svc, _ := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	if _, err := svc.SweepSalvage(context.Background()); err == nil || !strings.Contains(err.Error(), "23503") {
		t.Fatalf("SweepSalvage err = %v, want the non-pointer 23503 returned", err)
	}
}

// TestSweepSalvageEnqueueInsertErrorIsReturned: a non-FK insert error is a DB error: logged
// and returned, not swallowed.
func TestSweepSalvageEnqueueInsertErrorIsReturned(t *testing.T) {
	run := uuid.New()
	fs := &salvageStore{
		candidates: []store.ListSalvageCandidatesRow{{ID: run, Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: 1, Valid: true}, CheckpointTip: salvageTip, ForgeType: "github"}},
		insertErr:  map[uuid.UUID]error{run: errors.New("connection reset")},
	}
	svc, _ := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	if _, err := svc.SweepSalvage(context.Background()); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("SweepSalvage err = %v, want the insert error", err)
	}
}

// TestSweepSalvageOffMakesNoEnqueueAndNoBrokerCall: with UZI_SALVAGE_FORGES empty and no
// rows, the pass is the two due-list SELECTs and nothing else.
func TestSweepSalvageOffMakesNoEnqueueAndNoBrokerCall(t *testing.T) {
	fs := &salvageStore{candidates: []store.ListSalvageCandidatesRow{{ID: uuid.New(), Kind: runkind.Issue}}}
	svc, b := newSalvageSvc(t, fs, nil, 168*time.Hour)

	n, err := svc.SweepSalvage(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("SweepSalvage = (%d, %v), want (0, nil)", n, err)
	}
	if len(fs.candParams) != 0 || len(fs.inserts) != 0 {
		t.Errorf("salvage off still enqueued: list calls=%d inserts=%d", len(fs.candParams), len(fs.inserts))
	}
	if len(fs.expiryParams) != 1 || len(fs.pendingParams) != 1 {
		t.Errorf("due-list SELECTs = (%d, %d), want (1, 1)", len(fs.expiryParams), len(fs.pendingParams))
	}
	if len(b.creates)+len(b.deletes) != 0 || fs.claimCalls != 0 {
		t.Errorf("salvage off made forge work: creates=%d deletes=%d claim=%d", len(b.creates), len(b.deletes), fs.claimCalls)
	}
}

// TestSweepSalvagePendingTransitions pins each createSalvageRef outcome's store write, and
// that only a create that can land writes: unavailable and refused are decided from the
// list alone.
func TestSweepSalvagePendingTransitions(t *testing.T) {
	leak := "remote https://uzi-bot:" + salvagePATok + "@github.example.com failed"
	cases := []struct {
		name         string
		res          salvageResult
		fromRecovery bool
		err          error
		settled      string
		attempted    bool
		created      bool
	}{
		{"created from the branch ref", salvageCreated, false, nil, "", false, true},
		{"created from the recovery ref", salvageCreated, true, nil, "", false, true},
		{"unavailable", salvageUnavailable, false, nil, "unavailable", false, false},
		{"refused", salvageRefused, false, nil, "refused", false, false},
		{"failed with error", salvageFailed, false, errors.New(leak), "", true, false},
		{"create refused", salvageFailed, false, nil, "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			fs := &salvageStore{duePending: []store.RunSalvage{row}}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			b.result[row.RunID], b.fromRecovery[row.RunID] = tc.res, tc.fromRecovery
			if tc.err != nil {
				b.createErr[row.RunID] = tc.err
			}

			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			if len(b.lists) != 1 {
				t.Fatalf("lists = %d, want 1", len(b.lists))
			}
			if l := b.lists[0]; l.CloneURL != "https://github.example.com/team/repo.git" || l.Username != "uzi-bot" || l.PAT != salvagePATok {
				t.Errorf("list options = %+v, want the server-derived remote and the decrypted PAT", l)
			}
			wantCreates := 1
			if tc.res == salvageUnavailable || tc.res == salvageRefused {
				wantCreates = 0
			}
			if len(b.creates) != wantCreates {
				t.Fatalf("creates = %d, want %d", len(b.creates), wantCreates)
			}
			if wantCreates == 1 {
				o := b.creates[0]
				source := checkpointRefPrefix + row.Branch
				if tc.fromRecovery {
					source = pushbroker.RecoveryRefPrefix + row.RunID.String()
				}
				if o.CloneURL != "https://github.example.com/team/repo.git" || o.Ref != pushbroker.SalvageRef(row.RunID) ||
					o.Tip != row.Tip || o.SourceRef != source || o.Username != "uzi-bot" || o.PAT != salvagePATok {
					t.Errorf("create options = %+v, want the server-derived remote, the run's salvage ref at the row's tip from %s, and the decrypted PAT", o, source)
				}
			}
			if tc.created {
				if len(fs.created) != 1 || len(fs.promoted) != 1 {
					t.Fatalf("created=%d promoted=%d, want 1/1", len(fs.created), len(fs.promoted))
				}
				c := fs.created[0]
				if !c.CreatedAt.Time.Equal(salvageNow) || !c.ExpiresAt.Time.Equal(salvageNow.Add(168*time.Hour)) {
					t.Errorf("RecordSalvageCreated = %+v, want created now, expires now+168h", c)
				}
				if !fs.promoted[0].PromotedAt.Time.Equal(salvageNow) {
					t.Errorf("promoted_at = %v, want now", fs.promoted[0].PromotedAt.Time)
				}
			} else if len(fs.created)+len(fs.promoted) != 0 {
				t.Errorf("non-created outcome recorded a creation")
			}
			if tc.settled != "" {
				if len(fs.settled) != 1 || fs.settled[0].State != tc.settled {
					t.Errorf("settled = %+v, want %s", fs.settled, tc.settled)
				}
			} else if len(fs.settled) != 0 {
				t.Errorf("settled = %+v, want none", fs.settled)
			}
			if tc.attempted {
				if len(fs.attempts) != 1 || fs.attempts[0].AttemptCap != 10 || fs.attempts[0].LastError == "" {
					t.Fatalf("attempts = %+v, want one with cap 10 and an error", fs.attempts)
				}
				if strings.Contains(fs.attempts[0].LastError, salvagePATok) {
					t.Errorf("last_error carries the PAT: %q", fs.attempts[0].LastError)
				}
			} else if len(fs.attempts) != 0 {
				t.Errorf("attempts = %+v, want none", fs.attempts)
			}
		})
	}
}

// TestSweepSalvageExpiresAtRetention: expires_at = now + retention, or = now when the
// retention is non-positive.
func TestSweepSalvageExpiresAtRetention(t *testing.T) {
	for _, tc := range []struct {
		retention time.Duration
		want      time.Time
	}{
		{72 * time.Hour, salvageNow.Add(72 * time.Hour)},
		{0, salvageNow},
		{-time.Hour, salvageNow},
	} {
		row := pendingSalvageRow("github")
		fs := &salvageStore{duePending: []store.RunSalvage{row}}
		svc, b := newSalvageSvc(t, fs, []string{"github"}, tc.retention)
		b.result[row.RunID] = salvageCreated
		if _, err := svc.SweepSalvage(context.Background()); err != nil {
			t.Fatalf("SweepSalvage: %v", err)
		}
		if len(fs.created) != 1 || !fs.created[0].ExpiresAt.Time.Equal(tc.want) {
			t.Errorf("retention %v: RecordSalvageCreated = %+v, want expires %v", tc.retention, fs.created, tc.want)
		}
	}
}

// TestSweepSalvageRollbackSettlesDisabled: a pending row whose forge left
// UZI_SALVAGE_FORGES first CAS-deletes its own salvage ref at the recorded tip (a create
// may have landed unrecorded), then settles 'disabled'. No create is attempted.
func TestSweepSalvageRollbackSettlesDisabled(t *testing.T) {
	for _, forges := range [][]string{{"github"}, nil} {
		row := pendingSalvageRow("gitlab")
		fs := &salvageStore{duePending: []store.RunSalvage{row}}
		svc, b := newSalvageSvc(t, fs, forges, 168*time.Hour)
		if _, err := svc.SweepSalvage(context.Background()); err != nil {
			t.Fatalf("SweepSalvage: %v", err)
		}
		if len(b.deletes) != 1 || b.deletes[0].Ref != pushbroker.SalvageRef(row.RunID) || b.deletes[0].ExpectedOldTip != row.Tip {
			t.Fatalf("forges %v: deletes = %+v, want one CAS delete of the run's salvage ref at the tip", forges, b.deletes)
		}
		if len(fs.settled) != 1 || fs.settled[0].State != "disabled" || fs.settled[0].RunID != row.RunID {
			t.Errorf("forges %v: settled = %+v, want disabled", forges, fs.settled)
		}
		if len(b.creates) != 0 || len(fs.attempts) != 0 {
			t.Errorf("forges %v: rollback created=%d attempts=%d, want 0/0", forges, len(b.creates), len(fs.attempts))
		}
	}
}

// TestSweepSalvageUnrecordedCreateThenRollback is the orphan probe: pass 1's broker
// creates the salvage ref but RecordSalvageCreated fails, so the row stays pending with
// salvage_created_at NULL while refs/uzi-salvage/<run-id> exists. The forge is then rolled
// back. Pass 2 must CAS-delete that ref and settle 'disabled' only once the delete
// succeeds; a failing delete leaves the row pending with the error in last_error.
func TestSweepSalvageUnrecordedCreateThenRollback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleteErr error
	}{
		{"delete succeeds", nil},
		{"delete fails", errors.New("push https://uzi-bot:" + salvagePATok + "@github.example.com: 502")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			fs := &salvageStore{duePending: []store.RunSalvage{row}, createdErr: errors.New("db: connection reset")}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			b.result[row.RunID] = salvageCreated

			// Pass 1: the ref lands on the forge, the record fails.
			if _, err := svc.SweepSalvage(context.Background()); err == nil {
				t.Fatalf("pass 1: want the RecordSalvageCreated error returned")
			}
			if len(b.creates) != 1 || len(fs.created)+len(fs.promoted)+len(fs.settled) != 0 {
				t.Fatalf("pass 1: creates=%d created=%d promoted=%d settled=%d, want the create unrecorded",
					len(b.creates), len(fs.created), len(fs.promoted), len(fs.settled))
			}

			// Rollback, then pass 2 on the still-pending, unrecorded row.
			svc.p.SalvageForges = nil
			fs.createdErr = nil
			b.deleteErr = tc.deleteErr
			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("pass 2: %v", err)
			}
			if len(b.creates) != 1 {
				t.Errorf("pass 2 attempted a create on a rolled-back forge")
			}
			if len(b.deletes) != 1 || b.deletes[0].Ref != pushbroker.SalvageRef(row.RunID) || b.deletes[0].ExpectedOldTip != row.Tip {
				t.Fatalf("pass 2: deletes = %+v, want one CAS delete of refs/uzi-salvage/<run-id> at the tip", b.deletes)
			}
			if tc.deleteErr == nil {
				if len(fs.settled) != 1 || fs.settled[0].State != "disabled" || len(fs.attempts) != 0 {
					t.Fatalf("pass 2: settled=%+v attempts=%+v, want disabled after the delete", fs.settled, fs.attempts)
				}
				return
			}
			if len(fs.settled) != 0 {
				t.Fatalf("pass 2: settled %+v although the salvage ref delete failed (orphaned ref)", fs.settled)
			}
			if len(fs.attempts) != 1 || fs.attempts[0].AttemptCap != salvageNoCap ||
				!strings.Contains(fs.attempts[0].LastError, "502") || strings.Contains(fs.attempts[0].LastError, salvagePATok) {
				t.Fatalf("pass 2: attempts = %+v, want one uncapped attempt carrying the scrubbed delete error", fs.attempts)
			}
		})
	}
}

// TestSweepSalvageAttemptCapCleansUpFirst: the attempt that would reach the cap (and so
// settle 'failed', dropping the live pointer) makes NO create: it only CAS-deletes the
// run's salvage ref at the tip, and the cap applies only once that succeeds. A failed
// cleanup records the attempt uncapped with the cleanup error and the previous error; an
// attempt below the cap creates and makes no delete.
func TestSweepSalvageAttemptCapCleansUpFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		attempts  int32
		deleteErr error
		wantCap   int32
	}{
		{"below cap", salvageAttemptCap - 2, nil, salvageAttemptCap},
		{"at cap, cleanup ok", salvageAttemptCap - 1, nil, salvageAttemptCap},
		{"past cap, cleanup ok", salvageAttemptCap + 3, nil, salvageAttemptCap},
		{"at cap, cleanup fails", salvageAttemptCap - 1, errors.New("https://uzi-bot:" + salvagePATok + "@x: 503"), salvageNoCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			row.Attempts = tc.attempts
			row.LastError = pgtype.Text{String: "create: 500", Valid: true}
			fs := &salvageStore{duePending: []store.RunSalvage{row}}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			b.createErr[row.RunID] = errors.New("create: 500")
			b.deleteErr = tc.deleteErr
			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			id := row.RunID.String()
			wantOrder := orderOf("list:"+id, "create:"+id)
			if tc.attempts+1 >= salvageAttemptCap {
				// A successful cleanup is confirmed by a re-list; a failed one is not.
				wantOrder = orderOf("delete:"+id, "list:"+id)
				if tc.deleteErr != nil {
					wantOrder = orderOf("delete:" + id)
				}
				if b.deletes[0].Ref != pushbroker.SalvageRef(row.RunID) || b.deletes[0].ExpectedOldTip != row.Tip {
					t.Errorf("delete = %+v, want the run's salvage ref at the tip", b.deletes[0])
				}
			}
			if strings.Join(b.order, " ") != wantOrder {
				t.Fatalf("broker order = %v, want %s (a capping attempt never creates)", b.order, wantOrder)
			}
			if len(fs.attempts) != 1 || fs.attempts[0].AttemptCap != tc.wantCap {
				t.Fatalf("attempts = %+v, want one with cap %d", fs.attempts, tc.wantCap)
			}
			le := fs.attempts[0].LastError
			if !strings.Contains(le, "create: 500") || strings.Contains(le, salvagePATok) {
				t.Errorf("last_error = %q, want the attempt error, scrubbed", le)
			}
			if tc.deleteErr != nil && !strings.Contains(le, "503") {
				t.Errorf("last_error = %q, want the cleanup error visible", le)
			}
			if len(fs.settled) != 0 {
				t.Errorf("settled = %+v, want none", fs.settled)
			}
		})
	}
}

// TestSweepSalvageCapFiresDespiteBlockingCreate is the timeout probe: a create that
// blocks until the pass budget is spent must not keep a row at attempts = cap-1 from
// capping. The capping attempt makes no create, so its cleanup runs on a live budget
// and the capped failure is written on the first pass.
func TestSweepSalvageCapFiresDespiteBlockingCreate(t *testing.T) {
	row := pendingSalvageRow("github")
	row.Attempts = salvageAttemptCap - 1
	fs := &salvageStore{duePending: []store.RunSalvage{row}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	svc.salvagePassBudget = 50 * time.Millisecond
	b.block = true
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(b.creates) != 0 || len(b.deletes) != 1 {
		t.Fatalf("creates=%d deletes=%d, want 0/1: the capping attempt only cleans up", len(b.creates), len(b.deletes))
	}
	if len(fs.attempts) != 1 || fs.attempts[0].AttemptCap != salvageAttemptCap {
		t.Fatalf("attempts = %+v, want one capped attempt", fs.attempts)
	}
}

// TestSweepSalvageBackoffPastCap: a row with no recorded salvage ref at or past the cap is
// retried only once its updated_at is salvageRetryBackoff old; backed-off rows make no
// broker call and no write, and do not take the item budget from due rows.
func TestSweepSalvageBackoffPastCap(t *testing.T) {
	backedOff := func(age time.Duration) store.RunSalvage {
		r := pendingSalvageRow("github")
		r.Attempts = salvageAttemptCap + 2
		r.UpdatedAt = pgtype.Timestamptz{Time: salvageNow.Add(-age), Valid: true}
		return r
	}
	var rows []store.RunSalvage
	for range salvageMaxItems + 2 {
		rows = append(rows, backedOff(30*time.Minute))
	}
	due := backedOff(salvageRetryBackoff)                               // exactly 1h old: due
	below := pendingSalvageRow("github")                                // below the cap: never backed off
	below.UpdatedAt = pgtype.Timestamptz{Time: salvageNow, Valid: true} // touched just now
	half := backedOff(time.Minute)                                      // a recorded ref is never backed off
	half.SalvageCreatedAt = pgtype.Timestamptz{Time: salvageNow.Add(-time.Hour), Valid: true}
	half.ExpiresAt = pgtype.Timestamptz{Time: salvageNow.Add(time.Hour), Valid: true}
	rows = append(rows, due, below, half)

	fs := &salvageStore{duePending: rows}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	b.result[below.RunID] = salvageUnavailable
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	// The due row's capping cleanup (delete, then its confirming list), then the row below
	// the cap, whose list alone settles it unavailable.
	want := orderOf("delete:"+due.RunID.String(), "list:"+due.RunID.String(), "list:"+below.RunID.String())
	if got := strings.Join(b.order, " "); got != want {
		t.Fatalf("broker order = %s, want %s (backed-off rows skipped without using the budget)", got, want)
	}
	if len(fs.attempts) != 1 || fs.attempts[0].RunID != due.RunID {
		t.Errorf("attempts = %+v, want only the due row's", fs.attempts)
	}
	if len(fs.promoted) != 1 || fs.promoted[0].RunID != half.RunID {
		t.Errorf("promoted = %+v, want the half-promoted row finished despite its attempts", fs.promoted)
	}
	if fs.pendingParams[0].Lim != salvagePendingScanLimit {
		t.Errorf("pending scan limit = %d, want %d", fs.pendingParams[0].Lim, salvagePendingScanLimit)
	}
}

// TestSweepSalvageHardCeilingGivesUp: at salvageHardCeiling attempts a pending row with
// no recorded salvage ref is settled 'failed' through RecordSalvageAttemptFailed with the
// normal cap and NO forge call (not even the claim context), and last_error names the
// possibly orphaned ref and its tip. This holds on an enabled and on a rolled-back forge;
// one attempt below the ceiling still runs the cleanup.
func TestSweepSalvageHardCeilingGivesUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		forges   []string
		attempts int32
		giveUp   bool
	}{
		{"enabled, at ceiling", []string{"github"}, salvageHardCeiling, true},
		{"rolled back, at ceiling", nil, salvageHardCeiling, true},
		{"enabled, past ceiling", []string{"github"}, salvageHardCeiling + 4, true},
		{"enabled, below ceiling", []string{"github"}, salvageHardCeiling - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			row.Attempts = tc.attempts
			row.UpdatedAt = pgtype.Timestamptz{Time: salvageNow.Add(-2 * salvageRetryBackoff), Valid: true}
			fs := &salvageStore{duePending: []store.RunSalvage{row}}
			svc, b := newSalvageSvc(t, fs, tc.forges, 168*time.Hour)
			b.deleteErr = errors.New("401 bad credentials")
			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			if len(fs.attempts) != 1 || len(fs.settled) != 0 {
				t.Fatalf("attempts=%+v settled=%+v, want one attempt write and no settle", fs.attempts, fs.settled)
			}
			a := fs.attempts[0]
			if !tc.giveUp {
				if len(b.deletes) != 1 || a.AttemptCap != salvageNoCap {
					t.Fatalf("below ceiling: deletes=%d cap=%d, want the cleanup retried uncapped", len(b.deletes), a.AttemptCap)
				}
				return
			}
			if len(b.creates)+len(b.deletes) != 0 || fs.claimCalls != 0 {
				t.Fatalf("give-up made forge work: creates=%d deletes=%d claim=%d", len(b.creates), len(b.deletes), fs.claimCalls)
			}
			if a.AttemptCap != salvageAttemptCap {
				t.Errorf("give-up cap = %d, want the normal cap %d (which settles 'failed' and clears the pointer)", a.AttemptCap, salvageAttemptCap)
			}
			want := fmt.Sprintf("gave up after %d attempts; refs/uzi-salvage/%s may remain on the forge at %s and can be deleted by hand",
				tc.attempts, row.RunID, row.Tip)
			if a.LastError != want {
				t.Errorf("last_error = %q, want %q", a.LastError, want)
			}
		})
	}
}

// TestSweepSalvageDeadRemoteIsBounded simulates a permanently dead remote (the claim
// context never resolves) against a store that applies each attempt write, on the default
// 15s sweep tick. The row must settle within the hard ceiling, making at most one
// claim-context read per attempt and about 20 hourly attempts past the cap.
func TestSweepSalvageDeadRemoteIsBounded(t *testing.T) {
	row := pendingSalvageRow("github")
	row.UpdatedAt = pgtype.Timestamptz{Time: salvageNow, Valid: true}
	fs := &salvageStore{claimErr: errors.New("401 bad credentials")}
	svc, _ := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	clock := salvageNow
	svc.now = func() time.Time { return clock }
	settled := false
	var passes int
	for passes = 0; passes < 100_000 && !settled; passes++ {
		fs.duePending = []store.RunSalvage{row}
		fs.attempts = nil
		if _, err := svc.SweepSalvage(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", passes, err)
		}
		if len(fs.attempts) == 1 {
			a := fs.attempts[0]
			row.Attempts++
			row.UpdatedAt = pgtype.Timestamptz{Time: clock, Valid: true}
			row.LastError = pgtype.Text{String: a.LastError, Valid: true}
			settled = row.Attempts >= a.AttemptCap
		}
		clock = clock.Add(15 * time.Second)
	}
	if !settled {
		t.Fatalf("row still pending after %d passes (attempts %d)", passes, row.Attempts)
	}
	if row.Attempts != salvageHardCeiling+1 || fs.claimCalls != salvageHardCeiling {
		t.Errorf("attempts=%d claim reads=%d, want %d/%d", row.Attempts, fs.claimCalls, salvageHardCeiling+1, salvageHardCeiling)
	}
	// About 20 hourly retries past the cap: never faster (the backoff held), never much slower.
	el := clock.Sub(salvageNow)
	if lo, hi := time.Duration(salvageHardCeiling-salvageAttemptCap)*salvageRetryBackoff,
		time.Duration(salvageHardCeiling-salvageAttemptCap+2)*salvageRetryBackoff; el < lo || el > hi {
		t.Errorf("settled after %v, want between %v and %v (hourly retries past the cap)", el, lo, hi)
	}
}

// TestSweepSalvageCreatedOutcomeSurvivesBudget: the broker reports Created only after the
// pass budget expired (the forge applied the ref, the reply was late). The outcome writes
// run on the detached write context, so RecordSalvageCreated and MarkSalvagePromoted are
// still written instead of leaving the created ref unrecorded.
func TestSweepSalvageCreatedOutcomeSurvivesBudget(t *testing.T) {
	row := pendingSalvageRow("github")
	fs := &salvageStore{duePending: []store.RunSalvage{row}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	svc.salvagePassBudget = 50 * time.Millisecond
	b.block, b.blockRes = true, true
	b.result[row.RunID] = salvageCreated
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(fs.created) != 1 || len(fs.promoted) != 1 {
		t.Fatalf("created=%d promoted=%d, want 1/1 written after the budget expired", len(fs.created), len(fs.promoted))
	}
}

// TestSweepSalvageFinishesHalfPromotedRow: a pending row whose creation was recorded but
// not yet marked promoted is marked promoted with no broker call (never settled
// 'disabled', which would forget a created ref).
func TestSweepSalvageFinishesHalfPromotedRow(t *testing.T) {
	row := pendingSalvageRow("gitlab")
	row.SalvageCreatedAt = pgtype.Timestamptz{Time: salvageNow.Add(-time.Hour), Valid: true}
	row.ExpiresAt = pgtype.Timestamptz{Time: salvageNow.Add(time.Hour), Valid: true}
	fs := &salvageStore{duePending: []store.RunSalvage{row}}
	svc, b := newSalvageSvc(t, fs, nil, 168*time.Hour) // gitlab also rolled back
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(fs.promoted) != 1 || len(fs.settled) != 0 {
		t.Errorf("promoted=%d settled=%+v, want the row promoted and not settled", len(fs.promoted), fs.settled)
	}
	if len(b.creates)+len(b.deletes) != 0 {
		t.Errorf("half-promoted repair made broker calls")
	}
}

// TestSweepSalvageExpiry: a due salvage ref is CAS-deleted by its own ref name at the
// recorded tip and settles 'expired'; a delete error is recorded for retry. Expiry runs
// on a forge that has left UZI_SALVAGE_FORGES too.
func TestSweepSalvageExpiry(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		row := promotedSalvageRow("github")
		fs := &salvageStore{dueExpiry: []store.RunSalvage{row}}
		svc, b := newSalvageSvc(t, fs, nil, 168*time.Hour)
		b.salvage[row.RunID] = row.Tip
		n, err := svc.SweepSalvage(context.Background())
		if err != nil || n != 1 {
			t.Fatalf("SweepSalvage = (%d, %v), want (1, nil)", n, err)
		}
		if len(b.deletes) != 1 {
			t.Fatalf("deletes = %d, want 1", len(b.deletes))
		}
		d := b.deletes[0]
		if d.Ref != pushbroker.SalvageRef(row.RunID) || d.ExpectedOldTip != row.Tip || d.PAT != salvagePATok ||
			d.CloneURL != "https://github.example.com/team/repo.git" {
			t.Errorf("Delete options = %+v, want the run's salvage ref at the recorded tip", d)
		}
		if _, ok := b.salvage[row.RunID]; ok {
			t.Errorf("salvage ref still on the fake forge after expiry")
		}
		if len(fs.settled) != 1 || fs.settled[0].State != "expired" {
			t.Errorf("settled = %+v, want expired", fs.settled)
		}
		if len(b.creates) != 0 {
			t.Errorf("expiry called CreateRef")
		}
	})
	t.Run("delete error", func(t *testing.T) {
		row := promotedSalvageRow("github")
		fs := &salvageStore{dueExpiry: []store.RunSalvage{row}}
		svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
		b.deleteErr = errors.New("push https://uzi-bot:" + salvagePATok + "@github.example.com: 503")
		if _, err := svc.SweepSalvage(context.Background()); err != nil {
			t.Fatalf("SweepSalvage: %v", err)
		}
		if len(fs.settled) != 0 || len(fs.expireFailed) != 1 {
			t.Fatalf("settled=%+v expireFailed=%d, want the failure recorded and the row kept", fs.settled, len(fs.expireFailed))
		}
		if strings.Contains(fs.expireFailed[0].LastError, salvagePATok) {
			t.Errorf("expiry last_error carries the PAT: %q", fs.expireFailed[0].LastError)
		}
	})
}

// TestSweepSalvageSSRFRefusal: an un-allowlisted base URL or clone host is a failed
// attempt with no broker call, for a pending and for an expiry item.
func TestSweepSalvageSSRFRefusal(t *testing.T) {
	for _, tc := range []struct{ name, web, base string }{
		{"base url", "https://github.example.com/team/repo", "https://evil.example.net"},
		{"clone host", "https://evil.example.net/team/repo", "https://github.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pend, exp := pendingSalvageRow("github"), promotedSalvageRow("github")
			fs := &salvageStore{duePending: []store.RunSalvage{pend}, dueExpiry: []store.RunSalvage{exp}}
			fs.claimCtx = store.GetRunClaimContextRow{RepoWebUrl: tc.web, BaseUrl: tc.base, BotUsername: "uzi-bot"}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			if len(b.creates)+len(b.deletes) != 0 {
				t.Fatalf("SSRF refusal still reached the broker: creates=%d deletes=%d", len(b.creates), len(b.deletes))
			}
			if len(fs.attempts) != 1 || !strings.Contains(fs.attempts[0].LastError, "not allowlisted") {
				t.Errorf("pending attempts = %+v, want one allowlist failure", fs.attempts)
			}
			if len(fs.expireFailed) != 1 || !strings.Contains(fs.expireFailed[0].LastError, "not allowlisted") {
				t.Errorf("expiry failures = %+v, want one allowlist failure", fs.expireFailed)
			}
		})
	}
}

// TestSweepSalvageClaimContextErrorIsAttemptFailure: a claim-context read failure, or a
// run whose repo or connection is gone (no rows), counts as a failed attempt with no
// broker call, not a pass error.
func TestSweepSalvageClaimContextErrorIsAttemptFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"read fails", errors.New("db: connection reset"), "claim context"},
		{"run gone", pgx.ErrNoRows, "gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			fs := &salvageStore{duePending: []store.RunSalvage{row}, claimErr: tc.err}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			if len(fs.attempts) != 1 || len(b.lists)+len(b.creates) != 0 || !strings.Contains(fs.attempts[0].LastError, tc.want) {
				t.Fatalf("attempts=%+v lists=%d creates=%d, want one attempt naming %q and no broker call",
					fs.attempts, len(b.lists), len(b.creates), tc.want)
			}
		})
	}
}

// TestSweepSalvageFailsClosedWithoutGateOrBox: with no SSRF gate or no secret box wired,
// every item is a failed attempt with no claim-context read and no broker call.
func TestSweepSalvageFailsClosedWithoutGateOrBox(t *testing.T) {
	for _, tc := range []struct {
		name  string
		unset func(*Service)
	}{
		{"no allowlist", func(s *Service) { s.forgeBaseURLAllowed = nil }},
		{"no box", func(s *Service) { s.box = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			fs := &salvageStore{duePending: []store.RunSalvage{row}}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			tc.unset(svc)
			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			if len(fs.attempts) != 1 || !strings.Contains(fs.attempts[0].LastError, "not configured") ||
				fs.claimCalls != 0 || len(b.lists)+len(b.creates)+len(b.deletes) != 0 {
				t.Fatalf("attempts=%+v claim=%d broker=%d/%d/%d, want one fail-closed attempt and no forge work",
					fs.attempts, fs.claimCalls, len(b.lists), len(b.creates), len(b.deletes))
			}
		})
	}
}

// TestSweepSalvageRoundRobinAtMostFive: with 4 due expiries and 4 due pending rows each
// pass handles exactly 5, alternating expiry and pending. The first pass leads with
// expiry, the next with pending.
func TestSweepSalvageRoundRobinAtMostFive(t *testing.T) {
	var exp, pend []store.RunSalvage
	for range 4 {
		exp = append(exp, promotedSalvageRow("github"))
		pend = append(pend, pendingSalvageRow("github"))
	}
	fs := &salvageStore{dueExpiry: exp, duePending: pend}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	for _, r := range pend {
		b.result[r.RunID] = salvageUnavailable
	}
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	// An expiry is a delete plus its confirming list; an unavailable pending row is a list.
	exp0, exp1, exp2 := exp[0].RunID.String(), exp[1].RunID.String(), exp[2].RunID.String()
	pend0, pend1, pend2 := pend[0].RunID.String(), pend[1].RunID.String(), pend[2].RunID.String()
	want := []string{
		"delete:" + exp0, "list:" + exp0, "list:" + pend0,
		"delete:" + exp1, "list:" + exp1, "list:" + pend1,
		"delete:" + exp2, "list:" + exp2,
	}
	if strings.Join(b.order, " ") != strings.Join(want, " ") {
		t.Fatalf("broker order =\n  %v\nwant\n  %v", b.order, want)
	}
	// The fake due lists do not shrink, so the second pass sees the same rows.
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("second SweepSalvage: %v", err)
	}
	want2 := []string{
		"list:" + pend0, "delete:" + exp0, "list:" + exp0,
		"list:" + pend1, "delete:" + exp1, "list:" + exp1,
		"list:" + pend2,
	}
	if got := b.order[len(want):]; strings.Join(got, " ") != strings.Join(want2, " ") {
		t.Fatalf("second-pass broker order =\n  %v\nwant\n  %v", got, want2)
	}
	if fs.expiryParams[0].Lim != salvageMaxItems || fs.pendingParams[0].Lim != salvagePendingScanLimit {
		t.Errorf("due-list limits = (%d, %d), want (%d, %d)", fs.expiryParams[0].Lim, fs.pendingParams[0].Lim, salvageMaxItems, salvagePendingScanLimit)
	}
}

// TestSweepSalvageRoundRobinOneSided: only one list's rows still yields at most five, in
// order and with the right kind, whichever list leads.
func TestSweepSalvageRoundRobinOneSided(t *testing.T) {
	var rows []store.RunSalvage
	for range 7 {
		rows = append(rows, pendingSalvageRow("github"))
	}
	for _, lead := range []bool{false, true} {
		for _, expiry := range []bool{false, true} {
			var got []salvageItem
			if expiry {
				got = roundRobinSalvage(rows, nil, salvageMaxItems, lead)
			} else {
				got = roundRobinSalvage(nil, rows, salvageMaxItems, lead)
			}
			if len(got) != salvageMaxItems {
				t.Fatalf("lead=%v expiry=%v: items = %d, want %d", lead, expiry, len(got), salvageMaxItems)
			}
			for i, it := range got {
				if it.expiry != expiry || it.row.RunID != rows[i].RunID {
					t.Fatalf("lead=%v expiry=%v: item %d = %+v, want row %d in order", lead, expiry, i, it, i)
				}
			}
		}
	}
}

// TestSweepSalvageHangingExpiryDoesNotStarvePending: a due expiry whose remote hangs
// burns the whole pass budget. The lead alternates per pass, so a due pending row (here
// at cap-1, so its turn is the capping cleanup) still gets its turn within two passes.
func TestSweepSalvageHangingExpiryDoesNotStarvePending(t *testing.T) {
	exp := promotedSalvageRow("github")
	pend := pendingSalvageRow("github")
	pend.Attempts = salvageAttemptCap - 1
	fs := &salvageStore{dueExpiry: []store.RunSalvage{exp}, duePending: []store.RunSalvage{pend}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	svc.salvagePassBudget = 50 * time.Millisecond
	b.blockDelete = true

	for pass := range 2 {
		if _, err := svc.SweepSalvage(context.Background()); err != nil {
			t.Fatalf("pass %d: SweepSalvage: %v", pass, err)
		}
	}
	if !slices.Contains(b.order, "delete:"+pend.RunID.String()) {
		t.Fatalf("broker order = %v; the pending row never got its turn in two passes", b.order)
	}
	var pendingAttempted bool
	for _, a := range fs.attempts {
		pendingAttempted = pendingAttempted || a.RunID == pend.RunID
	}
	if !pendingAttempted {
		t.Fatalf("attempts = %+v; want the pending row's capping attempt recorded", fs.attempts)
	}
	if len(fs.expireFailed) == 0 {
		t.Errorf("the hanging expiry's timeout was not recorded")
	}
}

// TestSweepSalvagePassBudget: a broker call that never returns is cut off by the pass
// budget; its failure is still recorded (outcome writes are detached from the budget) and
// the remaining items wait for the next tick.
func TestSweepSalvagePassBudget(t *testing.T) {
	var pend []store.RunSalvage
	for range 3 {
		pend = append(pend, pendingSalvageRow("github"))
	}
	fs := &salvageStore{duePending: pend}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	svc.salvagePassBudget = 50 * time.Millisecond
	b.block = true

	start := time.Now()
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("pass took %v, want it bounded by the 50ms budget", el)
	}
	if len(b.creates) != 1 {
		t.Errorf("creates = %d, want 1 (the budget stops the rest)", len(b.creates))
	}
	if len(fs.attempts) != 1 || !strings.Contains(fs.attempts[0].LastError, "deadline") {
		t.Errorf("attempts = %+v, want the timed-out attempt recorded", fs.attempts)
	}
}

// TestSweepSalvageDefaultBudget pins the production pass budget.
func TestSweepSalvageDefaultBudget(t *testing.T) {
	if salvagePassBudgetDefault != 10*time.Second {
		t.Fatalf("salvagePassBudgetDefault = %v, want 10s", salvagePassBudgetDefault)
	}
	// Pass budget plus the in-flight item's detached write stays within the default
	// SWEEP_INTERVAL (15s) the pass shares with the run-liveness sweep.
	if salvagePassBudgetDefault+salvageWriteTimeout > 15*time.Second {
		t.Fatalf("budget %v + write %v exceeds the 15s sweep interval", salvagePassBudgetDefault, salvageWriteTimeout)
	}
	fs := &salvageStore{}
	svc, _ := newSalvageSvc(t, fs, nil, 0)
	var deadline time.Time
	svc.q = &deadlineProbe{salvageStore: fs, seen: &deadline}
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if left := time.Until(deadline); left <= 9*time.Second || left > 10*time.Second {
		t.Fatalf("pass deadline in %v, want ~10s", left)
	}
}

type deadlineProbe struct {
	*salvageStore
	seen *time.Time
}

func (d *deadlineProbe) ListSalvageDueExpiry(ctx context.Context, arg store.ListSalvageDueExpiryParams) ([]store.RunSalvage, error) {
	*d.seen, _ = ctx.Deadline()
	return d.salvageStore.ListSalvageDueExpiry(ctx, arg)
}

// TestSweepSalvagePanicIsRecoveredPerItem: a panicking broker call is recovered, counted
// as that item's failed attempt, and the next item still runs.
func TestSweepSalvagePanicIsRecoveredPerItem(t *testing.T) {
	bad, good := pendingSalvageRow("github"), pendingSalvageRow("github")
	fs := &salvageStore{duePending: []store.RunSalvage{bad, good}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	b.panicOn[bad.RunID] = true
	b.result[good.RunID] = salvageCreated
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(fs.attempts) != 1 || fs.attempts[0].RunID != bad.RunID || !strings.Contains(fs.attempts[0].LastError, "panic") {
		t.Fatalf("attempts = %+v, want the panicking item's failure", fs.attempts)
	}
	if strings.Contains(fs.attempts[0].LastError, salvagePATok) {
		t.Errorf("panic last_error carries the PAT: %q", fs.attempts[0].LastError)
	}
	if len(fs.promoted) != 1 || fs.promoted[0].RunID != good.RunID {
		t.Errorf("promoted = %+v, want the next item to still run", fs.promoted)
	}
}

// TestSweepSalvagePanicAtCapStaysUncapped: a panic in the capping attempt's cleanup
// (attempts = cap-1) is recorded with salvageNoCap, so the row is neither capped nor
// settled while its orphan cleanup is unproven.
func TestSweepSalvagePanicAtCapStaysUncapped(t *testing.T) {
	row := pendingSalvageRow("github")
	row.Attempts = salvageAttemptCap - 1
	fs := &salvageStore{duePending: []store.RunSalvage{row}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	b.panicOn[row.RunID] = true
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(b.deletes) != 1 || len(b.creates) != 0 {
		t.Fatalf("deletes=%d creates=%d, want the panicking cleanup only", len(b.deletes), len(b.creates))
	}
	if len(fs.attempts) != 1 || fs.attempts[0].AttemptCap != salvageNoCap || !strings.Contains(fs.attempts[0].LastError, "panic") {
		t.Fatalf("attempts = %+v, want one uncapped panic attempt", fs.attempts)
	}
	if strings.Contains(fs.attempts[0].LastError, salvagePATok) {
		t.Errorf("panic last_error carries the PAT: %q", fs.attempts[0].LastError)
	}
	if len(fs.settled)+len(fs.expireFailed)+len(fs.promoted) != 0 {
		t.Errorf("settled=%+v expireFailed=%d promoted=%d, want nothing settled", fs.settled, len(fs.expireFailed), len(fs.promoted))
	}
}

// TestSweepSalvageExpiryPanicUsesExpiryRecorder: a panic in an expiry item is recorded
// through RecordSalvageExpireFailed (the row keeps its state and pointer), never through
// the pending-attempt recorder, which would not match a promoted row.
func TestSweepSalvageExpiryPanicUsesExpiryRecorder(t *testing.T) {
	row := promotedSalvageRow("github")
	fs := &salvageStore{dueExpiry: []store.RunSalvage{row}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	b.panicOn[row.RunID] = true
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(fs.expireFailed) != 1 || fs.expireFailed[0].RunID != row.RunID || !strings.Contains(fs.expireFailed[0].LastError, "panic") {
		t.Fatalf("expireFailed = %+v, want the panic recorded as an expiry failure", fs.expireFailed)
	}
	if strings.Contains(fs.expireFailed[0].LastError, salvagePATok) {
		t.Errorf("panic last_error carries the PAT: %q", fs.expireFailed[0].LastError)
	}
	if len(fs.attempts)+len(fs.settled) != 0 {
		t.Errorf("attempts=%+v settled=%+v, want neither", fs.attempts, fs.settled)
	}
}

// TestSweepSalvageErrorBounded: a huge broker error is cut to 512 runes before it is
// persisted.
func TestSweepSalvageErrorBounded(t *testing.T) {
	row := pendingSalvageRow("github")
	fs := &salvageStore{duePending: []store.RunSalvage{row}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	b.createErr[row.RunID] = errors.New(strings.Repeat("é", 2000))
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(fs.attempts) != 1 || utf8.RuneCountInString(fs.attempts[0].LastError) > 512 || !utf8.ValidString(fs.attempts[0].LastError) {
		t.Fatalf("last_error not bounded to 512 valid runes: %d", utf8.RuneCountInString(fs.attempts[0].LastError))
	}
}

// TestSalvageErrTextSanitizes: last_error never carries a terminal escape, a bidi
// override, a NUL/C1 control or invalid UTF-8 (which Postgres rejects), and the PAT is
// still redacted.
func TestSalvageErrTextSanitizes(t *testing.T) {
	in := "\x1b[31mred\x1b[0m \u202Eevil\u202C nul\x00byte \xff\xfe bad c1\u009b2J iso\u2066x\u2069 " +
		"line1\nline2 pat=" + salvagePATok
	check := func(t *testing.T, got string) {
		t.Helper()
		if !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
			t.Fatalf("not valid NUL-free UTF-8: %q", got)
		}
		for _, r := range got {
			if unicode.IsControl(r) || isBidiControl(r) {
				t.Fatalf("control/bidi rune %U survived in %q", r, got)
			}
		}
		for _, want := range []string{"red", "evil", "nul", "byte", "bad", "c1", "iso", "line1 line2"} {
			if !strings.Contains(got, want) {
				t.Errorf("%q lost the visible text %q", got, want)
			}
		}
		if strings.Contains(got, salvagePATok) {
			t.Errorf("PAT survived: %q", got)
		}
	}
	t.Run("salvageErrText", func(t *testing.T) { check(t, salvageErrText(errors.New(in), salvagePATok)) })
	t.Run("persisted", func(t *testing.T) {
		row := pendingSalvageRow("github")
		fs := &salvageStore{duePending: []store.RunSalvage{row}}
		svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
		b.createErr[row.RunID] = errors.New(in)
		if _, err := svc.SweepSalvage(context.Background()); err != nil {
			t.Fatalf("SweepSalvage: %v", err)
		}
		if len(fs.attempts) != 1 {
			t.Fatalf("attempts = %d, want 1", len(fs.attempts))
		}
		check(t, fs.attempts[0].LastError)
	})
}

// TestSalvageErrTextSanitizesBeforeScrub: a credential split by a character the sanitizer
// drops (NUL, a bidi control) is rejoined BEFORE the PAT replace and secretscrub run, so it
// comes out redacted instead of being reassembled after them. The fixtures are built from
// fragments, never a full token literal.
func TestSalvageErrTextSanitizesBeforeScrub(t *testing.T) {
	head, tail := "abcdefghij", "KLMNOPQRSTUVWXYZ0123456789"
	for _, tc := range []struct {
		name, in string
		gone     []string
	}{
		{"classic token split by NUL", "push failed: token=" + "ghp_" + head + "\x00" + tail + " rejected", []string{head + tail, tail}},
		{"classic token split by RLO", "push failed: token=" + "ghp_" + head + "\u202E" + tail, []string{head + tail, tail}},
		{"item PAT split by NUL", "auth " + salvagePATok[:9] + "\x00" + salvagePATok[9:] + " denied", []string{salvagePATok, salvagePATok[9:]}},
		{"item PAT split by LRI", "auth " + salvagePATok[:9] + "\u2066" + salvagePATok[9:], []string{salvagePATok, salvagePATok[9:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := salvageErrText(errors.New(tc.in), salvagePATok)
			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Errorf("%q survived in %q", g, got)
				}
			}
			if !strings.Contains(got, "push failed") && !strings.Contains(got, "auth") {
				t.Errorf("lost the surrounding text: %q", got)
			}
		})
	}
	if got := salvageErrText(errors.New("a\u2028b\u2029c"), ""); got != "a b c" {
		t.Errorf("line/paragraph separators = %q, want %q", got, "a b c")
	}
}

// TestSweepSalvageDueListErrorIsReturned: a DB error reading the due lists is returned.
func TestSweepSalvageDueListErrorIsReturned(t *testing.T) {
	fs := &salvageStore{dueErr: errors.New("db down")}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	n, err := svc.SweepSalvage(context.Background())
	if err == nil || n != 0 || len(b.creates)+len(b.deletes) != 0 {
		t.Fatalf("SweepSalvage = (%d, %v), want (0, error) with no broker call", n, err)
	}
}

// TestSalvageSourceNeverDeletesCheckpointRefs is the static half of "salvage never
// deletes or moves a ref #1810 manages". salvage.go:
//
//   - references none of the branch checkpoint deleters or #1810's own seams
//     (deleteCheckpointFn, deleteCheckpointBestEffort, retainOrDeleteCheckpoint,
//     createRefFn, listRefTipsFn), and calls no pushbroker primitive directly
//     (pushbroker.Delete, CreateRef, ListRefTips), only its own seams;
//   - builds a pushbroker.DeleteOptions only as the literal argument of a
//     salvageDeleteRefFn call, with Ref = pushbroker.SalvageRef(...), a non-empty
//     ExpectedOldTip and no Branch (Delete refuses a salvage Ref without a tip, too);
//   - builds a pushbroker.CreateRefOptions only with Ref = pushbroker.SalvageRef(...).
//
// Comments are ignored (the AST carries identifiers only).
func TestSalvageSourceNeverDeletesCheckpointRefs(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "salvage.go", nil, 0)
	if err != nil {
		t.Fatalf("parse salvage.go: %v", err)
	}
	forbidden := map[string]bool{
		"deleteCheckpointFn": true, "deleteCheckpointBestEffort": true, "retainOrDeleteCheckpoint": true,
		"createRefFn": true, "listRefTipsFn": true,
	}
	isPB := func(e ast.Expr, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "pushbroker" && sel.Sel.Name == name
	}
	field := func(lit *ast.CompositeLit, name string) (ast.Expr, bool) {
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == name {
					return kv.Value, true
				}
			}
		}
		return nil, false
	}
	isSalvageRefCall := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		return ok && isPB(call.Fun, "SalvageRef")
	}

	// Every salvageDeleteRefFn call must take a DeleteOptions literal; those literals are
	// the only DeleteOptions allowed.
	allowedDelete := map[*ast.CompositeLit]bool{}
	deleteCalls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "salvageDeleteRefFn" {
			return true
		}
		deleteCalls++
		var lit *ast.CompositeLit
		if len(call.Args) == 2 {
			lit, _ = call.Args[1].(*ast.CompositeLit)
		}
		if lit == nil || !isPB(lit.Type, "DeleteOptions") {
			t.Errorf("%s: salvageDeleteRefFn is not called with a pushbroker.DeleteOptions literal", fset.Position(call.Pos()))
			return true
		}
		allowedDelete[lit] = true
		if ref, ok := field(lit, "Ref"); !ok || !isSalvageRefCall(ref) {
			t.Errorf("%s: a salvage Delete's Ref is not pushbroker.SalvageRef(...)", fset.Position(lit.Pos()))
		}
		if tip, ok := field(lit, "ExpectedOldTip"); !ok {
			t.Errorf("%s: a salvage Delete has no ExpectedOldTip (it must be a CAS)", fset.Position(lit.Pos()))
		} else if bl, ok := tip.(*ast.BasicLit); ok && bl.Value == `""` {
			t.Errorf("%s: a salvage Delete's ExpectedOldTip is empty", fset.Position(lit.Pos()))
		}
		if _, ok := field(lit, "Branch"); ok {
			t.Errorf("%s: a salvage Delete names a Branch (the branch checkpoint ref)", fset.Position(lit.Pos()))
		}
		return true
	})
	if deleteCalls == 0 {
		t.Fatal("found no salvageDeleteRefFn call in salvage.go; the guard is not looking at the right code")
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if forbidden[x.Name] {
				t.Errorf("%s: salvage.go references %s", fset.Position(x.Pos()), x.Name)
			}
		case *ast.SelectorExpr:
			for _, name := range []string{"Delete", "CreateRef", "ListRefTips"} {
				if isPB(x, name) {
					t.Errorf("%s: salvage.go calls pushbroker.%s directly instead of its seam", fset.Position(x.Pos()), name)
				}
			}
		case *ast.CompositeLit:
			switch {
			case isPB(x.Type, "DeleteOptions") && !allowedDelete[x]:
				t.Errorf("%s: a pushbroker.DeleteOptions outside a salvageDeleteRefFn call", fset.Position(x.Pos()))
			case isPB(x.Type, "CreateRefOptions"):
				if ref, ok := field(x, "Ref"); !ok || !isSalvageRefCall(ref) {
					t.Errorf("%s: a CreateRefOptions Ref is not pushbroker.SalvageRef(...)", fset.Position(x.Pos()))
				}
			}
		}
		return true
	})
	// A DeleteOptions value declared any other way (var, conversion) has no literal to check.
	ast.Inspect(f, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && vs.Type != nil && isPB(vs.Type, "DeleteOptions") {
			t.Errorf("%s: a pushbroker.DeleteOptions variable in salvage.go", fset.Position(vs.Pos()))
		}
		return true
	})
}

// TestCreateSalvageRefMapsOutcomes pins createSalvageRef on #1810's primitives: the list
// decides idempotent success, refusal, unavailability and the source (branch checkpoint
// ref first, then refs/uzi-recovery/<run-id>) with no write; CreateRef's sentinels map to
// created (nil, ErrRefExistsAtTip), refused (ErrRefExists) and unavailable
// (ErrSourceMissing: the source moved after the list), and anything else is a failure.
func TestCreateSalvageRefMapsOutcomes(t *testing.T) {
	other := errors.New("pushbroker: create ref: ng refs/uzi-salvage/x 500")
	for _, tc := range []struct {
		name       string
		tips       map[string]string // list result, keyed by "salvage", "branch", "recovery"
		listErr    error
		createErr  error
		want       salvageResult
		wantErr    bool
		wantSource string // "" = no create
	}{
		{"salvage at tip", map[string]string{"salvage": salvageTip}, nil, nil, salvageCreated, false, ""},
		{"salvage at another tip", map[string]string{"salvage": salvageOtherTip, "branch": salvageTip}, nil, nil, salvageRefused, false, ""},
		{"no source", map[string]string{}, nil, nil, salvageUnavailable, false, ""},
		{"sources elsewhere", map[string]string{"branch": salvageOtherTip, "recovery": salvageOtherTip}, nil, nil, salvageUnavailable, false, ""},
		{"list fails", nil, errors.New("pushbroker: list: 503"), nil, salvageFailed, true, ""},
		{"branch source", map[string]string{"branch": salvageTip, "recovery": salvageTip}, nil, nil, salvageCreated, false, "branch"},
		{"recovery source", map[string]string{"branch": salvageOtherTip, "recovery": salvageTip}, nil, nil, salvageCreated, false, "recovery"},
		{"created by a racer", map[string]string{"branch": salvageTip}, nil, pushbroker.ErrRefExistsAtTip, salvageCreated, false, "branch"},
		{"a racer at another tip", map[string]string{"branch": salvageTip}, nil, pushbroker.ErrRefExists, salvageRefused, false, "branch"},
		{"source moved after the list", map[string]string{"branch": salvageTip}, nil, pushbroker.ErrSourceMissing, salvageUnavailable, false, "branch"},
		{"create fails", map[string]string{"branch": salvageTip}, nil, other, salvageFailed, true, "branch"},
		{"invalid ref", map[string]string{"branch": salvageTip}, nil, pushbroker.ErrInvalidRef, salvageFailed, true, "branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			refs := map[string]string{
				"salvage":  pushbroker.SalvageRef(row.RunID),
				"branch":   checkpointRefPrefix + row.Branch,
				"recovery": pushbroker.RecoveryRefPrefix + row.RunID.String(),
			}
			svc := New(&salvageStore{}, nil, testParams())
			var listed []string
			var creates []pushbroker.CreateRefOptions
			svc.salvageListRefTipsFn = func(_ context.Context, _ pushbroker.ListRefsOptions, rs ...string) (map[string]string, error) {
				listed = rs
				out := map[string]string{}
				for k, v := range tc.tips {
					out[refs[k]] = v
				}
				return out, tc.listErr
			}
			svc.salvageCreateRefFn = func(_ context.Context, o pushbroker.CreateRefOptions) error {
				creates = append(creates, o)
				return tc.createErr
			}
			svc.salvageDeleteRefFn = func(context.Context, pushbroker.DeleteOptions) error {
				t.Error("createSalvageRef deleted a ref")
				return nil
			}
			res, err := svc.createSalvageRef(context.Background(), retentionForge{cloneURL: "https://x/r.git"}, row)
			if res != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("createSalvageRef = (%v, %v), want (%v, err=%v)", res, err, tc.want, tc.wantErr)
			}
			if strings.Join(listed, " ") != strings.Join([]string{refs["salvage"], refs["branch"], refs["recovery"]}, " ") {
				t.Errorf("listed %v, want the salvage, branch and recovery refs", listed)
			}
			if tc.wantSource == "" {
				if len(creates) != 0 {
					t.Fatalf("creates = %+v, want none", creates)
				}
				return
			}
			if len(creates) != 1 || creates[0].SourceRef != refs[tc.wantSource] || creates[0].Ref != refs["salvage"] || creates[0].Tip != row.Tip {
				t.Fatalf("creates = %+v, want one salvage create at the tip from %s", creates, refs[tc.wantSource])
			}
		})
	}
}

// TestDeleteSalvageRefConfirmsWithRelist: pushbroker.Delete's nil also covers some
// lock-failure refusals it reads as benign, so deleteSalvageRef re-lists. A salvage ref
// still at the tip after a nil Delete is an error (the row is retried, never settled); a
// ref that is gone or moved is success (we owned only our tip); a failed confirm list is
// an error.
func TestDeleteSalvageRefConfirmsWithRelist(t *testing.T) {
	for _, tc := range []struct {
		name    string
		after   string // the salvage ref's tip after the Delete ("" = absent)
		listErr error
		wantErr string
	}{
		{"deleted", "", nil, ""},
		{"moved by someone else", salvageOtherTip, nil, ""},
		{"benign refusal left it at the tip", salvageTip, nil, "still at"},
		{"confirm list fails", "", errors.New("pushbroker: list: 503"), "confirm delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			ref := pushbroker.SalvageRef(row.RunID)
			svc := New(&salvageStore{}, nil, testParams())
			var deletes []pushbroker.DeleteOptions
			svc.salvageDeleteRefFn = func(_ context.Context, o pushbroker.DeleteOptions) error {
				deletes = append(deletes, o)
				return nil
			}
			svc.salvageListRefTipsFn = func(_ context.Context, _ pushbroker.ListRefsOptions, rs ...string) (map[string]string, error) {
				if len(rs) != 1 || rs[0] != ref {
					t.Errorf("confirm listed %v, want only %s", rs, ref)
				}
				out := map[string]string{}
				if tc.after != "" {
					out[ref] = tc.after
				}
				return out, tc.listErr
			}
			err := svc.deleteSalvageRef(context.Background(), retentionForge{cloneURL: "https://x/r.git"}, row)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("deleteSalvageRef = %v, want error containing %q", err, tc.wantErr)
			}
			if len(deletes) != 1 || deletes[0].Ref != ref || deletes[0].ExpectedOldTip != row.Tip || deletes[0].Branch != "" {
				t.Fatalf("deletes = %+v, want one CAS delete of %s at the tip", deletes, ref)
			}
		})
	}
}

// TestSweepSalvageBenignDeleteRefusalIsRetried: through the whole pass, a nil Delete that
// left the salvage ref at the tip keeps an expiring row un-expired (its failure recorded
// for retry), and keeps a rolled-back pending row from settling 'disabled'.
func TestSweepSalvageBenignDeleteRefusalIsRetried(t *testing.T) {
	exp := promotedSalvageRow("github")
	pend := pendingSalvageRow("gitlab") // gitlab is not enabled: the rollback path
	fs := &salvageStore{dueExpiry: []store.RunSalvage{exp}, duePending: []store.RunSalvage{pend}}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	b.salvage[exp.RunID], b.salvage[pend.RunID] = exp.Tip, pend.Tip
	b.deleteNoop = true
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(fs.settled) != 0 {
		t.Fatalf("settled = %+v, want nothing settled while the salvage refs remain", fs.settled)
	}
	if len(fs.expireFailed) != 1 || !strings.Contains(fs.expireFailed[0].LastError, "still at") {
		t.Errorf("expireFailed = %+v, want the expiry retried", fs.expireFailed)
	}
	if len(fs.attempts) != 1 || fs.attempts[0].AttemptCap != salvageNoCap || !strings.Contains(fs.attempts[0].LastError, "still at") {
		t.Errorf("attempts = %+v, want one uncapped rollback attempt", fs.attempts)
	}
}
