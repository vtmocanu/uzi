package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// jobs.go is the service layer of the repo-less `job` run kind (PRD #1908 M2): create, the
// caller-scoped reads, the public status mapping and cancel. The HTTP surface (/api/v1/jobs) is a
// later milestone and consumes only the exported API below.
//
// Visibility is decided in SQL (queries/jobs.sql): a job is visible to a caller only when the
// caller's user owns the run and, for a product-token caller, job_origins.product_id is the
// caller's product. Anything else is indistinguishable from a missing job (ErrJobNotFound).

// Typed refusals. The recommended HTTP mapping (the handler owns it):
//
//	ErrJobInvalid          422 invalid_request (JobInvalidError names the field)
//	ErrJobTypeUnknown      422 unknown_job_type
//	ErrJobNotSupported     422 not_supported
//	IsNoModelCredential    422 no_model_credential
//	ErrJobTypeNotAllowed   403 job_type_not_allowed
//	ErrJobOverCap          429 over_cap
//	ErrJobNotFound         404 not_found
//	ErrJobTerminal         409 job_terminal
var (
	ErrJobInvalid        = errors.New("job request is invalid")
	ErrJobTypeUnknown    = errors.New("unknown job type")
	ErrJobNotSupported   = errors.New("job option is not supported")
	ErrJobTypeNotAllowed = errors.New("this product token may not create jobs of this type")
	ErrJobOverCap        = errors.New("too many active jobs")
	ErrJobNotFound       = errors.New("job not found")
	ErrJobTerminal       = errors.New("job is already finished")
)

// JobInvalidError is the field-naming ErrJobInvalid: errors.Is(err, ErrJobInvalid) holds.
type JobInvalidError struct {
	Field  string
	Reason string
}

func (e *JobInvalidError) Error() string { return "invalid " + e.Field + ": " + e.Reason }

// Is makes errors.Is(err, ErrJobInvalid) true for every JobInvalidError.
func (e *JobInvalidError) Is(target error) bool { return target == ErrJobInvalid }

func jobInvalid(field, format string, args ...any) error {
	return &JobInvalidError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Product bounds. The DB CHECKs are the backstop (job_inputs.name regex, octet caps); these are
// the tighter, friendlier service caps.
const (
	maxJobTitleBytes         = 200
	maxJobLabelBytes         = 200
	maxJobInputs             = 20
	maxJobInputTotalBytes    = 1 << 20 // 1 MiB of input content per job
	defaultJobMaxActive      = 10      // mirrors settings.DefaultJobMaxActivePerUser
	defaultJobListLimit      = 50
	maxJobListLimit          = 100
	defaultJobMessageLimit   = 200
	maxJobMessageLimit       = 500
	maxJobMessageTextBytes   = 8 << 10
	maxJobFailureReasonBytes = 500
)

// jobInputNameRE mirrors the job_inputs.name CHECK (00275); '..' is rejected separately.
var jobInputNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// JobCaller identifies who is acting on a job. ProductID nil is a uzc_ (CLI/user token) caller,
// who sees every job its user owns and may create any known type. A non-nil ProductID is a uzp_
// product-token caller: it sees only jobs originated for its product and may create only the
// types the product allows. ProductTokenID is recorded on create only for a product caller.
type JobCaller struct {
	UserID         uuid.UUID
	ProductID      *uuid.UUID
	ProductTokenID *uuid.UUID
}

func (c JobCaller) product() pgtype.UUID { return pgconv.UUIDPtr(c.ProductID) }

// JobInput is one inline text input of a job.
type JobInput struct {
	Name    string
	Content string
}

// CreateJobParams is a job create request after transport decoding.
type CreateJobParams struct {
	Caller           JobCaller
	JobType          string
	Title            string
	Prompt           string
	Inputs           []JobInput
	RequestedByLabel *string
	// WallSeconds is the optional wall-clock limit; nil uses the instance default. Values above
	// the 8h ceiling are clamped to it.
	WallSeconds *int
	// EgressProfile is a presence flag: a request that names an egress profile is refused with
	// ErrJobNotSupported (no egress control exists yet).
	EgressProfile bool
}

// JobView is the public projection of a job run. Status is the public vocabulary
// (queued|running|waiting|completed|failed|cancelled), never the raw runs.status.
type JobView struct {
	ID                uuid.UUID
	JobType           string
	Status            string
	Title             string
	FailOrigin        *string
	FailureReason     *string
	BudgetWallSeconds *int
	RequestedByLabel  *string
	ProductID         *uuid.UUID
	ProductTokenID    *uuid.UUID
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	UpdatedAt         time.Time
}

// JobFindingView is one structured finding of a job result.
type JobFindingView struct {
	Ordinal   int
	Severity  string
	MessageMD string
	URL       *string
	File      *string
	Line      *int
}

// JobResultView is a job's result. Ready is false while no result row exists yet (the job is
// still running or finished without reporting one); it is a value, not an error.
type JobResultView struct {
	Ready     bool
	JobStatus string
	Status    string
	ReportMD  string
	Findings  []JobFindingView
	CreatedAt time.Time
	UpdatedAt time.Time
}

// JobMessage is the narrowed, caller-safe projection of a run message: only human-readable text,
// status and error lines, secret-scrubbed and bounded. Tool inputs and outputs are never exposed.
type JobMessage struct {
	Seq       int
	CreatedAt time.Time
	Kind      string
	Text      string
}

// JobCursor is the keyset position of a job list page (created_at, id descending).
type JobCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// IsNoModelCredential reports whether err is the create-time refusal for a caller with no usable
// Anthropic credential (missing, or all disabled): the handler maps it to 422 no_model_credential.
func IsNoModelCredential(err error) bool {
	return errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrNoUsableCredential)
}

