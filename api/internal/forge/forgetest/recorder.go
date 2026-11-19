package forgetest

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// Recorder wraps a forge.Forge and appends one deterministic line per call:
// the method name plus its arguments, formatted by formatArg. It exists so a
// test can pin the exact set of forge calls a code path makes (a golden
// record, see AssertGolden) and notice when a later refactor changes it.
// Only calls are recorded, never results; the wrapped Forge supplies those.
//
// Safe for concurrent use. Calls() keeps call order.
//
// Recorder implements only forge.Forge. It hides any optional interface the
// wrapped value also implements (for example ProjectBoardSyncer), so code that
// type-asserts for one sees the Recorder as lacking it; a path that does so
// cannot be recorded through this wrapper.
type Recorder struct {
	inner forge.Forge

	mu    sync.Mutex // guards calls
	calls []string
}

var _ forge.Forge = (*Recorder)(nil)

// NewRecorder wraps inner. A test usually passes a hand-written stub that
// embeds BaseFake and overrides the methods the path under test reads.
func NewRecorder(inner forge.Forge) *Recorder { return &Recorder{inner: inner} }

// Calls returns a copy of the recorded lines in call order.
func (r *Recorder) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *Recorder) record(method string, args ...any) {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = formatArg(reflect.ValueOf(a))
	}
	line := method + "(" + strings.Join(parts, ", ") + ")"
	r.mu.Lock()
	r.calls = append(r.calls, line)
	r.mu.Unlock()
}

var timeType = reflect.TypeOf(time.Time{})

// formatArg renders v deterministically: times as RFC3339 UTC (zero time as
// "<zero>"), pointers dereferenced, structs as {Field:value ...} in field
// order, slices as [a b] (a nil slice or map as "nil", so it differs from an
// empty one), maps with sorted keys, strings quoted. A value it cannot render
// stably (unexported field, func, chan) becomes "<unformattable T>" rather
// than panicking.
func formatArg(v reflect.Value) string {
	if !v.IsValid() {
		return "<nil>"
	}
	if !v.CanInterface() {
		// Unexported struct field: reflect refuses Interface() on it.
		return "<unformattable " + v.Type().String() + ">"
	}
	if v.Type() == timeType {
		t := v.Interface().(time.Time)
		if t.IsZero() {
			return "<zero>"
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "<nil>"
		}
		return formatArg(v.Elem())
	case reflect.String:
		return fmt.Sprintf("%q", v.String())
	case reflect.Struct:
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < v.NumField(); i++ {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(v.Type().Field(i).Name + ":" + formatArg(v.Field(i)))
		}
		b.WriteString("}")
		return b.String()
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return "nil"
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = formatArg(v.Index(i))
		}
		return "[" + strings.Join(parts, " ") + "]"
	case reflect.Map:
		if v.IsNil() {
			return "nil"
		}
		type kv struct{ k, v string }
		var kvs []kv
		for it := v.MapRange(); it.Next(); {
			kvs = append(kvs, kv{formatArg(it.Key()), formatArg(it.Value())})
		}
		sort.Slice(kvs, func(i, j int) bool { return kvs[i].k < kvs[j].k })
		parts := make([]string, len(kvs))
		for i, e := range kvs {
			parts[i] = e.k + ":" + e.v
		}
		return "map[" + strings.Join(parts, " ") + "]"
	case reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Uintptr:
		// Printing these would expose an address.
		return "<unformattable " + v.Type().String() + ">"
	default:
		return fmt.Sprint(v.Interface())
	}
}

// RepositoryAuthorEligibility records the call and delegates to the wrapped Forge.
func (r *Recorder) RepositoryAuthorEligibility(ctx context.Context, a0 int64, a1 int64) (forge.AuthorEligibility, error) {
	r.record("RepositoryAuthorEligibility", a0, a1)
	return r.inner.RepositoryAuthorEligibility(ctx, a0, a1)
}

// VerifyToken records the call and delegates to the wrapped Forge.
func (r *Recorder) VerifyToken(ctx context.Context) (forge.BotIdentity, error) {
	r.record("VerifyToken")
	return r.inner.VerifyToken(ctx)
}

