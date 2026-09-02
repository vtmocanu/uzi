package forge

// github_mr.go is the GitHub merge-request seam: GetMergeRequest, the PR-comment
// stitch and its review-thread GraphQL joins, and the reply/resolve mutations.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v90/github"
)

func (g *github) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return MergeRequest{}, err
	}
	pr, _, err := g.client.PullRequests.Get(ctx, slug.owner, slug.repo, int(mrIID))
	if err != nil {
		return MergeRequest{}, g.wrapErr("get merge request", err)
	}
	return toGitHubMergeRequest(pr), nil
}

// toGitHubMergeRequest maps a go-github PullRequest onto the neutral MergeRequest.
// GitHub's PR number is GitLab's iid — see githubMRState for the state fold.
func toGitHubMergeRequest(pr *gh.PullRequest) MergeRequest {
	return MergeRequest{IID: int64(pr.GetNumber()), State: githubMRState(pr), WebURL: pr.GetHTMLURL()}
}

// githubMRState folds GitHub's PR lifecycle onto the neutral MRState* vocabulary.
// GitHub uses state open/closed with a SEPARATE `merged` bool — not a distinct
// merged state — so a merged PR is state="closed", merged=true. Merged therefore
// WINS over closed (mirrors forgejoMRState). An unknown state passes through
// verbatim; IsKnownMRState then ignores it, so a transient glitch cannot poison
// the MR-close watcher's baseline.
func githubMRState(pr *gh.PullRequest) string {
	if pr.GetMerged() {
		return MRStateMerged
	}
	switch pr.GetState() {
	case "open":
		return MRStateOpened
	case "closed":
		return MRStateClosed
	default:
		return pr.GetState()
	}
}

