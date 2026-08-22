package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// graphqlRoute is the ABSOLUTE path the mock mux serves for the GraphQL endpoint.
// The driver derives its endpoint from g.client.BaseURL(), which for the
// WithEnterpriseURLs httptest base is "<srv>/api/v3/", so graphqlEndpoint maps it
// to "<srv>/api/graphql" (GHES serves GraphQL at /api/graphql, not under /api/v3).
// newMockGitHub mounts any "/api/…" route key absolute (see there), so this
// matches exactly what the driver POSTs to. Verified end to end by the tests
// reaching this handler.
const graphqlRoute = "/api/graphql"

// gqlRequest is the shape the driver POSTs.
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// readGQL decodes the POSTed GraphQL request body.
func readGQL(t *testing.T, r *http.Request) gqlRequest {
	t.Helper()
	var req gqlRequest
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode graphql request: %v (body=%q)", err, string(body))
	}
	return req
}

// writeGQLData writes a GraphQL success envelope wrapping data.
func writeGQLData(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// TestGitHubGraphQLEndpointDerivation pins that the endpoint is derived from the
// REST base, never hardcoded — the default host maps to api.github.com/graphql
// and an enterprise/httptest base "<X>/api/v3" maps to "<X>/api/graphql".
func TestGitHubGraphQLEndpointDerivation(t *testing.T) {
	cases := []struct {
		restBase string
		want     string
	}{
		{"", "https://api.github.com/graphql"},
		{"https://api.github.com/", "https://api.github.com/graphql"},
		{"http://127.0.0.1:52345/api/v3/", "http://127.0.0.1:52345/api/graphql"},
		{"https://ghe.example.com/api/v3/", "https://ghe.example.com/api/graphql"},
	}
	for _, tc := range cases {
		if got := graphqlEndpoint(tc.restBase); got != tc.want {
			t.Errorf("graphqlEndpoint(%q) = %q, want %q", tc.restBase, got, tc.want)
		}
	}
}

// TestGitHubResolveProjectV2 pins the user-owner project resolve: the query hits
// the derived GraphQL route with the authenticated Bearer header, and the node id
// is returned.
func TestGitHubResolveProjectV2(t *testing.T) {
	var gotVars map[string]any
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			gotVars = readGQL(t, r).Variables
			writeGQLData(w, map[string]any{
				"user": map[string]any{
					"projectV2": map[string]any{"id": "PVT_123", "number": 7, "title": "Board"},
				},
			})
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	ref, err := d.ResolveProjectV2(context.Background(), "acme", 7, OwnerUser)
	if err != nil {
		t.Fatalf("ResolveProjectV2: %v", err)
	}
	if ref.ID != "PVT_123" || ref.Number != 7 || ref.Title != "Board" {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	// The GraphQL POST must carry the authenticated Bearer header (never logClient).
	if m.gotAuth != "Bearer ghp_classicTokenValue1234567890" {
		t.Fatalf("graphql call missing/incorrect auth header: %q", m.gotAuth)
	}
	if gotVars["login"] != "acme" {
		t.Errorf("login var not sent: %+v", gotVars)
	}
}

// TestGitHubResolveProjectV2Viewer pins the OwnerViewer path (the iota zero value,
// used when a token syncs its own personal project): the query must NOT declare the
// unused $login variable and must NOT send a login value, because GitHub's "All
// Variables Used" rule rejects an operation that declares a variable it never uses.
func TestGitHubResolveProjectV2Viewer(t *testing.T) {
	var gotQuery string
	var gotVars map[string]any
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			req := readGQL(t, r)
			gotQuery, gotVars = req.Query, req.Variables
			writeGQLData(w, map[string]any{
				"viewer": map[string]any{
					"projectV2": map[string]any{"id": "PVT_self", "number": 3, "title": "Mine"},
				},
			})
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	ref, err := d.ResolveProjectV2(context.Background(), "", 3, OwnerViewer)
	if err != nil {
		t.Fatalf("ResolveProjectV2(viewer): %v", err)
	}
	if ref.ID != "PVT_self" || ref.Number != 3 {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	if strings.Contains(gotQuery, "$login") {
		t.Errorf("viewer query must not declare $login (GitHub rejects unused variables): %q", gotQuery)
	}
	if _, present := gotVars["login"]; present {
		t.Errorf("viewer path must not send a login variable: %+v", gotVars)
	}
}

// TestGitHubResolveProjectV2OwnerIDViewer pins the same for owner-id resolution: the
// viewer query carries no variable declaration list at all.
func TestGitHubResolveProjectV2OwnerIDViewer(t *testing.T) {
	var gotQuery string
	var gotVars map[string]any
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			req := readGQL(t, r)
			gotQuery, gotVars = req.Query, req.Variables
			writeGQLData(w, map[string]any{"viewer": map[string]any{"id": "U_self"}})
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	id, err := d.ResolveProjectV2OwnerID(context.Background(), "", OwnerViewer)
	if err != nil {
		t.Fatalf("ResolveProjectV2OwnerID(viewer): %v", err)
	}
	if id != "U_self" {
		t.Fatalf("unexpected owner id: %q", id)
	}
	if strings.Contains(gotQuery, "$login") {
		t.Errorf("viewer owner-id query must not declare $login: %q", gotQuery)
	}
	if len(gotVars) != 0 {
		t.Errorf("viewer owner-id path must send no variables: %+v", gotVars)
	}
}

// TestGitHubProjectV2StatusField pins reading a single-select field's id + options.
func TestGitHubProjectV2StatusField(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			req := readGQL(t, r)
			if req.Variables["name"] != "Status" {
				t.Errorf("field name var wrong: %+v", req.Variables)
			}
			writeGQLData(w, map[string]any{
				"node": map[string]any{
					"field": map[string]any{
						"id":   "PVTSSF_1",
						"name": "Status",
						"options": []map[string]any{
							{"id": "opt_todo", "name": "Planned"},
							{"id": "opt_prog", "name": "In Progress"},
						},
					},
				},
			})
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	f, err := d.ProjectV2StatusFieldByName(context.Background(), "PVT_123", "Status")
	if err != nil {
		t.Fatalf("ProjectV2StatusFieldByName: %v", err)
	}
	if f.ID != "PVTSSF_1" || len(f.Options) != 2 {
		t.Fatalf("unexpected field: %+v", f)
	}
	if f.Options[0].ID != "opt_todo" || f.Options[0].Name != "Planned" || f.Options[1].Name != "In Progress" {
		t.Fatalf("options not mapped: %+v", f.Options)
	}
}

// TestGitHubAddProjectV2ItemIdempotent pins F3: addProjectV2ItemById returns the
// (possibly pre-existing) item id.
func TestGitHubAddProjectV2ItemIdempotent(t *testing.T) {
	var gotVars map[string]any
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			gotVars = readGQL(t, r).Variables
			// GitHub returns the existing item id when the content is already added.
			writeGQLData(w, map[string]any{
				"addProjectV2ItemById": map[string]any{
					"item": map[string]any{"id": "PVTI_existing"},
				},
			})
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	itemID, err := d.AddProjectV2Item(context.Background(), "PVT_123", "I_kwDO_issue")
	if err != nil {
		t.Fatalf("AddProjectV2Item: %v", err)
	}
	if itemID != "PVTI_existing" {
		t.Fatalf("expected existing item id, got %q", itemID)
	}
	if gotVars["projectId"] != "PVT_123" || gotVars["contentId"] != "I_kwDO_issue" {
		t.Errorf("mutation vars wrong: %+v", gotVars)
	}
}

// TestGitHubSetAndClearItemStatus pins that setting a value uses
// updateProjectV2ItemFieldValue and clearing (empty option) uses
// clearProjectV2ItemFieldValue.
func TestGitHubSetAndClearItemStatus(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		var gotQuery string
		var gotVars map[string]any
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
				req := readGQL(t, r)
				gotQuery, gotVars = req.Query, req.Variables
				writeGQLData(w, map[string]any{
					"updateProjectV2ItemFieldValue": map[string]any{
						"projectV2Item": map[string]any{"id": "PVTI_1"},
					},
				})
			},
		})
		d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
		if err := d.SetProjectV2ItemStatus(context.Background(), "PVT_123", "PVTI_1", "PVTSSF_1", "opt_prog"); err != nil {
			t.Fatalf("SetProjectV2ItemStatus: %v", err)
		}
		if !strings.Contains(gotQuery, "updateProjectV2ItemFieldValue") {
			t.Fatalf("set must use updateProjectV2ItemFieldValue, got: %s", gotQuery)
		}
		if gotVars["optionId"] != "opt_prog" {
			t.Errorf("optionId not sent: %+v", gotVars)
		}
	})

	t.Run("clear", func(t *testing.T) {
		var gotQuery string
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
				gotQuery = readGQL(t, r).Query
				writeGQLData(w, map[string]any{
					"clearProjectV2ItemFieldValue": map[string]any{
						"projectV2Item": map[string]any{"id": "PVTI_1"},
					},
				})
			},
		})
		d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
		if err := d.SetProjectV2ItemStatus(context.Background(), "PVT_123", "PVTI_1", "PVTSSF_1", ""); err != nil {
			t.Fatalf("SetProjectV2ItemStatus clear: %v", err)
		}
		if !strings.Contains(gotQuery, "clearProjectV2ItemFieldValue") {
			t.Fatalf("empty option must use clearProjectV2ItemFieldValue, got: %s", gotQuery)
		}
	})
}

