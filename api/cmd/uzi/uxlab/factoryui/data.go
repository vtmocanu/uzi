package factoryui

// Fixture data for the demo. No API, no DB — just seeded values that exercise every status
// colour and a multi-lane crew.

// Run is a board row plus everything the detail view needs to render it.
type Run struct {
	ID, Kind, Status, Title string
	Health, Age, Owner      string
	Verdict                 string // "", ideal, ok, issues — the judge badge
	ParkLine                string // limit_wait countdown line, "" otherwise
	Lanes                   []Lane
	Transcript              []Frame
}

// Lane is one crew member's presence in a run. State: working|waiting|stalled|idle|done.
type Lane struct {
	Role, Inst, Label, State string
}

// Frame is one transcript entry.
type Frame struct {
	Role, Kind, Text string
}

func leadPlan() []Frame {
	return []Frame{{"lead", "plan", "## Plan\n1. Back off pollInterval when the usage window is within 10% of the cap.\n2. Add a unit test for the near-cap case.\n3. Open an MR against a feature branch.\n\nReady for approval."}}
}

func crewTranscript() []Frame {
	return []Frame{
		{"lead", "text", "Planning the change. I'll split this into a scheduler tweak and a test, then dispatch a coder and a tester."},
		{"lead", "text", "Dispatched coder (scheduler headroom) and tester (regression sweep). Watching for the first diff."},
		{"coder", "tool_use", "Editing scheduler.go, adding the near-cap backoff."},
		{"lead", "text", "Coder reports the backoff is in. Asking the tester to add a boundary case before I review."},
		{"tester", "tool_use", "Running the regression sweep."},
		{"lead", "text", "Tester is green. Reviewing the diff now; will open an MR against a feature branch when it lands."},
		{"lead", "text", "Reviewed: the near-cap constant should be shared with the poller. Sending that back to the coder."},
	}
}

func fullCrew() []Lane {
	return []Lane{
		{"lead", "", "", "working"},
		{"coder", "3v6ptu", "scheduler headroom", "waiting"},
		{"tester", "2k9xqf", "regression sweep", "working"},
	}
}

// SeedRuns returns the demo board, ordered so the eye meets a run that needs a human early.
func SeedRuns() []Run {
	return []Run{
		{ID: "a1b2c3d4", Kind: "issue", Status: "running", Title: "Add rate-limit headroom to the scheduler poll",
			Age: "4m", Owner: "dana", Lanes: fullCrew(), Transcript: crewTranscript()},
		{ID: "b2c3d4e5", Kind: "ci_fix", Status: "awaiting_approval", Title: "Fix flaky pipeline on main",
			Age: "2m", Owner: "dana", Lanes: []Lane{{"lead", "", "", "waiting"}}, Transcript: leadPlan()},
		{ID: "c3d4e5f6", Kind: "issue", Status: "running", Title: "Refactor the forge sync loop for the GitHub driver",
			Health: "stalled", Age: "51m", Owner: "priya", Lanes: []Lane{
				{"lead", "", "", "working"}, {"coder", "9xk2mq", "driver port", "stalled"}}, Transcript: crewTranscript()},
		{ID: "d4e5f6a7", Kind: "chat", Status: "running", Title: "Explain the run lifecycle state machine",
			Age: "1m", Owner: "dana", Lanes: []Lane{{"lead", "", "", "working"}}, Transcript: crewTranscript()},
		{ID: "e5f6a7b8", Kind: "issue", Status: "completed", Title: "Wire the OIDC login button into the header",
			Age: "3h", Owner: "sam", Verdict: "ideal", Lanes: fullCrew(), Transcript: crewTranscript()},
		{ID: "f6a7b8c9", Kind: "issue", Status: "limit_wait", Title: "Port the judge to per-model usage folding",
			Age: "22m", Owner: "dana", ParkLine: "paused: Anthropic usage limit (five_hour) · resumes in 41m · attempt 2",
			Lanes: fullCrew(), Transcript: crewTranscript()},
		{ID: "a7b8c9d0", Kind: "issue", Status: "failed", Title: "Migrate per-user secrets into the vault hierarchy",
			Age: "5h", Owner: "sam", Verdict: "issues", Lanes: fullCrew(), Transcript: crewTranscript()},
		{ID: "b8c9d0e1", Kind: "ci_fix", Status: "completed", Title: "Repair the changelog assertion gate",
			Age: "1d", Owner: "priya", Verdict: "ok", Lanes: fullCrew(), Transcript: crewTranscript()},
	}
}

// Recommendation is one judge finding for the review overlay.
type Recommendation struct {
	Category, Target, Confidence, ID, Disposition string
}

// DemoReview is the fixed review shown by the [v] overlay on a completed run.
type DemoReview struct {
	Verdict, Summary  string
	Total, Todo, Done int
	Recommendations   []Recommendation
}

func SeedReview() DemoReview {
	return DemoReview{
		Verdict: "issues",
		Summary: "The change is sound and well-tested, but the near-cap backoff constant is\nduplicated between the scheduler and the poller.",
		Total:   3, Todo: 2, Done: 1,
		Recommendations: []Recommendation{
			{"improve_uzi", "api/internal/poller/scheduler.go", "high", "9f2a1b3c", "done"},
			{"adjust_template", "coder", "medium", "7c4d2e1a", ""},
			{"enable_tool", "worker: gofumpt", "low", "3b8e5f0d", ""},
		},
	}
}
