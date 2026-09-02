package forge

import (
	"context"
	"errors"
	"fmt"
)

// ErrGitHubUserNotFound is returned (wrapped) when a GitHub GraphQL operation
// reports a NOT_FOUND error type — for ResolveUserNodeID this means the login
// does not resolve to a user. graphqlDo (github_graphql.go) wraps its redacted error
// with this sentinel whenever any errors[] entry has type "NOT_FOUND", so
// callers can errors.Is against it to distinguish a bad username from a
// permission/transient failure. Every other GraphQL error propagates as a plain
// redacted error.
var ErrGitHubUserNotFound = errors.New("github: user not found")

// ProjectV2CollaboratorRole is the closed set of collaborator roles uzi sets on
// a Projects v2 board via updateProjectV2Collaborators. The wire values are the
// GitHub ProjectV2Roles enum strings.
type ProjectV2CollaboratorRole string

const (
	// RoleReaderCollaborator grants read access (ProjectV2Roles READER).
	RoleReaderCollaborator ProjectV2CollaboratorRole = "READER"
	// RoleNoneCollaborator revokes access (ProjectV2Roles NONE).
	RoleNoneCollaborator ProjectV2CollaboratorRole = "NONE"
)

// projectsync.go defines the OPTIONAL capability interface for GitHub Projects v2
// Status sync (PRD #364) and the github driver's implementation of it, built on
// the raw GraphQL helper in github_graphql.go (graphqlDo).
//
// This is deliberately NOT part of the neutral Forge interface (forge.go):
// GitLab and Forgejo have no Projects v2 equivalent, so only the github driver
// implements ProjectBoardSyncer. Call sites type-assert forge.Forge to it and
// skip the sync when the assertion fails — that assertion IS the forge-isolation
// guarantee (SC-4). The compile-time assertion at the bottom of this file pins
// that *github satisfies it; the gitlab/forgejo drivers deliberately do not.
//
// All types here are forge-agnostic in spelling even though only GitHub uses
// them today: node ids are opaque strings (PVT_…/PVTSSF_…/PVTI_… on GitHub),
// and the board concept is a single-select "Status"-like field with named
// options, one selected per item.

// ProjectV2OwnerKind selects how an owner login resolves to a node id: the
// authenticated viewer, a user, or an organization (PRD #364 D3). GitHub's
// GraphQL exposes viewer{id} / user(login){id} / organization(login){id}
// separately, so the caller must say which.
type ProjectV2OwnerKind int

const (
	// OwnerViewer resolves the authenticated token's own account (viewer{id}).
	// The owner login is ignored for this kind.
	OwnerViewer ProjectV2OwnerKind = iota
	// OwnerUser resolves a user account by login (user(login){id}).
	OwnerUser
	// OwnerOrg resolves an organization by login (organization(login){id}).
	OwnerOrg
)

// ProjectV2OwnerType is the GraphQL `__typename` of a repository owner — the
// closed set uzi distinguishes for the Provision feasibility nudge (PRD #576 M1,
// F-G). The wire values are GitHub's interface-implementation type names as
// returned by `repositoryOwner(login){ __typename }`.
type ProjectV2OwnerType string

const (
	// OwnerTypeUser is a personal account (repositoryOwner __typename "User"). A
	// PAT can create projects only for its own viewer, never for another user, so
	// Provision cannot produce a linkable project for a user-owned repo (F-A).
	OwnerTypeUser ProjectV2OwnerType = "User"
	// OwnerTypeOrg is an organization (repositoryOwner __typename "Organization").
	// Provision is the safe path here when the bot is an org member with project
	// rights.
	OwnerTypeOrg ProjectV2OwnerType = "Organization"
)

// ProjectV2Ref identifies a resolved Projects v2 board: its opaque node id, its
// human-facing number, and its title.
type ProjectV2Ref struct {
	// ID is the project's opaque node id (GitHub: PVT_…).
	ID string
	// Number is the project's human-facing number (the value in its URL).
	Number int
	// Title is the project's display title.
	Title string
}

