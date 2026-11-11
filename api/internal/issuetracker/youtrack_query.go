package issuetracker

import (
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// maxLabelNameLen bounds a tag name the driver will send.
const maxLabelNameLen = 200

// checkLabelName refuses a name that could change the meaning of a query or
// that carries invisible characters: braces (the query's quoting), control and
// format characters, and an empty name. The error never echoes the name.
func (y *youTrack) checkLabelName(name string) error {
	if labelNameProblem(name) {
		return y.red.sentinel(ErrInvalidLabelName, 0, "name is empty, too long, or has a brace or control character")
	}
	return nil
}

// labelNameProblem reports whether name is one checkLabelName refuses.
func labelNameProblem(name string) bool {
	bad := strings.TrimSpace(name) == "" || len(name) > maxLabelNameLen
	for _, c := range name {
		if c == '{' || c == '}' || InvisibleRune(c) {
			bad = true
		}
	}
	return bad
}

func (y *youTrack) checkLabelNames(names ...[]string) error {
	for _, list := range names {
		for _, n := range list {
			if err := y.checkLabelName(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// issueQuery builds the YouTrack search query for ListIssues. Every label name
// has passed checkLabelName, so none can close its brace group. The date floor
// is one UTC day early: the query's granularity is a day, and the driver
// filters again on the exact millisecond.
func issueQuery(shortName string, opts forge.ListIssuesOptions) string {
	parts := []string{"project: {" + shortName + "}"}
	for _, l := range opts.Labels {
		parts = append(parts, "tag: {"+l+"}")
	}
	switch opts.State {
	case forge.StateOpened:
		parts = append(parts, "#Unresolved")
	case forge.StateClosed:
		parts = append(parts, "#Resolved")
	}
	if opts.UpdatedAfter != nil {
		parts = append(parts, "updated: "+opts.UpdatedAfter.UTC().AddDate(0, 0, -1).Format(time.DateOnly)+" .. *")
	}
	parts = append(parts, "sort by: updated asc")
	return strings.Join(parts, " ")
}

// matchesIssue is the client-side filter that backs the query: the resolved tag
// id of every label (tagIDs, from resolveTagIDs, so a same-named tag of someone
// else does not match), exact state, the exact update instant, and the bound
// project.
func (y *youTrack) matchesIssue(w ytIssue, opts forge.ListIssuesOptions, tagIDs map[string]string) bool {
	if !y.inBoundProject(w) {
		return false
	}
	for _, l := range opts.Labels {
		found := false
		for _, t := range w.Tags {
			if t.ID == tagIDs[l] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	switch opts.State {
	case forge.StateOpened:
		if w.Resolved != nil {
			return false
		}
	case forge.StateClosed:
		if w.Resolved == nil {
			return false
		}
	}
	return opts.UpdatedAfter == nil || w.Updated >= opts.UpdatedAfter.UnixMilli()
}
