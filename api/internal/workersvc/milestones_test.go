package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
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

func frozenThree() []byte {
	return []byte(`[{"id":"m1","title":"One"},{"id":"m2","title":"Two"},{"id":"m3","title":"Three"}]`)
}

func strptr(s ...string) *[]string { return &s }

// TestProgressParams is the Decision 3/12 ceiling for the live progress report: every
// reported id must be a FROZEN member or the whole set is dropped (nil), independently
// for completed and in_progress; a non-issue kind or a NULL frozen list drops both.
func TestProgressParams(t *testing.T) {
	frozen := frozenThree()

	// Non-issue kind → both dropped regardless of a valid set.
	if c, ip := progressParams(runkind.CIFix, frozen, strptr("m1"), strptr("m2")); c != nil || ip != nil {
		t.Fatalf("non-issue kind must drop both, got completed=%q in_progress=%q", c, ip)
	}
	// NULL frozen list → nothing to validate against → both dropped.
	if c, ip := progressParams(runkind.Issue, nil, strptr("m1"), strptr("m2")); c != nil || ip != nil {
		t.Fatalf("NULL frozen must drop both, got completed=%q in_progress=%q", c, ip)
	}

	// A valid subset encodes; completed and in_progress independently.
	c, ip := progressParams(runkind.Issue, frozen, strptr("m1", "m3"), strptr("m2"))
	if string(c) != `["m1","m3"]` {
		t.Fatalf("completed = %q, want [\"m1\",\"m3\"]", c)
	}
	if string(ip) != `["m2"]` {
		t.Fatalf("in_progress = %q, want [\"m2\"]", ip)
	}

	// nil pointer → nil output (not reported), leaving the other set intact.
	c, ip = progressParams(runkind.Issue, frozen, nil, strptr("m1"))
	if c != nil {
		t.Fatalf("absent completed must be nil, got %q", c)
	}
	if string(ip) != `["m1"]` {
		t.Fatalf("in_progress = %q, want [\"m1\"]", ip)
	}

	// An empty (reported) set encodes to `[]` — distinct from nil (not reported).
	if c, _ := progressParams(runkind.Issue, frozen, strptr(), nil); string(c) != "[]" {
		t.Fatalf("empty completed must encode to [], got %q", c)
	}

	// Each rejection drops ONLY the offending set (nil), never the other.
	overCap := make([]string, maxMilestonesPerRun+1)
	for i := range overCap {
		overCap[i] = "m1" // membership is fine; the length is the violation
	}
	reject := []struct {
		name string
		set  *[]string
	}{
		{"non-member id", strptr("m1", "nope")},
		{"mis-shaped id", strptr("../x")},
		{"empty id", strptr("")},
		{"duplicate id", strptr("m1", "m1")},
		{"over the count cap", &overCap},
	}
	for _, tc := range reject {
		t.Run(tc.name, func(t *testing.T) {
			// Put the bad set on completed, a valid one on in_progress, and assert only
			// completed dropped.
			c, ip := progressParams(runkind.Issue, frozen, tc.set, strptr("m2"))
			if c != nil {
				t.Fatalf("bad completed must drop to nil, got %q", c)
			}
			if string(ip) != `["m2"]` {
				t.Fatalf("valid in_progress must survive the other set's rejection, got %q", ip)
			}
		})
	}
}

