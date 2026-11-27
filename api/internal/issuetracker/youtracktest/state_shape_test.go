package youtracktest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

type shapeIssue struct {
	IDReadable   string           `json:"idReadable"`
	CustomFields []map[string]any `json:"customFields"`
}

func decodeShape(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

func TestStateFieldShapes(t *testing.T) {
	const write = `{"customFields":[{"name":"State","$type":"StateIssueCustomField","value":{"id":"%s"}}]}`
	for _, tc := range []struct {
		name      string
		shape     StateFieldShape
		wantEntry bool
		wantType  bool
		wantWrite int
	}{
		{"plain", StateFieldPlain, true, true, http.StatusOK},
		{"absent", StateFieldAbsent, false, false, http.StatusBadRequest},
		{"untyped", StateFieldUntyped, true, false, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(t)
			n := srv.SeedIssue("s", "d")
			held := srv.IssueState(n)
			srv.SeedStateFieldShape(n, tc.shape)
			id := "UZI-" + strconv.FormatInt(n, 10)

			check := func(label string, it shapeIssue) {
				t.Helper()
				var entry map[string]any
				for _, cf := range it.CustomFields {
					if cf["name"] == "State" {
						entry = cf
					}
				}
				if (entry != nil) != tc.wantEntry {
					t.Fatalf("%s: State entry present = %v, want %v: %v", label, entry != nil, tc.wantEntry, it.CustomFields)
				}
				if entry != nil {
					if _, has := entry["$type"]; has != tc.wantType {
						t.Errorf("%s: State entry has $type = %v, want %v", label, has, tc.wantType)
					}
				}
			}

			status, body := call(t, srv, http.MethodGet, "/api/issues/"+id+"?fields=id,customFields(name,$type,value(name))", "")
			if status != http.StatusOK {
				t.Fatalf("get: status %d: %s", status, body)
			}
			var one shapeIssue
			decodeShape(t, body, &one)
			check("get", one)

			q := url.Values{"query": {"project: {UZI}"}, "fields": {"id,idReadable,customFields(name,$type,value(name))"}, "customFields": {"State"}}
			status, body = call(t, srv, http.MethodGet, "/api/issues?"+q.Encode(), "")
			if status != http.StatusOK {
				t.Fatalf("list: status %d: %s", status, body)
			}
			var list []shapeIssue
			decodeShape(t, body, &list)
			if len(list) != 1 {
				t.Fatalf("list: %d issues, want 1: %s", len(list), body)
			}
			check("list", list[0])

			var stateID string
			srv.mu.Lock()
			for _, st := range srv.m.states {
				if !st.resolved && !st.archived {
					stateID = st.id
					break
				}
			}
			srv.mu.Unlock()
			status, body = call(t, srv, http.MethodPost, "/api/issues/"+id+"?fields=id", fmt.Sprintf(write, stateID))
			if status != tc.wantWrite {
				t.Errorf("write: status %d, want %d: %s", status, tc.wantWrite, body)
			}
			if tc.wantWrite != http.StatusOK {
				// The refused write changed nothing, and IssueState reads the
				// model, not the wire shape.
				if got := srv.IssueState(n); got != held || got == "" {
					t.Errorf("IssueState = %q, want %q", got, held)
				}
			}
		})
	}
}

func TestSeedIDReadableOverridesOnlyTheWireValue(t *testing.T) {
	for _, override := range []string{"", "UZI-LONG-1234567890123456789012345678901234567890"} {
		srv := NewServer(t)
		n := srv.SeedIssue("s", "d")
		srv.SeedIDReadable(n, override)
		status, body := call(t, srv, http.MethodGet, "/api/issues/UZI-"+strconv.FormatInt(n, 10)+"?fields=idReadable", "")
		if status != http.StatusOK {
			t.Fatalf("get by the real id-readable: status %d: %s", status, body)
		}
		var it shapeIssue
		decodeShape(t, body, &it)
		if it.IDReadable != override {
			t.Errorf("idReadable = %q, want %q", it.IDReadable, override)
		}
	}
}

// An issue's shape overrides the project's state-machine shape, and the absent
// shape keeps any other custom field.
func TestStateFieldShapeOverridesTheProject(t *testing.T) {
	stateEntry := func(t *testing.T, srv *Server, n int64) (map[string]any, []map[string]any) {
		t.Helper()
		id := "UZI-" + strconv.FormatInt(n, 10)
		status, body := call(t, srv, http.MethodGet, "/api/issues/"+id+"?fields=id,customFields(name,$type,possibleEvents(id),value(name))", "")
		if status != http.StatusOK {
			t.Fatalf("get: status %d: %s", status, body)
		}
		var one shapeIssue
		decodeShape(t, body, &one)
		for _, cf := range one.CustomFields {
			if cf["name"] == "State" {
				return cf, one.CustomFields
			}
		}
		return nil, one.CustomFields
	}
	t.Run("untyped on a state-machine project", func(t *testing.T) {
		srv := NewServer(t)
		n := srv.SeedIssue("s", "d")
		srv.SetStateMachine(ProjectID, true)
		srv.SeedStateFieldShape(n, StateFieldUntyped)
		e, all := stateEntry(t, srv, n)
		if e == nil {
			t.Fatalf("no State entry: %v", all)
		}
		if _, has := e["$type"]; has {
			t.Errorf("State entry has $type: %v", e)
		}
		if _, has := e["possibleEvents"]; has {
			t.Errorf("State entry has possibleEvents: %v", e)
		}
	})
	t.Run("absent keeps another field", func(t *testing.T) {
		srv := NewServer(t)
		n := srv.SeedIssue("s", "d")
		srv.SetExtraStateMachineField(ProjectID, "Workflow")
		srv.SeedStateFieldShape(n, StateFieldAbsent)
		e, all := stateEntry(t, srv, n)
		if e != nil {
			t.Fatalf("State entry present: %v", e)
		}
		if len(all) != 1 || all[0]["name"] != "Workflow" {
			t.Errorf("custom fields = %v, want only Workflow", all)
		}
	})
}
