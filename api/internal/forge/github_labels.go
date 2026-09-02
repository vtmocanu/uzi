package forge

// github_labels.go is the GitHub label seam: ListLabels, listRepoLabels,
// EnsureLabels, UpdateIssueLabels, and ListIssueLabelEvents.

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v90/github"
)

func (g *github) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	labels, err := g.listRepoLabels(ctx, slug)
	if err != nil {
		return nil, err
	}
	out := make([]Label, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		out = append(out, Label{Name: l.Name, Color: l.Color})
	}
	return out, nil
}

// listRepoLabels returns every label on a repo, paginated internally. GitHub
// (unlike Forgejo) keys label writes by NAME, so the driver needs no name→id
// resolution — this backs ListLabels and EnsureLabels' missing-set computation.
func (g *github) listRepoLabels(ctx context.Context, slug repoSlug) ([]*gh.Label, error) {
	opt := &gh.ListOptions{PerPage: githubPerPage}
	wrap := func(e error) error { return g.wrapErr("list labels", e) }
	return paginate(wrap, func(page int) ([]*gh.Label, int, error) {
		opt.Page = page
		labels, resp, err := g.client.Issues.ListLabels(ctx, slug.owner, slug.repo, opt)
		if err != nil {
			return nil, 0, err
		}
		return labels, resp.NextPage, nil
	})
}

func (g *github) EnsureLabels(ctx context.Context, projectID int64, labels []Label) error {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	existing, err := g.listRepoLabels(ctx, slug)
	if err != nil {
		return err
	}
	have := make(map[string]struct{}, len(existing))
	for _, l := range existing {
		if l != nil {
			have[l.Name] = struct{}{}
		}
	}
	for _, l := range labels {
		if _, ok := have[l.Name]; ok {
			continue
		}
		// GitHub requires a 6-hex color with NO leading '#'; uzi's callers pass
		// "#rrggbb", so strip the '#' and default an empty one.
		color := strings.TrimPrefix(l.Color, "#")
		if color == "" {
			color = githubDefaultLabelColor
		}
		req := gh.CreateIssueLabelRequest{Name: l.Name, Color: &color}
		if _, _, err := g.client.Issues.CreateLabel(ctx, slug.owner, slug.repo, req); err != nil {
			return g.wrapErr(fmt.Sprintf("create label %q", l.Name), err)
		}
	}
	return nil
}

func (g *github) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	// GitHub's set-replace is PUT /issues/{n}/labels (ReplaceLabelsForIssue). Read
	// the current set, compute the target client-side (current − remove + add), and
	// PUT once. An unrelated label the caller neither adds nor removes SURVIVES
	// because it stays in target. The read/write is not transactional, so the same
	// lost-update window the Forgejo/GitLab drivers accept (D3) applies.
	cur, _, err := g.client.Issues.Get(ctx, slug.owner, slug.repo, int(issueIID))
	if err != nil {
		return g.wrapErr("update issue labels: read current issue", err)
	}
	current := map[string]struct{}{}
	for _, l := range cur.Labels {
		if l != nil {
			current[l.Name] = struct{}{}
		}
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
	if _, _, err := g.client.Issues.ReplaceLabelsForIssue(ctx, slug.owner, slug.repo, int(issueIID), names); err != nil {
		return g.wrapErr("update issue labels", err)
	}
	return nil
}

func (g *github) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// GET /issues/{n}/events is well-typed in go-github (unlike Forgejo's timeline,
	// which needed a hand-parse): each IssueEvent carries event, actor, created_at,
	// and a single label object. "labeled"→add, "unlabeled"→remove; other events are
	// skipped. Chronological (oldest first), matching the GitLab driver. Paginated.
	opt := &gh.ListOptions{PerPage: githubPerPage}
	wrap := func(e error) error { return g.wrapErr("list issue label events", e) }
	return paginate(wrap, func(page int) ([]LabelEvent, int, error) {
		opt.Page = page
		events, resp, err := g.client.Issues.ListIssueEvents(ctx, slug.owner, slug.repo, int(issueIID), opt)
		if err != nil {
			return nil, 0, err
		}
		var items []LabelEvent
		for _, e := range events {
			if e == nil {
				continue
			}
			var action string
			switch e.GetEvent() {
			case "labeled":
				action = "add"
			case "unlabeled":
				action = "remove"
			default:
				continue
			}
			ev := LabelEvent{
				ID:        e.GetID(),
				Action:    action,
				CreatedAt: e.GetCreatedAt().Time,
			}
			if l := e.GetLabel(); l != nil {
				ev.LabelName = l.Name
			}
			if a := e.GetActor(); a != nil {
				ev.Username = a.GetLogin()
			}
			items = append(items, ev)
		}
		return items, resp.NextPage, nil
	})
}