// SetState `running` must pass the validated progress params through to the SQL, and a
// non-member id must never reach the store.
func TestSetStateRunningPassesProgressParams(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	fs.runOwned.MilestonesFrozen = frozenThree()

	if _, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:                "running",
		MilestonesCompleted:  strptr("m1"),
		MilestonesInProgress: strptr("m2"),
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if string(fs.setRunningParams.MilestonesCompleted) != `["m1"]` {
		t.Fatalf("completed param = %q, want [\"m1\"]", fs.setRunningParams.MilestonesCompleted)
	}
	if string(fs.setRunningParams.MilestonesInProgress) != `["m2"]` {
		t.Fatalf("in_progress param = %q, want [\"m2\"]", fs.setRunningParams.MilestonesInProgress)
	}

	// A non-member completed id is dropped to NULL; the report still applies.
	fs2, svc2, wkr2, runID2 := milestonesRunFixture(t, runkind.Issue)
	fs2.runOwned.MilestonesFrozen = frozenThree()
	if _, applied, err := svc2.SetState(context.Background(), wkr2, runID2, StateRequest{
		State:               "running",
		MilestonesCompleted: strptr("ghost"),
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if fs2.setRunningParams.MilestonesCompleted != nil {
		t.Fatalf("a non-member id must be dropped to NULL, got %q", fs2.setRunningParams.MilestonesCompleted)
	}
}

// SetState `running` must always supply the budget-scaling config (harmless on a
// heartbeat, COALESCE-guarded in SQL).
func TestSetStateRunningPassesBudgetConfig(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	if _, _, err := svc.SetState(context.Background(), wkr, runID, StateRequest{State: "running"}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	p := fs.setRunningParams
	if p.RunMaxIterations != 5 || p.RunTimeoutSeconds != 7200 ||
		p.MilestoneBudgetCap != milestoneBudgetCap || p.BudgetWallCeilingSeconds != budgetWallCeilingSeconds {
		t.Fatalf("budget config = %+v, want {5,7200,%d,%d}", p, milestoneBudgetCap, budgetWallCeilingSeconds)
	}
}

// milestonesRunFixture is a run-owned SetState fixture with a chosen kind, so the
// kind gate (Decision 13) can be exercised on both the awaiting_approval and running
// paths.
func milestonesRunFixture(t *testing.T, kind string) (*fakeStore, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	wkrID, runID, userID := uuid.New(), uuid.New(), uuid.New()
	run := store.Run{ID: runID, UserID: userID, Kind: kind, Status: "running", WorkerID: pgconv.UUID(wkrID)}
	fs := &fakeStore{runOwned: run, setRunningRows: 1}
	return fs, New(fs, newBox(t), testParams()), store.Worker{ID: wkrID}, runID
}

// awaiting_approval carries the CANDIDATE list, stored exactly; the frozen column is
// not touched on this path.
func TestSetStateAwaitingApprovalPersistsCandidate(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
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
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	empty := []Milestone{}
	if _, _, err := svc.SetState(context.Background(), wkr, runID,
		StateRequest{State: "awaiting_approval", Milestones: &empty}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if string(fs.setAwaiting.MilestonesCandidate) != "[]" {
		t.Fatalf("empty candidate persisted as %q, want []", fs.setAwaiting.MilestonesCandidate)
	}
}

// PRD #265 M1: the terminal `completed` report reconciles the tracker — the lead's
// declared finished ids are subset-validated against the frozen list and passed to
// SetRunCompleted, exactly as the `running` path validates its progress set. A
// non-member id is dropped, and a report that declares nothing carries a nil param
// (additive-absent: byte-identical to a pre-#265 completion). The SQL UNION itself is
// exercised by the live-DB store test (union-not-overwrite); here we prove the service
// feeds it the right param.
func TestSetStateCompletedReconcilesMilestones(t *testing.T) {
	// A valid subset of the frozen list reaches SetRunCompleted verbatim.
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
	fs.runOwned.MilestonesFrozen = frozenThree()
	fs.setCompletedRows = 1
	if _, applied, err := svc.SetState(context.Background(), wkr, runID, StateRequest{
		State:               "completed",
		Branch:              strp("agent/issue-7"),
		MilestonesCompleted: strptr("m1", "m3"),
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if fs.setCompleted == nil || string(fs.setCompleted.MilestonesCompleted) != `["m1","m3"]` {
		t.Fatalf("completed param = %q, want [\"m1\",\"m3\"]", fs.setCompleted.MilestonesCompleted)
	}

	// A non-member id drops the WHOLE set to NULL (the column stays untouched by the SQL).
	fs2, svc2, wkr2, runID2 := milestonesRunFixture(t, runkind.Issue)
	fs2.runOwned.MilestonesFrozen = frozenThree()
	fs2.setCompletedRows = 1
	if _, applied, err := svc2.SetState(context.Background(), wkr2, runID2, StateRequest{
		State:               "completed",
		Branch:              strp("agent/issue-7"),
		MilestonesCompleted: strptr("m1", "ghost"),
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if fs2.setCompleted == nil || fs2.setCompleted.MilestonesCompleted != nil {
		t.Fatalf("a non-member id must drop to NULL, got %q", fs2.setCompleted.MilestonesCompleted)
	}

	// Additive-absent: a completion that declares nothing carries a nil param.
	fs3, svc3, wkr3, runID3 := milestonesRunFixture(t, runkind.Issue)
	fs3.runOwned.MilestonesFrozen = frozenThree()
	fs3.setCompletedRows = 1
	if _, applied, err := svc3.SetState(context.Background(), wkr3, runID3, StateRequest{
		State: "completed", Branch: strp("agent/issue-7"),
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if fs3.setCompleted == nil || fs3.setCompleted.MilestonesCompleted != nil {
		t.Fatalf("no declaration must leave the param nil, got %q", fs3.setCompleted.MilestonesCompleted)
	}

	// A non-issue run's declaration is dropped (kind gate): a ci_fix has no frozen list.
	fs4, svc4, wkr4, runID4 := milestonesRunFixture(t, runkind.CIFix)
	fs4.setCompletedRows = 1
	if _, applied, err := svc4.SetState(context.Background(), wkr4, runID4, StateRequest{
		State: "completed", MilestonesCompleted: strptr("m1"),
	}); err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	if fs4.setCompleted == nil || fs4.setCompleted.MilestonesCompleted != nil {
		t.Fatalf("a non-issue run must drop the declaration, got %q", fs4.setCompleted.MilestonesCompleted)
	}
}

// The autopilot `running` report carries the FROZEN list.
func TestSetStateRunningPersistsFrozenMilestones(t *testing.T) {
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
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
			fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
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
			fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
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
	for _, kind := range []string{runkind.CIFix, runkind.SelfImprove} {
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
	fs, svc, wkr, runID := milestonesRunFixture(t, runkind.Issue)
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
			claimRun: store.Run{ID: uuid.New(), Kind: runkind.Issue, Status: "claimed",
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
			claimRun: store.Run{ID: uuid.New(), Kind: runkind.Issue, Status: "claimed",
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

	// PRD #122 M2 (Decision 5/5b): the claim serves the EFFECTIVE budget from the
	// persisted columns, falling back to the global default when NULL.
	t.Run("scaled budget served from the persisted columns", func(t *testing.T) {
		fs := &fakeStore{
			claimRun: store.Run{ID: uuid.New(), Kind: runkind.Issue, Status: "claimed",
				IssueIid:            pgtype.Int8{Int64: 7, Valid: true},
				BudgetMaxIterations: pgtype.Int4{Int32: 35, Valid: true},
				BudgetWallSeconds:   pgtype.Int4{Int32: 28800, Valid: true}},
			claimCtx: claimCtx, anthropic: sealedTok,
		}
		payload, err := New(fs, box, testParams()).Claim(context.Background(), worker())
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if payload.Config.MaxIterations != 35 || payload.Config.RunTimeoutSeconds != 28800 {
			t.Fatalf("scaled budget = {%d,%d}, want {35,28800}", payload.Config.MaxIterations, payload.Config.RunTimeoutSeconds)
		}
	})

	// Regression gate: a NULL-budget run (0/1-milestone, or pre-feature) serves the
	// GLOBAL default — byte-for-byte today.
	t.Run("null budget serves the global default", func(t *testing.T) {
		fs := &fakeStore{
			claimRun: store.Run{ID: uuid.New(), Kind: runkind.Issue, Status: "claimed",
				IssueIid: pgtype.Int8{Int64: 8, Valid: true}},
			claimCtx: claimCtx, anthropic: sealedTok,
		}
		payload, err := New(fs, box, testParams()).Claim(context.Background(), worker())
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if payload.Config.MaxIterations != 5 || payload.Config.RunTimeoutSeconds != 7200 {
			t.Fatalf("default budget = {%d,%d}, want {5,7200}", payload.Config.MaxIterations, payload.Config.RunTimeoutSeconds)
		}
	})
}
