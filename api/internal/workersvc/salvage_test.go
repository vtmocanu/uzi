package workersvc

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
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

// salvageBroker fakes CreateSalvageRef and DeleteRef. order logs every call as
// "create:<run>" / "delete:<run>" so a test can check the round-robin.
type salvageBroker struct {
	t *testing.T

	mu        sync.Mutex
	result    map[uuid.UUID]pushbroker.SalvageResult
	createErr map[uuid.UUID]error
	panicOn   map[uuid.UUID]bool
	block     bool
	// blockRes: a blocked create returns its configured result once ctx is done (the
	// forge applied it but the reply arrived after the pass budget), not a failure.
	blockRes  bool
	deleteErr error
	creates   []pushbroker.CreateSalvageRefOptions
	deletes   []pushbroker.DeleteRefOptions
	order     []string
}

func (b *salvageBroker) create(ctx context.Context, o pushbroker.CreateSalvageRefOptions) (pushbroker.SalvageResult, error) {
	b.mu.Lock()
	b.creates = append(b.creates, o)
	b.order = append(b.order, "create:"+o.RunID.String())
	res, err, boom, block, blockRes := b.result[o.RunID], b.createErr[o.RunID], b.panicOn[o.RunID], b.block, b.blockRes
	b.mu.Unlock()
	if boom {
		panic("go-git nil deref for " + o.RunID.String() + " via " + o.PAT)
	}
	if block {
		<-ctx.Done()
		if blockRes {
			return res, err
		}
		return pushbroker.SalvageFailed, ctx.Err()
	}
	return res, err
}

func (b *salvageBroker) delete(_ context.Context, o pushbroker.DeleteRefOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !strings.HasPrefix(o.Ref, "refs/uzi-salvage/") {
		b.t.Errorf("DeleteRef called with %q; salvage may only delete refs/uzi-salvage/*", o.Ref)
	}
	b.deletes = append(b.deletes, o)
	b.order = append(b.order, "delete:"+strings.TrimPrefix(o.Ref, "refs/uzi-salvage/"))
	return b.deleteErr
}

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
		t:         t,
		result:    map[uuid.UUID]pushbroker.SalvageResult{},
		createErr: map[uuid.UUID]error{},
		panicOn:   map[uuid.UUID]bool{},
	}
	svc.createSalvageFn = b.create
	svc.deleteSalvageFn = b.delete
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

