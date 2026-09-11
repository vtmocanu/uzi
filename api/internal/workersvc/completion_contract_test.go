package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeCompletionInterlock is a CompletionInterlockReader whose value and error are set by
// the test — the rollout switch createRun consults (PRD #1226 M1, D1).
type fakeCompletionInterlock struct {
	on  bool
	err error
}

func (f fakeCompletionInterlock) CompletionInterlockRollout(context.Context) (bool, error) {
	return f.on, f.err
}

// newInterlockCreateSvc builds a Service wired for the createRun happy path (a clearing
// repo guard, a valid repo/issue) plus the given completion-interlock reader, and returns
// the service and the fake store so the caller can inspect createRunParams.
func newInterlockCreateSvc(t *testing.T, reader CompletionInterlockReader) (*Service, *fakeStore) {
	t.Helper()
	fs := &fakeStore{
		repoRow:         aValidRepoRow(),
		issueByID:       store.Issue{Title: "T", Labels: uziLabels(), HasPrdLink: true},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc := New(fs, newBox(t), testParams())
	svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: false}})
	if reader != nil {
		svc.SetCompletionInterlockSettings(reader)
	}
	return svc, fs
}

// TestCreateRunStampsInterlockWhenRolloutOn: with the rollout switch ON, createRun stamps
// completion_contract_version=1 on the new issue row BEFORE its first claim (PRD #1226 M1,
// D1), so the D2 hard claim clause is not vacuous for the plan-phase worker.
func TestCreateRunStampsInterlockWhenRolloutOn(t *testing.T) {
	svc, fs := newInterlockCreateSvc(t, fakeCompletionInterlock{on: true})
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, false, nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun store insert must run")
	}
	v := fs.createRunParams.CompletionContractVersion
	if !v.Valid || v.Int32 != 1 {
		t.Fatalf("completion_contract_version = %+v, want valid 1 (rollout on)", v)
	}
}

// TestCreateRunNoStampWhenRolloutOff: with the switch OFF the new run stays legacy —
// completion_contract_version NULL — so it is never interlocked.
func TestCreateRunNoStampWhenRolloutOff(t *testing.T) {
	svc, fs := newInterlockCreateSvc(t, fakeCompletionInterlock{on: false})
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, false, nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun store insert must run")
	}
	if fs.createRunParams.CompletionContractVersion.Valid {
		t.Fatalf("completion_contract_version = %+v, want NULL (rollout off)", fs.createRunParams.CompletionContractVersion)
	}
}

// TestCreateRunNoStampWhenReaderUnset: a nil reader (a deployment/test that never wired the
// switch) defaults OFF, the fail-safe direction — a new run is legacy, never interlocked.
func TestCreateRunNoStampWhenReaderUnset(t *testing.T) {
	svc, fs := newInterlockCreateSvc(t, nil)
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, false, nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if fs.createRunParams.CompletionContractVersion.Valid {
		t.Fatalf("completion_contract_version = %+v, want NULL (nil reader defaults off)", fs.createRunParams.CompletionContractVersion)
	}
}

// TestCreateRunNoStampOnReadError: a rollout read ERROR is treated as OFF (fail-safe) —
// the DELIBERATE opposite of the capability-aware fail-open, so the interlock never
// accidentally engages a still-rolling-out feature on a momentary settings-read blip.
func TestCreateRunNoStampOnReadError(t *testing.T) {
	svc, fs := newInterlockCreateSvc(t, fakeCompletionInterlock{on: true, err: context.DeadlineExceeded})
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, false, nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if fs.createRunParams.CompletionContractVersion.Valid {
		t.Fatalf("completion_contract_version = %+v, want NULL (read error fails safe to off)", fs.createRunParams.CompletionContractVersion)
	}
}

// TestBuildCompletionContract pins the frozen structural contract's exact wire shape (PRD
// #1226 M1, D1): one criterion per milestone with id "<milestone_id>.c1", the milestone
// title as text, a null audit slot, and an EMPTY (never null) finding_ids array.
func TestBuildCompletionContract(t *testing.T) {
	ms := []Milestone{{ID: "m1", Title: "First milestone"}, {ID: "m2", Title: "Second"}}
	raw, err := json.Marshal(ms)
	if err != nil {
		t.Fatalf("marshal milestones: %v", err)
	}
	got, err := buildCompletionContract(raw)
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	// The reserved slots must serialize as `null` / `[]`, not omitted or `null` arrays —
	// #1230/#1231 fill them and depend on the exact shape.
	if !strings.Contains(string(got), `"audit":null`) {
		t.Errorf("contract missing `\"audit\":null`: %s", got)
	}
	if !strings.Contains(string(got), `"finding_ids":[]`) {
		t.Errorf("contract missing `\"finding_ids\":[]`: %s", got)
	}
	var c completionContract
	if err := json.Unmarshal(got, &c); err != nil {
		t.Fatalf("unmarshal contract: %v", err)
	}
	if c.Profile != "structural" || c.Revision != 1 {
		t.Errorf("contract profile/revision = %q/%d, want structural/1", c.Profile, c.Revision)
	}
	if len(c.Criteria) != 2 {
		t.Fatalf("criteria = %d, want 2", len(c.Criteria))
	}
	if c.Criteria[0].ID != "m1.c1" || c.Criteria[0].MilestoneID != "m1" || c.Criteria[0].Text != "First milestone" {
		t.Errorf("criterion[0] = %+v, want id=m1.c1 milestone_id=m1 text=First milestone", c.Criteria[0])
	}
	if c.Criteria[0].Audit != nil {
		t.Errorf("criterion[0].Audit = %v, want nil (reserved for #1230/#1231)", c.Criteria[0].Audit)
	}
	if c.Criteria[0].FindingIDs == nil || len(c.Criteria[0].FindingIDs) != 0 {
		t.Errorf("criterion[0].FindingIDs = %v, want empty non-nil", c.Criteria[0].FindingIDs)
	}
}

// TestBuildCompletionContractEmpty: an interlocked run with NO milestones still freezes a
// valid contract with an empty (non-null) criteria array — D1 forbids exempting a run by
// milestone cardinality, so even a 0-milestone run gets a contract.
func TestBuildCompletionContractEmpty(t *testing.T) {
	for _, in := range [][]byte{nil, []byte(`[]`)} {
		got, err := buildCompletionContract(in)
		if err != nil {
			t.Fatalf("buildCompletionContract(%q): %v", in, err)
		}
		if !strings.Contains(string(got), `"criteria":[]`) {
			t.Errorf("empty-milestone contract must carry `\"criteria\":[]`, got %s", got)
		}
	}
}