// JobPublicStatuses is the closed public status vocabulary JobStatus returns, in lifecycle
// order. api/openapi/v1.yaml enumerates exactly these; TestV1OpenAPIEnumsMatchGo binds the two.
var JobPublicStatuses = []string{"queued", "running", "waiting", "completed", "failed", "cancelled"}

// JobStatus maps a raw runs.status to the public job status vocabulary. The mapping is total over
// the runs_status_check catalog (pinned by TestJobStatusCoversStatusCatalogLiveDB); an unknown
// value maps to "running" (never terminal) so a future status cannot read as finished.
func JobStatus(runStatus string) string {
	switch runStatus {
	case "queued":
		return "queued"
	case "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup":
		return "running"
	// A job never parks (PRD #1908 D-E): a disabled credential, a usage or time limit and a pool
	// or recovery situation each fail it or requeue it to queued instead. These four are mapped
	// defensively, so a row that somehow reaches one reads as the non-terminal "waiting" and
	// never as finished.
	case "pool_wait", "paused", "limit_wait", "recovery_wait":
		return "waiting"
	case "completed", "failed", "cancelled":
		return runStatus
	default:
		return "running"
	}
}

func jobTerminal(runStatus string) bool {
	return runStatus == "completed" || runStatus == "failed" || runStatus == "cancelled"
}

// jobCapSettings is the optional reader of the per-user active-job cap; *settings.Cache satisfies
// it and is wired as the run-health settings reader.
type jobCapSettings interface {
	JobMaxActivePerUser(ctx context.Context) (int, error)
}

// Compile-time proof that the production settings reader exposes the cap accessor, so the
// jobMaxActive type assertion below can never silently stop matching.
var _ jobCapSettings = (*settings.Cache)(nil)

var jobCapFallbackWarn sync.Once

func (s *Service) jobMaxActive(ctx context.Context) (int, error) {
	if r, ok := s.healthSettings.(jobCapSettings); ok && r != nil {
		n, err := r.JobMaxActivePerUser(ctx)
		if err != nil {
			return 0, err
		}
		if n < 1 {
			return defaultJobMaxActive, nil
		}
		return n, nil
	}
	jobCapFallbackWarn.Do(func() {
		slog.Warn("job active cap: settings reader does not expose JobMaxActivePerUser; using the built-in default",
			"default", defaultJobMaxActive)
	})
	return defaultJobMaxActive, nil
}