// ProjectV2Option is one selectable value of a single-select Status field: its
// opaque option id and its display name (the name is what maps 1:1 to a board
// column label).
type ProjectV2Option struct {
	ID   string
	Name string
}

// ProjectV2StatusField is a single-select "Status"-like field: its opaque field
// id (GitHub: PVTSSF_…) and its full option set. The column-name → option-id map
// the sync persists is built from Options.
type ProjectV2StatusField struct {
	ID      string
	Name    string
	Options []ProjectV2Option
}

// ProjectV2NewOption is one option to CREATE on a single-select field. GitHub's
// createProjectV2Field requires a color (one of its 8-color enum) and a
// description (String!, "" when none) per option (F9).
type ProjectV2NewOption struct {
	Name        string
	Color       string
	Description string
}

// ProjectV2ItemStatus is one project item's current Status, as read by the
// reverse-sync poller: the item's node id, the issue number it wraps (0 when the
// item's content is not an issue, e.g. a pull request or a draft), and the
// selected option id ("" when the item has no Status set). The caller matches by
// IssueNumber and diffs OptionID against its stored marker.
type ProjectV2ItemStatus struct {
	ItemID      string
	IssueNumber int64
	OptionID    string
}

// ProjectBoardSyncer is the optional capability a forge driver implements when it
// can project a label board onto a native single-select "Status" board and read
// it back (PRD #364). Only the github driver implements it; GitLab/Forgejo
// forges fail the type assertion and the sync is skipped for them (SC-4). Every
// method surfaces PAT-redacted errors, exactly like the neutral Forge methods.
type ProjectBoardSyncer interface {
	// ResolveProjectV2 resolves an owner login + project number to the project's
	// node id (F2). ownerKind selects the user vs. organization GraphQL root.
	ResolveProjectV2(ctx context.Context, owner string, number int, ownerKind ProjectV2OwnerKind) (ProjectV2Ref, error)
	// ProjectV2StatusFieldByName reads a single-select field by name (e.g.
	// "Status" or "uzi Status") on a project and returns its field id and the full
	// {id,name} option set (F2). It errors if no single-select field of that name
	// exists.
	ProjectV2StatusFieldByName(ctx context.Context, projectID, fieldName string) (ProjectV2StatusField, error)
	// ProjectV2StatusFieldByID reads a single-select field by its node id — the
	// rename-proof identity of the field (PRD #582) — and returns its id, name, and
	// full {id,name} option set. Unlike ProjectV2StatusFieldByName, which resolves the
	// currently-named "Status"/"uzi Status" field and so silently follows a rename or
	// the wrong same-named field, this addresses the EXACT field the caller already
	// holds an id for. It errors if the id resolves to no single-select field. The
	// projectID param is accepted for signature symmetry/logging with the by-name
	// method even though the field node id alone addresses the field.
	ProjectV2StatusFieldByID(ctx context.Context, projectID, fieldID string) (ProjectV2StatusField, error)
	// ResolveIssueNodeID resolves a repo issue to its content node id — the
	// contentId AddProjectV2Item needs (F3).
	ResolveIssueNodeID(ctx context.Context, owner, repo string, issueNumber int) (string, error)
	// AddProjectV2Item idempotently adds contentID to the project and returns the
	// item's node id; an already-present content returns its existing item id (F3).
	AddProjectV2Item(ctx context.Context, projectID, contentID string) (string, error)
	// SetProjectV2ItemStatus sets the item's Status to optionID, or CLEARS it (Open
	// → "No Status") when optionID is "" (F2). Callers apply their own live-value
	// no-op guard before calling — this always issues the mutation.
	SetProjectV2ItemStatus(ctx context.Context, projectID, itemID, fieldID, optionID string) error
	// ReadProjectV2ItemStatuses returns every project item's current Status for the
	// given single-select field, paginating items in pages of 100 (F7). Items whose
	// content is not an issue carry IssueNumber 0; items with no value carry
	// OptionID "".
	ReadProjectV2ItemStatuses(ctx context.Context, projectID, fieldID string) ([]ProjectV2ItemStatus, error)
	// ReadProjectV2ItemStatus returns ONE project item's current Status option id for
	// the given single-select field ("" when the item has no value set — "No Status").
	// A single-item read used by the forward-move live-value no-op guard (D7) so a uzi
	// move can beat a racing GitHub-side drag; cheaper than ReadProjectV2ItemStatuses
	// when only one item is in question.
	ReadProjectV2ItemStatus(ctx context.Context, itemID, fieldID string) (string, error)
	// ResolveProjectV2OwnerID resolves an owner to its node id for createProjectV2
	// (F8): viewer/user/organization per ownerKind.
	ResolveProjectV2OwnerID(ctx context.Context, owner string, ownerKind ProjectV2OwnerKind) (string, error)
	// ResolveRepositoryNodeID resolves owner/repo to the repository node id, needed
	// by createProjectV2's repositoryId and linkProjectV2ToRepository (F8).
	ResolveRepositoryNodeID(ctx context.Context, owner, repo string) (string, error)
	// CreateProjectV2 creates a project owned by ownerID with the given title;
	// repositoryID may be "" to create an unlinked project (F8).
	CreateProjectV2(ctx context.Context, ownerID, title, repositoryID string) (ProjectV2Ref, error)
	// CreateProjectV2Field creates uzi's OWN SINGLE_SELECT field with the given
	// options and returns the field id + created options (F9). Preferred over
	// overwriting the undeletable built-in "Status".
	CreateProjectV2Field(ctx context.Context, projectID, name string, options []ProjectV2NewOption) (ProjectV2StatusField, error)
	// LinkProjectV2ToRepository links a project to a repo so it appears under the
	// repo's Projects tab (F8). Not required for the board's fields/items to work.
	LinkProjectV2ToRepository(ctx context.Context, projectID, repositoryID string) error
	// RepoSlug resolves a numeric forge project id to its owner/name pair (PRD #364
	// M3). The adopt/seed flow needs the owner and repo STRINGS for
	// ResolveProjectV2/ResolveIssueNodeID/ResolveRepositoryNodeID, whereas the rest
	// of uzi keys a repo by its numeric forge_project_id — this bridges the two.
	RepoSlug(ctx context.Context, forgeProjectID int64) (owner, name string, err error)
	// GetProjectV2Visibility reads a board's current public flag by node id
	// (node(id){ ... on ProjectV2 { public } }) (PRD #557 M1). A private board is
	// visible only to accounts granted project-level access.
	GetProjectV2Visibility(ctx context.Context, projectID string) (bool, error)
	// SetProjectV2Visibility writes a board's public flag (updateProjectV2) (PRD
	// #557 M1). true makes the board internet-visible; false makes it private.
	SetProjectV2Visibility(ctx context.Context, projectID string, public bool) error
	// SetProjectV2Collaborator sets one user's collaborator role on a board
	// (updateProjectV2Collaborators, upsert semantics) (PRD #557 M1). Grant Reader
	// with RoleReaderCollaborator; revoke with RoleNoneCollaborator. GitHub exposes
	// no readable collaborator list, so this is a write-only operation.
	SetProjectV2Collaborator(ctx context.Context, projectID, userID string, role ProjectV2CollaboratorRole) error
	// ResolveUserNodeID resolves a login to its user node id (user(login){ id })
	// (PRD #557 M1). Returns ErrGitHubUserNotFound when the login does not resolve
	// (GitHub's NOT_FOUND), so callers can map a bad username to 422 while every
	// other error stays a 500.
	ResolveUserNodeID(ctx context.Context, login string) (string, error)
	// ResolveRepositoryOwnerType reports whether a repo owner login is a User or an
	// Organization via `repositoryOwner(login){ __typename }` (PRD #576 M1, F-G).
	// GitHub's repositoryOwner interface resolves to a User or Organization object,
	// whose __typename distinguishes the two; the Provision feasibility nudge uses
	// this to steer user-owned repos toward Adopt (Provision cannot own a project
	// under a personal account — F-A). Any other/empty __typename is a redacted
	// error.
	ResolveRepositoryOwnerType(ctx context.Context, owner string) (ProjectV2OwnerType, error)
}

