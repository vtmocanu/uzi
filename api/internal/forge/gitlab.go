package forge

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// developerAccessLevel is GitLab's Developer role access level. The privilege
// checker requires the bot to sit at exactly this level, and a protected default
// branch must not admit push at or below it.
const developerAccessLevel = int(gitlab.DeveloperPermissions) // 30

// perPage is the pagination page size for every list call. 100 is GitLab's
// maximum, minimizing round-trips on busy projects.
const perPage = 100

// gitlabDefaultLabelColor is used when EnsureLabels is handed a label with no
// color. GitLab's label-create API rejects a create with a missing color, so
// the driver supplies a neutral default rather than fail (mirrors forgejo/github).
const gitlabDefaultLabelColor = "#ededed"

// maxTraceBytes is the fail-closed ceiling on a single job trace JobLogTail will
// process. Well above any real CI log's tail need (the snapshot keeps only the
// last CI_FIX_LOG_TAIL_BYTES, 32 KiB), so a legitimate trace never trips it; a
// pathologically large one errors rather than being scanned/truncated.
const maxTraceBytes = 16 << 20 // 16 MiB

// gitLab is the GitLab REST driver. The embedded redactor scrubs the PAT out of
// every error the underlying client hands back before it leaves this package.
//
// baseURL/token/logClient are retained alongside the SDK client so JobLogTail
// can bypass the SDK and stream the job trace under an io.LimitReader: client-go's
// GetTraceFile buffers the whole trace into memory before returning it, so a raw
// bounded GET is the only way to cap the TRANSFER rather than a post-buffer copy.
//
// logClient is a dedicated client for that raw trace GET that REFUSES to follow
// redirects (CheckRedirect → http.ErrUseLastResponse). The SDK's own client
// follows up to 10 redirects, and Go does NOT strip the PRIVATE-TOKEN header on a
// cross-host redirect (only Authorization/Cookie/WWW-Authenticate), so a
// hostile-but-allowlisted forge answering the trace request with a cross-host 302
// would otherwise replay the bot PAT to the redirect target.
type gitLab struct {
	client    *gitlab.Client
	redact    redactor
	baseURL   string
	token     string
	logClient *http.Client
}

// newGitLab builds a GitLab driver against baseURL using token, with a bounded
// per-call HTTP timeout. The official client's retryable transport already
// honors 429 + Retry-After and backs off on 5xx; we route it through our
// timeout client so no call can hang forever. baseURL is assumed
// allowlist-checked by the caller.
func newGitLab(baseURL, token string, timeout time.Duration) (*gitLab, error) {
	hc := timeoutClient(timeout)
	client, err := gitlab.NewClient(token,
		gitlab.WithBaseURL(baseURL),
		gitlab.WithHTTPClient(hc),
	)
	if err != nil {
		// NewClient failure can only stem from the base URL here; still route
		// it through a redactor in case the token ever appears.
		return nil, newRedactor(token).error(fmt.Errorf("gitlab: new client: %w", err))
	}
	// logClient is a separate client for JobLogTail's raw trace GET: it shares the
	// per-call timeout but REFUSES every redirect, so a cross-host 302 cannot replay
	// the PAT header to the redirect target (mirrors the GitHub driver's logClient).
	logClient := &http.Client{
		Timeout:       hc.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &gitLab{
		client:    client,
		redact:    newRedactor(token),
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		token:     token,
		logClient: logClient,
	}, nil
}

// wrapErr adds op context and routes the error through the PAT redactor so no
// error this driver surfaces can carry the token. A nil error passes through as
// nil. (No rate-limit classification: the go-gitlab client handles 429/Retry-After
// at the transport layer; see the client-construction comment.)
func (g *gitLab) wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return g.redact.error(fmt.Errorf("gitlab: %s: %w", op, err))
}

func (g *gitLab) VerifyToken(ctx context.Context) (BotIdentity, error) {
	u, _, err := g.client.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		return BotIdentity{}, g.wrapErr("verify token", err)
	}
	// IsAdmin rides on the same GET /user response VerifyToken already makes:
	// GitLab only returns is_admin:true for an admin caller, and omits it for a
	// regular user, so the decoded bool is false for every compliant bot.
	return BotIdentity{ForgeUserID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin}, nil
}