// validatedJob is a create request after validation and normalization.
type validatedJob struct {
	title  string
	prompt string
	inputs []JobInput
	label  *string
	wall   pgtype.Int4
}

func validateCreateJob(p CreateJobParams) (validatedJob, error) {
	var v validatedJob
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return v, jobInvalid("title", "is required")
	}
	if len(title) > maxJobTitleBytes {
		return v, jobInvalid("title", "must be at most %d bytes", maxJobTitleBytes)
	}
	if err := termsafe.Validate("title", title); err != nil {
		return v, jobInvalid("title", "%s", err.Error())
	}
	v.title = title

	prompt, _ := stripNUL(p.Prompt)
	if strings.TrimSpace(prompt) == "" {
		return v, jobInvalid("prompt", "is required")
	}
	if len(prompt) > MaxIssueDescriptionBytes {
		return v, jobInvalid("prompt", "must be at most %d bytes", MaxIssueDescriptionBytes)
	}
	if !utf8.ValidString(prompt) {
		return v, jobInvalid("prompt", "must be valid UTF-8")
	}
	v.prompt = prompt

	if len(p.Inputs) > maxJobInputs {
		return v, jobInvalid("inputs", "at most %d inputs are allowed", maxJobInputs)
	}
	seen := make(map[string]bool, len(p.Inputs))
	total := 0
	for i, in := range p.Inputs {
		field := fmt.Sprintf("inputs[%d].name", i)
		if !jobInputNameRE.MatchString(in.Name) || strings.Contains(in.Name, "..") {
			return v, jobInvalid(field, "must be 1-100 characters of letters, digits, '.', '_' or '-', start with a letter or digit and not contain '..'")
		}
		if seen[in.Name] {
			return v, jobInvalid(field, "duplicates an earlier input name")
		}
		seen[in.Name] = true
		content, _ := stripNUL(in.Content)
		if !utf8.ValidString(content) {
			return v, jobInvalid(fmt.Sprintf("inputs[%d].content", i), "must be valid UTF-8")
		}
		total += len(content)
		if total > maxJobInputTotalBytes {
			return v, jobInvalid("inputs", "total input content must be at most %d bytes", maxJobInputTotalBytes)
		}
		v.inputs = append(v.inputs, JobInput{Name: in.Name, Content: content})
	}

	if p.RequestedByLabel != nil {
		label := strings.TrimSpace(*p.RequestedByLabel)
		if label != "" {
			if len(label) > maxJobLabelBytes {
				return v, jobInvalid("requested_by_label", "must be at most %d bytes", maxJobLabelBytes)
			}
			if err := termsafe.Validate("requested_by_label", label); err != nil {
				return v, jobInvalid("requested_by_label", "%s", err.Error())
			}
			v.label = &label
		}
	}

	if p.WallSeconds != nil {
		if *p.WallSeconds < 1 {
			return v, jobInvalid("wall_seconds", "must be positive")
		}
		w := *p.WallSeconds
		if w > budgetWallCeilingSeconds {
			w = budgetWallCeilingSeconds
		}
		v.wall = pgtype.Int4{Int32: int32(w), Valid: true}
	}
	return v, nil
}

func knownJobType(t string) bool {
	for _, k := range runkind.JobTypes() {
		if k == t {
			return true
		}
	}
	return false
}

// errJobNoTransaction: a live store without a transaction beginner cannot create a job atomically.
var errJobNoTransaction = errors.New("workersvc: job create needs a transaction beginner")

