package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestJobStatusMapping pins the public job status for every raw run status (PRD #1908 D-E).
func TestJobStatusMapping(t *testing.T) {
	want := map[string]string{
		"queued":            "queued",
		"claimed":           "running",
		"running":           "running",
		"awaiting_approval": "running",
		"awaiting_input":    "running",
		"awaiting_followup": "running",
		"pool_wait":         "waiting",
		"paused":            "waiting",
		"limit_wait":        "waiting",
		"recovery_wait":     "waiting",
		"completed":         "completed",
		"failed":            "failed",
		"cancelled":         "cancelled",
	}
	for in, out := range want {
		if got := JobStatus(in); got != out {
			t.Errorf("JobStatus(%q) = %q, want %q", in, got, out)
		}
	}
	// An unknown future status must never read as terminal.
	if got := JobStatus("something_new"); got != "running" {
		t.Errorf("JobStatus(unknown) = %q, want running", got)
	}
}

func TestIsNoModelCredential(t *testing.T) {
	for _, err := range []error{ErrNoCredentialForHarness, ErrHarnessCredentialDisabled, ErrNoUsableCredential} {
		if !IsNoModelCredential(err) {
			t.Errorf("IsNoModelCredential(%v) = false, want true", err)
		}
	}
	if IsNoModelCredential(ErrJobOverCap) || IsNoModelCredential(nil) {
		t.Error("IsNoModelCredential accepted an unrelated error")
	}
}

func TestCreateJobRunRefusalsBeforeTheStore(t *testing.T) {
	svc := &Service{} // no store: these refusals must return before any read
	ctx := context.Background()
	base := CreateJobParams{Caller: JobCaller{UserID: uuid.New()}, JobType: runkind.JobTypeResearch, Title: "t", Prompt: "p"}

	egress := base
	egress.EgressProfile = true
	if _, err := svc.CreateJobRun(ctx, egress); !errors.Is(err, ErrJobNotSupported) {
		t.Errorf("egress profile err = %v, want ErrJobNotSupported", err)
	}
	unknown := base
	unknown.JobType = "nope"
	if _, err := svc.CreateJobRun(ctx, unknown); !errors.Is(err, ErrJobTypeUnknown) {
		t.Errorf("unknown type err = %v, want ErrJobTypeUnknown", err)
	}
	bad := base
	bad.Title = ""
	_, err := svc.CreateJobRun(ctx, bad)
	var inv *JobInvalidError
	if !errors.Is(err, ErrJobInvalid) || !errors.As(err, &inv) || inv.Field != "title" {
		t.Errorf("empty title err = %v, want a JobInvalidError on title", err)
	}
}

func TestValidateCreateJob(t *testing.T) {
	label := func(s string) *string { return &s }
	ok := CreateJobParams{Title: "Quarterly summary", Prompt: "do it", RequestedByLabel: label("  alice@example.com ")}
	v, err := validateCreateJob(ok)
	if err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	if v.label == nil || *v.label != "alice@example.com" {
		t.Errorf("label = %v, want trimmed", v.label)
	}

	// NUL is stripped from the prompt and input content rather than refused.
	v, err = validateCreateJob(CreateJobParams{Title: "t", Prompt: "a\x00b", Inputs: []JobInput{{Name: "a.txt", Content: "x\x00y"}}})
	if err != nil || v.prompt != "ab" || v.inputs[0].Content != "xy" {
		t.Errorf("NUL strip: prompt=%q inputs=%+v err=%v", v.prompt, v.inputs, err)
	}

	tooMany := make([]JobInput, maxJobInputs+1)
	for i := range tooMany {
		tooMany[i] = JobInput{Name: "f" + strings.Repeat("a", i)}
	}
	cases := []struct {
		name  string
		p     CreateJobParams
		field string
	}{
		{"blank title", CreateJobParams{Title: "  ", Prompt: "p"}, "title"},
		{"long title", CreateJobParams{Title: strings.Repeat("a", maxJobTitleBytes+1), Prompt: "p"}, "title"},
		{"control char title", CreateJobParams{Title: "a\nb", Prompt: "p"}, "title"},
		{"bidi title", CreateJobParams{Title: "a‮b", Prompt: "p"}, "title"},
		{"blank prompt", CreateJobParams{Title: "t", Prompt: " \x00 "}, "prompt"},
		{"oversize prompt", CreateJobParams{Title: "t", Prompt: strings.Repeat("a", MaxIssueDescriptionBytes+1)}, "prompt"},
		{"bad utf8 prompt", CreateJobParams{Title: "t", Prompt: "a\xffb"}, "prompt"},
		{"too many inputs", CreateJobParams{Title: "t", Prompt: "p", Inputs: tooMany}, "inputs"},
		{"dotdot name", CreateJobParams{Title: "t", Prompt: "p", Inputs: []JobInput{{Name: "a..b"}}}, "inputs[0].name"},
		{"leading dot name", CreateJobParams{Title: "t", Prompt: "p", Inputs: []JobInput{{Name: ".env"}}}, "inputs[0].name"},
		{"slash name", CreateJobParams{Title: "t", Prompt: "p", Inputs: []JobInput{{Name: "a/b"}}}, "inputs[0].name"},
		{"long name", CreateJobParams{Title: "t", Prompt: "p", Inputs: []JobInput{{Name: strings.Repeat("a", 101)}}}, "inputs[0].name"},
		{"duplicate name", CreateJobParams{Title: "t", Prompt: "p", Inputs: []JobInput{{Name: "a"}, {Name: "a"}}}, "inputs[1].name"},
		{"total bytes", CreateJobParams{Title: "t", Prompt: "p", Inputs: []JobInput{
			{Name: "a", Content: strings.Repeat("x", maxJobInputTotalBytes)}, {Name: "b", Content: "y"}}}, "inputs"},
		{"long label", CreateJobParams{Title: "t", Prompt: "p", RequestedByLabel: label(strings.Repeat("a", maxJobLabelBytes+1))}, "requested_by_label"},
		{"control label", CreateJobParams{Title: "t", Prompt: "p", RequestedByLabel: label("a\tb")}, "requested_by_label"},
		{"zero wall", CreateJobParams{Title: "t", Prompt: "p", WallSeconds: new(int)}, "wall_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateCreateJob(tc.p)
			var inv *JobInvalidError
			if !errors.Is(err, ErrJobInvalid) || !errors.As(err, &inv) {
				t.Fatalf("err = %v, want ErrJobInvalid", err)
			}
			if inv.Field != tc.field {
				t.Errorf("field = %q, want %q (%v)", inv.Field, tc.field, err)
			}
		})
	}

	// The wall clock is clamped to the 8h ceiling, not refused.
	huge := budgetWallCeilingSeconds * 10
	v, err = validateCreateJob(CreateJobParams{Title: "t", Prompt: "p", WallSeconds: &huge})
	if err != nil || !v.wall.Valid || int(v.wall.Int32) != budgetWallCeilingSeconds {
		t.Errorf("wall clamp = %+v err=%v, want %d", v.wall, err, budgetWallCeilingSeconds)
	}
	small := 90
	v, _ = validateCreateJob(CreateJobParams{Title: "t", Prompt: "p", WallSeconds: &small})
	if v.wall.Int32 != 90 {
		t.Errorf("wall = %d, want 90 unchanged", v.wall.Int32)
	}
}

