package workersvc

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/settings"
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

	unknown := base
	unknown.JobType = "nope"
	if _, err := svc.CreateJobRun(ctx, unknown); !errors.Is(err, ErrJobTypeUnknown) {
		t.Errorf("unknown type err = %v, want ErrJobTypeUnknown", err)
	}
	prod := base
	pid := uuid.New()
	prod.Caller.ProductID = &pid
	if _, err := svc.CreateJobRun(ctx, prod); !errors.Is(err, ErrJobInvalid) {
		t.Errorf("product caller without token id err = %v, want ErrJobInvalid", err)
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
	v, err := validateDefaultCreateJob(ok)
	if err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	if v.label == nil || *v.label != "alice@example.com" {
		t.Errorf("label = %v, want trimmed", v.label)
	}

	// NUL is stripped from the prompt and input content rather than refused.
	v, err = validateDefaultCreateJob(CreateJobParams{Title: "t", Prompt: "a\x00b", Inputs: []JobInput{{Name: "a.txt", Content: "x\x00y"}}})
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
		{"bidi title", CreateJobParams{Title: "a\u202eb", Prompt: "p"}, "title"},
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
			_, err := validateDefaultCreateJob(tc.p)
			var inv *JobInvalidError
			if !errors.Is(err, ErrJobInvalid) || !errors.As(err, &inv) {
				t.Fatalf("err = %v, want ErrJobInvalid", err)
			}
			if inv.Field != tc.field {
				t.Errorf("field = %q, want %q (%v)", inv.Field, tc.field, err)
			}
		})
	}

	// The wall clock is clamped to the configured 24h ceiling, not refused.
	huge := 24 * 3600 * 10
	v, err = validateDefaultCreateJob(CreateJobParams{Title: "t", Prompt: "p", WallSeconds: &huge})
	if err != nil || !v.wall.Valid || int(v.wall.Int32) != 24*3600 {
		t.Errorf("wall clamp = %+v err=%v, want %d", v.wall, err, 24*3600)
	}
	small := 90
	v, _ = validateDefaultCreateJob(CreateJobParams{Title: "t", Prompt: "p", WallSeconds: &small})
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

// TestDefaultJobMaxActiveMatchesSettings ties the service's fallback to the settings default.
func TestDefaultJobMaxActiveMatchesSettings(t *testing.T) {
	n, err := strconv.Atoi(settings.DefaultJobMaxActivePerUser)
	if err != nil {
		t.Fatal(err)
	}
	if n != defaultJobMaxActive {
		t.Errorf("defaultJobMaxActive = %d, settings default = %d", defaultJobMaxActive, n)
	}
}

// TestCreateJobRunRefusesWithoutTransaction pins the fail-closed guard: a live store without a
// transaction beginner must refuse rather than split the cap lock from the inserts.
func TestCreateJobRunRefusesWithoutTransaction(t *testing.T) {
	svc := &Service{q: store.New(nil)}
	p := CreateJobParams{Caller: JobCaller{UserID: uuid.New()}, JobType: runkind.JobTypeResearch, Title: "t", Prompt: "p"}
	if _, err := svc.CreateJobRun(context.Background(), p); !errors.Is(err, errJobNoTransaction) {
		t.Fatalf("err = %v, want errJobNoTransaction", err)
	}
}

func validateDefaultCreateJob(p CreateJobParams) (validatedJob, error) {
	return validateCreateJob(p, 24*3600, 6*3600)
}

func TestJobWallDefaultsAndConfiguredCeiling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requested     *int
		ceiling, want int32
	}{
		{"default 12h", nil, 24 * 3600, 12 * 3600},
		{"caller 1h", func() *int { n := 3600; return &n }(), 24 * 3600, 3600},
		{"30h capped at 24h", func() *int { n := 30 * 3600; return &n }(), 24 * 3600, 24 * 3600},
		{"10h ceiling caps default", nil, 10 * 3600, 10 * 3600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := validateCreateJob(CreateJobParams{Title: "t", Prompt: "p", WallSeconds: tc.requested}, tc.ceiling, 6*3600)
			if err != nil || !v.wall.Valid || v.wall.Int32 != tc.want {
				t.Fatalf("wall=%v err=%v want=%d", v.wall, err, tc.want)
			}
		})
	}
}

func TestJobImplicitDefaultPreservesLongBase(t *testing.T) {
	for _, tc := range []struct{ base, ceiling, want int32 }{{16 * 3600, 24 * 3600, 16 * 3600}, {30 * 3600, 30 * 3600, 30 * 3600}, {100 * 3600, 72 * 3600, 100 * 3600}, {6 * 3600, 24 * 3600, 12 * 3600}, {6 * 3600, 10 * 3600, 10 * 3600}} {
		v, err := validateCreateJob(CreateJobParams{Title: "t", Prompt: "p"}, tc.ceiling, tc.base)
		if err != nil || !v.wall.Valid || v.wall.Int32 != tc.want {
			t.Fatalf("base=%d ceiling=%d wall=%v err=%v want%d", tc.base, tc.ceiling, v.wall, err, tc.want)
		}
	}
}
func TestBudgetDurationSecondsSQLRange(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want int32
	}{{24 * time.Hour, 86400}, {100 * time.Hour, 360000}, {time.Duration(math.MaxInt32) * time.Second, math.MaxInt32}, {time.Duration(math.MaxInt32)*time.Second + time.Hour, math.MaxInt32}} {
		if got := budgetDurationSeconds(tc.d); got != tc.want {
			t.Fatalf("duration=%v seconds=%d want%d", tc.d, got, tc.want)
		}
	}
}

func TestLongBaseJobCallerStillUsesCeiling(t *testing.T) {
	requested := 200 * 3600
	v, err := validateCreateJob(CreateJobParams{Title: "t", Prompt: "p", WallSeconds: &requested}, 72*3600, 100*3600)
	if err != nil || !v.wall.Valid || v.wall.Int32 != 72*3600 {
		t.Fatalf("caller wall=%v err=%v want259200", v.wall, err)
	}
}
