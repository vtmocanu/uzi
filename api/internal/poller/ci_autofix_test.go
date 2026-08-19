package poller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Shared identity for the fake repo/owner every CI-autofix test detects against.
var (
	cfRepoID = uuid.New()
	cfUserID = uuid.New()
)

const cfRef = "agent/issue-7"

// ── fakes ────────────────────────────────────────────────────────────────────

// cfStore is an in-memory ciAutofixStore: a candidate list, an attempt ledger keyed
// by ref, and an active-fix-target map keyed by ref. It records every write so a
// test can assert the exact ledger transition.
type cfStore struct {
	candidates []store.ListCIAutofixCandidateRefsRow
	candErr    error

	attempts map[string]store.CiAutofixAttempt

	// activeTarget[ref] = the target pipeline id of an active ci_fix run on the ref.
	// A present entry means GetActiveCIFixTargetForRef returns it; absent → ErrNoRows.
	activeTarget map[string]pgtype.Int8

	upserts    []store.UpsertCIAutofixAttemptParams
	records    []store.RecordCIAutofixPipelineParams
	haltSets   []store.SetCIAutofixHaltNotifiedParams
	getErr     error
	activeErr  error
	upsertErr  error
	recordErr  error
	haltSetErr error

	ops *[]string
}

func (s *cfStore) ListCIAutofixCandidateRefs(context.Context, uuid.UUID) ([]store.ListCIAutofixCandidateRefsRow, error) {
	return s.candidates, s.candErr
}

func (s *cfStore) GetCIAutofixAttempt(_ context.Context, arg store.GetCIAutofixAttemptParams) (store.CiAutofixAttempt, error) {
	if s.getErr != nil {
		return store.CiAutofixAttempt{}, s.getErr
	}
	if a, ok := s.attempts[arg.Ref]; ok {
		return a, nil
	}
	// Mirror the generated :one — a zero-value row alongside ErrNoRows.
	return store.CiAutofixAttempt{}, pgx.ErrNoRows
}

func (s *cfStore) UpsertCIAutofixAttempt(_ context.Context, arg store.UpsertCIAutofixAttemptParams) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts = append(s.upserts, arg)
	if s.attempts == nil {
		s.attempts = map[string]store.CiAutofixAttempt{}
	}
	cur := s.attempts[arg.Ref]
	cur.RepoID = arg.RepoID
	cur.Ref = arg.Ref
	cur.AttemptCount++ // INSERT count=1 or increment
	cur.LastSignature = arg.LastSignature
	cur.LastPipelineID = arg.LastPipelineID
	cur.HaltNotified = false // mirror the SQL ON CONFLICT: a proceed resets the halt latch
	s.attempts[arg.Ref] = cur
	if s.ops != nil {
		*s.ops = append(*s.ops, "upsert")
	}
	return nil
}

func (s *cfStore) RecordCIAutofixPipeline(_ context.Context, arg store.RecordCIAutofixPipelineParams) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.records = append(s.records, arg)
	if s.attempts == nil {
		s.attempts = map[string]store.CiAutofixAttempt{}
	}
	cur := s.attempts[arg.Ref]
	cur.RepoID = arg.RepoID
	cur.Ref = arg.Ref
	cur.LastPipelineID = arg.LastPipelineID // counter untouched
	s.attempts[arg.Ref] = cur
	if s.ops != nil {
		*s.ops = append(*s.ops, "record")
	}
	return nil
}

func (s *cfStore) SetCIAutofixHaltNotified(_ context.Context, arg store.SetCIAutofixHaltNotifiedParams) error {
	if s.haltSetErr != nil {
		return s.haltSetErr
	}
	s.haltSets = append(s.haltSets, arg)
	if s.attempts == nil {
		s.attempts = map[string]store.CiAutofixAttempt{}
	}
	cur := s.attempts[arg.Ref]
	cur.RepoID = arg.RepoID
	cur.Ref = arg.Ref
	cur.HaltNotified = true
	cur.LastPipelineID = arg.LastPipelineID
	s.attempts[arg.Ref] = cur
	if s.ops != nil {
		*s.ops = append(*s.ops, "halt")
	}
	return nil
}

