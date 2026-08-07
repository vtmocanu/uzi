package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

func twoMilestones() []Milestone {
	return []Milestone{{ID: "m1", Title: "First milestone"}, {ID: "m2", Title: "Second milestone"}}
}

// TestValidateMilestones is the Decision 12 ceiling: every rejected shape returns
// ok=false (the WHOLE list is dropped, never partially clamped), and an accepted
// list comes back trimmed and NUL-free.
func TestValidateMilestones(t *testing.T) {
	over := make([]Milestone, maxMilestonesPerRun+1)
	for i := range over {
		over[i] = Milestone{ID: "m" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Title: "t"}
	}
	longTitle := strings.Repeat("x", maxMilestoneTitleRunes+1)
	// A title of runes each 2 bytes: at the RUNE cap it is accepted, one rune over is not —
	// proving the cap is measured in code points, not bytes.
	multibyteAtCap := strings.Repeat("é", maxMilestoneTitleRunes)
	multibyteOverCap := strings.Repeat("é", maxMilestoneTitleRunes+1)

	cases := []struct {
		name    string
		in      []Milestone
		wantErr bool
	}{
		{name: "nil is a valid empty list", in: nil},
		{name: "empty is a valid empty list", in: []Milestone{}},
		{name: "a normal list", in: twoMilestones()},
		{name: "at the count cap", in: over[:maxMilestonesPerRun]},
		{name: "id with dot/dash/underscore", in: []Milestone{{ID: "phase-1.a_b", Title: "t"}}},
		{name: "over the count cap", in: over, wantErr: true},
		{name: "empty id", in: []Milestone{{ID: "", Title: "t"}}, wantErr: true},
		{name: "id not leading alphanumeric", in: []Milestone{{ID: "-x", Title: "t"}}, wantErr: true},
		{name: "id with space", in: []Milestone{{ID: "a b", Title: "t"}}, wantErr: true},
		{name: "id with slash (traversal)", in: []Milestone{{ID: "../x", Title: "t"}}, wantErr: true},
		{name: "over-long id", in: []Milestone{{ID: strings.Repeat("a", maxMilestoneIDRunes+1), Title: "t"}}, wantErr: true},
		{name: "duplicate ids", in: []Milestone{{ID: "m1", Title: "a"}, {ID: "m1", Title: "b"}}, wantErr: true},
		{name: "empty title", in: []Milestone{{ID: "m1", Title: ""}}, wantErr: true},
		{name: "whitespace-only title", in: []Milestone{{ID: "m1", Title: "   "}}, wantErr: true},
		{name: "over-long title", in: []Milestone{{ID: "m1", Title: longTitle}}, wantErr: true},
		{name: "multibyte title at the rune cap", in: []Milestone{{ID: "m1", Title: multibyteAtCap}}},
		{name: "multibyte title one rune over the cap", in: []Milestone{{ID: "m1", Title: multibyteOverCap}}, wantErr: true},
		{name: "NUL in title", in: []Milestone{{ID: "m1", Title: "bad\x00title"}}, wantErr: true},
		{name: "control char in title", in: []Milestone{{ID: "m1", Title: "line\nbreak"}}, wantErr: true},
		{name: "bidi override in title", in: []Milestone{{ID: "m1", Title: "safe\u202ednammoc"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := validateMilestones(tc.in)
			if tc.wantErr {
				if ok {
					t.Fatalf("want rejection, got ok with %+v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("want acceptance, got rejection")
			}
		})
	}

	// On success the title is trimmed.
	got, ok := validateMilestones([]Milestone{{ID: "m1", Title: "  padded  "}})
	if !ok || len(got) != 1 || got[0].Title != "padded" {
		t.Fatalf("trim: got %+v ok=%v, want a single {m1, padded}", got, ok)
	}
}

// milestonesRunFixture is a run-owned SetState fixture with a chosen kind, so the
// kind gate (Decision 13) can be exercised on both the awaiting_approval and running
// paths.
func milestonesRunFixture(t *testing.T, kind string) (*fakeStore, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	wkrID, runID, userID := uuid.New(), uuid.New(), uuid.New()
	run := store.Run{ID: runID, UserID: userID, Kind: kind, Status: "running", WorkerID: pgUUID(wkrID)}
	fs := &fakeStore{runOwned: run, setRunningRows: 1}
	return fs, New(fs, newBox(t), testParams()), store.Worker{ID: wkrID}, runID
}

// awaiting_approval carries the CANDIDATE list, stored exactly; the frozen column is
// not touched on this path.
func TestSetStateAwaitingApprovalPersistsCandidate(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, RunKindIssue)
	ms := twoMilestones()

	if _, applied, err := svc.SetState(context.Background(), wkr, runID,
		StateRequest{State: "awaiting_approval", Milestones: &ms}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	var got []Milestone
	if err := json.Unmarshal(fs.setAwaiting.MilestonesCandidate, &got); err != nil {
		t.Fatalf("candidate is not JSON: %v", err)
	}
	if len(got) != 2 || got[0].ID != "m1" || got[1].Title != "Second milestone" {
		t.Fatalf("candidate = %+v", got)
	}
}

// An empty (reported) candidate survives as `[]`, distinct from NULL (never reported).
func TestSetStateAwaitingApprovalEmptyCandidateIsEmptyArray(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, RunKindIssue)
	empty := []Milestone{}
	if _, _, err := svc.SetState(context.Background(), wkr, runID,
		StateRequest{State: "awaiting_approval", Milestones: &empty}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if string(fs.setAwaiting.MilestonesCandidate) != "[]" {
		t.Fatalf("empty candidate persisted as %q, want []", fs.setAwaiting.MilestonesCandidate)
	}
}

// The autopilot `running` report carries the FROZEN list.
func TestSetStateRunningPersistsFrozenMilestones(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, RunKindIssue)
	ms := twoMilestones()

	if _, applied, err := svc.SetState(context.Background(), wkr, runID,
		StateRequest{State: "running", Milestones: &ms}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	var got []Milestone
	if err := json.Unmarshal(fs.setRunningParams.MilestonesFrozen, &got); err != nil {
		t.Fatalf("frozen is not JSON: %v", err)
	}
	if len(got) != 2 || got[0].ID != "m1" {
		t.Fatalf("frozen = %+v", got)
	}
}

// Decision 12: an invalid list is DROPPED (the column param is left NULL), and the
// state report still applies — a bad milestone list never fails the run.
func TestSetStateDropsInvalidMilestones(t *testing.T) {
	over := make([]Milestone, maxMilestonesPerRun+1)
	for i := range over {
		over[i] = Milestone{ID: "m" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Title: "t"}
	}
	cases := []struct {
		name string
		ms   []Milestone
	}{
		{"over the count cap", over},
		{"over-long title", []Milestone{{ID: "m1", Title: strings.Repeat("x", maxMilestoneTitleRunes+1)}}},
		{"NUL in title", []Milestone{{ID: "m1", Title: "bad\x00title"}}},
		{"mis-shaped id", []Milestone{{ID: "../x", Title: "t"}}},
		{"duplicate ids", []Milestone{{ID: "m1", Title: "a"}, {ID: "m1", Title: "b"}}},
	}
	for _, tc := range cases {
		t.Run("awaiting_approval/"+tc.name, func(t *testing.T) {
			fs, svc, wkr, runID := milestonesRunFixture(t, RunKindIssue)
			ms := tc.ms
			_, applied, err := svc.SetState(context.Background(), wkr, runID,
				StateRequest{State: "awaiting_approval", Milestones: &ms})
			if err != nil || !applied {
				t.Fatalf("a bad milestone list must NOT fail the report: applied=%v err=%v", applied, err)
			}
			if fs.setAwaiting.MilestonesCandidate != nil {
				t.Fatalf("invalid milestones must be dropped to NULL, got %q", fs.setAwaiting.MilestonesCandidate)
			}
		})
		t.Run("running/"+tc.name, func(t *testing.T) {
			fs, svc, wkr, runID := milestonesRunFixture(t, RunKindIssue)
			ms := tc.ms
			_, applied, err := svc.SetState(context.Background(), wkr, runID,
				StateRequest{State: "running", Milestones: &ms})
			if err != nil || !applied {
				t.Fatalf("a bad milestone list must NOT fail the report: applied=%v err=%v", applied, err)
			}
			if fs.setRunningParams.MilestonesFrozen != nil {
				t.Fatalf("invalid milestones must be dropped to NULL, got %q", fs.setRunningParams.MilestonesFrozen)
			}
		})
	}
}

// Decision 13: only an issue run may carry milestones. A list from any other kind is
// dropped (NULL), never persisted, on BOTH write paths.
func TestSetStateDropsMilestonesFromNonIssueKind(t *testing.T) {
	for _, kind := range []string{RunKindCIFix, RunKindSelfImprove} {
		t.Run("running/"+kind, func(t *testing.T) {
			fs, svc, wkr, runID := milestonesRunFixture(t, kind)
			ms := twoMilestones()
			if _, _, err := svc.SetState(context.Background(), wkr, runID,
				StateRequest{State: "running", Milestones: &ms}); err != nil {
				t.Fatalf("SetState: %v", err)
			}
			if fs.setRunningParams.MilestonesFrozen != nil {
				t.Fatalf("a %s run must not carry milestones, got %q", kind, fs.setRunningParams.MilestonesFrozen)
			}
		})
		t.Run("awaiting_approval/"+kind, func(t *testing.T) {
			fs, svc, wkr, runID := milestonesRunFixture(t, kind)
			ms := twoMilestones()
			if _, _, err := svc.SetState(context.Background(), wkr, runID,
				StateRequest{State: "awaiting_approval", Milestones: &ms}); err != nil {
				t.Fatalf("SetState: %v", err)
			}
			if fs.setAwaiting.MilestonesCandidate != nil {
				t.Fatalf("a %s run must not carry milestones, got %q", kind, fs.setAwaiting.MilestonesCandidate)
			}
		})
	}
}

// Back-compat: a report that says nothing about milestones sends a NULL param on
// both write paths — the columns are untouched, exactly as a pre-feature run.
func TestSetStateBackCompatNoMilestones(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, RunKindIssue)
	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running", IterationCount: 2}); err != nil {
		t.Fatalf("SetState running: %v", err)
	}
	if fs.setRunningParams.MilestonesFrozen != nil {
		t.Fatalf("absent milestones must send NULL, got %q", fs.setRunningParams.MilestonesFrozen)
	}
	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "awaiting_approval"}); err != nil {
		t.Fatalf("SetState awaiting_approval: %v", err)
	}
	if fs.setAwaiting.MilestonesCandidate != nil {
		t.Fatalf("absent milestones must send NULL, got %q", fs.setAwaiting.MilestonesCandidate)
	}
}

