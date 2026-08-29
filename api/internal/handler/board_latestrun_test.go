package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func txt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
func nullTxt() pgtype.Text     { return pgtype.Text{} }
func i8(v int64) pgtype.Int8   { return pgtype.Int8{Int64: v, Valid: true} }
func tstamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func TestMapLatestRun(t *testing.T) {
	viewer := uuid.New()
	runID := uuid.New()
	created := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	updated := created.Add(5 * time.Minute)

	t.Run("owner's run maps all fields and is mine", func(t *testing.T) {
		dto := mapLatestRun(runID, viewer, "completed", "issue", 2, true, i8(7), txt("https://gl.example/x/-/merge_requests/7"), txt("merged"), txt("boom"), nullTxt(), nullTxt(),
			"ok", nullTxt(), pgtype.Timestamptz{},
			txt("Vlad"), txt("laptop"), 3, tstamp(created), tstamp(updated), viewer)
		if dto.IsPlanning {
			t.Fatal("a completed run must not be is_planning")
		}
		if dto.MrState == nil || *dto.MrState != "merged" {
			t.Fatalf("mr_state should be carried, got %v", dto.MrState)
		}
		if dto.MrWebURL == nil || *dto.MrWebURL != "https://gl.example/x/-/merge_requests/7" {
			t.Fatalf("mr_web_url should be carried, got %v", dto.MrWebURL)
		}
		if dto.ID != runID.String() || dto.Status != "completed" {
			t.Fatalf("id/status wrong: %+v", dto)
		}
		if dto.RunCount != 3 {
			t.Fatalf("run_count should be 3, got %d", dto.RunCount)
		}
		if dto.MrIID == nil || *dto.MrIID != 7 {
			t.Fatalf("mr_iid should be 7, got %v", dto.MrIID)
		}
		if dto.FailureReason == nil || *dto.FailureReason != "boom" {
			t.Fatalf("failure_reason not carried: %v", dto.FailureReason)
		}
		if dto.WorkerName == nil || *dto.WorkerName != "laptop" {
			t.Fatalf("worker_name not carried: %v", dto.WorkerName)
		}
		if dto.OwnerName != "Vlad" {
			t.Fatalf("owner_name should prefer display name, got %q", dto.OwnerName)
		}
		if !dto.IsMine {
			t.Fatal("a run owned by the viewer must be is_mine")
		}
		if !dto.CreatedAt.Equal(created) || !dto.UpdatedAt.Equal(updated) {
			t.Fatalf("timestamps wrong: %+v", dto)
		}
	})

	t.Run("another owner's run is not mine: no email, failure_reason gated, stop_kind exposed", func(t *testing.T) {
		otherOwner := uuid.New()
		// A non-owner viewer of a shared board: owner_name is empty when the display
		// name is absent (never the email — Decision 5, the anti-leak guarantee), and
		// failure_reason is withheld even though the run has one (it can carry a verbatim
		// reject reason or a raw agent error). stop_kind (a non-sensitive enum) stays
		// visible so the badge can still classify the run as stopped.
		dto := mapLatestRun(runID, otherOwner, "failed", "issue", 3, false, pgtype.Int8{}, nullTxt(), nullTxt(),
			txt("panic: raw agent internals"), txt("plan_rejected"), nullTxt(),
			"ok", nullTxt(), pgtype.Timestamptz{},
			nullTxt(), nullTxt(), 1, tstamp(created), tstamp(updated), viewer)
		if dto.IsMine {
			t.Fatal("a run owned by someone else must not be is_mine")
		}
		if dto.MrState != nil {
			t.Fatalf("null mr_state should map to nil, got %v", *dto.MrState)
		}
		if dto.OwnerName != "" {
			t.Fatalf("owner_name must be empty when display name is absent, never an email; got %q", dto.OwnerName)
		}
		if dto.MrIID != nil {
			t.Fatalf("null mr_iid should map to nil, got %v", *dto.MrIID)
		}
		if dto.FailureReason != nil {
			t.Fatalf("failure_reason must be withheld from a non-owner viewer, got %q", *dto.FailureReason)
		}
		if dto.StopKind == nil || *dto.StopKind != "plan_rejected" {
			t.Fatalf("stop_kind must stay exposed to a non-owner viewer, got %v", dto.StopKind)
		}
		if dto.WorkerName != nil {
			t.Fatalf("null worker_name should map to nil: %+v", dto)
		}
	})

	t.Run("stop_reason is owner-gated free text; stop_kind stays exposed (issue #525)", func(t *testing.T) {
		otherOwner := uuid.New()
		// stop_reason is the operator's OPTIONAL free-text cancel reason. Like
		// failure_reason it can carry verbatim typed text, so a shared board must gate
		// it to the owner (Decision 5). stop_kind (a non-sensitive enum) stays visible.

		// (a) The owner sees the reason.
		mine := mapLatestRun(runID, viewer, "cancelled", "issue", 3, false, pgtype.Int8{}, nullTxt(), nullTxt(),
			nullTxt(), txt("cancelled"), txt("wrong branch, my mistake"),
			"ok", nullTxt(), pgtype.Timestamptz{},
			nullTxt(), nullTxt(), 1, tstamp(created), tstamp(updated), viewer)
		if mine.StopReason == nil || *mine.StopReason != "wrong branch, my mistake" {
			t.Fatalf("owner must see stop_reason, got %v", mine.StopReason)
		}
		if mine.StopKind == nil || *mine.StopKind != "cancelled" {
			t.Fatalf("owner stop_kind should be carried, got %v", mine.StopKind)
		}

		// (b) A non-owner viewer does NOT see the reason, but stop_kind stays exposed.
		theirs := mapLatestRun(runID, otherOwner, "cancelled", "issue", 3, false, pgtype.Int8{}, nullTxt(), nullTxt(),
			nullTxt(), txt("cancelled"), txt("wrong branch, my mistake"),
			"ok", nullTxt(), pgtype.Timestamptz{},
			nullTxt(), nullTxt(), 1, tstamp(created), tstamp(updated), viewer)
		if theirs.IsMine {
			t.Fatal("a run owned by someone else must not be is_mine")
		}
		if theirs.StopReason != nil {
			t.Fatalf("stop_reason must be withheld from a non-owner viewer, got %q", *theirs.StopReason)
		}
		if theirs.StopKind == nil || *theirs.StopKind != "cancelled" {
			t.Fatalf("stop_kind must stay exposed to a non-owner viewer, got %v", theirs.StopKind)
		}
	})

	t.Run("blank display name leaves owner name empty", func(t *testing.T) {
		dto := mapLatestRun(runID, viewer, "queued", "issue", 0, false, pgtype.Int8{}, nullTxt(), nullTxt(), nullTxt(), nullTxt(), nullTxt(),
			"ok", nullTxt(), pgtype.Timestamptz{},
			txt(""), nullTxt(), 1, tstamp(created), tstamp(updated), viewer)
		if dto.IsPlanning {
			t.Fatal("a queued run is not running, so it must not be is_planning")
		}
		if dto.OwnerName != "" {
			t.Fatalf("owner_name should be empty when the display name is blank, got %q", dto.OwnerName)
		}
	})

	t.Run("health: enum + since unconditional, reason owner-gated (PRD #47)", func(t *testing.T) {
		since := created.Add(2 * time.Minute)
		// The owner of a flagged run sees the enum, the since, AND the reason.
		mine := mapLatestRun(runID, viewer, "running", "issue", 0, false, pgtype.Int8{}, nullTxt(), nullTxt(), nullTxt(), nullTxt(), nullTxt(),
			"waiting_worker", txt("your vault is locked"), tstamp(since),
			txt("Vlad"), nullTxt(), 1, tstamp(created), tstamp(updated), viewer)
		// This owner's run is running at iteration 0 with no persisted plan: the board card
		// carries the issue #321 planning flag (kind issue is planning-capable).
		if !mine.IsPlanning {
			t.Fatal("a running issue run at iteration 0 with no plan must be is_planning")
		}
		if mine.Health != "waiting_worker" {
			t.Fatalf("owner health = %q, want waiting_worker", mine.Health)
		}
		if mine.HealthSince == nil || !mine.HealthSince.Equal(since) {
			t.Fatalf("owner health_since should be carried, got %v", mine.HealthSince)
		}
		if mine.HealthReason == nil || *mine.HealthReason != "your vault is locked" {
			t.Fatalf("owner health_reason should be carried, got %v", mine.HealthReason)
		}

		// A non-owner viewer gets the enum and the since (non-sensitive, like stop_kind)
		// but NOT the reason, which can name owner state (Decision 6).
		other := uuid.New()
		theirs := mapLatestRun(runID, other, "running", "issue", 0, false, pgtype.Int8{}, nullTxt(), nullTxt(), nullTxt(), nullTxt(), nullTxt(),
			"waiting_worker", txt("your vault is locked"), tstamp(since),
			nullTxt(), nullTxt(), 1, tstamp(created), tstamp(updated), viewer)
		if theirs.Health != "waiting_worker" {
			t.Fatalf("non-owner health enum should still be exposed, got %q", theirs.Health)
		}
		if theirs.HealthSince == nil || !theirs.HealthSince.Equal(since) {
			t.Fatalf("non-owner health_since should still be exposed, got %v", theirs.HealthSince)
		}
		if theirs.HealthReason != nil {
			t.Fatalf("health_reason must be withheld from a non-owner viewer, got %q", *theirs.HealthReason)
		}
	})
}