// TestGitHubReadProjectV2ItemStatusesPaginates pins the ≥2-page cursor walk and
// per-item {itemId, issueNumber, optionId} extraction — matching the option value
// to the requested field id and leaving items with no value at "".
func TestGitHubReadProjectV2ItemStatusesPaginates(t *testing.T) {
	var cursors []any
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			req := readGQL(t, r)
			cursors = append(cursors, req.Variables["cursor"])
			// First call (cursor null) → page 1 with hasNextPage; second → page 2 end.
			if req.Variables["cursor"] == nil {
				writeGQLData(w, map[string]any{
					"node": map[string]any{
						"items": map[string]any{
							"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "CUR1"},
							"nodes": []map[string]any{
								{
									"id":      "PVTI_1",
									"content": map[string]any{"number": 101},
									"fieldValues": map[string]any{"nodes": []map[string]any{
										{"optionId": "opt_prog", "field": map[string]any{"id": "PVTSSF_1"}},
										{"optionId": "IGNORED", "field": map[string]any{"id": "OTHER_FIELD"}},
									}},
								},
							},
						},
					},
				})
				return
			}
			writeGQLData(w, map[string]any{
				"node": map[string]any{
					"items": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": "CUR2"},
						"nodes": []map[string]any{
							{
								"id":          "PVTI_2",
								"content":     map[string]any{"number": 102},
								"fieldValues": map[string]any{"nodes": []map[string]any{}}, // no value set
							},
						},
					},
				},
			})
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	statuses, err := d.ReadProjectV2ItemStatuses(context.Background(), "PVT_123", "PVTSSF_1")
	if err != nil {
		t.Fatalf("ReadProjectV2ItemStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 items across 2 pages, got %d: %+v", len(statuses), statuses)
	}
	if len(cursors) != 2 || cursors[0] != nil || cursors[1] != "CUR1" {
		t.Fatalf("cursor walk wrong: %+v", cursors)
	}
	if statuses[0].ItemID != "PVTI_1" || statuses[0].IssueNumber != 101 || statuses[0].OptionID != "opt_prog" {
		t.Fatalf("item 1 wrong (field match failed?): %+v", statuses[0])
	}
	if statuses[1].ItemID != "PVTI_2" || statuses[1].IssueNumber != 102 || statuses[1].OptionID != "" {
		t.Fatalf("item 2 wrong (no-value must be empty): %+v", statuses[1])
	}
}