func (s *cfStore) GetActiveCIFixTargetForRef(_ context.Context, arg store.GetActiveCIFixTargetForRefParams) (pgtype.Int8, error) {
	if s.activeErr != nil {
		return pgtype.Int8{}, s.activeErr
	}
	if t, ok := s.activeTarget[arg.Ref.String]; ok {
		return t, nil
	}
	return pgtype.Int8{}, pgx.ErrNoRows
}

type cfRunCall struct {
	userID, repoID   uuid.UUID
	ref, title, desc string
	snapshot         workersvc.FailureSnapshot
	ciConfigPaths    []string
}

type cfRuns struct {
	err   error
	calls []cfRunCall
	runID uuid.UUID
	ops   *[]string
}

func (r *cfRuns) CreateAutoCIFixRun(_ context.Context, userID, repoID uuid.UUID, ref, title, desc string, snap workersvc.FailureSnapshot, ciConfigPaths []string) (store.Run, error) {
	r.calls = append(r.calls, cfRunCall{userID, repoID, ref, title, desc, snap, ciConfigPaths})
	if r.ops != nil {
		*r.ops = append(*r.ops, "create")
	}
	if r.err != nil {
		return store.Run{}, r.err
	}
	if r.runID == (uuid.UUID{}) {
		r.runID = uuid.New()
	}
	return store.Run{ID: r.runID}, nil
}

type cfNotifyCall struct {
	kind    string
	userID  uuid.UUID
	runID   *uuid.UUID
	payload notifysvc.CIAutofixPayload
}

type cfNotifier struct {
	calls []cfNotifyCall
	err   error
	ops   *[]string
}

func (n *cfNotifier) Notify(_ context.Context, note notifysvc.Notification) (store.Notification, error) {
	p, _ := note.Payload.(notifysvc.CIAutofixPayload)
	n.calls = append(n.calls, cfNotifyCall{note.Kind, note.UserID, note.RunID, p})
	if n.ops != nil {
		*n.ops = append(*n.ops, "notify")
	}
	if n.err != nil {
		return store.Notification{}, n.err
	}
	return store.Notification{ID: uuid.New()}, nil
}

// cfForge extends the autopilot test's apForge shape with the pipeline reads the
// CI-autofix snapshot needs. Reusing apForge's method set would not give us
// controllable ListPipelineJobs/JobLogTail, so this is a focused fake.
type cfForge struct {
	jobs      []forge.Job
	logTail   string
	jobsErr   error
	configErr error
	notes     []apNote
	noteErr   error
	ops       *[]string
}

func (f *cfForge) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return f.jobs, f.jobsErr
}
func (f *cfForge) JobLogTail(context.Context, int64, int64, int) (string, error) {
	return f.logTail, nil
}
func (f *cfForge) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", f.configErr
}
func (f *cfForge) CreateIssueNote(_ context.Context, _, iid int64, body string) (forge.IssueNote, error) {
	if f.noteErr != nil {
		return forge.IssueNote{}, f.noteErr
	}
	f.notes = append(f.notes, apNote{iid, body})
	if f.ops != nil {
		*f.ops = append(*f.ops, "comment")
	}
	return forge.IssueNote{ID: 1, Body: body}, nil
}

// Unused-by-ci-autofix interface methods, stubbed to satisfy forge.Forge.
func (f *cfForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{}, nil
}
func (f *cfForge) ListProjects(context.Context) ([]forge.Project, error)    { return nil, nil }
func (f *cfForge) ListLabels(context.Context, int64) ([]forge.Label, error) { return nil, nil }
func (f *cfForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }
func (f *cfForge) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *cfForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *cfForge) UpdateIssueDescription(context.Context, int64, int64, string) error { return nil }
func (f *cfForge) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return nil
}
func (f *cfForge) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, nil
}
func (f *cfForge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, nil
}
func (f *cfForge) UserExists(context.Context, string) (bool, error) { return false, nil }
func (f *cfForge) GetMergeRequest(context.Context, int64, int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{}, nil
}
func (f *cfForge) TokenInfo(context.Context) (forge.TokenInfo, error) { return forge.TokenInfo{}, nil }
func (f *cfForge) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleNone, false, nil
}
func (f *cfForge) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{}, nil
}
func (f *cfForge) LatestPipeline(context.Context, int64, string) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *cfForge) LatestMRPipeline(context.Context, int64, int64) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *cfForge) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return nil, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func cfRepoRow() store.ListEnabledReposWithConnectionsRow {
	return store.ListEnabledReposWithConnectionsRow{
		ID:                cfRepoID,
		ForgeProjectID:    42,
		PathWithNamespace: "grp/proj",
	}
}