// --- github driver implementation ------------------------------------------

// ownerRoot returns the GraphQL root selector for an owner kind. viewer takes no
// login argument; user/organization take login:$login.
func ownerRoot(kind ProjectV2OwnerKind) string {
	switch kind {
	case OwnerViewer:
		return "viewer"
	case OwnerOrg:
		return "organization(login: $login)"
	default:
		return "user(login: $login)"
	}
}

// ownerUsesLogin reports whether ownerRoot(kind) references the $login variable.
// The viewer root does not, and GraphQL's "All Variables Used" rule (spec §5.8.4,
// enforced by GitHub's server) rejects an operation that DECLARES $login without
// using it — so the $login declaration and its variable value must both be omitted
// on the viewer path, not merely left unreferenced.
func ownerUsesLogin(kind ProjectV2OwnerKind) bool { return kind != OwnerViewer }

func (g *github) ResolveProjectV2(ctx context.Context, owner string, number int, ownerKind ProjectV2OwnerKind) (ProjectV2Ref, error) {
	loginDecl := ""
	if ownerUsesLogin(ownerKind) {
		loginDecl = "$login: String!, "
	}
	query := fmt.Sprintf(`query(%s$number: Int!) {
  %s {
    projectV2(number: $number) { id number title }
  }
}`, loginDecl, ownerRoot(ownerKind))
	var out struct {
		User struct {
			ProjectV2 struct {
				ID     string `json:"id"`
				Number int    `json:"number"`
				Title  string `json:"title"`
			} `json:"projectV2"`
		} `json:"user"`
		Organization struct {
			ProjectV2 struct {
				ID     string `json:"id"`
				Number int    `json:"number"`
				Title  string `json:"title"`
			} `json:"projectV2"`
		} `json:"organization"`
		Viewer struct {
			ProjectV2 struct {
				ID     string `json:"id"`
				Number int    `json:"number"`
				Title  string `json:"title"`
			} `json:"projectV2"`
		} `json:"viewer"`
	}
	vars := map[string]any{"number": number}
	if ownerUsesLogin(ownerKind) {
		vars["login"] = owner
	}
	if err := g.graphqlDo(ctx, query, vars, &out); err != nil {
		return ProjectV2Ref{}, err
	}
	var p struct {
		ID     string
		Number int
		Title  string
	}
	switch ownerKind {
	case OwnerViewer:
		p.ID, p.Number, p.Title = out.Viewer.ProjectV2.ID, out.Viewer.ProjectV2.Number, out.Viewer.ProjectV2.Title
	case OwnerOrg:
		p.ID, p.Number, p.Title = out.Organization.ProjectV2.ID, out.Organization.ProjectV2.Number, out.Organization.ProjectV2.Title
	default:
		p.ID, p.Number, p.Title = out.User.ProjectV2.ID, out.User.ProjectV2.Number, out.User.ProjectV2.Title
	}
	if p.ID == "" {
		return ProjectV2Ref{}, fmt.Errorf("github: resolve project: no project #%d found for owner %q", number, owner)
	}
	return ProjectV2Ref{ID: p.ID, Number: p.Number, Title: p.Title}, nil
}