func (g *gitLab) TokenInfo(ctx context.Context) (TokenInfo, error) {
	t, resp, err := g.client.PersonalAccessTokens.GetSinglePersonalAccessToken(gitlab.WithContext(ctx))
	if err != nil {
		// A 404 here means the endpoint itself is absent (GitLab < 15.5), not that
		// the token is bad — surface the distinct sentinel so the checker warns
		// rather than blocks. The sentinel carries no token material.
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return TokenInfo{}, ErrTokenIntrospectionUnsupported
		}
		return TokenInfo{}, g.wrapErr("token info", err)
	}
	info := TokenInfo{
		Scopes: append([]string(nil), t.Scopes...),
		Active: t.Active && !t.Revoked,
	}
	if t.ExpiresAt != nil {
		info.ExpiresAt = time.Time(*t.ExpiresAt)
	}
	return info, nil
}

// roleForAccessLevel maps a GitLab access level onto the neutral Role. GitLab's
// levels are 0 (none), 5 (Minimal), 10 (Guest), 20 (Reporter), 30 (Developer),
// 40 (Maintainer), 50 (Owner), 60 (Admin). Developer is exactly the write role —
// it is the level uzi's bot must hold — so everything under it collapses to read
// and everything over it to admin/owner. The precision uzi loses (Guest vs
// Reporter) is precision it never used: privcheck asserts write and reports
// anything else.
func roleForAccessLevel(lvl int) Role {
	switch {
	case lvl <= 0:
		return RoleNone
	case lvl < developerAccessLevel:
		return RoleRead
	case lvl == developerAccessLevel:
		return RoleWrite
	case lvl < int(gitlab.OwnerPermissions):
		return RoleAdmin
	default:
		return RoleOwner
	}
}

func (g *gitLab) ProjectRole(ctx context.Context, projectID, forgeUserID int64) (Role, bool, error) {
	// members/all resolves EFFECTIVE membership (direct + inherited group), which
	// is what actually governs what the bot can do — a group-inherited Maintainer
	// role would be invisible to the direct-members endpoint.
	m, resp, err := g.client.ProjectMembers.GetInheritedProjectMember(projectID, forgeUserID, gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// Not a member (removed or demoted below any membership after the repo
			// was enabled). Reported as a finding by the checker, not an error.
			return RoleNone, false, nil
		}
		return RoleNone, false, g.wrapErr("project role", err)
	}
	return roleForAccessLevel(int(m.AccessLevel)), true, nil
}

func (g *gitLab) DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error) {
	pb, resp, err := g.client.ProtectedBranches.GetProtectedBranch(projectID, branch, gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// No protection rule for this branch at all — so nothing restricts push
			// or merge, and a write-role bot may do both. Reporting the zero value
			// here would say "cannot push, cannot merge" about the one branch state
			// where the bot certainly can: see BranchProtection's invariant.
			return BranchProtection{
				Protected:         false,
				WriteRoleCanPush:  true,
				WriteRoleCanMerge: true,
			}, nil
		}
		return BranchProtection{}, g.wrapErr("branch protection", err)
	}
	bp := BranchProtection{Protected: true}
	for _, pl := range pb.PushAccessLevels {
		if pl == nil {
			continue
		}
		lvl := int(pl.AccessLevel)
		// A push level of 0 is "No one"; >= Maintainer (40) excludes a Developer
		// bot. Only a nonzero level at or below Developer (30) lets the bot push.
		if lvl > 0 && lvl <= developerAccessLevel {
			bp.WriteRoleCanPush = true
		}
		// A per-user allow-to-push entry naming the bot lets it push regardless of
		// role (a false negative the role check alone would miss).
		if botUserID != 0 && pl.UserID == botUserID {
			bp.BotCanPush = true
		}
	}
	// merge_access_levels is the same shape as push_access_levels and governs who
	// may merge an MR into the branch. GitLab's initial default for a new project
	// sets it to Maintainer, so a Developer bot cannot merge — but it is a
	// setting, and safe-by-default is not safe. uzi modelled merge on no forge
	// until now, delegating "the agent can only ever open an MR" to a sentence in
	// the setup docs.
	for _, ml := range pb.MergeAccessLevels {
		if ml == nil {
			continue
		}
		lvl := int(ml.AccessLevel)
		if lvl > 0 && lvl <= developerAccessLevel {
			bp.WriteRoleCanMerge = true
		}
		if botUserID != 0 && ml.UserID == botUserID {
			bp.BotCanMerge = true
		}
	}
	return bp, nil
}