func cfCand(pipelineID int64) store.ListCIAutofixCandidateRefsRow {
	return store.ListCIAutofixCandidateRefsRow{
		Ref:            pgtype.Text{String: cfRef, Valid: true},
		MrIid:          pgtype.Int8{Int64: 101, Valid: true},
		UserID:         cfUserID,
		PipelineID:     pipelineID,
		PipelineWebUrl: "https://forge/grp/proj/-/pipelines/" + strconv.FormatInt(pipelineID, 10),
		Sha:            "deadbeef",
	}
}

// A single failing job with a stable log tail. Two snapshots built from the same
// job bytes normalize to the same FailureSignature, which drives the no-progress test.
func cfJob() forge.Job {
	return forge.Job{ID: 1, Name: "build", Stage: "build", Status: "failed", WebURL: "https://forge/job/1"}
}

func newCF(st *cfStore, runs *cfRuns, notifier *cfNotifier) *CIAutoFix {
	var n CIAutofixNotifier
	if notifier != nil {
		n = notifier
	}
	return NewCIAutoFix(st, runs, n, 5, 4096, 2, []string{".gitlab-ci.yml"})
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCIAutofixProceedStartsRun(t *testing.T) {
	var ops []string
	st := &cfStore{candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9001)}, ops: &ops}
	runs := &cfRuns{ops: &ops}
	notifier := &cfNotifier{ops: &ops}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "panic: boom\nexit 1", ops: &ops}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("CreateAutoCIFixRun calls = %d, want 1", len(runs.calls))
	}
	c := runs.calls[0]
	if c.userID != cfUserID || c.repoID != cfRepoID || c.ref != cfRef {
		t.Fatalf("run call = %+v, want owner/repo/ref", c)
	}
	if len(c.ciConfigPaths) == 0 {
		t.Fatalf("expected the config-path watch set to be threaded, got %v", c.ciConfigPaths)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("expected one UpsertCIAutofixAttempt, got %d", len(st.upserts))
	}
	if got := st.attempts[cfRef].AttemptCount; got != 1 {
		t.Fatalf("attempt count = %d, want 1", got)
	}
	if !st.upserts[0].LastSignature.Valid || st.upserts[0].LastPipelineID.Int64 != 9001 {
		t.Fatalf("upsert did not stamp sig+pipeline: %+v", st.upserts[0])
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0].body, "Automatic CI fix started") {
		t.Fatalf("expected one start comment, got %+v", f.notes)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].kind != "ci_autofix_started" || notifier.calls[0].runID == nil {
		t.Fatalf("expected one started notification anchored to the run, got %+v", notifier.calls)
	}
	// create → upsert → comment → notify.
	if strings.Join(ops, ",") != "create,upsert,comment,notify" {
		t.Fatalf("op order = %v, want [create upsert comment notify]", ops)
	}
}

func TestCIAutofixDedupSkipsHandledPipeline(t *testing.T) {
	st := &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9001)},
		attempts: map[string]store.CiAutofixAttempt{
			cfRef: {Ref: cfRef, AttemptCount: 1, LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true}},
		},
	}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 0 || len(st.upserts) != 0 || len(st.records) != 0 || len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("dedup must be a no-op: runs=%d upserts=%d records=%d notes=%d notifs=%d",
			len(runs.calls), len(st.upserts), len(st.records), len(f.notes), len(notifier.calls))
	}
}

