package forge

// forgejo_labels.go is the Forgejo label seam: ListLabels, listRepoLabels,
// EnsureLabels, UpdateIssueLabels, resolveLabelIDs, and ListIssueLabelEvents.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	gitea "code.gitea.io/sdk/gitea"
)

func (f *forgejo) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	labels, err := f.listRepoLabels(c, slug)
	if err != nil {
		return nil, err
	}
	out := make([]Label, 0, len(labels))
	for _, l := range labels {
		out = append(out, Label{Name: l.Name, Color: l.Color})
	}
	return out, nil
}

// listRepoLabels returns every label on a repo, paginated internally. It backs
// both ListLabels and the name→id resolution EnsureLabels/UpdateIssueLabels/
// CreateIssue need (Forgejo's label writes are keyed by id, not name).
func (f *forgejo) listRepoLabels(c *gitea.Client, slug repoSlug) ([]*gitea.Label, error) {
	opt := gitea.ListLabelsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	wrap := func(e error) error { return f.wrapErr("list labels", e) }
	return paginate(wrap, func(page int) ([]*gitea.Label, int, error) {
		opt.Page = page
		labels, resp, err := c.ListRepoLabels(slug.owner, slug.repo, opt)
		if err != nil {
			return nil, 0, err
		}
		return labels, resp.NextPage, nil
	})
}

func (f *forgejo) EnsureLabels(ctx context.Context, projectID int64, labels []Label) error {
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	existing, err := f.listRepoLabels(c, slug)
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
		color := l.Color
		if color == "" {
			color = forgejoDefaultLabelColor
		}
		if _, _, err := c.CreateLabel(slug.owner, slug.repo, gitea.CreateLabelOption{Name: l.Name, Color: color}); err != nil {
			return f.wrapErr(fmt.Sprintf("create label %q", l.Name), err)
		}
	}
	return nil
}

func (f *forgejo) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	// Forgejo has no add/remove delta — only a full-set PUT (ReplaceIssueLabels).
	// Read the current set immediately before the write (D3), compute the target
	// set client-side (current − remove + add), and PUT once. The read/write is not
	// transactional, so a label a human adds in the ~1 RTT window is dropped by our
	// PUT — the accepted, documented lost-update window (D3); the single-column
	// board invariant still holds because the PUT itself is atomic server-side.
	current, currentIDs, err := f.issueLabelNames(c, slug, issueIID)
	if err != nil {
		return err
	}
	removeSet := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		removeSet[r] = struct{}{}
	}
	target := map[string]struct{}{}
	for name := range current {
		if _, drop := removeSet[name]; !drop {
			target[name] = struct{}{}
		}
	}
	for _, a := range add {
		target[a] = struct{}{}
	}
	// No-op: a card move that changes nothing must issue ZERO PUTs (D3).
	if sameNameSet(current, target) {
		return nil
	}
	names := make([]string, 0, len(target))
	for name := range target {
		names = append(names, name)
	}
	ids, err := f.resolveLabelIDs(c, slug, names, currentIDs)
	if err != nil {
		return err
	}
	if _, _, err := c.ReplaceIssueLabels(slug.owner, slug.repo, issueIID, gitea.IssueLabelsOption{Labels: ids}); err != nil {
		return f.wrapErr("update issue labels", err)
	}
	return nil
}

