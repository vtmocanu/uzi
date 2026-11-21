package issuetracker

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestStatesAndProjects(t *testing.T) {
	y, _ := newFakeDriver(t)
	ctx := context.Background()
	acct, err := y.VerifyToken(ctx)
	if err != nil || acct.Login != youtracktestBot {
		t.Fatalf("VerifyToken = %+v, %v", acct, err)
	}
	projects, err := y.ListProjects(ctx)
	if err != nil || len(projects) != 2 {
		t.Fatalf("ListProjects = %+v, %v", projects, err)
	}
	states, err := y.ListStates(ctx, "0-1")
	if err != nil || len(states) != 3 || states[0].Resolved || !states[1].Resolved || states[2].Name != "Won't fix" {
		t.Fatalf("ListStates = %+v, %v", states, err)
	}
	if _, err := y.ListStates(ctx, "../x"); err == nil {
		t.Fatal("ListStates accepted a malformed project id")
	}
}

// statesDriver serves the State field of project 0-1 with the given bundle
// values, a raw JSON array.
func statesDriver(t *testing.T, values string) *youTrack {
	t.Helper()
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bundle/values") {
			writeRaw(w, values)
			return
		}
		writeRaw(w, `[{"id":"60-1","$type":"StateProjectCustomField","field":{"name":"State"},"bundle":{"id":"40-1"}}]`)
	})
	return y
}

func TestListStatesDropsBadNamesAndRefusesBadIDs(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("a", maxLabelNameLen+1)
	y := statesDriver(t, `[
		{"id":"10-1","name":"Open","ordinal":0},
		{"id":"10-2","name":"`+long+`","ordinal":1},
		{"id":"10-3","name":"Zero\u200bwidth","ordinal":2},
		{"id":"10-4","name":"  ","ordinal":3},
		{"id":"10-5","name":"Fixed","localizedName":"Er\u202eledigt","isResolved":true,"archived":true,"ordinal":4},
		{"id":"10-6","name":"Done","localizedName":"Erledigt","isResolved":true,"ordinal":5}]`)
	got, err := y.ListStates(ctx, "0-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []State{
		{ID: "10-1", Name: "Open"},
		{ID: "10-5", Name: "Fixed", Resolved: true, Archived: true, Ordinal: 4},
		{ID: "10-6", Name: "Done", LocalizedName: "Erledigt", Resolved: true, Ordinal: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("ListStates = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("state %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	y = statesDriver(t, `[{"id":"10-1","name":"Open"},{"id":"../x","name":"Fixed"}]`)
	if got, err := y.ListStates(ctx, "0-1"); err == nil {
		t.Errorf("ListStates accepted a malformed state id: %+v", got)
	}
}
