package issuetracker

import (
	"context"
	"net/http"
	"net/url"
	"slices"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// listTags returns the tags the token can see, in the server's order.
func (y *youTrack) listTags(ctx context.Context, fields string) ([]ytTag, error) {
	tags, err := ytList[ytTag](ctx, y, []string{"tags"}, url.Values{"fields": {fields}})
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		if !entityIDRE.MatchString(t.ID) {
			return nil, y.fail("youtrack: tag with a malformed id in the response")
		}
	}
	return tags, nil
}

// sharingCopy is the sharing settings of the eligibility tag by id, the form
// POST /api/tags takes.
type sharingCopy struct {
	Read ytSharingIDs `json:"readSharingSettings"`
	Use  ytSharingIDs `json:"tagSharingSettings"`
}

type ytSharingIDs struct {
	Groups []ytIDRef `json:"permittedGroups"`
	Users  []ytIDRef `json:"permittedUsers"`
}

type ytIDRef struct {
	ID string `json:"id"`
}

// queryTagNames gives the tag names the ListIssues query narrows by, one per
// distinct label. Every label but the eligibility one is its own name (the
// account-owned tag resolveTagIDs found carries exactly that name). The
// eligibility label stands for the stored tag id, so its term uses the tag's
// live name, read by id: the stored name may be stale after a rename in
// YouTrack. A tag that cannot be read (gone, or no longer visible to the token)
// is an error, so nothing is listed. A live name that labelNameProblem refuses
// (a brace would break the query's quoting) gets no term at all: the project
// term and the client-side id filter still select the right issues, at the cost
// of a wider page walk.
func (y *youTrack) queryTagNames(ctx context.Context, labels []string) ([]string, error) {
	names := make([]string, 0, len(labels))
	seen := make(map[string]bool, len(labels))
	for _, l := range labels {
		if seen[l] {
			continue
		}
		seen[l] = true
		if l != y.eligibility.Name {
			names = append(names, l)
			continue
		}
		live, err := y.eligibilityLiveName(ctx)
		if err != nil {
			return nil, err
		}
		if !labelNameProblem(live) {
			names = append(names, live)
		}
	}
	return names, nil
}

// eligibilityLiveName reads the eligibility tag by its stored id and returns
// the name it has in YouTrack now.
func (y *youTrack) eligibilityLiveName(ctx context.Context) (string, error) {
	var t ytTag
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodGet,
		path:   []string{"tags", y.eligibility.ID},
		query:  url.Values{"fields": {tagFields}},
		limit:  objectBytes,
		prov:   forge.ResponseSuccess,
	}, &t)
	if err != nil {
		return "", err
	}
	if t.ID != y.eligibility.ID {
		return "", y.fail("youtrack: the eligibility tag read by id came back as another tag")
	}
	return t.Name, nil
}

// eligibilitySharing reads the eligibility tag's read and tag sharing settings
// as ids, plus its owner. A tag the driver creates gets the same settings, so
// whoever can see and apply the eligibility tag can see and apply the tags the
// driver makes. The eligibility tag's owner has both rights by owning it, which
// the settings do not list, so the owner is added to the users of both copies
// (once, and not when the owner is account, the token's own account, which owns
// the new tag anyway).
func (y *youTrack) eligibilitySharing(ctx context.Context, account Account) (sharingCopy, error) {
	const spec = "permittedGroups(id),permittedUsers(id)"
	var t struct {
		Owner *ytUser    `json:"owner"`
		Read  *ytSharing `json:"readSharingSettings"`
		Use   *ytSharing `json:"tagSharingSettings"`
	}
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodGet,
		path:   []string{"tags", y.eligibility.ID},
		query:  url.Values{"fields": {"owner(id),readSharingSettings(" + spec + "),tagSharingSettings(" + spec + ")"}},
		limit:  objectBytes,
		prov:   forge.ResponseSuccess,
	}, &t)
	if err != nil {
		return sharingCopy{}, err
	}
	if t.Owner != nil && !entityIDRE.MatchString(t.Owner.ID) {
		return sharingCopy{}, y.fail("youtrack: eligibility tag owner with a malformed id in the response")
	}
	var c sharingCopy
	for _, p := range []struct {
		from *ytSharing
		to   *ytSharingIDs
	}{{t.Read, &c.Read}, {t.Use, &c.Use}} {
		p.to.Groups, p.to.Users = []ytIDRef{}, []ytIDRef{}
		if p.from == nil {
			continue
		}
		for _, g := range p.from.Groups {
			if !entityIDRE.MatchString(g.ID) {
				return sharingCopy{}, y.fail("youtrack: sharing settings with a malformed id in the response")
			}
			p.to.Groups = append(p.to.Groups, ytIDRef{ID: g.ID})
		}
		for _, u := range p.from.Users {
			if !entityIDRE.MatchString(u.ID) {
				return sharingCopy{}, y.fail("youtrack: sharing settings with a malformed id in the response")
			}
			p.to.Users = append(p.to.Users, ytIDRef{ID: u.ID})
		}
	}
	if t.Owner != nil && t.Owner.ID != account.ID {
		owner := ytIDRef{ID: t.Owner.ID}
		for _, to := range []*ytSharingIDs{&c.Read, &c.Use} {
			if !slices.Contains(to.Users, owner) {
				to.Users = append(to.Users, owner)
			}
		}
	}
	return c, nil
}