// issueLabelNames returns the labels currently on an issue as a name set plus a
// name→id map, paginated internally. Both are computed from one read so the
// no-op check and the id resolution share it.
func (f *forgejo) issueLabelNames(c *gitea.Client, slug repoSlug, issueIID int64) (map[string]struct{}, map[string]int64, error) {
	opt := gitea.ListLabelsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	names := map[string]struct{}{}
	ids := map[string]int64{}
	page := 0
	for {
		page++
		labels, resp, err := c.GetIssueLabels(slug.owner, slug.repo, issueIID, opt)
		if err != nil {
			return nil, nil, f.wrapErr("list issue labels", err)
		}
		for _, l := range labels {
			names[l.Name] = struct{}{}
			ids[l.Name] = l.ID
		}
		if len(names) > maxForgeItems {
			return nil, nil, f.wrapErr("list issue labels", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, nil, f.wrapErr("list issue labels", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return names, ids, nil
}

// resolveLabelIDs maps label names to Forgejo label ids. It uses known (already
// on the issue) ids first, then fetches the repo catalog once for any remaining
// name. A name still unresolved is CREATED (with the default color) and its new id
// used — matching GitLab, whose issue-create / add_labels auto-create a referenced
// label server-side (gitlab.go CreateIssue just forwards the labels param). This is
// load-bearing: uzi's CreateIssue stamps the PRD trigger label, which is NOT a
// board column and so is never EnsureLabels'd, so erroring here would 502 every
// issue creation on a Forgejo repo that lacks the label — parity that a Forgejo
// e2e (M9) caught. Silently dropping a name is still never done: the board's
// single-column invariant needs every target label present.
//
// Shared with UpdateIssueLabels (not CreateIssue-only): in the rare case a board
// column is deleted mid-move, this recreates it with the DEFAULT color, not its
// configured one, until the next EnsureLabels re-pins it — a benign self-heal,
// delete-race edge only.
func (f *forgejo) resolveLabelIDs(c *gitea.Client, slug repoSlug, names []string, known map[string]int64) ([]int64, error) {
	ids := make([]int64, 0, len(names))
	var missing []string
	for _, name := range names {
		if id, ok := known[name]; ok {
			ids = append(ids, id)
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return ids, nil
	}
	catalog, err := f.listRepoLabels(c, slug)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]int64, len(catalog))
	for _, l := range catalog {
		byName[l.Name] = l.ID
	}
	for _, name := range missing {
		id, ok := byName[name]
		if !ok {
			created, _, err := c.CreateLabel(slug.owner, slug.repo, gitea.CreateLabelOption{Name: name, Color: forgejoDefaultLabelColor})
			if err != nil {
				return nil, f.wrapErr(fmt.Sprintf("create missing label %q", name), err)
			}
			id = created.ID
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (f *forgejo) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	// Hand-rolled parse rather than the SDK's ListIssueTimeline: gitea-sdk types the
	// timeline's `label` field as []*Label, but Forgejo 16.0.0 serializes a label
	// event's label as a SINGLE object (verified live + swagger $ref), so the SDK
	// call ERRORS on exactly the label events we need. See forgejoTimelineEntry.
	var out []LabelEvent
	for page := 1; ; page++ {
		raw, err := f.rawGetLimited(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?page=%d&limit=%d",
			url.PathEscape(slug.owner), url.PathEscape(slug.repo), issueIID, page, forgejoPerPage), maxTraceBytes+1)
		if err != nil {
			return nil, err // already redacted by rawGetLimited
		}
		var entries []forgejoTimelineEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, f.wrapErr("parse issue timeline", err)
		}
		for _, e := range entries {
			// Only label events; other timeline entries (comments, milestone/title
			// changes, assignments) have type != "label" and a null label.
			if e.Type != "label" || e.Label == nil {
				continue
			}
			ev := LabelEvent{
				ID:        e.ID,
				Action:    forgejoLabelAction(e.Body),
				LabelName: e.Label.Name,
				CreatedAt: e.CreatedAt,
			}
			if e.User != nil {
				ev.Username = e.User.Login
			}
			out = append(out, ev)
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list issue label events", forgePaginationCapErr("item", maxForgeItems))
		}
		// The timeline is chronological (oldest first), matching the GitLab driver.
		if len(entries) < forgejoPerPage {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list issue label events", forgePaginationCapErr("page", maxForgePages))
		}
	}
	return out, nil
}

// forgejoLabelAction decodes Forgejo's UNDOCUMENTED label-event convention (R6):
// a label add records body == "1" (issue_label.go: Content "1"), a remove records
// body == "" (deleteIssueLabel omits Content). Both verified live on 16.0.0. This
// is pinned by a test so an SDK/Forgejo change to the convention fails loudly
// rather than silently attributing every event as a remove.
func forgejoLabelAction(body string) string {
	if body == "1" {
		return "add"
	}
	return "remove"
}

// forgejoTimelineEntry is the subset of a Forgejo issue-timeline entry uzi reads.
// It exists because the gitea SDK's TimelineComment types `label` as []*Label,
// which cannot unmarshal Forgejo's single-object label (verified live). Only the
// fields the label-event mapping needs are modelled.
type forgejoTimelineEntry struct {
	ID        int64            `json:"id"`
	Type      string           `json:"type"`
	Body      string           `json:"body"`
	User      *forgejoUserRef  `json:"user"`
	Label     *forgejoLabelRef `json:"label"`
	CreatedAt time.Time        `json:"created_at"`
}

type forgejoUserRef struct {
	Login string `json:"login"`
}

type forgejoLabelRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