func (g *github) ProjectV2StatusFieldByName(ctx context.Context, projectID, fieldName string) (ProjectV2StatusField, error) {
	const query = `query($projectId: ID!, $name: String!) {
  node(id: $projectId) {
    ... on ProjectV2 {
      field(name: $name) {
        ... on ProjectV2SingleSelectField {
          id
          name
          options { id name }
        }
      }
    }
  }
}`
	var out struct {
		Node struct {
			Field struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Options []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"options"`
			} `json:"field"`
		} `json:"node"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"projectId": projectID, "name": fieldName}, &out); err != nil {
		return ProjectV2StatusField{}, err
	}
	if out.Node.Field.ID == "" {
		return ProjectV2StatusField{}, fmt.Errorf("github: status field: no single-select field %q on project", fieldName)
	}
	return ProjectV2StatusField{
		ID:      out.Node.Field.ID,
		Name:    out.Node.Field.Name,
		Options: toProjectV2Options(out.Node.Field.Options),
	}, nil
}

func (g *github) ProjectV2StatusFieldByID(ctx context.Context, projectID, fieldID string) (ProjectV2StatusField, error) {
	const query = `query($fieldId: ID!) {
  node(id: $fieldId) {
    ... on ProjectV2SingleSelectField {
      id
      name
      options { id name }
    }
  }
}`
	var out struct {
		Node struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Options []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"options"`
		} `json:"node"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"fieldId": fieldID}, &out); err != nil {
		return ProjectV2StatusField{}, err
	}
	if out.Node.ID == "" {
		return ProjectV2StatusField{}, fmt.Errorf("github: status field: no single-select field with id %q on project", fieldID)
	}
	return ProjectV2StatusField{
		ID:      out.Node.ID,
		Name:    out.Node.Name,
		Options: toProjectV2Options(out.Node.Options),
	}, nil
}