// GetProject records the call and delegates to the wrapped Forge.
func (r *Recorder) GetProject(ctx context.Context, a0 int64) (forge.Project, error) {
	r.record("GetProject", a0)
	return r.inner.GetProject(ctx, a0)
}

// ListProjects records the call and delegates to the wrapped Forge.
func (r *Recorder) ListProjects(ctx context.Context) ([]forge.Project, error) {
	r.record("ListProjects")
	return r.inner.ListProjects(ctx)
}

// ListLabels records the call and delegates to the wrapped Forge.
func (r *Recorder) ListLabels(ctx context.Context, a0 int64) ([]forge.Label, error) {
	r.record("ListLabels", a0)
	return r.inner.ListLabels(ctx, a0)
}

// EnsureLabels records the call and delegates to the wrapped Forge.
func (r *Recorder) EnsureLabels(ctx context.Context, a0 int64, a1 []forge.Label) error {
	r.record("EnsureLabels", a0, a1)
	return r.inner.EnsureLabels(ctx, a0, a1)
}

// ListIssues records the call and delegates to the wrapped Forge.
func (r *Recorder) ListIssues(ctx context.Context, a0 int64, a1 forge.ListIssuesOptions) ([]forge.Issue, error) {
	r.record("ListIssues", a0, a1)
	return r.inner.ListIssues(ctx, a0, a1)
}

// GetIssue records the call and delegates to the wrapped Forge.
func (r *Recorder) GetIssue(ctx context.Context, a0 int64, a1 int64) (forge.Issue, error) {
	r.record("GetIssue", a0, a1)
	return r.inner.GetIssue(ctx, a0, a1)
}

// CreateIssue records the call and delegates to the wrapped Forge.
func (r *Recorder) CreateIssue(ctx context.Context, a0 int64, a1 string, a2 string, a3 []string) (forge.Issue, error) {
	r.record("CreateIssue", a0, a1, a2, a3)
	return r.inner.CreateIssue(ctx, a0, a1, a2, a3)
}

// UpdateIssueLabels records the call and delegates to the wrapped Forge.
func (r *Recorder) UpdateIssueLabels(ctx context.Context, a0 int64, a1 int64, a2 []string, a3 []string) error {
	r.record("UpdateIssueLabels", a0, a1, a2, a3)
	return r.inner.UpdateIssueLabels(ctx, a0, a1, a2, a3)
}

// UpdateIssueDescription records the call and delegates to the wrapped Forge.
func (r *Recorder) UpdateIssueDescription(ctx context.Context, a0 int64, a1 int64, a2 string) error {
	r.record("UpdateIssueDescription", a0, a1, a2)
	return r.inner.UpdateIssueDescription(ctx, a0, a1, a2)
}

// SetIssueState records the call and delegates to the wrapped Forge.
func (r *Recorder) SetIssueState(ctx context.Context, a0 int64, a1 int64, a2 forge.IssueState) error {
	r.record("SetIssueState", a0, a1, a2)
	return r.inner.SetIssueState(ctx, a0, a1, a2)
}

// UserExists records the call and delegates to the wrapped Forge.
func (r *Recorder) UserExists(ctx context.Context, a0 string) (bool, error) {
	r.record("UserExists", a0)
	return r.inner.UserExists(ctx, a0)
}

// ListIssueLabelEvents records the call and delegates to the wrapped Forge.
func (r *Recorder) ListIssueLabelEvents(ctx context.Context, a0 int64, a1 int64) ([]forge.LabelEvent, error) {
	r.record("ListIssueLabelEvents", a0, a1)
	return r.inner.ListIssueLabelEvents(ctx, a0, a1)
}

// ListIssueComments records the call and delegates to the wrapped Forge.
func (r *Recorder) ListIssueComments(ctx context.Context, a0 int64, a1 int64) ([]forge.IssueComment, error) {
	r.record("ListIssueComments", a0, a1)
	return r.inner.ListIssueComments(ctx, a0, a1)
}