func (g *gitLab) ListProjects(ctx context.Context) ([]Project, error) {
	opt := &gitlab.ListProjectsOptions{
		ListOptions:    gitlab.ListOptions{Page: 1, PerPage: perPage},
		Membership:     gitlab.Ptr(true),
		MinAccessLevel: gitlab.Ptr(gitlab.DeveloperPermissions),
		// Simple trims the payload to the fields we map; we don't need the
		// heavy statistics/permissions blocks.
		Simple: gitlab.Ptr(true),
	}
	wrap := func(e error) error { return g.wrapErr("list projects", e) }
	return paginate(wrap, func(page int) ([]Project, int, error) {
		opt.Page = int64(page)
		projects, resp, err := g.client.Projects.ListProjects(opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, 0, err
		}
		var items []Project
		for _, p := range projects {
			if p == nil {
				continue
			}
			items = append(items, Project{
				ForgeProjectID:    p.ID,
				PathWithNamespace: p.PathWithNamespace,
				WebURL:            p.WebURL,
				DefaultBranch:     p.DefaultBranch,
			})
		}
		return items, int(resp.NextPage), nil
	})
}

func (g *gitLab) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	opt := &gitlab.ListLabelsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	wrap := func(e error) error { return g.wrapErr("list labels", e) }
	return paginate(wrap, func(page int) ([]Label, int, error) {
		opt.Page = int64(page)
		labels, resp, err := g.client.Labels.ListLabels(projectID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, 0, err
		}
		var items []Label
		for _, l := range labels {
			if l == nil {
				continue
			}
			items = append(items, Label{Name: l.Name, Color: l.Color})
		}
		return items, int(resp.NextPage), nil
	})
}

func (g *gitLab) EnsureLabels(ctx context.Context, projectID int64, labels []Label) error {
	existing, err := g.ListLabels(ctx, projectID)
	if err != nil {
		return err
	}
	have := make(map[string]struct{}, len(existing))
	for _, l := range existing {
		have[l.Name] = struct{}{}
	}
	for _, l := range labels {
		if _, ok := have[l.Name]; ok {
			continue
		}
		opt := &gitlab.CreateLabelOptions{Name: gitlab.Ptr(l.Name)}
		color := l.Color
		if color == "" {
			color = gitlabDefaultLabelColor
		}
		opt.Color = gitlab.Ptr(color)
		if _, _, err := g.client.Labels.CreateLabel(projectID, opt, gitlab.WithContext(ctx)); err != nil {
			return g.wrapErr(fmt.Sprintf("create label %q", l.Name), err)
		}
	}
	return nil
}