func (g *github) ResolveIssueNodeID(ctx context.Context, owner, repo string, issueNumber int) (string, error) {
	const query = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) { id }
  }
}`
	var out struct {
		Repository struct {
			Issue struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"repository"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"owner": owner, "name": repo, "number": issueNumber}, &out); err != nil {
		return "", err
	}
	if out.Repository.Issue.ID == "" {
		return "", fmt.Errorf("github: resolve issue node: no issue #%d in %s/%s", issueNumber, owner, repo)
	}
	return out.Repository.Issue.ID, nil
}

func (g *github) AddProjectV2Item(ctx context.Context, projectID, contentID string) (string, error) {
	const mutation = `mutation($projectId: ID!, $contentId: ID!) {
  addProjectV2ItemById(input: {projectId: $projectId, contentId: $contentId}) {
    item { id }
  }
}`
	var out struct {
		AddProjectV2ItemByID struct {
			Item struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	if err := g.graphqlDo(ctx, mutation, map[string]any{"projectId": projectID, "contentId": contentID}, &out); err != nil {
		return "", err
	}
	if out.AddProjectV2ItemByID.Item.ID == "" {
		return "", fmt.Errorf("github: add project item: mutation returned no item id")
	}
	return out.AddProjectV2ItemByID.Item.ID, nil
}

func (g *github) SetProjectV2ItemStatus(ctx context.Context, projectID, itemID, fieldID, optionID string) error {
	// An empty optionID clears the value (Open → "No Status"), which is a distinct
	// mutation from setting one — updateProjectV2ItemFieldValue has no "unset".
	if optionID == "" {
		const mutation = `mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!) {
  clearProjectV2ItemFieldValue(input: {projectId: $projectId, itemId: $itemId, fieldId: $fieldId}) {
    projectV2Item { id }
  }
}`
		return g.graphqlDo(ctx, mutation, map[string]any{"projectId": projectID, "itemId": itemID, "fieldId": fieldID}, nil)
	}
	const mutation = `mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
  updateProjectV2ItemFieldValue(input: {projectId: $projectId, itemId: $itemId, fieldId: $fieldId, value: {singleSelectOptionId: $optionId}}) {
    projectV2Item { id }
  }
}`
	return g.graphqlDo(ctx, mutation, map[string]any{"projectId": projectID, "itemId": itemID, "fieldId": fieldID, "optionId": optionID}, nil)
}

func (g *github) ReadProjectV2ItemStatuses(ctx context.Context, projectID, fieldID string) ([]ProjectV2ItemStatus, error) {
	const query = `query($projectId: ID!, $cursor: String) {
  node(id: $projectId) {
    ... on ProjectV2 {
      items(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          content { ... on Issue { number } }
          fieldValues(first: 20) {
            nodes {
              ... on ProjectV2ItemFieldSingleSelectValue {
                optionId
                field { ... on ProjectV2SingleSelectField { id } }
              }
            }
          }
        }
      }
    }
  }
}`
	var out []ProjectV2ItemStatus
	var cursor *string
	page := 0
	for {
		page++
		var resp struct {
			Node struct {
				Items struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID      string `json:"id"`
						Content struct {
							Number int64 `json:"number"`
						} `json:"content"`
						FieldValues struct {
							Nodes []struct {
								OptionID string `json:"optionId"`
								Field    struct {
									ID string `json:"id"`
								} `json:"field"`
							} `json:"nodes"`
						} `json:"fieldValues"`
					} `json:"nodes"`
				} `json:"items"`
			} `json:"node"`
		}
		vars := map[string]any{"projectId": projectID}
		if cursor != nil {
			vars["cursor"] = *cursor
		} else {
			vars["cursor"] = nil
		}
		if err := g.graphqlDo(ctx, query, vars, &resp); err != nil {
			return nil, err
		}
		for _, n := range resp.Node.Items.Nodes {
			st := ProjectV2ItemStatus{ItemID: n.ID, IssueNumber: n.Content.Number}
			// Pick the single-select value belonging to the requested field; an item
			// with no value on it stays OptionID "".
			for _, fv := range n.FieldValues.Nodes {
				if fv.Field.ID == fieldID {
					st.OptionID = fv.OptionID
					break
				}
			}
			out = append(out, st)
		}
		if len(out) > maxForgeItems {
			return nil, g.redact.error(forgePaginationCapErr("item", maxForgeItems))
		}
		if !resp.Node.Items.PageInfo.HasNextPage || resp.Node.Items.PageInfo.EndCursor == "" {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(forgePaginationCapErr("page", maxForgePages))
		}
		next := resp.Node.Items.PageInfo.EndCursor
		cursor = &next
	}
	return out, nil
}

func (g *github) ReadProjectV2ItemStatus(ctx context.Context, itemID, fieldID string) (string, error) {
	const query = `query($itemId: ID!) {
  node(id: $itemId) {
    ... on ProjectV2Item {
      fieldValues(first: 20) {
        nodes {
          ... on ProjectV2ItemFieldSingleSelectValue {
            optionId
            field { ... on ProjectV2SingleSelectField { id } }
          }
        }
      }
    }
  }
}`
	var out struct {
		Node struct {
			FieldValues struct {
				Nodes []struct {
					OptionID string `json:"optionId"`
					Field    struct {
						ID string `json:"id"`
					} `json:"field"`
				} `json:"nodes"`
			} `json:"fieldValues"`
		} `json:"node"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"itemId": itemID}, &out); err != nil {
		return "", err
	}
	for _, fv := range out.Node.FieldValues.Nodes {
		if fv.Field.ID == fieldID {
			return fv.OptionID, nil
		}
	}
	return "", nil
}

