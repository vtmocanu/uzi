package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #767 M2: the run-eligibility gate is widened so an issue is uzi's to run if it
// carries the configured uzi_label OR is assigned to the uzi-bot account
// (conn.bot_forge_user_id ∈ issue.assignee_ids). These cases drive the real
// createRun/CreateRun path against the fake store and are calibrated to FAIL against
// the pre-change binary: an assigned-only, unlabelled issue was refused
// ErrNotPRDIssue pre-change (the gate was label-only) and now RUNS on every create
// path.

// runAllPathsAssigned mirrors runAllPaths but additionally seeds the cached issue's
// assignee_ids and the connection's bot_forge_user_id, so the OR-assignment half of
// the gate is exercised on each of the four create wrappers.
func runAllPathsAssigned(t *testing.T, wire func(*Service), labels, assigneeIDs []byte, botID int64) map[string]pathResult {
	t.Helper()
	user, repo := uuid.New(), uuid.New()
	out := map[string]pathResult{}
	invoke := func(name string, call func(svc *Service) error) {
		fs := &fakeStore{
			issueByID:       store.Issue{Title: "T", Labels: labels, AssigneeIds: assigneeIDs},
			repoRow:         store.GetRepoForUserRow{BotForgeUserID: botID},
			createRunResult: store.Run{ID: uuid.New()},
		}
		svc := New(fs, newBox(t), testParams())
		if wire != nil {
			wire(svc)
		}
		err := call(svc)
		out[name] = pathResult{err: err, reached: fs.createRunParams != nil}
	}
	invoke("CreateRun", func(svc *Service) error {
		_, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", nil, nil, false, nil)
		return err
	})
	invoke("CreateScheduledRun", func(svc *Service) error {
		_, err := svc.CreateScheduledRun(context.Background(), user, repo, 4, "desc", nil, nil, nil, false, nil)
		return err
	})
	invoke("CreateAutopilotRun", func(svc *Service) error {
		_, err := svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc")
		return err
	})
	invoke("CreateScheduledAutopilotRun", func(svc *Service) error {
		_, err := svc.CreateScheduledAutopilotRun(context.Background(), user, repo, 4, "desc", nil, nil, nil, false)
		return err
	})
	return out
}

// TestAssignmentEligibilityGate is the M2 headline: an assigned-only issue (no uzi
// label) runs on every path, a labelled issue still runs, and neither is refused with
// ErrNotPRDIssue. The bot-id guard (botID <= 0, or a human-only co-assignee) never
// grants eligibility by accident.
func TestAssignmentEligibilityGate(t *testing.T) {
	const bot = int64(4242)
	uziWired := func(s *Service) { s.SetSettings(fakeSettings{uziLabel: "uzi"}) }

	cases := []struct {
		name        string
		wire        func(*Service)
		labels      []byte
		assigneeIDs []byte
		botID       int64
		wantErr     error // nil ⇒ the run must fire on every path
	}{
		{
			// The headline reversal (fails pre-change): no uzi label, but the bot is an
			// assignee — refused pre-change, runs now on every path.
			name:        "assigned-only unlabelled issue runs on every path",
			wire:        uziWired,
			labels:      labelsJSON(t),
			assigneeIDs: []byte("[4242]"),
			botID:       bot,
		},
		{
			// The label half is unchanged: uzi label + empty assignees still runs.
			name:        "uzi-labelled issue with no assignees still runs",
			wire:        uziWired,
			labels:      labelsJSON(t, "uzi"),
			assigneeIDs: []byte("[]"),
			botID:       bot,
		},
		{
			// The bot alongside a human co-assignee is still a match (membership only).
			name:        "bot among multiple assignees runs",
			wire:        uziWired,
			labels:      labelsJSON(t),
			assigneeIDs: []byte("[7,4242,9]"),
			botID:       bot,
		},
		{
			// Neither signal → refused.
			name:        "neither label nor bot assignment is refused",
			wire:        uziWired,
			labels:      labelsJSON(t),
			assigneeIDs: []byte("[]"),
			botID:       bot,
			wantErr:     ErrNotPRDIssue,
		},
		{
			// A human co-assignee whose id is NOT the bot's does not grant eligibility.
			name:        "a different (human) assignee id is refused",
			wire:        uziWired,
			labels:      labelsJSON(t),
			assigneeIDs: []byte("[7]"),
			botID:       bot,
			wantErr:     ErrNotPRDIssue,
		},
		{
			// Guard: an assignee id of 0 must never match the botID<=0 guard nor a real bot.
			name:        "assignee id 0 with a resolved bot is refused",
			wire:        uziWired,
			labels:      labelsJSON(t),
			assigneeIDs: []byte("[0]"),
			botID:       bot,
			wantErr:     ErrNotPRDIssue,
		},
		{
			// Guard: a connection with no resolved bot (BotForgeUserID = 0) never grants
			// assignment-eligibility, even if the assignees jsonb happens to contain 0.
			name:        "unset bot id (0) never grants assignment eligibility",
			wire:        uziWired,
			labels:      labelsJSON(t),
			assigneeIDs: []byte("[0]"),
			botID:       0,
			wantErr:     ErrNotPRDIssue,
		},
		{
			// An undecodable assignees value gives the gate no basis for consent.
			name:        "undecodable assignee_ids is not eligible",
			wire:        uziWired,
			labels:      labelsJSON(t),
			assigneeIDs: []byte("{not json"),
			botID:       bot,
			wantErr:     ErrNotPRDIssue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := runAllPathsAssigned(t, tc.wire, tc.labels, tc.assigneeIDs, tc.botID)
			for path, r := range results {
				if tc.wantErr != nil {
					if r.err != tc.wantErr {
						t.Errorf("[%s] err = %v, want %v", path, r.err, tc.wantErr)
					}
					if r.reached {
						t.Errorf("[%s] a refused run must never reach CreateRun", path)
					}
					continue
				}
				if r.err != nil {
					t.Errorf("[%s] unexpected err = %v", path, r.err)
				}
				if !r.reached {
					t.Errorf("[%s] an eligible issue must reach CreateRun", path)
				}
			}
		})
	}
}

// TestIsAssignedToBot unit-tests the predicate directly: match, no-match, malformed
// jsonb → false, and botID 0 → false.
func TestIsAssignedToBot(t *testing.T) {
	cases := []struct {
		name        string
		assigneeIDs []byte
		botID       int64
		want        bool
	}{
		{"match, single", []byte("[4242]"), 4242, true},
		{"match, among several", []byte("[1,4242,9]"), 4242, true},
		{"no match, different id", []byte("[7]"), 4242, false},
		{"no match, empty set", []byte("[]"), 4242, false},
		{"malformed jsonb is false", []byte("{not json"), 4242, false},
		{"jsonb null is false", []byte("null"), 4242, false},
		{"botID 0 never matches", []byte("[0]"), 0, false},
		{"negative botID never matches", []byte("[-1]"), -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAssignedToBot(tc.assigneeIDs, tc.botID); got != tc.want {
				t.Errorf("isAssignedToBot(%q, %d) = %v, want %v", tc.assigneeIDs, tc.botID, got, tc.want)
			}
		})
	}
}