func (g *gitLab) ListIssues(ctx context.Context, projectID int64, opts ListIssuesOptions) ([]Issue, error) {
	opt := &gitlab.ListProjectIssuesOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage},
		// state=all remains the DEFAULT (opts.State's zero value): the Closed column
		// and de-label/close eviction both depend on seeing closed issues, so every
		// pre-M6 caller must keep getting it.
		//
		// GitLab's REQUEST vocabulary is opened/closed/all, which happens to be
		// identical to uzi's neutral vocabulary — so this driver needs no translation
		// and the mapping below is a pass-through. That coincidence is exactly why a
		// GitLab-shaped fake cannot catch a driver that fails to translate; see the
		// Forgejo driver, where the vocabularies differ.
		State: gitlab.Ptr(gitlabIssueStateParam(opts.State)),
	}
	if len(opts.Labels) > 0 {
		labels := gitlab.LabelOptions(opts.Labels)
		opt.Labels = &labels
	}
	if opts.UpdatedAfter != nil {
		opt.UpdatedAfter = opts.UpdatedAfter
	}
	var out []Issue
	page := 0
	for {
		page++
		issues, resp, err := g.client.Issues.ListProjectIssues(projectID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.wrapErr("list issues", err)
		}
		for _, i := range issues {
			if i == nil {
				continue
			}
			out = append(out, toIssue(i))
			// opts.Limit == 0 is the no-cap default (every pre-#158 caller); a positive
			// Limit stops as soon as that many issues are collected, truncating this page.
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list issues", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list issues", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *gitLab) GetIssue(ctx context.Context, projectID, issueIID int64) (Issue, error) {
	i, _, err := g.client.Issues.GetIssue(projectID, issueIID, gitlab.WithContext(ctx))
	if err != nil {
		return Issue{}, g.wrapErr("get issue", err)
	}
	return toIssue(i), nil
}

func (g *gitLab) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (Issue, error) {
	opt := &gitlab.CreateIssueOptions{
		Title:       gitlab.Ptr(title),
		Description: gitlab.Ptr(description),
	}
	if len(labels) > 0 {
		l := gitlab.LabelOptions(labels)
		opt.Labels = &l
	}
	i, _, err := g.client.Issues.CreateIssue(projectID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return Issue{}, g.wrapErr("create issue", err)
	}
	return toIssue(i), nil
}

func (g *gitLab) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	opt := &gitlab.UpdateIssueOptions{}
	if len(add) > 0 {
		l := gitlab.LabelOptions(add)
		opt.AddLabels = &l
	}
	if len(remove) > 0 {
		l := gitlab.LabelOptions(remove)
		opt.RemoveLabels = &l
	}
	if _, _, err := g.client.Issues.UpdateIssue(projectID, issueIID, opt, gitlab.WithContext(ctx)); err != nil {
		return g.wrapErr("update issue labels", err)
	}
	return nil
}

// SetIssueState closes or reopens an issue via GitLab's state_event verb.
// gitlab.UpdateIssueOptions.StateEvent is a *string with `omitempty` and takes
// the MUTATE vocabulary "close"/"reopen" (not the query-filter opened/closed the
// GitLab list lane uses), so state travels alone and nothing else is clobbered.
func (g *gitLab) SetIssueState(ctx context.Context, projectID, issueIID int64, state IssueState) error {
	ev := gitlabIssueStateEvent(state)
	opt := &gitlab.UpdateIssueOptions{StateEvent: &ev}
	if _, _, err := g.client.Issues.UpdateIssue(projectID, issueIID, opt, gitlab.WithContext(ctx)); err != nil {
		return g.wrapErr("set issue state", err)
	}
	return nil
}

// UpdateIssueDescription sends only the description (PRD #72 M5).
// gitlab.UpdateIssueOptions.Description is a *string with `omitempty`, so no other
// field of the issue is transmitted and nothing else can be clobbered.
func (g *gitLab) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	opt := &gitlab.UpdateIssueOptions{Description: &description}
	if _, _, err := g.client.Issues.UpdateIssue(projectID, issueIID, opt, gitlab.WithContext(ctx)); err != nil {
		return g.wrapErr("update issue description", err)
	}
	return nil
}

func (g *gitLab) UserExists(ctx context.Context, username string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	// GitLab's username filter is an exact match, so a non-empty result means the
	// account exists. We only need to know if any row came back.
	users, _, err := g.client.Users.ListUsers(&gitlab.ListUsersOptions{
		Username: gitlab.Ptr(username),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return false, g.wrapErr("lookup user", err)
	}
	return len(users) > 0, nil
}

func (g *gitLab) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	opt := &gitlab.ListLabelEventsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	wrap := func(e error) error { return g.wrapErr("list issue label events", e) }
	return paginate(wrap, func(page int) ([]LabelEvent, int, error) {
		opt.Page = int64(page)
		events, resp, err := g.client.ResourceLabelEvents.ListIssueLabelEvents(projectID, issueIID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, 0, err
		}
		var items []LabelEvent
		for _, e := range events {
			if e == nil {
				continue
			}
			items = append(items, toLabelEvent(e))
		}
		return items, int(resp.NextPage), nil
	})
}