// TestSweepSalvagePendingTransitions pins each CreateSalvageRef outcome's store write.
func TestSweepSalvagePendingTransitions(t *testing.T) {
	leak := "remote https://uzi-bot:" + salvagePATok + "@github.example.com failed"
	cases := []struct {
		name      string
		res       pushbroker.SalvageResult
		err       error
		settled   string
		attempted bool
		created   bool
	}{
		{"created", pushbroker.SalvageCreated, nil, "", false, true},
		{"unavailable", pushbroker.SalvageUnavailable, nil, "unavailable", false, false},
		{"refused", pushbroker.SalvageRefused, nil, "refused", false, false},
		{"failed with error", pushbroker.SalvageFailed, errors.New(leak), "", true, false},
		{"failed without error", pushbroker.SalvageFailed, nil, "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			fs := &salvageStore{duePending: []store.RunSalvage{row}}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			b.result[row.RunID], b.createErr[row.RunID] = tc.res, tc.err

			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			if len(b.creates) != 1 {
				t.Fatalf("creates = %d, want 1", len(b.creates))
			}
			o := b.creates[0]
			if o.CloneURL != "https://github.example.com/team/repo.git" || o.Branch != row.Branch || o.Tip != row.Tip ||
				o.RunID != row.RunID || o.Username != "uzi-bot" || o.PAT != salvagePATok {
				t.Errorf("create options = %+v, want the server-derived remote, the row's branch/tip and the decrypted PAT", o)
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
		b.result[row.RunID] = pushbroker.SalvageCreated
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
			b.result[row.RunID] = pushbroker.SalvageCreated

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
// settle 'failed', dropping the live pointer) first CAS-deletes the run's salvage ref at the
// tip; the cap applies only once that succeeds. A failed cleanup records the attempt
// uncapped with both errors; an attempt below the cap makes no delete.
func TestSweepSalvageAttemptCapCleansUpFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		attempts  int32
		deleteErr error
		wantDel   int
		wantCap   int32
	}{
		{"below cap", salvageAttemptCap - 2, nil, 0, salvageAttemptCap},
		{"at cap, cleanup ok", salvageAttemptCap - 1, nil, 1, salvageAttemptCap},
		{"past cap, cleanup ok", salvageAttemptCap + 3, nil, 1, salvageAttemptCap},
		{"at cap, cleanup fails", salvageAttemptCap - 1, errors.New("https://uzi-bot:" + salvagePATok + "@x: 503"), 1, salvageNoCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := pendingSalvageRow("github")
			row.Attempts = tc.attempts
			fs := &salvageStore{duePending: []store.RunSalvage{row}}
			svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
			b.createErr[row.RunID] = errors.New("create: 500")
			b.deleteErr = tc.deleteErr
			if _, err := svc.SweepSalvage(context.Background()); err != nil {
				t.Fatalf("SweepSalvage: %v", err)
			}
			if len(b.deletes) != tc.wantDel {
				t.Fatalf("deletes = %d, want %d", len(b.deletes), tc.wantDel)
			}
			if tc.wantDel == 1 && (b.deletes[0].Ref != pushbroker.SalvageRef(row.RunID) || b.deletes[0].ExpectedOldTip != row.Tip) {
				t.Errorf("delete = %+v, want the run's salvage ref at the tip", b.deletes[0])
			}
			if strings.Join(b.order, " ") != "create:"+row.RunID.String()+strings.Repeat(" delete:"+row.RunID.String(), tc.wantDel) {
				t.Errorf("broker order = %v, want the create, then any cleanup delete", b.order)
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
	b.result[row.RunID] = pushbroker.SalvageCreated
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
			t.Errorf("DeleteRef options = %+v, want the run's salvage ref at the recorded tip", d)
		}
		if len(fs.settled) != 1 || fs.settled[0].State != "expired" {
			t.Errorf("settled = %+v, want expired", fs.settled)
		}
		if len(b.creates) != 0 {
			t.Errorf("expiry called CreateSalvageRef")
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

// TestSweepSalvageClaimContextErrorIsAttemptFailure: a claim-context read failure counts
// as a failed attempt, not a pass error.
func TestSweepSalvageClaimContextErrorIsAttemptFailure(t *testing.T) {
	row := pendingSalvageRow("github")
	fs := &salvageStore{duePending: []store.RunSalvage{row}, claimErr: errors.New("no rows in result set")}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	if len(fs.attempts) != 1 || len(b.creates) != 0 {
		t.Fatalf("attempts=%d creates=%d, want 1/0", len(fs.attempts), len(b.creates))
	}
}

// TestSweepSalvageRoundRobinAtMostFive: with 4 due expiries and 4 due pending rows the
// pass handles exactly 5, alternating expiry and pending, expiry first.
func TestSweepSalvageRoundRobinAtMostFive(t *testing.T) {
	var exp, pend []store.RunSalvage
	for range 4 {
		exp = append(exp, promotedSalvageRow("github"))
		pend = append(pend, pendingSalvageRow("github"))
	}
	fs := &salvageStore{dueExpiry: exp, duePending: pend}
	svc, b := newSalvageSvc(t, fs, []string{"github"}, 168*time.Hour)
	for _, r := range pend {
		b.result[r.RunID] = pushbroker.SalvageUnavailable
	}
	if _, err := svc.SweepSalvage(context.Background()); err != nil {
		t.Fatalf("SweepSalvage: %v", err)
	}
	want := []string{
		"delete:" + exp[0].RunID.String(), "create:" + pend[0].RunID.String(),
		"delete:" + exp[1].RunID.String(), "create:" + pend[1].RunID.String(),
		"delete:" + exp[2].RunID.String(),
	}
	if strings.Join(b.order, " ") != strings.Join(want, " ") {
		t.Fatalf("broker order =\n  %v\nwant\n  %v", b.order, want)
	}
	if fs.expiryParams[0].Lim != salvageMaxItems || fs.pendingParams[0].Lim != salvageMaxItems {
		t.Errorf("due-list limits = (%d, %d), want %d each", fs.expiryParams[0].Lim, fs.pendingParams[0].Lim, salvageMaxItems)
	}
}

// TestSweepSalvageRoundRobinOneSided: only pending rows still yields at most five.
func TestSweepSalvageRoundRobinOneSided(t *testing.T) {
	var pend []store.RunSalvage
	for range 7 {
		pend = append(pend, pendingSalvageRow("github"))
	}
	got := roundRobinSalvage(nil, pend, salvageMaxItems)
	if len(got) != salvageMaxItems {
		t.Fatalf("items = %d, want %d", len(got), salvageMaxItems)
	}
	for i, it := range got {
		if it.expiry || it.row.RunID != pend[i].RunID {
			t.Fatalf("item %d = %+v, want pending row %d in order", i, it, i)
		}
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
	b.result[good.RunID] = pushbroker.SalvageCreated
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
// deletes or moves a ref #1810 manages": salvage.go references none of the branch
// checkpoint deleters (deleteCheckpointFn, deleteCheckpointBestEffort, pushbroker.Delete,
// pushbroker.DeleteOptions), and its only pushbroker delete is DeleteRef on SalvageRef.
// Comments are ignored (the AST carries identifiers only).
func TestSalvageSourceNeverDeletesCheckpointRefs(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "salvage.go", nil, 0)
	if err != nil {
		t.Fatalf("parse salvage.go: %v", err)
	}
	forbidden := map[string]bool{"deleteCheckpointFn": true, "deleteCheckpointBestEffort": true, "retainOrDeleteCheckpoint": true}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if forbidden[x.Name] {
				t.Errorf("%s: salvage.go references %s", fset.Position(x.Pos()), x.Name)
			}
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "pushbroker" &&
				(x.Sel.Name == "Delete" || x.Sel.Name == "DeleteOptions") {
				t.Errorf("%s: salvage.go uses pushbroker.%s (the branch checkpoint deleter)", fset.Position(x.Pos()), x.Sel.Name)
			}
		case *ast.CompositeLit:
			// Every DeleteRefOptions literal names its Ref via pushbroker.SalvageRef.
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DeleteRefOptions" {
				return true
			}
			found := false
			for _, e := range x.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Ref" {
					continue
				}
				if call, ok := kv.Value.(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.SelectorExpr); ok && fn.Sel.Name == "SalvageRef" {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("%s: a DeleteRefOptions Ref is not pushbroker.SalvageRef(...)", fset.Position(x.Pos()))
			}
		}
		return true
	})
}
