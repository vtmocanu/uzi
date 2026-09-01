// Package forgetest provides shared test-support scaffolding for the
// forge.Forge driver contract. Its centrepiece is BaseFake: an embeddable
// implementation of all 25 Forge methods with safe defaults, so a hand-written
// test fake can embed it and override only the methods it actually exercises.
//
// The point is the interface-change tax. Adding a method to forge.Forge used to
// cost 3 drivers + 6 hand-written fakes; with BaseFake it costs 3 drivers +
// BaseFake, and the six fakes inherit the new method for free.
//
// Default semantics are deliberately loud, not silent. An unstubbed method
// returns notStubbed(<name>) — an error that wraps ErrNotStubbed and names the
// method — so a fake can never silently hide a code-path change behind a
// zero-value success. The single exception is the two pipeline reads
// (LatestPipeline, LatestMRPipeline): they return forge.ErrNoPipeline because
// consumers legitimately branch on that absence as a non-error, and all six
// existing fakes already return it. That behavior is preserved, not stubbed.
package forgetest

import (
	"context"
	"errors"
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// ErrNotStubbed is the sentinel every unstubbed BaseFake method wraps. Callers
// can both test errors.Is(err, ErrNotStubbed) and read the offending method
// name out of the error string (see notStubbed).
var ErrNotStubbed = errors.New("forgetest: method not stubbed")

// notStubbed builds the error an unstubbed method returns: it names the method
// and wraps ErrNotStubbed, so errors.Is still matches while the message tells a
// reader exactly which method a fake forgot to override.
func notStubbed(method string) error {
	return fmt.Errorf("forgetest: method %s not stubbed: %w", method, ErrNotStubbed)
}

// BaseFake is an empty struct that implements the entire forge.Forge interface
// (all 25 methods) with safe defaults, designed to be embedded by value in a
// hand-written test fake used through a pointer. Every method has a pointer
// receiver so method promotion works when the embedder is used as *fakeForge.
//
// Every method returns notStubbed(<name>) for its error, except the two
// pipeline reads which return forge.ErrNoPipeline (the established absence
// sentinel consumers branch on). Override only the methods a given test drives.
type BaseFake struct{}

// Compile-time assertion that BaseFake satisfies the full driver contract. This
// is NOT a call, so it creates no reachability for deadcode analysis — the
// table-driven test in basefake_test.go is what invokes every method.
var _ forge.Forge = (*BaseFake)(nil)

// VerifyToken implements forge.Forge.
func (*BaseFake) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{}, notStubbed("VerifyToken")
}

// ListProjects implements forge.Forge.
func (*BaseFake) ListProjects(context.Context) ([]forge.Project, error) {
	return nil, notStubbed("ListProjects")
}

// ListLabels implements forge.Forge.
func (*BaseFake) ListLabels(context.Context, int64) ([]forge.Label, error) {
	return nil, notStubbed("ListLabels")
}

// EnsureLabels implements forge.Forge.
func (*BaseFake) EnsureLabels(context.Context, int64, []forge.Label) error {
	return notStubbed("EnsureLabels")
}

// ListIssues implements forge.Forge.
func (*BaseFake) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return nil, notStubbed("ListIssues")
}

// GetIssue implements forge.Forge.
func (*BaseFake) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, notStubbed("GetIssue")
}

// CreateIssue implements forge.Forge.
func (*BaseFake) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, notStubbed("CreateIssue")
}

// UpdateIssueLabels implements forge.Forge.
func (*BaseFake) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return notStubbed("UpdateIssueLabels")
}

// UpdateIssueDescription implements forge.Forge.
func (*BaseFake) UpdateIssueDescription(context.Context, int64, int64, string) error {
	return notStubbed("UpdateIssueDescription")
}

// UserExists implements forge.Forge.
func (*BaseFake) UserExists(context.Context, string) (bool, error) {
	return false, notStubbed("UserExists")
}

// ListIssueLabelEvents implements forge.Forge.
func (*BaseFake) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, notStubbed("ListIssueLabelEvents")
}

// ListIssueComments implements forge.Forge.
func (*BaseFake) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, notStubbed("ListIssueComments")
}

// CreateIssueNote implements forge.Forge.
func (*BaseFake) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, notStubbed("CreateIssueNote")
}

// GetMergeRequest implements forge.Forge.
func (*BaseFake) GetMergeRequest(context.Context, int64, int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{}, notStubbed("GetMergeRequest")
}

// ListMergeRequestComments implements forge.Forge.
func (*BaseFake) ListMergeRequestComments(context.Context, int64, int64) ([]forge.MRComment, error) {
	return nil, notStubbed("ListMergeRequestComments")
}

// ReplyMergeRequestComment implements forge.Forge.
func (*BaseFake) ReplyMergeRequestComment(context.Context, int64, int64, string, string) error {
	return notStubbed("ReplyMergeRequestComment")
}

// ResolveMergeRequestThread implements forge.Forge.
func (*BaseFake) ResolveMergeRequestThread(context.Context, int64, int64, string) error {
	return notStubbed("ResolveMergeRequestThread")
}

// TokenInfo implements forge.Forge.
func (*BaseFake) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return forge.TokenInfo{}, notStubbed("TokenInfo")
}

// ProjectRole implements forge.Forge.
func (*BaseFake) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleNone, false, notStubbed("ProjectRole")
}

// DefaultBranchProtection implements forge.Forge.
func (*BaseFake) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{}, notStubbed("DefaultBranchProtection")
}

// LatestPipeline implements forge.Forge. It returns forge.ErrNoPipeline (the
// established absence sentinel), NOT ErrNotStubbed: consumers branch on "no
// pipeline" as a non-error, and every existing fake already returns this.
func (*BaseFake) LatestPipeline(context.Context, int64, string) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}

// LatestMRPipeline implements forge.Forge. Like LatestPipeline it returns
// forge.ErrNoPipeline rather than ErrNotStubbed (documented absence exception).
func (*BaseFake) LatestMRPipeline(context.Context, int64, int64) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}

// ListPipelineJobs implements forge.Forge.
func (*BaseFake) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return nil, notStubbed("ListPipelineJobs")
}

// JobLogTail implements forge.Forge.
func (*BaseFake) JobLogTail(context.Context, int64, int64, int) (string, error) {
	return "", notStubbed("JobLogTail")
}

// ProjectCIConfigPath implements forge.Forge.
func (*BaseFake) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", notStubbed("ProjectCIConfigPath")
}