func (g *github) ResolveProjectV2OwnerID(ctx context.Context, owner string, ownerKind ProjectV2OwnerKind) (string, error) {
	// viewer references no $login, so the declaration (and value) must be omitted
	// entirely — GitHub rejects an operation that declares an unused variable.
	sig := ""
	vars := map[string]any{}
	if ownerUsesLogin(ownerKind) {
		sig = "($login: String!)"
		vars["login"] = owner
	}
	query := fmt.Sprintf(`query%s {
  %s { id }
}`, sig, ownerRoot(ownerKind))
	var out struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := g.graphqlDo(ctx, query, vars, &out); err != nil {
		return "", err
	}
	var id string
	switch ownerKind {
	case OwnerViewer:
		id = out.Viewer.ID
	case OwnerOrg:
		id = out.Organization.ID
	default:
		id = out.User.ID
	}
	if id == "" {
		return "", fmt.Errorf("github: resolve owner id: no node id for owner %q", owner)
	}
	return id, nil
}

func (g *github) ResolveRepositoryNodeID(ctx context.Context, owner, repo string) (string, error) {
	const query = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) { id }
}`
	var out struct {
		Repository struct {
			ID string `json:"id"`
		} `json:"repository"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"owner": owner, "name": repo}, &out); err != nil {
		return "", err
	}
	if out.Repository.ID == "" {
		return "", fmt.Errorf("github: resolve repository node: no node id for %s/%s", owner, repo)
	}
	return out.Repository.ID, nil
}