// TestGitHubReadProjectV2ItemStatus pins the single-item read the forward-move live
// no-op guard (D7) uses: it returns the option id for the value matching the requested
// field, and "" when the item has no value or only values on OTHER fields.
func TestGitHubReadProjectV2ItemStatus(t *testing.T) {
	t.Run("matching field", func(t *testing.T) {
		var gotVars map[string]any
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
				gotVars = readGQL(t, r).Variables
				writeGQLData(w, map[string]any{
					"node": map[string]any{
						"fieldValues": map[string]any{"nodes": []map[string]any{
							{"optionId": "opt_prog", "field": map[string]any{"id": "PVTSSF_1"}},
							{"optionId": "IGNORED", "field": map[string]any{"id": "OTHER_FIELD"}},
						}},
					},
				})
			},
		})
		d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

		opt, err := d.ReadProjectV2ItemStatus(context.Background(), "PVTI_1", "PVTSSF_1")
		if err != nil {
			t.Fatalf("ReadProjectV2ItemStatus: %v", err)
		}
		if opt != "opt_prog" {
			t.Fatalf("want opt_prog for the matching field, got %q", opt)
		}
		if gotVars["itemId"] != "PVTI_1" {
			t.Errorf("itemId var not sent: %+v", gotVars)
		}
	})

	t.Run("no value set", func(t *testing.T) {
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, _ *http.Request) {
				writeGQLData(w, map[string]any{
					"node": map[string]any{"fieldValues": map[string]any{"nodes": []map[string]any{}}},
				})
			},
		})
		d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

		opt, err := d.ReadProjectV2ItemStatus(context.Background(), "PVTI_2", "PVTSSF_1")
		if err != nil {
			t.Fatalf("ReadProjectV2ItemStatus: %v", err)
		}
		if opt != "" {
			t.Fatalf("an item with no value must read \"\", got %q", opt)
		}
	})

	t.Run("only other fields", func(t *testing.T) {
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, _ *http.Request) {
				writeGQLData(w, map[string]any{
					"node": map[string]any{
						"fieldValues": map[string]any{"nodes": []map[string]any{
							{"optionId": "opt_other", "field": map[string]any{"id": "OTHER_FIELD"}},
						}},
					},
				})
			},
		})
		d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

		opt, err := d.ReadProjectV2ItemStatus(context.Background(), "PVTI_3", "PVTSSF_1")
		if err != nil {
			t.Fatalf("ReadProjectV2ItemStatus: %v", err)
		}
		if opt != "" {
			t.Fatalf("a value only on a non-matching field must read \"\", got %q", opt)
		}
	})
}

