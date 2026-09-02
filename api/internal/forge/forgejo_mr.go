package forge

// forgejo_mr.go is the Forgejo merge-request seam: GetMergeRequest, the PR-comment
// stitch across issue-notes and pull reviews, and the reply/resolve mutations.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gitea "code.gitea.io/sdk/gitea"
)

func (f *forgejo) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return MergeRequest{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return MergeRequest{}, err
	}
	pr, _, err := c.GetPullRequest(slug.owner, slug.repo, mrIID)
	if err != nil {
		return MergeRequest{}, f.wrapErr("get merge request", err)
	}
	return toForgejoMergeRequest(pr), nil
}

// toForgejoMergeRequest maps a gitea PullRequest onto the neutral MergeRequest.
// Forgejo's PR index (json "number") is GitLab's iid, and its state vocabulary
// differs — see forgejoMRState.
func toForgejoMergeRequest(pr *gitea.PullRequest) MergeRequest {
	return MergeRequest{IID: pr.Index, State: forgejoMRState(pr), WebURL: pr.HTMLURL}
}

// forgejoMRState maps a Forgejo PR onto the neutral MRState* vocabulary. Forgejo
// says "open"/"closed" (not GitLab's "opened") and carries merged separately, so a
// merged PR is state="closed" with merged=true — verified live on 16.0.0. Merged
// therefore wins over closed. Forgejo has no "locked" lifecycle state (IsLocked is
// a separate flag, not a state), so MRStateLocked is never produced. An unknown
// state passes through verbatim; IsKnownMRState then ignores it, so a transient
// forge glitch cannot poison the MR-close watcher's baseline.
func forgejoMRState(pr *gitea.PullRequest) string {
	if pr.HasMerged {
		return MRStateMerged
	}
	switch pr.State {
	case gitea.StateOpen:
		return MRStateOpened
	case gitea.StateClosed:
		return MRStateClosed
	default:
		return string(pr.State)
	}
}

// ListMergeRequestComments returns a PR's human + review-bot comments oldest-first
// (PRD #700). Forgejo/Gitea splits MR feedback across two surfaces, so the driver
// reads both (mirroring the existing ListIssueComments / pull-review calls):
//   - ListIssueComments on the PR index → the top-level conversation notes (summary
//     state, no diff anchor). A PR shares its issue index, so this is the same call
//     ListIssueComments uses for issues.
//   - ListPullReviews → each review's summary Body (non-empty only), and
//     ListPullReviewComments per review → the inline diff comments (Path/Line,
//     ReplyID = the review-comment id, HeadSHA from the comment/review commit id).
//
// ResolveID stays EMPTY on every comment: Forgejo has no resolvable-thread concept
// this feature uses (resolve is a documented no-op — see ResolveMergeRequestThread).
// Gitea's comment/review endpoints return no forge system notes, so no D2 filter is
// needed; the driver sorts the merged list by CreatedAt for oldest-first (D8).
func (f *forgejo) ListMergeRequestComments(ctx context.Context, projectID, mrIID int64) ([]MRComment, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}

	var out []MRComment

	// Top-level conversation notes (the PR shares its issue index).
	icOpt := gitea.ListIssueCommentOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	for page := 0; ; {
		page++
		comments, resp, err := c.ListIssueComments(slug.owner, slug.repo, mrIID, icOpt)
		if err != nil {
			return nil, f.wrapErr("list merge request comments", err)
		}
		for _, cm := range comments {
			if cm == nil {
				continue
			}
			mc := MRComment{ID: cm.ID, Body: cm.Body, CreatedAt: cm.Created, ReviewState: ReviewCommentSummary}
			if cm.Poster != nil {
				mc.AuthorForgeUserID = cm.Poster.ID
				mc.AuthorUsername = cm.Poster.UserName
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		icOpt.Page = resp.NextPage
	}

	// Pull reviews: a non-empty Body is a review-summary comment; each review's
	// inline comments carry the diff anchor and the reply id.
	revOpt := gitea.ListPullReviewsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	for page := 0; ; {
		page++
		reviews, resp, err := c.ListPullReviews(slug.owner, slug.repo, mrIID, revOpt)
		if err != nil {
			return nil, f.wrapErr("list merge request comments", err)
		}
		for _, r := range reviews {
			if r == nil {
				continue
			}
			if r.Body != "" {
				mc := MRComment{ID: r.ID, Body: r.Body, CreatedAt: r.Submitted, HeadSHA: r.CommitID, ReviewState: ReviewCommentSummary}
				if r.Reviewer != nil {
					mc.AuthorForgeUserID = r.Reviewer.ID
					mc.AuthorUsername = r.Reviewer.UserName
				}
				out = append(out, mc)
			}
			reviewComments, _, err := c.ListPullReviewComments(slug.owner, slug.repo, mrIID, r.ID)
			if err != nil {
				return nil, f.wrapErr("list merge request comments", err)
			}
			for _, rc := range reviewComments {
				if rc == nil {
					continue
				}
				mc := MRComment{
					ID:          rc.ID,
					Body:        rc.Body,
					CreatedAt:   rc.Created,
					HeadSHA:     rc.CommitID,
					ReplyID:     strconv.FormatInt(rc.ID, 10),
					ReviewState: ReviewCommentInline,
				}
				if rc.Reviewer != nil {
					mc.AuthorForgeUserID = rc.Reviewer.ID
					mc.AuthorUsername = rc.Reviewer.UserName
				}
				if rc.Path != "" {
					path := rc.Path
					mc.Path = &path
				}
				if rc.LineNum != 0 {
					line := int(rc.LineNum) //nolint:gosec // G115: a review-comment line number from the forge; cannot realistically exceed int range
					mc.Line = &line
				}
				out = append(out, mc)
			}
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		revOpt.Page = resp.NextPage
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ReplyMergeRequestComment posts an in-thread reply to the review comment
// identified by replyID (a review-comment id from ListMergeRequestComments).
func (f *forgejo) ReplyMergeRequestComment(ctx context.Context, projectID, mrIID int64, replyID, body string) error {
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	commentID, err := strconv.ParseInt(strings.TrimSpace(replyID), 10, 64)
	if err != nil {
		return fmt.Errorf("forgejo: reply merge request comment: invalid reply id %q", replyID)
	}
	if _, _, err := c.CreatePullReviewCommentReply(slug.owner, slug.repo, mrIID, commentID,
		gitea.CreatePullReviewCommentReplyOptions{Body: body}); err != nil {
		return f.wrapErr("reply merge request comment", err)
	}
	return nil
}

// ResolveMergeRequestThread is a documented no-op on Forgejo: per PRD #700 this
// driver is reply-only and never resolves a thread, so it returns
// ErrResolveUnsupported for the worker to swallow. MRComment.ResolveID is
// correspondingly always empty on Forgejo, so a caller keyed on it never reaches
// here with a real anchor; the sentinel also covers a caller that tries anyway.
func (f *forgejo) ResolveMergeRequestThread(context.Context, int64, int64, string) error {
	return ErrResolveUnsupported
}