// CreateJobRun creates a queued job run for the caller. Validation runs before any transaction;
// the harness resolution (an EXPLICIT Claude pin: a job is Claude-only, never falling back to
// Codex), the per-user advisory lock, the active-job cap, the product allow-list, the run INSERT,
// its inputs and its origin all commit in ONE transaction, so the cap cannot be raced past.
func (s *Service) CreateJobRun(ctx context.Context, p CreateJobParams) (JobView, error) {
	if p.EgressProfile {
		return JobView{}, fmt.Errorf("%w: egress_profile", ErrJobNotSupported)
	}
	if !knownJobType(p.JobType) {
		return JobView{}, ErrJobTypeUnknown
	}
	v, err := validateCreateJob(p)
	if err != nil {
		return JobView{}, err
	}
	if p.Caller.ProductID != nil && p.Caller.ProductTokenID == nil {
		// The revoke sweep keys on the token id; a product job without one could never be revoked.
		return JobView{}, jobInvalid("caller", "a product caller requires its token id")
	}
	if _, live := s.q.(*store.Queries); live && s.txBeginner == nil {
		// Without a transaction createRunAtomic's no-tx branch would split the advisory lock, the
		// cap check and the inserts apart. Fail closed rather than lose the cap's atomicity.
		return JobView{}, errJobNoTransaction
	}
	maxActive, err := s.jobMaxActive(ctx)
	if err != nil {
		return JobView{}, err
	}

	explicit := HarnessClaude
	runID := uuid.New()
	var originCreatedAt pgtype.Timestamptz
	run, err := s.createRunResolved(ctx, p.Caller.UserID, &explicit, func(q Store, resolved resolvedHarness) (store.Run, error) {
		qq, ok := q.(*store.Queries)
		if !ok {
			return store.Run{}, errHarnessStoreUnavailable
		}
		if p.Caller.ProductID != nil {
			policy, perr := qq.GetProductJobPolicy(ctx, *p.Caller.ProductID)
			if perr != nil {
				if errors.Is(perr, pgx.ErrNoRows) {
					return store.Run{}, ErrJobTypeNotAllowed
				}
				return store.Run{}, perr
			}
			allowed := false
			for _, t := range policy {
				if t == p.JobType {
					allowed = true
				}
			}
			if !allowed {
				return store.Run{}, ErrJobTypeNotAllowed
			}
		}
		if err := qq.LockJobCreate(ctx, p.Caller.UserID); err != nil {
			return store.Run{}, err
		}
		active, err := qq.CountActiveJobRunsForUser(ctx, p.Caller.UserID)
		if err != nil {
			return store.Run{}, err
		}
		if active >= int64(maxActive) {
			return store.Run{}, ErrJobOverCap
		}
		run, err := qq.CreateJobRun(ctx, store.CreateJobRunParams{
			RunID:             runID,
			UserID:            p.Caller.UserID,
			JobType:           pgconv.TextOrNull(p.JobType),
			IssueTitle:        v.title,
			IssueDescription:  v.prompt,
			BudgetWallSeconds: v.wall,
			Harness:           string(resolved.Harness),
		})
		if err != nil {
			return store.Run{}, err
		}
		for i, in := range v.inputs {
			if err := qq.CreateJobInput(ctx, store.CreateJobInputParams{
				RunID: run.ID, Ordinal: int32(i), Name: in.Name, ContentMd: in.Content,
			}); err != nil {
				return store.Run{}, err
			}
		}
		// The origin's product columns are set ONLY for a product-token caller.
		origin := store.CreateJobOriginParams{RunID: run.ID, RequestedByLabel: pgconv.TextPtr(v.label)}
		if p.Caller.ProductID != nil {
			origin.ProductID = pgconv.UUIDPtr(p.Caller.ProductID)
			origin.ProductTokenID = pgconv.UUIDPtr(p.Caller.ProductTokenID)
		}
		originCreatedAt, err = qq.CreateJobOrigin(ctx, origin)
		return run, err
	})
	if err != nil {
		return JobView{}, err
	}
	s.notify(run.ID, "queued")
	logRunCreated(run)
	view := JobView{
		ID:                run.ID,
		JobType:           p.JobType,
		Status:            JobStatus(run.Status),
		Title:             run.IssueTitle,
		BudgetWallSeconds: intPtr(run.BudgetWallSeconds),
		RequestedByLabel:  v.label,
		CreatedAt:         run.CreatedAt.Time,
		UpdatedAt:         run.UpdatedAt.Time,
	}
	if p.Caller.ProductID != nil {
		view.ProductID = p.Caller.ProductID
		view.ProductTokenID = p.Caller.ProductTokenID
	}
	if view.CreatedAt.IsZero() {
		view.CreatedAt = originCreatedAt.Time
	}
	return view, nil
}