// TestGitHubCreateProjectV2Field pins one provisioning mutation: SINGLE_SELECT
// field create returns the field id + created options, and each option carries a
// color (defaulted when absent).
func TestGitHubCreateProjectV2Field(t *testing.T) {
	var gotOptions []any
	var gotQuery string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			req := readGQL(t, r)
			gotQuery = req.Query
			if opts, ok := req.Variables["options"].([]any); ok {
				gotOptions = opts
			}
			writeGQLData(w, map[string]any{
				"createProjectV2Field": map[string]any{
					"projectV2Field": map[string]any{
						"id":   "PVTSSF_new",
						"name": "uzi Status",
						"options": []map[string]any{
							{"id": "o1", "name": "Planned"},
							{"id": "o2", "name": "In Progress"},
						},
					},
				},
			})
		},
	})
	d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")

	f, err := d.CreateProjectV2Field(context.Background(), "PVT_123", "uzi Status", []ProjectV2NewOption{
		{Name: "Planned", Color: "BLUE", Description: ""},
		{Name: "In Progress"}, // color defaulted
	})
	if err != nil {
		t.Fatalf("CreateProjectV2Field: %v", err)
	}
	if f.ID != "PVTSSF_new" || len(f.Options) != 2 {
		t.Fatalf("unexpected field: %+v", f)
	}
	if !strings.Contains(gotQuery, "SINGLE_SELECT") {
		t.Errorf("create must request dataType SINGLE_SELECT: %s", gotQuery)
	}
	if len(gotOptions) != 2 {
		t.Fatalf("expected 2 option inputs, got %d: %+v", len(gotOptions), gotOptions)
	}
	// The color-defaulted option must carry a non-empty color enum on the wire.
	second, _ := gotOptions[1].(map[string]any)
	if second["color"] == "" || second["color"] == nil {
		t.Errorf("defaulted color must be sent, got: %+v", second)
	}
}