func TestCIAutofixCapHalts(t *testing.T) {
	// count == maxAttempts (2) → capped. Fresh pipeline (9010) is not deduped.
	st := &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9010)},
		attempts: map[string]store.CiAutofixAttempt{
			cfRef: {Ref: cfRef, AttemptCount: 2, LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true}},
		},
	}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("capped halt must not start a run, got %d", len(runs.calls))
	}
	if len(st.haltSets) != 1 || st.haltSets[0].LastPipelineID.Int64 != 9010 {
		t.Fatalf("expected SetCIAutofixHaltNotified stamping 9010, got %+v", st.haltSets)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0].body, "attempt limit (2)") {
		t.Fatalf("expected one cap-halt comment, got %+v", f.notes)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].kind != "ci_autofix_halted" || notifier.calls[0].runID != nil {
		t.Fatalf("expected one halted notification with no run anchor, got %+v", notifier.calls)
	}
}

func TestCIAutofixNoProgressHalts(t *testing.T) {
	// One attempt already spent, and its recorded signature equals the CURRENT
	// snapshot's signature (same failing job bytes) → no-progress halt. FailureSignature
	// stability is what makes this deterministic.
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "panic: boom\nexit 1"}
	snap, err := workersvc.BuildFailureSnapshot(context.Background(), f, 42,
		store.PipelineStatus{PipelineID: 9001, Ref: cfRef, Sha: "deadbeef", WebUrl: "x"}, 5, 4096)
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	priorSig := workersvc.FailureSignature(snap)

	st := &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9010)}, // new pipeline, same failure
		attempts: map[string]store.CiAutofixAttempt{
			cfRef: {
				Ref:            cfRef,
				AttemptCount:   1, // below the cap of 2
				LastSignature:  pgtype.Text{String: priorSig, Valid: true},
				LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true},
			},
		},
	}
	runs := &cfRuns{}
	notifier := &cfNotifier{}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 0 {
		t.Fatalf("no-progress halt must not start a run, got %d", len(runs.calls))
	}
	if len(st.haltSets) != 1 {
		t.Fatalf("expected one SetCIAutofixHaltNotified, got %d", len(st.haltSets))
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0].body, "did not change the failure signature") {
		t.Fatalf("expected one no-progress comment, got %+v", f.notes)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].kind != "ci_autofix_halted" {
		t.Fatalf("expected one halted notification, got %+v", notifier.calls)
	}
}

func TestCIAutofixHaltLatchSilent(t *testing.T) {
	// Already halt-notified: a fresh pipeline is silently recorded, no second comment.
	st := &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9020)},
		attempts: map[string]store.CiAutofixAttempt{
			cfRef: {
				Ref:            cfRef,
				AttemptCount:   2,
				HaltNotified:   true,
				LastPipelineID: pgtype.Int8{Int64: 9010, Valid: true},
			},
		},
	}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 0 || len(st.haltSets) != 0 || len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("halt latch must be silent: runs=%d halts=%d notes=%d notifs=%d",
			len(runs.calls), len(st.haltSets), len(f.notes), len(notifier.calls))
	}
	if len(st.records) != 1 || st.records[0].LastPipelineID.Int64 != 9020 {
		t.Fatalf("expected a silent RecordCIAutofixPipeline of 9020, got %+v", st.records)
	}
}

func TestCIAutofixActiveRunSwallowCaps(t *testing.T) {
	// An active fix is in flight, target 9001. The candidate pipeline 9010 is newer,
	// so the recorded id is capped to min(9010, 9001) = 9001 (the fix's own result
	// pipeline is not deduped away).
	st := &cfStore{
		candidates:   []store.ListCIAutofixCandidateRefsRow{cfCand(9010)},
		activeTarget: map[string]pgtype.Int8{cfRef: {Int64: 9001, Valid: true}},
	}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 0 || len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("active-run swallow must not run/comment/notify: runs=%d notes=%d notifs=%d",
			len(runs.calls), len(f.notes), len(notifier.calls))
	}
	if len(st.records) != 1 || st.records[0].LastPipelineID.Int64 != 9001 {
		t.Fatalf("expected RecordCIAutofixPipeline capped to 9001, got %+v", st.records)
	}
}