func timePtrTz(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func uuidPtrPg(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	v := uuid.UUID(u.Bytes)
	return &v
}

func jobViewFromRow(f store.ListJobsForCallerRow) JobView {
	v := JobView{
		ID:                f.ID,
		JobType:           f.JobType.String,
		Status:            JobStatus(f.Status),
		Title:             f.Title,
		FailOrigin:        textPtr(f.FailOrigin),
		BudgetWallSeconds: intPtr(f.BudgetWallSeconds),
		RequestedByLabel:  textPtr(f.RequestedByLabel),
		ProductID:         uuidPtrPg(f.ProductID),
		ProductTokenID:    uuidPtrPg(f.ProductTokenID),
		CreatedAt:         f.CreatedAt.Time,
		StartedAt:         timePtrTz(f.StartedAt),
		FinishedAt:        timePtrTz(f.FinishedAt),
		UpdatedAt:         f.UpdatedAt.Time,
	}
	if f.FailureReason.Valid {
		// failure_reason is server-authored but can quote worker text: scrub, then bound.
		r := scrubThenBound(f.FailureReason.String, maxJobFailureReasonBytes)
		v.FailureReason = &r
	}
	return v
}

// jobStore is the caller-scoped read surface; *store.Queries satisfies it.
type jobStore interface {
	GetJobForCaller(ctx context.Context, arg store.GetJobForCallerParams) (store.GetJobForCallerRow, error)
	ListJobsForCaller(ctx context.Context, arg store.ListJobsForCallerParams) ([]store.ListJobsForCallerRow, error)
	GetJobResultForCaller(ctx context.Context, arg store.GetJobResultForCallerParams) (store.GetJobResultForCallerRow, error)
	ListJobFindingsForCaller(ctx context.Context, arg store.ListJobFindingsForCallerParams) ([]store.ListJobFindingsForCallerRow, error)
	ListJobMessagesForCaller(ctx context.Context, arg store.ListJobMessagesForCallerParams) ([]store.ListJobMessagesForCallerRow, error)
}

func (s *Service) jobReads() (jobStore, error) {
	q, ok := s.q.(jobStore)
	if !ok {
		return nil, errHarnessStoreUnavailable
	}
	return q, nil
}

// GetJobForCaller returns one job visible to the caller, else ErrJobNotFound.
func (s *Service) GetJobForCaller(ctx context.Context, caller JobCaller, runID uuid.UUID) (JobView, error) {
	q, err := s.jobReads()
	if err != nil {
		return JobView{}, err
	}
	row, err := q.GetJobForCaller(ctx, store.GetJobForCallerParams{RunID: runID, UserID: caller.UserID, ProductID: caller.product()})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobView{}, ErrJobNotFound
		}
		return JobView{}, err
	}
	return jobViewFromRow(store.ListJobsForCallerRow(row)), nil
}

// ListJobsForCaller returns one page of the caller's visible jobs, newest first, plus the cursor
// of the next page (nil when this is the last). cursor nil starts at the newest job.
func (s *Service) ListJobsForCaller(ctx context.Context, caller JobCaller, cursor *JobCursor, limit int) ([]JobView, *JobCursor, error) {
	q, err := s.jobReads()
	if err != nil {
		return nil, nil, err
	}
	if limit <= 0 {
		limit = defaultJobListLimit
	}
	if limit > maxJobListLimit {
		limit = maxJobListLimit
	}
	params := store.ListJobsForCallerParams{UserID: caller.UserID, ProductID: caller.product(), Lim: int32(limit) + 1}
	if cursor != nil {
		params.CursorCreatedAt = pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}
		params.CursorID = pgconv.UUID(cursor.ID)
	}
	rows, err := q.ListJobsForCaller(ctx, params)
	if err != nil {
		return nil, nil, err
	}
	var next *JobCursor
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = &JobCursor{CreatedAt: last.CreatedAt.Time, ID: last.ID}
	}
	out := make([]JobView, 0, len(rows))
	for _, r := range rows {
		out = append(out, jobViewFromRow(r))
	}
	return out, next, nil
}