// CreateIssueNote records the call and delegates to the wrapped Forge.
func (r *Recorder) CreateIssueNote(ctx context.Context, a0 int64, a1 int64, a2 string) (forge.IssueNote, error) {
	r.record("CreateIssueNote", a0, a1, a2)
	return r.inner.CreateIssueNote(ctx, a0, a1, a2)
}

// GetMergeRequest records the call and delegates to the wrapped Forge.
func (r *Recorder) GetMergeRequest(ctx context.Context, a0 int64, a1 int64) (forge.MergeRequest, error) {
	r.record("GetMergeRequest", a0, a1)
	return r.inner.GetMergeRequest(ctx, a0, a1)
}

// ListMergeRequestComments records the call and delegates to the wrapped Forge.
func (r *Recorder) ListMergeRequestComments(ctx context.Context, a0 int64, a1 int64) ([]forge.MRComment, error) {
	r.record("ListMergeRequestComments", a0, a1)
	return r.inner.ListMergeRequestComments(ctx, a0, a1)
}

// ReplyMergeRequestComment records the call and delegates to the wrapped Forge.
func (r *Recorder) ReplyMergeRequestComment(ctx context.Context, a0 int64, a1 int64, a2 string, a3 string) error {
	r.record("ReplyMergeRequestComment", a0, a1, a2, a3)
	return r.inner.ReplyMergeRequestComment(ctx, a0, a1, a2, a3)
}

// ResolveMergeRequestThread records the call and delegates to the wrapped Forge.
func (r *Recorder) ResolveMergeRequestThread(ctx context.Context, a0 int64, a1 int64, a2 string) error {
	r.record("ResolveMergeRequestThread", a0, a1, a2)
	return r.inner.ResolveMergeRequestThread(ctx, a0, a1, a2)
}

// TokenInfo records the call and delegates to the wrapped Forge.
func (r *Recorder) TokenInfo(ctx context.Context) (forge.TokenInfo, error) {
	r.record("TokenInfo")
	return r.inner.TokenInfo(ctx)
}

// ProjectRole records the call and delegates to the wrapped Forge.
func (r *Recorder) ProjectRole(ctx context.Context, a0 int64, a1 int64) (forge.Role, bool, error) {
	r.record("ProjectRole", a0, a1)
	return r.inner.ProjectRole(ctx, a0, a1)
}

// DefaultBranchProtection records the call and delegates to the wrapped Forge.
func (r *Recorder) DefaultBranchProtection(ctx context.Context, a0 int64, a1 string, a2 int64) (forge.BranchProtection, error) {
	r.record("DefaultBranchProtection", a0, a1, a2)
	return r.inner.DefaultBranchProtection(ctx, a0, a1, a2)
}

// LatestPipeline records the call and delegates to the wrapped Forge.
func (r *Recorder) LatestPipeline(ctx context.Context, a0 int64, a1 string) (forge.Pipeline, error) {
	r.record("LatestPipeline", a0, a1)
	return r.inner.LatestPipeline(ctx, a0, a1)
}

// LatestMRPipeline records the call and delegates to the wrapped Forge.
func (r *Recorder) LatestMRPipeline(ctx context.Context, a0 int64, a1 int64) (forge.Pipeline, error) {
	r.record("LatestMRPipeline", a0, a1)
	return r.inner.LatestMRPipeline(ctx, a0, a1)
}

// ListPipelineJobs records the call and delegates to the wrapped Forge.
func (r *Recorder) ListPipelineJobs(ctx context.Context, a0 int64, a1 int64) ([]forge.Job, error) {
	r.record("ListPipelineJobs", a0, a1)
	return r.inner.ListPipelineJobs(ctx, a0, a1)
}

// ListPipelineJobAttempts records the call and delegates to the wrapped Forge.
func (r *Recorder) ListPipelineJobAttempts(ctx context.Context, a0 int64, a1 int64) ([]forge.Job, error) {
	r.record("ListPipelineJobAttempts", a0, a1)
	return r.inner.ListPipelineJobAttempts(ctx, a0, a1)
}

// JobLogTail records the call and delegates to the wrapped Forge.
func (r *Recorder) JobLogTail(ctx context.Context, a0 int64, a1 int64, a2 int) (string, error) {
	r.record("JobLogTail", a0, a1, a2)
	return r.inner.JobLogTail(ctx, a0, a1, a2)
}

