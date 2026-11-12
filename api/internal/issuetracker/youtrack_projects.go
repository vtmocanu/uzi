package issuetracker

import (
	"context"
	"net/url"
)

// ListProjects implements Driver.
func (y *youTrack) ListProjects(ctx context.Context) ([]Project, error) {
	wire, err := ytList[ytProject](ctx, y, []string{"admin", "projects"}, url.Values{"fields": {"id,shortName,name"}})
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(wire))
	for _, p := range wire {
		if !entityIDRE.MatchString(p.ID) || !shortNameRE.MatchString(p.ShortName) {
			return nil, y.fail("youtrack: project with a malformed id in the response")
		}
		out = append(out, Project(p))
	}
	return out, nil
}

type ytProjectField struct {
	ID    string `json:"id"`
	Type  string `json:"$type"`
	Field *struct {
		Name string `json:"name"`
	} `json:"field"`
}

type ytStateValue struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsResolved bool   `json:"isResolved"`
	Ordinal    int    `json:"ordinal"`
}

// ListStates implements Driver. It finds the project's State field (named by
// Options.StateField, "State" by default) and lists the values of its bundle.
func (y *youTrack) ListStates(ctx context.Context, projectID string) ([]State, error) {
	if !entityIDRE.MatchString(projectID) {
		return nil, y.fail("youtrack: invalid project id")
	}
	fields, err := ytList[ytProjectField](ctx, y, []string{"admin", "projects", projectID, "customFields"},
		url.Values{"fields": {"id,field(name),$type,bundle(id)"}})
	if err != nil {
		return nil, err
	}
	var fieldID string
	for _, f := range fields {
		if f.Type == "StateProjectCustomField" && f.Field != nil && f.Field.Name == y.stateField {
			fieldID = f.ID
			break
		}
	}
	if fieldID == "" {
		return nil, y.fail("youtrack: the project has no %q state field", y.stateField)
	}
	if !entityIDRE.MatchString(fieldID) {
		return nil, y.fail("youtrack: state field with a malformed id in the response")
	}
	values, err := ytList[ytStateValue](ctx, y, []string{"admin", "projects", projectID, "customFields", fieldID, "bundle", "values"},
		url.Values{"fields": {"id,name,isResolved,ordinal"}})
	if err != nil {
		return nil, err
	}
	out := make([]State, 0, len(values))
	for _, v := range values {
		out = append(out, State{ID: v.ID, Name: v.Name, Resolved: v.IsResolved, Ordinal: v.Ordinal})
	}
	return out, nil
}