func TestAssembleCards(t *testing.T) {
	viewer := uuid.New()
	other := uuid.New()
	run10, run20 := uuid.New(), uuid.New()
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	// Three issues: 10 (viewer's run), 20 (another owner's run), 30 (no run).
	issues := []store.Issue{
		{ForgeIssueIid: 10, Title: "ten", State: "opened", Labels: []byte(`["In Progress"]`)},
		{ForgeIssueIid: 20, Title: "twenty", State: "opened", Labels: []byte(`[]`)},
		{ForgeIssueIid: 30, Title: "thirty", State: "opened", Labels: []byte(`[]`)},
	}
	// Deliberately NOT in issue order (20 before 10): a correct assembly keys by
	// issue_iid, so a positional/cross-keying bug would surface here.
	runRows := []store.ListLatestRunsForRepoRow{
		{IssueIid: i8(20), ID: run20, UserID: other, Status: "completed", Kind: "issue", IterationCount: 4, HasPlanMd: pgtype.Bool{Bool: true, Valid: true}, MrIid: i8(5), MrWebUrl: txt("https://forge.example/grp/repo/pulls/5"), MrState: txt("closed"),
			FailureReason: txt("raw agent internals"), OwnerName: nullTxt(), RunCount: 2, CreatedAt: tstamp(now), UpdatedAt: tstamp(now)},
		{IssueIid: i8(10), ID: run10, UserID: viewer, Status: "running", Kind: "issue", IterationCount: 0, HasPlanMd: pgtype.Bool{Bool: false, Valid: true},
			OwnerName: txt("Vlad"), WorkerName: txt("laptop"), RunCount: 1, CreatedAt: tstamp(now), UpdatedAt: tstamp(now)},
	}
	position := map[string]int{"In Progress": 0}

	cards := assembleCards(issues, runRows, nil, position, viewer, "forgejo", nil)
	byIID := make(map[int64]cardDTO, len(cards))
	for _, c := range cards {
		byIID[c.IID] = c
	}
	if len(cards) != 3 {
		t.Fatalf("expected 3 cards, got %d", len(cards))
	}
	// PRD #65 D2: every card carries the board's forge for the per-card MR/PR noun.
	for _, c := range cards {
		if c.ForgeType != "forgejo" {
			t.Fatalf("card %d forge_type = %q, want forgejo", c.IID, c.ForgeType)
		}
	}

	// (1) each latest_run lands on the RIGHT issue (no cross-keying).
	if lr := byIID[10].LatestRun; lr == nil || lr.ID != run10.String() {
		t.Fatalf("issue 10 must carry its own run %s, got %+v", run10, lr)
	}
	if lr := byIID[20].LatestRun; lr == nil || lr.ID != run20.String() {
		t.Fatalf("issue 20 must carry its own run %s, got %+v", run20, lr)
	}
	// (2) an issue with no run gets latest_run: null.
	if byIID[30].LatestRun != nil {
		t.Fatalf("issue 30 has no run and must have latest_run null, got %+v", byIID[30].LatestRun)
	}
	// (3) is_mine and owner_name fallback flow through.
	if !byIID[10].LatestRun.IsMine || byIID[10].LatestRun.OwnerName != "Vlad" {
		t.Fatalf("issue 10: viewer's run should be mine + named Vlad, got %+v", byIID[10].LatestRun)
	}
	if byIID[20].LatestRun.IsMine {
		t.Fatal("issue 20: another owner's run must not be is_mine")
	}
	// Decision 5: owner_name is the display name or empty, never the email. This
	// run's owner has no display name, so a shared board shows an empty owner, not
	// the leaked email address.
	if byIID[20].LatestRun.OwnerName != "" {
		t.Fatalf("issue 20: owner_name must be empty (no display name), never an email; got %q", byIID[20].LatestRun.OwnerName)
	}
	if byIID[20].LatestRun.MrIID == nil || *byIID[20].LatestRun.MrIID != 5 {
		t.Fatalf("issue 20: mr_iid should flow through, got %v", byIID[20].LatestRun.MrIID)
	}
	if byIID[20].LatestRun.MrState == nil || *byIID[20].LatestRun.MrState != "closed" {
		t.Fatalf("issue 20: mr_state should flow through, got %v", byIID[20].LatestRun.MrState)
	}
	if byIID[20].LatestRun.MrWebURL == nil || *byIID[20].LatestRun.MrWebURL != "https://forge.example/grp/repo/pulls/5" {
		t.Fatalf("issue 20: mr_web_url should flow through, got %v", byIID[20].LatestRun.MrWebURL)
	}
	// Decision 5: another owner's failure_reason is withheld from this viewer.
	if byIID[20].LatestRun.FailureReason != nil {
		t.Fatalf("issue 20: failure_reason must be withheld from a non-owner viewer, got %q", *byIID[20].LatestRun.FailureReason)
	}
	// (4) run_count keys onto the right issue (drives the "×N" retry hint).
	if byIID[20].LatestRun.RunCount != 2 {
		t.Fatalf("issue 20: run_count should be 2, got %d", byIID[20].LatestRun.RunCount)
	}
	if byIID[10].LatestRun.RunCount != 1 {
		t.Fatalf("issue 10: run_count should be 1, got %d", byIID[10].LatestRun.RunCount)
	}
	// issue #321: the planning flag threads through the list projection onto each card.
	// Issue 10 is running at iteration 0 with no plan (planning); issue 20 is completed.
	if !byIID[10].LatestRun.IsPlanning {
		t.Fatal("issue 10: running issue run at iteration 0 with no plan must be is_planning")
	}
	if byIID[20].LatestRun.IsPlanning {
		t.Fatal("issue 20: a completed run must not be is_planning")
	}
	// Column resolution flows through the assembly too.
	if byIID[10].Column != "In Progress" {
		t.Fatalf("issue 10 column = %q, want In Progress", byIID[10].Column)
	}
}