func TestCIAutofixCreateRaceSwallows(t *testing.T) {
	// CreateAutoCIFixRun loses the race to the manual button (ErrActiveFixExists):
	// swallow, no comment, counter not advanced (a RecordCIAutofixPipeline only).
	st := &cfStore{candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9001)}}
	runs := &cfRuns{err: workersvc.ErrActiveFixExists}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected one create attempt, got %d", len(runs.calls))
	}
	if len(st.upserts) != 0 {
		t.Fatalf("a swallowed create must not advance the counter, upserts=%d", len(st.upserts))
	}
	if len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("a swallowed create must not comment/notify: notes=%d notifs=%d", len(f.notes), len(notifier.calls))
	}
	if len(st.records) != 1 || st.records[0].LastPipelineID.Int64 != 9001 {
		t.Fatalf("expected a silent RecordCIAutofixPipeline of 9001, got %+v", st.records)
	}
}

func TestCIAutofixBranchInUseSwallows(t *testing.T) {
	// CreateAutoCIFixRun loses the race to an issue run on the same branch
	// (ErrBranchInUse): swallow exactly like ErrActiveFixExists — no comment, counter
	// not advanced, a silent RecordCIAutofixPipeline only.
	st := &cfStore{candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9001)}}
	runs := &cfRuns{err: workersvc.ErrBranchInUse}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected one create attempt, got %d", len(runs.calls))
	}
	if len(st.upserts) != 0 {
		t.Fatalf("a swallowed create must not advance the counter, upserts=%d", len(st.upserts))
	}
	if len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("a swallowed create must not comment/notify: notes=%d notifs=%d", len(f.notes), len(notifier.calls))
	}
	if len(st.records) != 1 || st.records[0].LastPipelineID.Int64 != 9001 {
		t.Fatalf("expected a silent RecordCIAutofixPipeline of 9001, got %+v", st.records)
	}
}

func TestCIAutofixUnparseableRefSkipped(t *testing.T) {
	// The candidate query only yields agent/issue-N branches, but the detector guards
	// against a ref it cannot attribute to an issue: skip it entirely, with no forge or
	// store writes (not even a GetCIAutofixAttempt-driven record).
	cand := cfCand(9001)
	cand.Ref = pgtype.Text{String: "feature/not-an-issue", Valid: true}
	st := &cfStore{candidates: []store.ListCIAutofixCandidateRefsRow{cand}}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 0 || len(st.upserts) != 0 || len(st.records) != 0 || len(st.haltSets) != 0 || len(f.notes) != 0 || len(notifier.calls) != 0 {
		t.Fatalf("unparseable ref must be skipped with no writes: runs=%d upserts=%d records=%d halts=%d notes=%d notifs=%d",
			len(runs.calls), len(st.upserts), len(st.records), len(st.haltSets), len(f.notes), len(notifier.calls))
	}
}

func TestCIAutofixHaltLatchWriteFailsNoComment(t *testing.T) {
	// RECORD-THEN-COMMENT: the latch write must precede the comment. If
	// SetCIAutofixHaltNotified fails, NO comment (and no notification) is posted — a
	// comment without the latch could re-post every tick.
	st := &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{cfCand(9010)},
		attempts: map[string]store.CiAutofixAttempt{
			cfRef: {Ref: cfRef, AttemptCount: 2, LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true}},
		},
		haltSetErr: errors.New("latch write failed"),
	}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(f.notes) != 0 {
		t.Fatalf("a failed latch write must post NO comment, got %+v", f.notes)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("a failed latch write must not notify, got %+v", notifier.calls)
	}
	if len(runs.calls) != 0 {
		t.Fatalf("a halt must not start a run, got %d", len(runs.calls))
	}
}

