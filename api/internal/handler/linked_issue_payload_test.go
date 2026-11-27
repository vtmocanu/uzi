package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func linkedRepoRow(conn uuid.UUID) store.GetRepoForUserRow {
	return store.GetRepoForUserRow{
		IssueTrackerConnectionID: pgtype.UUID{Bytes: conn, Valid: true},
		IssueTrackerProjectShort: pgtype.Text{String: "ABC", Valid: true},
		EligibilityTagName:       pgtype.Text{String: "send-to-uzi", Valid: true},
	}
}

func TestRepoToDTOIssueTrackerLink(t *testing.T) {
	conn := uuid.New()
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	t.Run("linked repo carries the link block and is not linkable", func(t *testing.T) {
		dto := repoToDTO(store.Repo{
			Enabled:                  true,
			AwaitingFirstEnable:      true,
			IssueTrackerConnectionID: pgtype.UUID{Bytes: conn, Valid: true},
			IssueTrackerProjectID:    pgtype.Text{String: "0-7", Valid: true},
			IssueTrackerProjectShort: pgtype.Text{String: "ABC", Valid: true},
			EligibilityTagID:         pgtype.Text{String: "5-1", Valid: true},
			EligibilityTagName:       pgtype.Text{String: "send-to-uzi", Valid: true},
			IssueTrackerLinkedAt:     pgtype.Timestamptz{Time: at, Valid: true},
		})
		l := dto.IssueTrackerLink
		if l == nil || l.ConnectionID != conn.String() || l.ProjectID != "0-7" || l.ProjectShortName != "ABC" ||
			l.TagID != "5-1" || l.TagName != "send-to-uzi" || !l.LinkedAt.Equal(at) {
			t.Fatalf("link = %+v", l)
		}
		if dto.IssueTrackerLinkable {
			t.Error("a linked repo must not be linkable")
		}
	})
	t.Run("repoToDTO leaves the linkable flag to the handlers", func(t *testing.T) {
		dto := repoToDTO(store.Repo{AwaitingFirstEnable: true})
		if dto.IssueTrackerLink != nil || dto.IssueTrackerLinkable {
			t.Fatalf("link = %+v linkable = %v", dto.IssueTrackerLink, dto.IssueTrackerLinkable)
		}
	})
	t.Run("an enabled unlinked repo has no link and is not linkable", func(t *testing.T) {
		dto := repoToDTO(store.Repo{Enabled: true})
		if dto.IssueTrackerLink != nil || dto.IssueTrackerLinkable {
			t.Fatalf("link = %+v linkable = %v", dto.IssueTrackerLink, dto.IssueTrackerLinkable)
		}
		b, err := json.Marshal(dto)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["issue_tracker_link"]; ok {
			t.Errorf("issue_tracker_link = %s, want the key omitted", m["issue_tracker_link"])
		}
	})
}