// The claim replays the FROZEN list; a run with no frozen list omits the key
// entirely (omitempty), keeping today's claim wire shape.
func TestClaimServesFrozenMilestones(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-MILESTONE-abcdef1234567890"))
	sealedTok, _ := box.Seal([]byte("anthropic-MILESTONE-abcdef1234567890"))
	claimCtx := store.GetRunClaimContextRow{
		RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
		ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
		BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
	}

	t.Run("frozen list is served", func(t *testing.T) {
		frozen := []byte(`[{"id":"m1","title":"First"},{"id":"m2","title":"Second"}]`)
		fs := &fakeStore{
			claimRun: store.Run{ID: uuid.New(), Kind: RunKindIssue, Status: "claimed",
				IssueIid: pgtype.Int8{Int64: 5, Valid: true}, MilestonesFrozen: frozen},
			claimCtx: claimCtx, anthropic: sealedTok,
		}
		payload, err := New(fs, box, testParams()).Claim(context.Background(), worker())
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(payload.Milestones) != 2 || payload.Milestones[0].ID != "m1" || payload.Milestones[1].Title != "Second" {
			t.Fatalf("payload.Milestones = %+v", payload.Milestones)
		}
		b, _ := json.Marshal(payload)
		if !strings.Contains(string(b), `"milestones":[{"id":"m1","title":"First"}`) {
			t.Fatalf("claim JSON missing milestones: %s", b)
		}
	})

	t.Run("no frozen list omits the key", func(t *testing.T) {
		fs := &fakeStore{
			claimRun: store.Run{ID: uuid.New(), Kind: RunKindIssue, Status: "claimed",
				IssueIid: pgtype.Int8{Int64: 6, Valid: true}},
			claimCtx: claimCtx, anthropic: sealedTok,
		}
		payload, err := New(fs, box, testParams()).Claim(context.Background(), worker())
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if payload.Milestones != nil {
			t.Fatalf("a run with no frozen list must carry nil milestones, got %+v", payload.Milestones)
		}
		if b, _ := json.Marshal(payload); strings.Contains(string(b), "milestones") {
			t.Fatalf("omitempty must drop the key entirely: %s", b)
		}
	})
}