// TestGitHubGraphQLErrorRedacted pins that a GraphQL errors array (even on HTTP
// 200) surfaces as a Go error, and that a reflected PAT is scrubbed.
func TestGitHubGraphQLErrorRedacted(t *testing.T) {
	const token = "ghp_secretTokenValue1234567890abcd"
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, _ *http.Request) {
			// A hostile/broken forge reflecting the token into a GraphQL error message.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":   nil,
				"errors": []map[string]any{{"message": "Could not resolve to a node with token " + token}},
			})
		},
	})
	d := newGitHubRawDriver(t, m, token)

	_, err := d.ResolveProjectV2(context.Background(), "acme", 7, OwnerUser)
	if err == nil {
		t.Fatal("a non-empty GraphQL errors array must yield an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in graphql error: %q", err.Error())
	}
	if !strings.Contains(err.Error(), redactPlaceholder) {
		t.Errorf("expected redaction placeholder in %q", err.Error())
	}
}

// TestForgeIsolationCapability pins SC-4: the github driver implements
// ProjectBoardSyncer, while the gitlab and forgejo drivers do NOT — a type
// assertion on them fails, which is exactly how the sync is skipped for
// non-GitHub forges. Compile-time proof lives in projectsync.go
// (var _ ProjectBoardSyncer = (*github)(nil)); this asserts the negative half at
// runtime against the concrete drivers.
func TestForgeIsolationCapability(t *testing.T) {
	var ghd Forge = &github{}
	if _, ok := ghd.(ProjectBoardSyncer); !ok {
		t.Fatal("github driver must implement ProjectBoardSyncer")
	}
	var gld Forge = &gitLab{}
	if _, ok := gld.(ProjectBoardSyncer); ok {
		t.Fatal("gitlab driver must NOT implement ProjectBoardSyncer (SC-4 forge isolation)")
	}
	var fjd Forge = &forgejo{}
	if _, ok := fjd.(ProjectBoardSyncer); ok {
		t.Fatal("forgejo driver must NOT implement ProjectBoardSyncer (SC-4 forge isolation)")
	}
}

// TestGitHubGetSetProjectV2Visibility pins PRD #557 M1: reading a board's public
// flag (node(id){ ... on ProjectV2 { public } }) and writing it through
// updateProjectV2 with the projectId + public variables.
func TestGitHubGetSetProjectV2Visibility(t *testing.T) {
	// Read leg.
	var readVars map[string]any
	mr := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			readVars = readGQL(t, r).Variables
			writeGQLData(w, map[string]any{"node": map[string]any{"public": true}})
		},
	})
	dr := newGitHubRawDriver(t, mr, "ghp_classicTokenValue1234567890")
	public, err := dr.GetProjectV2Visibility(context.Background(), "PVT_123")
	if err != nil {
		t.Fatalf("GetProjectV2Visibility: %v", err)
	}
	if !public {
		t.Fatalf("expected public=true, got %v", public)
	}
	if readVars["projectId"] != "PVT_123" {
		t.Errorf("projectId var not sent on read: %+v", readVars)
	}

	// Write leg.
	var setQuery string
	var setVars map[string]any
	mw := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			req := readGQL(t, r)
			setQuery, setVars = req.Query, req.Variables
			writeGQLData(w, map[string]any{"updateProjectV2": map[string]any{"projectV2": map[string]any{"id": "PVT_123"}}})
		},
	})
	dw := newGitHubRawDriver(t, mw, "ghp_classicTokenValue1234567890")
	if err := dw.SetProjectV2Visibility(context.Background(), "PVT_123", false); err != nil {
		t.Fatalf("SetProjectV2Visibility: %v", err)
	}
	if !strings.Contains(setQuery, "updateProjectV2") {
		t.Errorf("set visibility query must name updateProjectV2: %q", setQuery)
	}
	if setVars["projectId"] != "PVT_123" {
		t.Errorf("projectId var not sent on set: %+v", setVars)
	}
	if setVars["public"] != false {
		t.Errorf("public var not sent on set: %+v", setVars)
	}
}