func TestCIAutofixLatchResetsOnProceed(t *testing.T) {
	// Fix-1 regression: a no-progress halt below the cap comments once and latches, a
	// following DIFFERENT-signature proceed resets the latch, and the later cap halt
	// then posts its own DISTINCT message — the no-progress halt must not permanently
	// suppress the real cap comment.
	ctx := context.Background()

	// sigA: the first (stuck) failure's signature.
	fA := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "panic: boom\nexit 1"}
	snapA, err := workersvc.BuildFailureSnapshot(ctx, fA, 42,
		store.PipelineStatus{PipelineID: 9001, Ref: cfRef, Sha: "deadbeef", WebUrl: "x"}, 5, 4096)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	sigA := workersvc.FailureSignature(snapA)

	// One auto attempt already spent on sigA, not yet halt-notified.
	st := &cfStore{
		attempts: map[string]store.CiAutofixAttempt{
			cfRef: {
				Ref:            cfRef,
				AttemptCount:   1,
				LastSignature:  pgtype.Text{String: sigA, Valid: true},
				LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true},
			},
		},
	}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	d := newCF(st, runs, notifier)

	// Step A: the SAME signature on a new pipeline (count 1 < cap 2) → no-progress halt.
	st.candidates = []store.ListCIAutofixCandidateRefsRow{cfCand(9010)}
	d.detect(ctx, cfRepoRow(), fA)

	if len(st.haltSets) != 1 {
		t.Fatalf("step A: expected one halt latch, got %d", len(st.haltSets))
	}
	if len(fA.notes) != 1 || !strings.Contains(fA.notes[0].body, "did not change the failure signature") {
		t.Fatalf("step A: expected one no-progress comment, got %+v", fA.notes)
	}
	if !st.attempts[cfRef].HaltNotified {
		t.Fatalf("step A: expected the halt latch to be set")
	}

	// Step B: a DIFFERENT failure signature → PROCEED. The proceed must reset the latch.
	fB := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "error: totally different\nexit 2"}
	snapB, err := workersvc.BuildFailureSnapshot(ctx, fB, 42,
		store.PipelineStatus{PipelineID: 9020, Ref: cfRef, Sha: "deadbeef", WebUrl: "x"}, 5, 4096)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	if workersvc.FailureSignature(snapB) == sigA {
		t.Fatalf("test needs two distinct signatures; both hashed to %s", sigA)
	}
	st.candidates = []store.ListCIAutofixCandidateRefsRow{cfCand(9020)}
	d.detect(ctx, cfRepoRow(), fB)

	if len(runs.calls) != 1 {
		t.Fatalf("step B: expected one proceed/run, got %d", len(runs.calls))
	}
	if st.attempts[cfRef].HaltNotified {
		t.Fatalf("step B: a proceed must reset the halt latch")
	}
	if len(fB.notes) != 1 || !strings.Contains(fB.notes[0].body, "Automatic CI fix started") {
		t.Fatalf("step B: expected one start comment, got %+v", fB.notes)
	}

	// Step C: now at the cap (count == 2) → cap halt must post its DISTINCT cap message,
	// NOT be suppressed by the earlier no-progress latch.
	fC := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "error: totally different\nexit 2"}
	st.candidates = []store.ListCIAutofixCandidateRefsRow{cfCand(9030)}
	d.detect(ctx, cfRepoRow(), fC)

	if len(runs.calls) != 1 {
		t.Fatalf("step C: cap halt must not start a run, got %d", len(runs.calls))
	}
	if len(st.haltSets) != 2 {
		t.Fatalf("step C: expected the cap halt to latch again (2 total), got %d", len(st.haltSets))
	}
	if len(fC.notes) != 1 || !strings.Contains(fC.notes[0].body, "attempt limit (2)") {
		t.Fatalf("step C: expected one cap-halt comment (not suppressed), got %+v", fC.notes)
	}
}

func TestCIAutofixNoCandidatesNoOp(t *testing.T) {
	st := &cfStore{}
	runs := &cfRuns{}
	notifier := &cfNotifier{}
	f := &cfForge{}

	newCF(st, runs, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(runs.calls) != 0 || len(st.upserts) != 0 || len(st.records) != 0 {
		t.Fatal("empty candidate set must be a no-op")
	}
}