func (g *github) CreateProjectV2(ctx context.Context, ownerID, title, repositoryID string) (ProjectV2Ref, error) {
	const mutation = `mutation($ownerId: ID!, $title: String!, $repositoryId: ID) {
  createProjectV2(input: {ownerId: $ownerId, title: $title, repositoryId: $repositoryId}) {
    projectV2 { id number title }
  }
}`
	// repositoryId is nullable (ID, not ID!); send JSON null when unset so an
	// unlinked project can be created.
	var repoVar any
	if repositoryID != "" {
		repoVar = repositoryID
	}
	var out struct {
		CreateProjectV2 struct {
			ProjectV2 struct {
				ID     string `json:"id"`
				Number int    `json:"number"`
				Title  string `json:"title"`
			} `json:"projectV2"`
		} `json:"createProjectV2"`
	}
	if err := g.graphqlDo(ctx, mutation, map[string]any{"ownerId": ownerID, "title": title, "repositoryId": repoVar}, &out); err != nil {
		return ProjectV2Ref{}, err
	}
	if out.CreateProjectV2.ProjectV2.ID == "" {
		return ProjectV2Ref{}, fmt.Errorf("github: create project: mutation returned no project id")
	}
	return ProjectV2Ref{
		ID:     out.CreateProjectV2.ProjectV2.ID,
		Number: out.CreateProjectV2.ProjectV2.Number,
		Title:  out.CreateProjectV2.ProjectV2.Title,
	}, nil
}

func (g *github) CreateProjectV2Field(ctx context.Context, projectID, name string, options []ProjectV2NewOption) (ProjectV2StatusField, error) {
	const mutation = `mutation($projectId: ID!, $name: String!, $options: [ProjectV2SingleSelectFieldOptionInput!]!) {
  createProjectV2Field(input: {projectId: $projectId, dataType: SINGLE_SELECT, name: $name, singleSelectOptions: $options}) {
    projectV2Field {
      ... on ProjectV2SingleSelectField {
        id
        name
        options { id name }
      }
    }
  }
}`
	optVars := make([]map[string]any, 0, len(options))
	for _, o := range options {
		color := o.Color
		if color == "" {
			// color is a required enum; default to GRAY rather than send an empty
			// value that fails the mutation.
			color = "GRAY"
		}
		optVars = append(optVars, map[string]any{"name": o.Name, "color": color, "description": o.Description})
	}
	var out struct {
		CreateProjectV2Field struct {
			ProjectV2Field struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Options []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"options"`
			} `json:"projectV2Field"`
		} `json:"createProjectV2Field"`
	}
	if err := g.graphqlDo(ctx, mutation, map[string]any{"projectId": projectID, "name": name, "options": optVars}, &out); err != nil {
		return ProjectV2StatusField{}, err
	}
	if out.CreateProjectV2Field.ProjectV2Field.ID == "" {
		return ProjectV2StatusField{}, fmt.Errorf("github: create field: mutation returned no field id")
	}
	return ProjectV2StatusField{
		ID:      out.CreateProjectV2Field.ProjectV2Field.ID,
		Name:    out.CreateProjectV2Field.ProjectV2Field.Name,
		Options: toProjectV2Options(out.CreateProjectV2Field.ProjectV2Field.Options),
	}, nil
}

func (g *github) LinkProjectV2ToRepository(ctx context.Context, projectID, repositoryID string) error {
	const mutation = `mutation($projectId: ID!, $repositoryId: ID!) {
  linkProjectV2ToRepository(input: {projectId: $projectId, repositoryId: $repositoryId}) {
    repository { id }
  }
}`
	return g.graphqlDo(ctx, mutation, map[string]any{"projectId": projectID, "repositoryId": repositoryID}, nil)
}

// RepoSlug resolves a numeric forge project id to its owner/name pair by reusing
// the driver's cached repoSlugFor (github.go) — the same resolver the pipeline
// reads use. The adopt flow (PRD #364 M3) needs the owner/repo strings the
// Projects v2 GraphQL queries take, not the numeric id uzi stores.
func (g *github) RepoSlug(ctx context.Context, forgeProjectID int64) (string, string, error) {
	s, err := g.repoSlugFor(ctx, forgeProjectID)
	if err != nil {
		return "", "", err
	}
	return s.owner, s.repo, nil
}

func (g *github) GetProjectV2Visibility(ctx context.Context, projectID string) (bool, error) {
	const query = `query($projectId: ID!) {
  node(id: $projectId) {
    ... on ProjectV2 { public }
  }
}`
	var out struct {
		Node struct {
			Public bool `json:"public"`
		} `json:"node"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"projectId": projectID}, &out); err != nil {
		return false, err
	}
	return out.Node.Public, nil
}