func TestJobRunRefusesPauseExtendAndCredentialPin(t *testing.T) {
	job := store.Run{Status: "running", Kind: runkind.Job}

	err := pauseRefusalReason(job)
	if !errors.Is(err, ErrPauseNotSupported) || err.Error() != "job runs never park; cancel the job instead" {
		t.Errorf("pauseRefusalReason(job) = %v", err)
	}
	err = extendRefusalReason(job, 7200, 57600)
	if !errors.Is(err, ErrExtendNotTimed) || !strings.Contains(err.Error(), "job") {
		t.Errorf("extendRefusalReason(job) = %v, want ErrExtendNotTimed naming the job", err)
	}
	if err := extendRefusalReason(store.Run{Status: "failed", Kind: runkind.Job}, 7200, 57600); !errors.Is(err, ErrRunTerminal) {
		t.Errorf("terminal job extend = %v, want ErrRunTerminal", err)
	}

	for _, mode := range []string{CredentialOverrideModeInherit, CredentialOverrideModeAuto, CredentialOverrideModeDefault, CredentialOverrideModePinned} {
		id := uuid.New()
		if _, err := validateCredentialOverrideOn(context.Background(), nil, uuid.New(), runkind.Job, harnessClaude, mode, &id); !errors.Is(err, ErrCredentialOverrideLaneNotSwitchable) {
			t.Errorf("job override mode=%s err = %v, want ErrCredentialOverrideLaneNotSwitchable", mode, err)
		}
	}
}