// TestGitHubSetProjectV2Collaborator pins PRD #557 M1: the collaborators variable
// carries a single element with userId + role, for both a READER grant and a NONE
// revoke.
func TestGitHubSetProjectV2Collaborator(t *testing.T) {
	cases := []struct {
		name string
		role ProjectV2CollaboratorRole
		want string
	}{
		{"grant reader", RoleReaderCollaborator, "READER"},
		{"revoke", RoleNoneCollaborator, "NONE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			var gotVars map[string]any
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
					req := readGQL(t, r)
					gotQuery, gotVars = req.Query, req.Variables
					writeGQLData(w, map[string]any{"updateProjectV2Collaborators": map[string]any{"clientMutationId": nil}})
				},
			})
			d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
			if err := d.SetProjectV2Collaborator(context.Background(), "PVT_123", "U_42", tc.role); err != nil {
				t.Fatalf("SetProjectV2Collaborator: %v", err)
			}
			if !strings.Contains(gotQuery, "updateProjectV2Collaborators") {
				t.Errorf("query must name updateProjectV2Collaborators: %q", gotQuery)
			}
			if gotVars["projectId"] != "PVT_123" {
				t.Errorf("projectId var not sent: %+v", gotVars)
			}
			list, ok := gotVars["collaborators"].([]any)
			if !ok || len(list) != 1 {
				t.Fatalf("collaborators must be a 1-element array: %+v", gotVars["collaborators"])
			}
			elem, ok := list[0].(map[string]any)
			if !ok {
				t.Fatalf("collaborator element must be an object: %+v", list[0])
			}
			if elem["userId"] != "U_42" {
				t.Errorf("collaborator userId wrong: %+v", elem)
			}
			if elem["role"] != tc.want {
				t.Errorf("collaborator role = %v, want %q", elem["role"], tc.want)
			}
		})
	}
}

// TestGitHubResolveUserNodeID pins PRD #557 M1: a login resolves to its node id;
// a GitHub NOT_FOUND envelope surfaces as ErrGitHubUserNotFound; and a generic
// (non-NOT_FOUND) error does NOT — so a bad username maps to 422 while a
// permission/transient error stays a 500. Redaction still holds on the NOT_FOUND
// path.
func TestGitHubResolveUserNodeID(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotVars map[string]any
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
				gotVars = readGQL(t, r).Variables
				writeGQLData(w, map[string]any{"user": map[string]any{"id": "U_99"}})
			},
		})
		d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
		id, err := d.ResolveUserNodeID(context.Background(), "octocat")
		if err != nil {
			t.Fatalf("ResolveUserNodeID: %v", err)
		}
		if id != "U_99" {
			t.Fatalf("unexpected id: %q", id)
		}
		if gotVars["login"] != "octocat" {
			t.Errorf("login var not sent: %+v", gotVars)
		}
	})

	t.Run("not found is typed", func(t *testing.T) {
		const token = "ghp_secretTokenValue1234567890abcd"
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"user": nil},
					"errors": []map[string]any{{
						"type": "NOT_FOUND",
						// Reflect the token to prove redaction survives the wrap.
						"message": "Could not resolve to a User with the login of 'nouser' " + token,
					}},
				})
			},
		})
		d := newGitHubRawDriver(t, m, token)
		_, err := d.ResolveUserNodeID(context.Background(), "nouser")
		if err == nil {
			t.Fatal("a NOT_FOUND envelope must yield an error")
		}
		if !errors.Is(err, ErrGitHubUserNotFound) {
			t.Fatalf("NOT_FOUND must be errors.Is ErrGitHubUserNotFound, got: %v", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("token leaked in not-found error: %q", err.Error())
		}
	})

	t.Run("generic error is not typed", func(t *testing.T) {
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			graphqlRoute: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data":   nil,
					"errors": []map[string]any{{"type": "FORBIDDEN", "message": "insufficient scope"}},
				})
			},
		})
		d := newGitHubRawDriver(t, m, "ghp_classicTokenValue1234567890")
		_, err := d.ResolveUserNodeID(context.Background(), "octocat")
		if err == nil {
			t.Fatal("a FORBIDDEN envelope must yield an error")
		}
		if errors.Is(err, ErrGitHubUserNotFound) {
			t.Fatalf("a non-NOT_FOUND error must NOT be ErrGitHubUserNotFound: %v", err)
		}
	})
}