// createTag creates a tag owned by the token's account, with the sharing
// settings in share, and returns it.
func (y *youTrack) createTag(ctx context.Context, name string, share sharingCopy) (ytTag, error) {
	var t ytTag
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodPost,
		path:   []string{"tags"},
		query:  url.Values{"fields": {ownedTagFields}},
		body: map[string]any{
			"name":                name,
			"readSharingSettings": share.Read,
			"tagSharingSettings":  share.Use,
		},
		limit: objectBytes,
		prov:  forge.ResponseSuccess,
	}, &t)
	if err != nil {
		return ytTag{}, err
	}
	if !entityIDRE.MatchString(t.ID) {
		return ytTag{}, y.fail("youtrack: tag with a malformed id in the response")
	}
	return t, nil
}

// resolveTagIDs maps label names to tag ids, the one place a name becomes a tag.
//   - The eligibility name is the stored eligibility tag's id, whoever owns that
//     tag and whatever other tag carries the name. It is never created.
//   - Any other name is the visible tag the token's own account owns. With create
//     set, a missing one is created with the eligibility tag's sharing settings
//     plus its owner (see eligibilitySharing); without it, the name is left out
//     of the result.
//
// The eligibility tag is never a candidate for another name, even when its
// YouTrack name drifted to one: name matching skips the eligibility tag's id,
// so such a name gets an account-owned tag of its own.
//
// A tag of the same name that someone else owns is never used for such a name,
// because its owner may change what it means or who can see it. It makes no
// request when every name is the eligibility name.
func (y *youTrack) resolveTagIDs(ctx context.Context, names []string, create bool) (map[string]string, error) {
	ids := make(map[string]string, len(names))
	var others []string
	for _, n := range names {
		if _, done := ids[n]; done {
			continue
		}
		if n == y.eligibility.Name {
			ids[n] = y.eligibility.ID
			continue
		}
		ids[n] = ""
		others = append(others, n)
	}
	if len(others) == 0 {
		return ids, nil
	}
	account, err := y.connAccount(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := y.listTags(ctx, ownedTagFields)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.ID == y.eligibility.ID || t.Owner == nil || t.Owner.ID != account.ID {
			continue
		}
		if _, dup := owned[t.Name]; !dup {
			owned[t.Name] = t.ID
		}
	}
	var share *sharingCopy
	for _, n := range others {
		id, ok := owned[n]
		switch {
		case ok:
			ids[n] = id
		case !create:
			delete(ids, n)
		default:
			if share == nil {
				c, err := y.eligibilitySharing(ctx, account)
				if err != nil {
					return nil, err
				}
				share = &c
			}
			t, err := y.createTag(ctx, n, *share)
			if err != nil {
				return nil, err
			}
			ids[n] = t.ID
		}
	}
	return ids, nil
}

// ListLabels implements forge.IssueTracker. Labels are YouTrack tags, global
// to the account; the result is every visible tag name, whoever owns the tag. A
// name carried by several tags is listed once.
func (y *youTrack) ListLabels(ctx context.Context, projectID int64) ([]forge.Label, error) {
	if _, err := y.requireProject(projectID); err != nil {
		return nil, err
	}
	tags, err := y.listTags(ctx, tagFields)
	if err != nil {
		return nil, err
	}
	out := make([]forge.Label, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, forge.Label{Name: t.Name})
	}
	return out, nil
}

// EnsureLabels implements forge.IssueTracker: it creates the tags the token's
// account does not own yet, by name (see resolveTagIDs). It never creates the
// eligibility tag. Label.Color is ignored, because YouTrack colours are palette
// ids.
func (y *youTrack) EnsureLabels(ctx context.Context, projectID int64, labels []forge.Label) error {
	if _, err := y.requireProject(projectID); err != nil {
		return err
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	if err := y.checkLabelNames(names); err != nil {
		return err
	}
	_, err := y.resolveTagIDs(ctx, names, true)
	return err
}

// UpdateIssueLabels implements forge.IssueTracker. It makes one REST call per
// tag, adds first and then removes, so it is not atomic. The commands API is
// not used, so a label name never enters a command query. Names resolve to tag
// ids (see resolveTagIDs) and "already on the issue" is decided by id. An add
// whose tag does not exist is created; a remove whose tag does not exist or is
// not on the issue is skipped.
func (y *youTrack) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if _, err := y.requireProject(projectID); err != nil {
		return err
	}
	if err := y.checkLabelNames(add, remove); err != nil {
		return err
	}
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(w.Tags))
	for _, t := range w.Tags {
		present[t.ID] = true
	}
	if len(add) > 0 {
		ids, err := y.resolveTagIDs(ctx, add, true)
		if err != nil {
			return err
		}
		for _, n := range add {
			if id := ids[n]; !present[id] {
				if err := y.attachTag(ctx, w.ID, id); err != nil {
					return err
				}
				present[id] = true
			}
		}
	}
	if len(remove) > 0 {
		ids, err := y.resolveTagIDs(ctx, remove, false)
		if err != nil {
			return err
		}
		for _, n := range remove {
			if id, ok := ids[n]; ok && present[id] {
				if err := y.detachTag(ctx, w.ID, id); err != nil {
					return err
				}
				delete(present, id)
			}
		}
	}
	return nil
}

func (y *youTrack) attachTag(ctx context.Context, issueID, tagID string) error {
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodPost,
		path:   []string{"issues", issueID, "tags"},
		query:  url.Values{"fields": {tagFields}},
		body:   map[string]string{"id": tagID},
		limit:  objectBytes,
		prov:   forge.ResponseSuccess,
	}, nil)
	return err
}

func (y *youTrack) detachTag(ctx context.Context, issueID, tagID string) error {
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodDelete,
		path:   []string{"issues", issueID, "tags", tagID},
		limit:  objectBytes,
		prov:   forge.ResponseSuccess,
	}, nil)
	return err
}