// GetJobResult returns the job's result and findings. A visible job with no result row yet yields
// JobResultView{Ready: false} and a nil error; an invisible job is ErrJobNotFound.
func (s *Service) GetJobResult(ctx context.Context, caller JobCaller, runID uuid.UUID) (JobResultView, error) {
	job, err := s.GetJobForCaller(ctx, caller, runID)
	if err != nil {
		return JobResultView{}, err
	}
	q, err := s.jobReads()
	if err != nil {
		return JobResultView{}, err
	}
	res, err := q.GetJobResultForCaller(ctx, store.GetJobResultForCallerParams{RunID: runID, UserID: caller.UserID, ProductID: caller.product()})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JobResultView{Ready: false, JobStatus: job.Status}, nil
		}
		return JobResultView{}, err
	}
	rows, err := q.ListJobFindingsForCaller(ctx, store.ListJobFindingsForCallerParams{RunID: runID, UserID: caller.UserID, ProductID: caller.product()})
	if err != nil {
		return JobResultView{}, err
	}
	findings := make([]JobFindingView, 0, len(rows))
	for _, f := range rows {
		fv := JobFindingView{Ordinal: int(f.Ordinal), Severity: f.Severity, MessageMD: f.MessageMd, URL: textPtr(f.Url), File: textPtr(f.File)}
		if f.Line.Valid {
			n := int(f.Line.Int32)
			fv.Line = &n
		}
		findings = append(findings, fv)
	}
	return JobResultView{
		Ready:     true,
		JobStatus: job.Status,
		Status:    res.Status,
		ReportMD:  res.ReportMd,
		Findings:  findings,
		CreatedAt: res.CreatedAt.Time,
		UpdatedAt: res.UpdatedAt.Time,
	}, nil
}

// ListJobMessages returns up to limit of the job's caller-safe messages after afterSeq. Only the
// text/status/error kinds are selected in SQL, and each text is secret-scrubbed and bounded again
// here; tool payloads never reach the caller.
func (s *Service) ListJobMessages(ctx context.Context, caller JobCaller, runID uuid.UUID, afterSeq int32, limit int) ([]JobMessage, error) {
	if _, err := s.GetJobForCaller(ctx, caller, runID); err != nil {
		return nil, err
	}
	q, err := s.jobReads()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultJobMessageLimit
	}
	if limit > maxJobMessageLimit {
		limit = maxJobMessageLimit
	}
	rows, err := q.ListJobMessagesForCaller(ctx, store.ListJobMessagesForCallerParams{
		RunID: runID, AfterSeq: afterSeq, UserID: caller.UserID, ProductID: caller.product(), Lim: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]JobMessage, 0, len(rows))
	for _, m := range rows {
		out = append(out, JobMessage{
			Seq:       int(m.Seq),
			CreatedAt: m.CreatedAt.Time,
			Kind:      m.Kind,
			Text:      scrubThenBound(m.Text, maxJobMessageTextBytes),
		})
	}
	return out, nil
}

// CancelJob cancels a job the caller can see, through the same path as POST /api/runs/{id}/inputs
// kind=cancel. A finished job is ErrJobTerminal. A queued job ends cancelled immediately; a running
// one is cancelled asynchronously by its worker, so the returned view may still read running.
func (s *Service) CancelJob(ctx context.Context, caller JobCaller, runID uuid.UUID) (JobView, error) {
	job, err := s.GetJobForCaller(ctx, caller, runID)
	if err != nil {
		return JobView{}, err
	}
	if jobTerminal(job.Status) {
		return JobView{}, ErrJobTerminal
	}
	if _, err := s.SubmitInputWithOptions(ctx, caller.UserID, runID, "cancel", "", nil, SubmitInputOptions{}); err != nil {
		switch {
		case errors.Is(err, ErrRunTerminal):
			return JobView{}, ErrJobTerminal
		case errors.Is(err, ErrRunNotFound):
			return JobView{}, ErrJobNotFound
		case errors.Is(err, ErrOutcomePendingConfirmationRequired):
			// The job already journaled a terminal outcome awaiting delivery: it has finished its
			// work, so a cancel is refused as terminal rather than discarding that outcome.
			return JobView{}, ErrJobTerminal
		}
		return JobView{}, err
	}
	return s.GetJobForCaller(ctx, caller, runID)
}