func TestIssueTrackerConfigRoute(t *testing.T) {
	get := func(t *testing.T, allowed []string) map[string]json.RawMessage {
		t.Helper()
		h := &Handler{cfg: config.Config{IssueTrackerAllowedBaseURLs: allowed}}
		rr := httptest.NewRecorder()
		h.IssueTrackerConfig(rr, httptest.NewRequest(http.MethodGet, "/api/issue-trackers/config", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	t.Run("an empty allowlist is an empty array, never null", func(t *testing.T) {
		for _, in := range [][]string{nil, {}} {
			if got := string(get(t, in)["allowed_base_urls"]); got != "[]" {
				t.Errorf("allowed_base_urls = %s for %#v, want []", got, in)
			}
		}
	})
	t.Run("a configured list and the kinds are returned", func(t *testing.T) {
		m := get(t, []string{"https://tracker.example.test"})
		var urls, kinds []string
		if err := json.Unmarshal(m["allowed_base_urls"], &urls); err != nil || !slices.Equal(urls, []string{"https://tracker.example.test"}) {
			t.Errorf("allowed_base_urls = %s (%v)", m["allowed_base_urls"], err)
		}
		if err := json.Unmarshal(m["kinds"], &kinds); err != nil || !slices.Contains(kinds, "youtrack") {
			t.Errorf("kinds = %s (%v)", m["kinds"], err)
		}
	})
}

func TestLinkedIssueKeyAndDetail(t *testing.T) {
	linked := linkedRepoRow(uuid.New())
	unlinked := store.GetRepoForUserRow{IssueTrackerProjectShort: pgtype.Text{String: "ABC", Valid: true}}
	if got := linkedIssueKey(linked, 12); got != "ABC-12" {
		t.Errorf("linked key = %q", got)
	}
	if got := linkedIssueKey(unlinked, 12); got != "" {
		t.Errorf("unlinked key = %q, want empty", got)
	}
	if got := linkedIssueKey(store.GetRepoForUserRow{IssueTrackerConnectionID: linked.IssueTrackerConnectionID}, 12); got != "" {
		t.Errorf("no short name key = %q, want empty", got)
	}

	position := map[string]int{"In Progress": 0, "Human Review": 1}
	issue := forge.Issue{IID: 12, State: "opened", Labels: []string{"send-to-uzi", "In Progress"}}

	t.Run("linked: key, eligibility and the cached column; a label never decides it", func(t *testing.T) {
		dto := buildIssueDetail(issue, position, "gitlab", 0)
		applyLinkedIssueDetail(&dto, linked, issue, "Human Review", position)
		if dto.IssueKey != "ABC-12" || dto.EligibilityLabel != "send-to-uzi" || dto.Column != "Human Review" {
			t.Errorf("detail = %+v", dto)
		}
	})
	t.Run("linked and uncached: the Open lane even with a column-named label", func(t *testing.T) {
		dto := buildIssueDetail(issue, position, "gitlab", 0)
		applyLinkedIssueDetail(&dto, linked, issue, "", position)
		if dto.Column != "" || dto.Closed {
			t.Errorf("column = %q closed = %v, want the Open lane", dto.Column, dto.Closed)
		}
	})
	t.Run("unlinked: neither field and the label-resolved column", func(t *testing.T) {
		dto := buildIssueDetail(issue, position, "gitlab", 0)
		applyLinkedIssueDetail(&dto, unlinked, issue, "Human Review", position)
		b, _ := json.Marshal(dto)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if _, ok := m["issue_key"]; ok {
			t.Error("issue_key must be omitted on an unlinked repo")
		}
		if _, ok := m["eligibility_label"]; ok {
			t.Error("eligibility_label must be omitted on an unlinked repo")
		}
		if dto.Column != "In Progress" {
			t.Errorf("column = %q, want In Progress from the label", dto.Column)
		}
	})
}

func TestStampLinkedIssueKeys(t *testing.T) {
	cards := func() []cardDTO { return []cardDTO{{IID: 3}, {IID: 4, IssueKey: "OLD-4"}} }
	l := cards()
	stampLinkedIssueKeys(l, linkedRepoRow(uuid.New()))
	if l[0].IssueKey != "ABC-3" || l[1].IssueKey != "ABC-4" {
		t.Errorf("linked keys = %q, %q", l[0].IssueKey, l[1].IssueKey)
	}
	u := cards()
	stampLinkedIssueKeys(u, store.GetRepoForUserRow{})
	if u[0].IssueKey != "" || u[1].IssueKey != "OLD-4" {
		t.Errorf("unlinked keys changed: %q, %q", u[0].IssueKey, u[1].IssueKey)
	}
	if got := linkedEligibilityLabel(store.GetRepoForUserRow{EligibilityTagName: pgtype.Text{String: "x", Valid: true}}); got != "" {
		t.Errorf("unlinked eligibility label = %q, want empty", got)
	}
}