func (g *github) SetProjectV2Visibility(ctx context.Context, projectID string, public bool) error {
	const mutation = `mutation($projectId: ID!, $public: Boolean!) {
  updateProjectV2(input: {projectId: $projectId, public: $public}) {
    projectV2 { id }
  }
}`
	return g.graphqlDo(ctx, mutation, map[string]any{"projectId": projectID, "public": public}, nil)
}

func (g *github) SetProjectV2Collaborator(ctx context.Context, projectID, userID string, role ProjectV2CollaboratorRole) error {
	const mutation = `mutation($projectId: ID!, $collaborators: [ProjectV2Collaborator!]!) {
  updateProjectV2Collaborators(input: {projectId: $projectId, collaborators: $collaborators}) {
    clientMutationId
  }
}`
	// The ProjectV2Collaborator input carries userId, role, teamId; uzi sends a
	// single user grant/revoke (userId + role only).
	collaborators := []map[string]any{{"userId": userID, "role": string(role)}}
	return g.graphqlDo(ctx, mutation, map[string]any{"projectId": projectID, "collaborators": collaborators}, nil)
}

func (g *github) ResolveUserNodeID(ctx context.Context, login string) (string, error) {
	const query = `query($login: String!) {
  user(login: $login) { id }
}`
	var out struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"login": login}, &out); err != nil {
		if errors.Is(err, ErrGitHubUserNotFound) {
			return "", ErrGitHubUserNotFound
		}
		return "", err
	}
	if out.User.ID == "" {
		// Defensive: a null user with no errors entry (belt and braces) — treat as
		// not found rather than returning an empty id.
		return "", ErrGitHubUserNotFound
	}
	return out.User.ID, nil
}

func (g *github) ResolveRepositoryOwnerType(ctx context.Context, owner string) (ProjectV2OwnerType, error) {
	const query = `query($login: String!) {
  repositoryOwner(login: $login) { __typename }
}`
	var out struct {
		RepositoryOwner struct {
			TypeName string `json:"__typename"`
		} `json:"repositoryOwner"`
	}
	if err := g.graphqlDo(ctx, query, map[string]any{"login": owner}, &out); err != nil {
		return "", err
	}
	switch out.RepositoryOwner.TypeName {
	case string(OwnerTypeUser):
		return OwnerTypeUser, nil
	case string(OwnerTypeOrg):
		return OwnerTypeOrg, nil
	default:
		// An empty or unexpected __typename (owner not found, or a future owner
		// interface implementation) — redact and refuse rather than guess. No PAT
		// is interpolated into this message, but route it through redact for
		// consistency with the other resolvers.
		return "", g.wrapErr("resolve owner type", fmt.Errorf("unexpected owner __typename %q for %q", out.RepositoryOwner.TypeName, owner))
	}
}

// toProjectV2Options maps the decoded {id,name} option nodes onto the neutral
// option slice. Shared by the field-read and field-create paths.
func toProjectV2Options(nodes []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}) []ProjectV2Option {
	out := make([]ProjectV2Option, 0, len(nodes))
	for _, o := range nodes {
		out = append(out, ProjectV2Option{ID: o.ID, Name: o.Name})
	}
	return out
}

// Compile-time proof that the github driver satisfies the optional capability
// (SC-4's positive half). The gitlab/forgejo drivers deliberately do NOT
// implement ProjectBoardSyncer — call sites type-assert and skip the sync for
// them; TestForgeIsolationCapability in projectsync_test.go pins the negative
// half.
var _ ProjectBoardSyncer = (*github)(nil)