// ListIssueComments returns an issue's human comments, oldest-first (PRD #381).
// GitLab's ListIssueNotes returns comments AND system notes in one list and
// defaults to created_at DESC, so the driver asks for ascending order (OrderBy/
// Sort) and filters out System notes (D2) — leaving only human comments in
// oldest-first order (D8).
func (g *gitLab) ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]IssueComment, error) {
	opt := &gitlab.ListIssueNotesOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage},
		OrderBy:     gitlab.Ptr("created_at"),
		Sort:        gitlab.Ptr("asc"),
	}
	wrap := func(e error) error { return g.wrapErr("list issue comments", e) }
	return paginate(wrap, func(page int) ([]IssueComment, int, error) {
		opt.Page = int64(page)
		notes, resp, err := g.client.Notes.ListIssueNotes(projectID, issueIID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, 0, err
		}
		var items []IssueComment
		for _, n := range notes {
			if n == nil || n.System {
				continue
			}
			var createdAt time.Time
			if n.CreatedAt != nil {
				createdAt = *n.CreatedAt
			}
			items = append(items, IssueComment{
				AuthorForgeUserID: n.Author.ID,
				AuthorUsername:    n.Author.Username,
				Body:              n.Body,
				CreatedAt:         createdAt,
			})
		}
		return items, int(resp.NextPage), nil
	})
}