// maxJobDetailInputs bounds the inputs listed on the run detail (the create cap is maxJobInputs;
// the DB has no row-count CHECK, so the read bounds itself).
const maxJobDetailInputs = 50

// JobInputSize is one input of a job as the run detail shows it: the name and the content's byte
// size, never the content.
type JobInputSize struct {
	Name      string
	SizeBytes int
}

// JobRunResult is a job's stored result for the run detail.
type JobRunResult struct {
	Status   string
	ReportMD string
	Findings []JobFindingView
}

// JobRunDetail is the job-specific block of the cookie run detail (PRD #1908 D-D). It is read
// AFTER the caller passed the run-read authorization (owner or admin), so nothing here is
// caller-scoped in SQL. Result is nil while no result row exists.
type JobRunDetail struct {
	JobType          string
	Inputs           []JobInputSize
	RequestedByLabel *string
	ProductName      *string
	Result           *JobRunResult
}

// jobDetailStore is the run-detail read surface; *store.Queries satisfies it.
type jobDetailStore interface {
	GetJobOriginForRun(ctx context.Context, runID uuid.UUID) (store.GetJobOriginForRunRow, error)
	ListJobInputSizesForRun(ctx context.Context, arg store.ListJobInputSizesForRunParams) ([]store.ListJobInputSizesForRunRow, error)
	GetJobResultForRun(ctx context.Context, runID uuid.UUID) (store.GetJobResultForRunRow, error)
	ListJobFindingsForRun(ctx context.Context, runID uuid.UUID) ([]store.ListJobFindingsForRunRow, error)
}

// RunJobDetail loads the job block for a kind='job' run the caller may already read. A run of
// another kind returns (nil, nil). The result's free text was scrubbed at ingest.
func (s *Service) RunJobDetail(ctx context.Context, run store.Run) (*JobRunDetail, error) {
	if run.Kind != runkind.Job {
		return nil, nil
	}
	q, ok := s.q.(jobDetailStore)
	if !ok {
		return nil, errHarnessStoreUnavailable
	}
	d := &JobRunDetail{JobType: run.JobType.String, Inputs: []JobInputSize{}}
	origin, err := q.GetJobOriginForRun(ctx, run.ID)
	switch {
	case err == nil:
		d.RequestedByLabel = textPtr(origin.RequestedByLabel)
		d.ProductName = textPtr(origin.ProductName)
	case errors.Is(err, pgx.ErrNoRows):
		// A job row without an origin cannot be created through CreateJobRun; render it bare.
	default:
		return nil, err
	}
	inputs, err := q.ListJobInputSizesForRun(ctx, store.ListJobInputSizesForRunParams{RunID: run.ID, Lim: maxJobDetailInputs})
	if err != nil {
		return nil, err
	}
	for _, in := range inputs {
		d.Inputs = append(d.Inputs, JobInputSize{Name: in.Name, SizeBytes: int(in.SizeBytes)})
	}
	res, err := q.GetJobResultForRun(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.ListJobFindingsForRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	findings := make([]JobFindingView, 0, len(rows))
	for _, f := range rows {
		fv := JobFindingView{Ordinal: int(f.Ordinal), Severity: f.Severity, MessageMD: f.MessageMd, URL: textPtr(f.Url), File: textPtr(f.File)}
		if f.Line.Valid {
			n := int(f.Line.Int32)
			fv.Line = &n
		}
		findings = append(findings, fv)
	}
	d.Result = &JobRunResult{Status: res.Status, ReportMD: res.ReportMd, Findings: findings}
	return d, nil
}