// ListMergeRequestComments returns a PR's human + review-bot comments oldest-first
// (PRD #700). GitHub has no single endpoint for this, so the driver STITCHES three
// REST sources with a GraphQL read (Resolved facts):
//   - Issues.ListComments → the PR's top-level conversation notes (summary state,
//     no diff anchor, no reply/resolve thread).
//   - PullRequests.ListComments → inline review comments, each carrying Path/Line,
//     a REST ID that IS the databaseId (the reply anchor), and CommitID (HeadSHA).
//   - PullRequests.ListReviews → review-summary bodies (non-empty Body only).
//   - the GraphQL reviewThreads query → each thread's node id (the RESOLVE anchor)
//     joined to the REST inline comments on the shared databaseId.
//
// So an inline comment carries TWO anchors: ReplyID (REST databaseId) for
// CreateCommentInReplyTo and ResolveID (GraphQL thread node id) for
// resolveReviewThread. GitHub's issue/PR-comment and review endpoints return no
// forge system notes (those live on the separate events/timeline endpoints uzi
// never calls), so no D2 filter is needed; the driver sorts the merged list by
// CreatedAt for oldest-first (D8).
func (g *github) ListMergeRequestComments(ctx context.Context, projectID, mrIID int64) ([]MRComment, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	number := int(mrIID)

	var out []MRComment

	// Source A: top-level PR conversation notes.
	icOpt := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	for page := 0; ; {
		page++
		comments, resp, err := g.client.Issues.ListComments(ctx, slug.owner, slug.repo, number, icOpt)
		if err != nil {
			return nil, g.wrapErr("list merge request comments", err)
		}
		for _, c := range comments {
			if c == nil {
				continue
			}
			mc := MRComment{
				ID:          c.GetID(), // REST databaseId of the issue comment
				Body:        c.GetBody(),
				CreatedAt:   c.GetCreatedAt().Time,
				ReviewState: ReviewCommentSummary,
			}
			if u := c.GetUser(); u != nil {
				mc.AuthorForgeUserID = u.GetID()
				mc.AuthorUsername = u.GetLogin()
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		icOpt.Page = resp.NextPage
	}

	// Source B: inline review comments. Record where each databaseId landed in out
	// so the GraphQL stitch can set its ResolveID.
	idxByDBID := map[int64]int{}
	prOpt := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	for page := 0; ; {
		page++
		comments, resp, err := g.client.PullRequests.ListComments(ctx, slug.owner, slug.repo, number, prOpt)
		if err != nil {
			return nil, g.wrapErr("list merge request comments", err)
		}
		for _, c := range comments {
			if c == nil {
				continue
			}
			mc := MRComment{
				ID:          c.GetID(), // REST databaseId of the inline review comment
				Body:        c.GetBody(),
				CreatedAt:   c.GetCreatedAt().Time,
				HeadSHA:     c.GetCommitID(),
				ReviewState: ReviewCommentInline,
			}
			if u := c.GetUser(); u != nil {
				mc.AuthorForgeUserID = u.GetID()
				mc.AuthorUsername = u.GetLogin()
			}
			if dbID := c.GetID(); dbID != 0 {
				mc.ReplyID = strconv.FormatInt(dbID, 10)
				idxByDBID[dbID] = len(out)
			}
			if p := c.Path; p != nil {
				path := *p
				mc.Path = &path
			}
			if c.Line != nil {
				line := *c.Line
				mc.Line = &line
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		prOpt.Page = resp.NextPage
	}

	// Source C: review-summary bodies (a review with only inline comments has an
	// empty Body and is skipped — its comments already came from source B).
	revOpt := &gh.ListOptions{PerPage: githubPerPage}
	for page := 0; ; {
		page++
		reviews, resp, err := g.client.PullRequests.ListReviews(ctx, slug.owner, slug.repo, number, revOpt)
		if err != nil {
			return nil, g.wrapErr("list merge request comments", err)
		}
		for _, r := range reviews {
			if r == nil || r.GetBody() == "" {
				continue
			}
			mc := MRComment{
				ID:          r.GetID(), // REST databaseId of the review summary
				Body:        r.GetBody(),
				CreatedAt:   r.GetSubmittedAt().Time,
				HeadSHA:     r.GetCommitID(),
				ReviewState: ReviewCommentSummary,
			}
			if u := r.GetUser(); u != nil {
				mc.AuthorForgeUserID = u.GetID()
				mc.AuthorUsername = u.GetLogin()
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		revOpt.Page = resp.NextPage
	}

	// GraphQL stitch: map each inline comment's databaseId → its thread node id
	// (the resolve anchor REST does not expose) and fold it onto the source-B
	// MRComments already collected.
	threadByDBID, err := g.reviewThreadIDsByDatabaseID(ctx, slug, number)
	if err != nil {
		return nil, err
	}
	for dbID, idx := range idxByDBID {
		if threadID, ok := threadByDBID[dbID]; ok {
			out[idx].ResolveID = threadID
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// reviewThreadIDsByDatabaseID runs the GraphQL reviewThreads query and returns a
// map from each thread comment's REST databaseId to the thread's node id — the
// join key ListMergeRequestComments uses to attach a resolve anchor to a REST
// inline comment (Resolved facts). It reuses the driver's graphqlDo helper (auth +
// endpoint + redaction), never a hand-rolled POST. It pages BOTH connections: the
// outer reviewThreads cursor here, and — for any thread with more than one page of
// comments — the inner comments cursor via reviewThreadCommentDBIDs, so neither a
// PR with >100 review threads nor a thread with >100 comments silently drops a
// ResolveID anchor. Both loops are backstopped by maxForgeItems/maxForgePages.
func (g *github) reviewThreadIDsByDatabaseID(ctx context.Context, slug repoSlug, number int) (map[int64]string, error) {
	const query = `query($owner: String!, $name: String!, $number: Int!, $threadCursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $threadCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          comments(first: 100) {
            pageInfo { hasNextPage endCursor }
            nodes { databaseId }
          }
        }
      }
    }
  }
}`
	m := map[int64]string{}
	var threadCursor *string
	page := 0
	// fetched counts every comment NODE returned (including databaseId==0 and
	// duplicate ids), accumulated across outer pages — not the deduplicated map
	// size. The cap must key off nodes actually fetched, because a single outer
	// page carrying duplicate or zero databaseIds can pull far more than
	// maxForgeItems nodes while len(m) stays under the ceiling.
	fetched := 0
	for {
		page++
		var resp struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID       string `json:"id"`
							Comments struct {
								PageInfo struct {
									HasNextPage bool   `json:"hasNextPage"`
									EndCursor   string `json:"endCursor"`
								} `json:"pageInfo"`
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": slug.owner, "name": slug.repo, "number": number}
		if threadCursor != nil {
			vars["threadCursor"] = *threadCursor
		} else {
			vars["threadCursor"] = nil
		}
		if err := g.graphqlDo(ctx, query, vars, &resp); err != nil {
			return nil, err
		}
		for _, node := range resp.Repository.PullRequest.ReviewThreads.Nodes {
			if node.ID == "" {
				continue
			}
			for _, cn := range node.Comments.Nodes {
				fetched++
				if cn.DatabaseID != 0 {
					m[cn.DatabaseID] = node.ID
				}
			}
			// Enforce the item cap PER THREAD, not once per outer page, and key it
			// off FETCHED comment nodes (duplicate and zero databaseIds included),
			// not the deduplicated map size: without this a single reviewThreads
			// page of up to 100 threads could each fan out to
			// reviewThreadCommentDBIDs and accumulate before the bound fired,
			// amplifying the maxForgeItems ceiling by the page size, and duplicate
			// or zero ids collapsing in the map could keep len(m) under the cap
			// while far more nodes were actually fetched.
			if fetched > maxForgeItems {
				return nil, g.redact.error(forgePaginationCapErr("item", maxForgeItems))
			}
			if node.Comments.PageInfo.HasNextPage {
				// Bound the inner fan-out by the GLOBAL remaining budget so one
				// thread's overflow cannot exceed maxForgeItems either.
				rest, restFetched, err := g.reviewThreadCommentDBIDs(ctx, node.ID, node.Comments.PageInfo.EndCursor, maxForgeItems-fetched)
				if err != nil {
					return nil, err
				}
				for _, dbID := range rest {
					m[dbID] = node.ID
				}
				fetched += restFetched
				if fetched > maxForgeItems {
					return nil, g.redact.error(forgePaginationCapErr("item", maxForgeItems))
				}
			}
		}
		if !resp.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage || resp.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor == "" {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(forgePaginationCapErr("page", maxForgePages))
		}
		next := resp.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
		threadCursor = &next
	}
	return m, nil
}

// reviewThreadCommentDBIDs pages the remaining comment databaseIds of one review
// thread past the first page the outer reviewThreads query already returned. GitHub
// cannot advance a nested connection cursor from the outer query, so it re-fetches
// the thread node by id and walks its comments connection from afterCursor (the
// outer page's comments.endCursor). It reuses graphqlDo and is backstopped by
// maxForgePages exactly as the outer loop; budget is the caller's remaining global
// item allowance, so one thread's comment fan-out cannot push the total past
// maxForgeItems. It counts every comment NODE fetched (duplicate and zero
// databaseIds included) against budget — not distinct ids — and returns that
// fetched-node count as its second return so the caller can accumulate it into the
// outer per-page fetched total.
func (g *github) reviewThreadCommentDBIDs(ctx context.Context, threadID, afterCursor string, budget int) ([]int64, int, error) {
	const query = `query($threadId: ID!, $commentCursor: String) {
  node(id: $threadId) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $commentCursor) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId }
      }
    }
  }
}`
	var out []int64
	cursor := afterCursor
	page := 0
	fetched := 0
	for {
		page++
		var resp struct {
			Node struct {
				Comments struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						DatabaseID int64 `json:"databaseId"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"node"`
		}
		vars := map[string]any{"threadId": threadID, "commentCursor": cursor}
		if err := g.graphqlDo(ctx, query, vars, &resp); err != nil {
			return nil, fetched, err
		}
		for _, cn := range resp.Node.Comments.Nodes {
			fetched++
			if cn.DatabaseID != 0 {
				out = append(out, cn.DatabaseID)
			}
		}
		if fetched > budget {
			return nil, fetched, g.redact.error(forgePaginationCapErr("item", maxForgeItems))
		}
		if !resp.Node.Comments.PageInfo.HasNextPage || resp.Node.Comments.PageInfo.EndCursor == "" {
			break
		}
		if page >= maxForgePages {
			return nil, fetched, g.redact.error(forgePaginationCapErr("page", maxForgePages))
		}
		cursor = resp.Node.Comments.PageInfo.EndCursor
	}
	return out, fetched, nil
}

// ReplyMergeRequestComment posts an in-thread reply keyed on replyID, the REST
// databaseId of the review comment it answers (an MRComment.ReplyID).
func (g *github) ReplyMergeRequestComment(ctx context.Context, projectID, mrIID int64, replyID, body string) error {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	commentID, err := strconv.ParseInt(strings.TrimSpace(replyID), 10, 64)
	if err != nil {
		return fmt.Errorf("github: reply merge request comment: invalid reply id %q", replyID)
	}
	if _, _, err := g.client.PullRequests.CreateCommentInReplyTo(ctx, slug.owner, slug.repo, int(mrIID), body, commentID); err != nil {
		return g.wrapErr("reply merge request comment", err)
	}
	return nil
}

// ResolveMergeRequestThread resolves the review thread keyed on resolveID, the
// GraphQL thread node id (an MRComment.ResolveID). go-github ships no GraphQL
// client, so this issues the resolveReviewThread mutation through graphqlDo — the
// same authenticated, redacted endpoint path the read stitch uses (Resolved facts).
func (g *github) ResolveMergeRequestThread(ctx context.Context, _, _ int64, resolveID string) error {
	if strings.TrimSpace(resolveID) == "" {
		return fmt.Errorf("github: resolve merge request thread: empty resolve id")
	}
	const mutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`
	return g.graphqlDo(ctx, mutation, map[string]any{"threadId": resolveID}, nil)
}