// ListMergeRequestRefs records the call and delegates to the wrapped Forge.
func (r *Recorder) ListMergeRequestRefs(ctx context.Context, a0 int64, a1 forge.ListMergeRequestsOptions) ([]forge.MergeRequestRef, error) {
	r.record("ListMergeRequestRefs", a0, a1)
	return r.inner.ListMergeRequestRefs(ctx, a0, a1)
}

// GetMergeRequestSummary records the call and delegates to the wrapped Forge.
func (r *Recorder) GetMergeRequestSummary(ctx context.Context, a0 int64, a1 int64) (forge.MergeRequestSummary, error) {
	r.record("GetMergeRequestSummary", a0, a1)
	return r.inner.GetMergeRequestSummary(ctx, a0, a1)
}

// ListChecks records the call and delegates to the wrapped Forge.
func (r *Recorder) ListChecks(ctx context.Context, a0 int64, a1 string) ([]forge.Check, error) {
	r.record("ListChecks", a0, a1)
	return r.inner.ListChecks(ctx, a0, a1)
}

// ListWorkflowRuns records the call and delegates to the wrapped Forge.
func (r *Recorder) ListWorkflowRuns(ctx context.Context, a0 int64, a1 forge.ListWorkflowRunsOptions) ([]forge.WorkflowRun, error) {
	r.record("ListWorkflowRuns", a0, a1)
	return r.inner.ListWorkflowRuns(ctx, a0, a1)
}

// ListRunArtifacts records the call and delegates to the wrapped Forge.
func (r *Recorder) ListRunArtifacts(ctx context.Context, a0 int64, a1 int64) ([]forge.Artifact, error) {
	r.record("ListRunArtifacts", a0, a1)
	return r.inner.ListRunArtifacts(ctx, a0, a1)
}

// DownloadArtifact records the call and delegates to the wrapped Forge.
func (r *Recorder) DownloadArtifact(ctx context.Context, a0 int64, a1 int64, a2 int64) ([]byte, error) {
	r.record("DownloadArtifact", a0, a1, a2)
	return r.inner.DownloadArtifact(ctx, a0, a1, a2)
}

// GetWorkflowRun records the call and delegates to the wrapped Forge.
func (r *Recorder) GetWorkflowRun(ctx context.Context, a0 int64, a1 int64) (forge.WorkflowRun, error) {
	r.record("GetWorkflowRun", a0, a1)
	return r.inner.GetWorkflowRun(ctx, a0, a1)
}

// ListMergeRequestReviews records the call and delegates to the wrapped Forge.
func (r *Recorder) ListMergeRequestReviews(ctx context.Context, a0 int64, a1 int64) ([]forge.Review, error) {
	r.record("ListMergeRequestReviews", a0, a1)
	return r.inner.ListMergeRequestReviews(ctx, a0, a1)
}

// ProjectCIConfigPath records the call and delegates to the wrapped Forge.
func (r *Recorder) ProjectCIConfigPath(ctx context.Context, a0 int64) (string, error) {
	r.record("ProjectCIConfigPath", a0)
	return r.inner.ProjectCIConfigPath(ctx, a0)
}

// BranchHead records the call and delegates to the wrapped Forge.
func (r *Recorder) BranchHead(ctx context.Context, a0 int64, a1 string) (string, error) {
	r.record("BranchHead", a0, a1)
	return r.inner.BranchHead(ctx, a0, a1)
}

// RefHead records the call and delegates to the wrapped Forge.
func (r *Recorder) RefHead(ctx context.Context, a0 int64, a1 string) (string, error) {
	r.record("RefHead", a0, a1)
	return r.inner.RefHead(ctx, a0, a1)
}

// CompareAncestry records the call and delegates to the wrapped Forge.
func (r *Recorder) CompareAncestry(ctx context.Context, a0 int64, a1 string, a2 string) (forge.Ancestry, error) {
	r.record("CompareAncestry", a0, a1, a2)
	return r.inner.CompareAncestry(ctx, a0, a1, a2)
}
