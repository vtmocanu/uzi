package issuetracker

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestLabelEventsOrdinalAndExpansion(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("t", "")
	a, b, c, d := srv.SeedOwnTag("a"), srv.SeedOwnTag("b"), srv.SeedOwnTag("c"), srv.SeedOwnTag("d")
	srv.SeedTagActivity(n, "alice", []string{a, b}, []string{c})
	srv.SeedTagActivity(n, "bob", []string{d}, nil)
	got, err := y.ListIssueLabelEvents(context.Background(), 1, n)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id           int64
		action, name string
		user         string
	}{{1, "add", "a", "alice"}, {2, "add", "b", "alice"}, {3, "remove", "c", "alice"}, {4, "add", "d", "bob"}}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		e := got[i]
		if e.ID != w.id || e.Action != w.action || e.LabelName != w.name || e.Username != w.user {
			t.Errorf("event %d = %+v, want %+v", i, e, w)
		}
	}
}

// activityHandler serves issue UZI-1 and the given activities body.
func activityHandler(activities string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/activities") {
			writeRaw(w, activities)
			return
		}
		writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
	}
}

func lowerMaxItems(t *testing.T, n int) {
	t.Helper()
	old := maxItems
	maxItems = n
	t.Cleanup(func() { maxItems = old })
}

func TestLabelEventsNestedTagBomb(t *testing.T) {
	// 200k empty tags in one activity fit under listPageBytes.
	body := `[{"id":"3-1","timestamp":1,"added":[` + strings.Repeat("{},", 199_999) + `{}]}]`
	if int64(len(body)) >= listPageBytes {
		t.Fatalf("fixture is %d bytes, not under the page cap", len(body))
	}
	y, _ := loggedDriver(t, activityHandler(body))
	got, err := y.ListIssueLabelEvents(context.Background(), 1, 1)
	if err == nil || got != nil {
		t.Fatalf("got %d events, %v; want an error", len(got), err)
	}
}

func TestNestedTagsCountTowardTheItemCeiling(t *testing.T) {
	lowerMaxItems(t, 5)
	// Two activities, three tags each: two items, eight counting the tags.
	act := `{"id":"3-1","timestamp":1,"added":[{"id":"5-1","name":"a"},{"id":"5-2","name":"b"}],"removed":[{"id":"5-3","name":"c"}]}`
	y, _ := loggedDriver(t, activityHandler("["+act+","+act+"]"))
	if _, err := y.ListIssueLabelEvents(context.Background(), 1, 1); err == nil {
		t.Fatal("nested tags did not count toward the item ceiling")
	}
}