func (g *gitLab) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error) {
	note, _, err := g.client.Notes.CreateIssueNote(projectID, issueIID, &gitlab.CreateIssueNoteOptions{
		Body: gitlab.Ptr(body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return IssueNote{}, g.wrapErr("create issue note", err)
	}
	return IssueNote{ID: note.ID, Body: note.Body}, nil
}

func (g *gitLab) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	mr, _, err := g.client.MergeRequests.GetMergeRequest(projectID, mrIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return MergeRequest{}, g.wrapErr("get merge request", err)
	}
	return toMergeRequest(mr), nil
}

// ListMergeRequestComments returns an MR's human + review-bot comments oldest-first
// (PRD #700). GitLab models MR review as DISCUSSIONS (each a resolvable thread of
// notes), so the driver lists ListMergeRequestDiscussions and flattens each
// Discussion.Notes into MRComments, dropping System notes (D2) exactly as
// ListIssueComments does. The discussion id is BOTH the reply anchor and the
// resolve anchor. A note carrying a diff Position is inline (Path/Line/HeadSHA
// populated); one without is a summary/top-level note. The discussions endpoint has
// no sort option, so the driver sorts the flattened list by CreatedAt to guarantee
// oldest-first (D8).
func (g *gitLab) ListMergeRequestComments(ctx context.Context, projectID, mrIID int64) ([]MRComment, error) {
	opt := &gitlab.ListMergeRequestDiscussionsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	wrap := func(e error) error { return g.wrapErr("list merge request comments", e) }
	out, err := paginate(wrap, func(page int) ([]MRComment, int, error) {
		opt.Page = int64(page)
		discussions, resp, err := g.client.Discussions.ListMergeRequestDiscussions(projectID, mrIID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, 0, err
		}
		var items []MRComment
		for _, d := range discussions {
			if d == nil {
				continue
			}
			for _, n := range d.Notes {
				if n == nil || n.System {
					continue
				}
				var createdAt time.Time
				if n.CreatedAt != nil {
					createdAt = *n.CreatedAt
				}
				c := MRComment{
					ID:                n.ID,
					AuthorForgeUserID: n.Author.ID,
					AuthorUsername:    n.Author.Username,
					Body:              n.Body,
					CreatedAt:         createdAt,
					// The discussion id is both anchors on GitLab.
					ReplyID:     d.ID,
					ResolveID:   d.ID,
					ReviewState: ReviewCommentSummary,
				}
				if p := n.Position; p != nil {
					c.HeadSHA = p.HeadSHA
					if p.NewPath != "" {
						path := p.NewPath
						c.Path = &path
						c.ReviewState = ReviewCommentInline
					}
					if p.NewLine != 0 {
						line := int(p.NewLine)
						c.Line = &line
					}
				}
				items = append(items, c)
			}
		}
		return items, int(resp.NextPage), nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ReplyMergeRequestComment adds a note to the discussion (thread) identified by
// replyID (a discussion id from ListMergeRequestComments).
func (g *gitLab) ReplyMergeRequestComment(ctx context.Context, projectID, mrIID int64, replyID, body string) error {
	_, _, err := g.client.Discussions.AddMergeRequestDiscussionNote(projectID, mrIID, replyID,
		&gitlab.AddMergeRequestDiscussionNoteOptions{Body: gitlab.Ptr(body)}, gitlab.WithContext(ctx))
	if err != nil {
		return g.wrapErr("reply merge request comment", err)
	}
	return nil
}

// ResolveMergeRequestThread marks the discussion (thread) identified by resolveID
// resolved. On GitLab the resolve anchor equals the reply anchor (the discussion id).
func (g *gitLab) ResolveMergeRequestThread(ctx context.Context, projectID, mrIID int64, resolveID string) error {
	_, _, err := g.client.Discussions.ResolveMergeRequestDiscussion(projectID, mrIID, resolveID,
		&gitlab.ResolveMergeRequestDiscussionOptions{Resolved: gitlab.Ptr(true)}, gitlab.WithContext(ctx))
	if err != nil {
		return g.wrapErr("resolve merge request thread", err)
	}
	return nil
}

// toLabelEvent maps a client-go resource label event to the neutral domain type.
// A nil CreatedAt yields the zero time; a system event with no user yields an
// empty Username (the caller falls back to the issue author in that case).
func toLabelEvent(e *gitlab.LabelEvent) LabelEvent {
	le := LabelEvent{
		ID:        e.ID,
		Action:    e.Action,
		LabelName: e.Label.Name,
		Username:  e.User.Username,
	}
	if e.CreatedAt != nil {
		le.CreatedAt = *e.CreatedAt
	}
	return le
}

// toMergeRequest maps a client-go merge request to the neutral domain type. The
// State field is one of the MRState* constants (opened|closed|merged|locked).
func toMergeRequest(mr *gitlab.MergeRequest) MergeRequest {
	return MergeRequest{
		IID:    mr.IID,
		State:  mr.State,
		WebURL: mr.WebURL,
	}
}

// toIssue maps a client-go issue to the neutral domain type. A nil author (rare
// but possible for system issues) yields an empty Author; a nil UpdatedAt
// yields the zero time, which the sync engine treats as "no HWM advance".
//
// Labels is normalized to a non-nil slice. GitLab returns no labels array at all
// for an issue carrying none, so []string(i.Labels) is nil, which json.Marshal
// writes into the cache as the jsonb scalar `null` rather than `[]` — a value the
// column's NOT NULL does not exclude and that decodes back to nil without error.
// This is the belt; handler.decodeLabels is the braces, and it is the one that
// matters for rows already stored.
//
// Unreachable until PRD #102 M6, whose additive fetch is the first thing that
// caches an issue with no labels at all.
func toIssue(i *gitlab.Issue) Issue {
	labels := []string(i.Labels)
	if labels == nil {
		labels = []string{}
	}
	issue := Issue{
		IID:         i.IID,
		Title:       i.Title,
		State:       i.State,
		Labels:      labels,
		Description: i.Description,
		WebURL:      i.WebURL,
		Assignees:   []int64{},
	}
	for _, a := range i.Assignees {
		if a != nil {
			issue.Assignees = append(issue.Assignees, a.ID)
		}
	}
	if i.Author != nil {
		issue.Author = i.Author.Username
	}
	if i.UpdatedAt != nil {
		issue.UpdatedAt = *i.UpdatedAt
	}
	return issue
}

// gitlabIssueStateParam maps the neutral state onto GitLab's `state` query
// parameter. The two vocabularies coincide (opened/closed/all), so this is a
// pass-through with an explicit default — written out rather than inlined so the
// Forgejo driver's genuine translation has a visible counterpart, and so an
// unrecognised value can never reach the API as a silent filter.
func gitlabIssueStateParam(s IssueState) string {
	switch s {
	case StateOpened:
		return "opened"
	case StateClosed:
		return "closed"
	default:
		return "all"
	}
}

// gitlabIssueStateEvent maps the neutral state onto GitLab's state_event MUTATE
// verb, which is a distinct vocabulary from the state query filter above:
// closing an issue is state_event="close", reopening it is "reopen". This is
// deliberately NOT gitlabIssueStateParam — that returns the filter word
// (opened/closed) the API would reject as a state_event. StateClosed closes;
// anything else (StateOpened, or a caller-bug value) reopens, the safe direction.
func gitlabIssueStateEvent(s IssueState) string {
	if s == StateClosed {
		return "close"
	}
	return "reopen"
}
