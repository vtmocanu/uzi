package issuetracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// columnDriver is newFakeDriver with the board columns mapped to State values
// the fake's bundle has once "In Progress" is seeded.
func columnDriver(t *testing.T, mutate func(*Options)) (*youTrack, *youtracktest.Server) {
	t.Helper()
	srv := youtracktest.NewServer(t)
	srv.SeedState("In Progress", false)
	opts := fakeOptions(srv)
	opts.ColumnStates = map[string]string{"In Progress": "In Progress", "Done": "Fixed", "Lost": "Won't fix"}
	if mutate != nil {
		mutate(&opts)
	}
	y, err := newYouTrackOver(opts, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return y, srv
}

// writesOf returns the fake's logged requests that are not GETs.
func writesOf(srv *youtracktest.Server) []string {
	var out []string
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r.Method+" "+r.Path)
		}
	}
	return out
}

func TestSetIssueStateRefusesAClosedStateThatIsNotDone(t *testing.T) {
	y, srv := columnDriver(t, func(o *Options) { o.ClosedState = "Won't fix" })
	n := srv.SeedIssue("t", "")
	srv.ResetRequests()
	err := y.SetIssueState(context.Background(), testProjectID, n, forge.StateClosed)
	if !errors.Is(err, ErrStateNotAllowed) {
		t.Fatalf("SetIssueState = %v, want ErrStateNotAllowed", err)
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent: %v", w)
	}
	if got := srv.IssueState(n); got != "Open" {
		t.Errorf("issue state = %q, want it untouched", got)
	}
}

func TestSetIssueStateRefusesABundleEditedAfterMapping(t *testing.T) {
	for name, edit := range map[string]func(*youtracktest.Server){
		"Fixed renamed to Won't fix": func(s *youtracktest.Server) { s.RenameState("Fixed", "Won't fix") },
		"Fixed no longer resolved":   func(s *youtracktest.Server) { s.SetStateResolved("Fixed", false) },
		"Fixed archived":             func(s *youtracktest.Server) { s.SetStateArchived("Fixed", true) },
	} {
		t.Run(name, func(t *testing.T) {
			y, srv := newFakeDriver(t)
			n := srv.SeedIssue("t", "")
			ctx := context.Background()
			if err := y.SetIssueState(ctx, testProjectID, n, forge.StateClosed); err != nil {
				t.Fatalf("before the edit: %v", err)
			}
			if err := y.SetIssueState(ctx, testProjectID, n, forge.StateOpened); err != nil {
				t.Fatalf("reopen: %v", err)
			}
			edit(srv)
			srv.ResetRequests()
			err := y.SetIssueState(ctx, testProjectID, n, forge.StateClosed)
			if !errors.Is(err, ErrStateNotAllowed) {
				t.Fatalf("SetIssueState = %v, want ErrStateNotAllowed", err)
			}
			if w := writesOf(srv); len(w) != 0 {
				t.Errorf("writes sent: %v", w)
			}
		})
	}
}

func TestSetIssueStateRefusesAResolvedOpenState(t *testing.T) {
	y, srv := columnDriver(t, func(o *Options) { o.OpenState = "Fixed" })
	n := srv.SeedIssue("t", "")
	srv.ResetRequests()
	if err := y.SetIssueState(context.Background(), testProjectID, n, forge.StateOpened); !errors.Is(err, ErrStateNotAllowed) {
		t.Fatalf("SetIssueState(opened) = %v, want ErrStateNotAllowed", err)
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent: %v", w)
	}
}

func TestMoveToColumnUnmappedSendsNothing(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	srv.ResetRequests()
	err := y.MoveToColumn(context.Background(), testProjectID, n, forge.ColumnMove{Column: "Review"})
	if !errors.Is(err, ErrColumnNotMapped) {
		t.Fatalf("MoveToColumn = %v, want ErrColumnNotMapped", err)
	}
	if got := srv.Requests(); len(got) != 0 {
		t.Errorf("requests sent: %v", got)
	}
}

func TestMoveToColumnWritesTheMappedState(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	if err := y.MoveToColumn(context.Background(), testProjectID, n, forge.ColumnMove{Column: "In Progress"}); err != nil {
		t.Fatalf("MoveToColumn: %v", err)
	}
	if got := srv.IssueState(n); got != "In Progress" {
		t.Errorf("issue state = %q, want In Progress", got)
	}
}

func TestMoveToColumnEmptyUsesTheOpenState(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	ctx := context.Background()
	if err := y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: "In Progress"}); err != nil {
		t.Fatal(err)
	}
	if err := y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: ""}); err != nil {
		t.Fatalf("MoveToColumn(\"\"): %v", err)
	}
	if got := srv.IssueState(n); got != "Open" {
		t.Errorf("issue state = %q, want the open state", got)
	}
	y2, srv2 := columnDriver(t, func(o *Options) { o.OpenState = "" })
	n2 := srv2.SeedIssue("t", "")
	srv2.ResetRequests()
	if err := y2.MoveToColumn(ctx, testProjectID, n2, forge.ColumnMove{Column: ""}); !errors.Is(err, ErrStateMappingUnset) {
		t.Errorf("with no open state: %v, want ErrStateMappingUnset", err)
	}
	if got := srv2.Requests(); len(got) != 0 {
		t.Errorf("requests sent: %v", got)
	}
}

func TestMoveToColumnRefusesAResolvedColumnState(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	for _, col := range []string{"Done", "Lost"} {
		srv.ResetRequests()
		if err := y.MoveToColumn(context.Background(), testProjectID, n, forge.ColumnMove{Column: col}); !errors.Is(err, ErrStateNotAllowed) {
			t.Errorf("MoveToColumn(%q) = %v, want ErrStateNotAllowed", col, err)
		}
		if w := writesOf(srv); len(w) != 0 {
			t.Errorf("%q: writes sent: %v", col, w)
		}
	}
}

func TestMoveToColumnRefusesABundleEditedAfterMapping(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	srv.SetStateResolved("In Progress", true)
	srv.ResetRequests()
	if err := y.MoveToColumn(context.Background(), testProjectID, n, forge.ColumnMove{Column: "In Progress"}); !errors.Is(err, ErrStateNotAllowed) {
		t.Fatalf("MoveToColumn = %v, want ErrStateNotAllowed", err)
	}
	srv.SetStateResolved("In Progress", false)
	srv.RenameState("In Progress", "Doing")
	if err := y.MoveToColumn(context.Background(), testProjectID, n, forge.ColumnMove{Column: "In Progress"}); !errors.Is(err, ErrStateNotAllowed) {
		t.Fatalf("after a rename: %v, want ErrStateNotAllowed", err)
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent: %v", w)
	}
}

func TestMoveToColumnOnAMovedIssueIsNotFound(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	srv.MoveIssue(n, youtracktest.OtherProjectID)
	srv.ResetRequests()
	err := y.MoveToColumn(context.Background(), testProjectID, n, forge.ColumnMove{Column: "In Progress"})
	var nf *forge.IssueNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("MoveToColumn = %v, want *forge.IssueNotFoundError", err)
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent: %v", w)
	}
}

func TestMoveToColumnProjectMismatchSendsNothing(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	srv.ResetRequests()
	if err := y.MoveToColumn(context.Background(), testProjectID+1, n, forge.ColumnMove{Column: "In Progress"}); !errors.Is(err, ErrProjectMismatch) {
		t.Fatalf("MoveToColumn = %v, want ErrProjectMismatch", err)
	}
	if got := srv.Requests(); len(got) != 0 {
		t.Errorf("requests sent: %v", got)
	}
}

func TestNewSentinelsRedactTheToken(t *testing.T) {
	// Both configured names carry the fake's token, the one secret a bad config
	// could plausibly paste into a state name.
	y, srv := columnDriver(t, nil)
	y.closedState = srv.Token
	y.columnStates = map[string]string{"c": srv.Token}
	n := srv.SeedIssue("t", "")
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"closed":   {y.SetIssueState(ctx, testProjectID, n, forge.StateClosed), ErrStateNotAllowed},
		"column":   {y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: "c"}), ErrStateNotAllowed},
		"unmapped": {y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: "nope"}), ErrColumnNotMapped},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: %v, want %v", name, tc.err, tc.want)
			continue
		}
		if strings.Contains(tc.err.Error(), srv.Token) {
			t.Errorf("%s: the error carries the token: %q", name, tc.err)
		}
	}
}

func TestCreateIssueReadBackOutsideTheProjectIsNotFound(t *testing.T) {
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
			writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
		default:
			// The read-back answers an issue that sits in another project.
			writeRaw(w, rawIssue("2-1", "OTH-1", 1, "0-2", ""))
		}
	})
	_, err := y.CreateIssue(context.Background(), testProjectID, "t", "d", nil)
	var nf *forge.IssueNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("CreateIssue = %v, want *forge.IssueNotFoundError", err)
	}
}

func TestListIssuesWithStateReturnsTheStateNames(t *testing.T) {
	y, srv := columnDriver(t, nil)
	a := srv.SeedIssue("a", "")
	b := srv.SeedIssue("b", "")
	ctx := context.Background()
	if err := y.MoveToColumn(ctx, testProjectID, b, forge.ColumnMove{Column: "In Progress"}); err != nil {
		t.Fatal(err)
	}
	got, err := y.ListIssuesWithState(ctx, testProjectID, forge.ListIssuesOptions{State: forge.StateAll})
	if err != nil {
		t.Fatal(err)
	}
	states := map[int64]string{}
	for _, s := range got {
		states[s.Issue.IID] = s.State
	}
	if len(got) != 2 || states[a] != "Open" || states[b] != "In Progress" {
		t.Errorf("listed %v, want a Open and b In Progress", states)
	}
	// A State field under another name is not read.
	srv.SetStateField("Stage")
	got, err = y.ListIssuesWithState(ctx, testProjectID, forge.ListIssuesOptions{State: forge.StateAll})
	if err != nil || len(got) != 2 || got[0].State != "" {
		t.Errorf("with a renamed field: %+v, %v; want empty State names", got, err)
	}
}

func TestListIssuesWithStateDropsABadStateName(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("a", "")
	srv.RenameState("Open", "Op\u202een")
	got, err := y.ListIssuesWithState(context.Background(), testProjectID, forge.ListIssuesOptions{State: forge.StateAll})
	if err != nil || len(got) != 1 || got[0].Issue.IID != n || got[0].State != "" {
		t.Errorf("listed %+v, %v; want the issue with an empty State", got, err)
	}
}

func TestListIssuesAndListIssuesWithStateSendTheSameRequests(t *testing.T) {
	y, srv := columnDriver(t, nil)
	srv.SeedIssue("a", "", youtracktest.EligibilityTagID)
	srv.SeedIssue("b", "")
	ctx := context.Background()
	for name, opts := range map[string]forge.ListIssuesOptions{
		"project": {State: forge.StateAll},
		"tag":     {State: forge.StateAll, Labels: []string{youtracktest.EligibilityTagName}},
	} {
		srv.ResetRequests()
		plain, err := y.ListIssues(ctx, testProjectID, opts)
		if err != nil {
			t.Fatal(err)
		}
		first := srv.Requests()
		srv.ResetRequests()
		stated, err := y.ListIssuesWithState(ctx, testProjectID, opts)
		if err != nil {
			t.Fatal(err)
		}
		if second := srv.Requests(); !slices.Equal(first, second) {
			t.Errorf("%s: ListIssues sent %v, ListIssuesWithState %v", name, first, second)
		}
		if len(plain) != len(stated) {
			t.Fatalf("%s: %d issues against %d", name, len(plain), len(stated))
		}
		for i := range plain {
			if plain[i].IID != stated[i].Issue.IID {
				t.Errorf("%s: issue %d differs", name, i)
			}
		}
		if name == "project" && (len(first) != 1 || first[0].Path != "/api/issues") {
			t.Errorf("project listing sent %v, want one GET of /api/issues", first)
		}
	}
}

func TestListIssuesWithStateRefusesTooManyCustomFields(t *testing.T) {
	field := func(i int) string {
		return fmt.Sprintf(`{"$type":"SimpleIssueCustomField","name":"f%d","value":"x"}`, i)
	}
	issueWith := func(n int) string {
		fields := make([]string, 0, n+1)
		fields = append(fields, `{"$type":"StateIssueCustomField","name":"State","value":{"$type":"StateBundleElement","name":"Open"}}`)
		for i := 1; i < n; i++ {
			fields = append(fields, field(i))
		}
		return strings.TrimSuffix(rawIssue("2-1", "UZI-1", 1, "0-1", ""), `}`) + `,"customFields":[` + strings.Join(fields, ",") + `]}`
	}
	for name, tc := range map[string]struct {
		n       int
		wantErr bool
	}{"at the cap": {maxCustomFields, false}, "over the cap": {maxCustomFields + 1, true}} {
		t.Run(name, func(t *testing.T) {
			body := "[" + issueWith(tc.n) + "]"
			y, _ := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, body) })
			got, err := y.ListIssuesWithState(context.Background(), testProjectID, forge.ListIssuesOptions{State: forge.StateAll})
			switch {
			case tc.wantErr && (err == nil || !strings.Contains(err.Error(), "custom fields")):
				t.Errorf("got %+v, %v; want the custom field ceiling", got, err)
			case !tc.wantErr && (err != nil || len(got) != 1 || got[0].State != "Open"):
				t.Errorf("got %+v, %v; want one issue in Open", got, err)
			}
		})
	}
}

func TestSetIssueStateRefusesAWontFixRelabelledByLocalizedName(t *testing.T) {
	y, srv := columnDriver(t, func(o *Options) { o.ClosedState = "Won't fix" })
	srv.SetStateLocalizedName("Won't fix", "Fixed")
	n := srv.SeedIssue("t", "")
	srv.ResetRequests()
	err := y.SetIssueState(context.Background(), testProjectID, n, forge.StateClosed)
	if !errors.Is(err, ErrStateNotAllowed) {
		t.Fatalf("SetIssueState = %v, want ErrStateNotAllowed", err)
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent: %v", w)
	}
}

func TestStateChecksRunBeforeTheIssueIsRead(t *testing.T) {
	// A moved-away issue and a disallowed target: the state check comes first.
	y, srv := columnDriver(t, func(o *Options) { o.ClosedState = "Won't fix" })
	n := srv.SeedIssue("t", "")
	srv.MoveIssue(n, youtracktest.OtherProjectID)
	srv.ResetRequests()
	ctx := context.Background()
	for name, err := range map[string]error{
		"SetIssueState": y.SetIssueState(ctx, testProjectID, n, forge.StateClosed),
		"MoveToColumn":  y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: "Lost"}),
	} {
		if !errors.Is(err, ErrStateNotAllowed) || isNotFound(err) {
			t.Errorf("%s = %v, want ErrStateNotAllowed and not an IssueNotFoundError", name, err)
		}
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent: %v", w)
	}
}

func TestOnlyTheListingAsksForCustomFields(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedIssue("t", "")
	ctx := context.Background()
	srv.ResetRequests()
	if _, err := y.GetIssue(ctx, testProjectID, n); err != nil {
		t.Fatal(err)
	}
	if err := y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: "In Progress"}); err != nil {
		t.Fatal(err)
	}
	if _, err := y.CreateIssue(ctx, testProjectID, "c", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Requests() {
		if r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/api/issues/") || r.Method == http.MethodPost && r.Path == "/api/issues" {
			if strings.Contains(r.RawQuery, "customFields") {
				t.Errorf("%s %s asked for custom fields: %s", r.Method, r.Path, r.RawQuery)
			}
		}
	}
	srv.ResetRequests()
	if _, err := y.ListIssuesWithState(ctx, testProjectID, forge.ListIssuesOptions{State: forge.StateAll}); err != nil {
		t.Fatal(err)
	}
	rs := srv.Requests()
	if len(rs) == 0 {
		t.Fatal("the listing sent no request")
	}
	for _, r := range rs {
		if !strings.Contains(r.RawQuery, "customFields") {
			t.Errorf("listing request %s %s did not ask for customFields: %s", r.Method, r.Path, r.RawQuery)
		}
	}
}

func TestListIssuesWithStateReadsTheFirstResolvedStateOfAResolvedIssue(t *testing.T) {
	y, srv := columnDriver(t, nil)
	n := srv.SeedResolvedIssue("old")
	srv.SetStateArchived("Fixed", true)
	srv.SeedState("Done", true)
	got, err := y.ListIssuesWithState(context.Background(), testProjectID, forge.ListIssuesOptions{State: forge.StateAll})
	if err != nil || len(got) != 1 || got[0].Issue.IID != n {
		t.Fatalf("listed %+v, %v", got, err)
	}
	// Open is unresolved and Fixed archived; "Won't fix" is the first resolved,
	// non-archived state in the bundle.
	if got[0].State != "Won't fix" {
		t.Errorf("State = %q, want the first resolved, non-archived state", got[0].State)
	}
}

func TestSetIssueStateRefusesAWontFixRelabelledWithACurlyApostrophe(t *testing.T) {
	y, srv := columnDriver(t, nil)
	curly := "Won\u2019t fix"
	srv.RenameState("Won't fix", curly)
	srv.SetStateLocalizedName(curly, "Fixed")
	y.closedState = curly
	n := srv.SeedIssue("t", "")
	srv.ResetRequests()
	err := y.SetIssueState(context.Background(), testProjectID, n, forge.StateClosed)
	if !errors.Is(err, ErrStateNotAllowed) {
		t.Fatalf("SetIssueState = %v, want ErrStateNotAllowed", err)
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent: %v", w)
	}
}

func TestAFillerInAStateNameIsDroppedAndNotClosable(t *testing.T) {
	y, srv := columnDriver(t, nil)
	name := "Won't fix\u3164"
	srv.RenameState("Won't fix", name)
	states, err := y.ListStates(context.Background(), "0-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Name == name {
			t.Errorf("ListStates kept %q", name)
		}
	}
	if ClosableState(State{Name: name, Resolved: true}) {
		t.Error("ClosableState accepted a name with a filler")
	}
}

func TestListIssuesWithStateReadsAStateMachineField(t *testing.T) {
	field := func(typ string) string {
		return `{"$type":"` + typ + `","name":"State","value":{"$type":"StateBundleElement","name":"Review"}}`
	}
	for typ, want := range map[string]string{
		"StateIssueCustomField":        "Review",
		"StateMachineIssueCustomField": "Review",
		"SimpleIssueCustomField":       "",
	} {
		body := "[" + strings.TrimSuffix(rawIssue("2-1", "UZI-1", 1, "0-1", ""), `}`) + `,"customFields":[` + field(typ) + `]}]`
		y, _ := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, body) })
		got, err := y.ListIssuesWithState(context.Background(), testProjectID, forge.ListIssuesOptions{State: forge.StateAll})
		if err != nil || len(got) != 1 || got[0].State != want {
			t.Errorf("%s: got %+v, %v; want State %q", typ, got, err, want)
		}
	}
}

func TestRepeatedCustomFieldsKeyLeavesNoStaleState(t *testing.T) {
	// The second "customFields" key reuses the first one's backing array and
	// omits the value; the first key's State name must not survive.
	const state = `{"$type":"StateIssueCustomField","name":"State"`
	raw := strings.TrimSuffix(rawIssue("2-1", "UZI-1", 1, "0-1", ""), `}`) +
		`,"customFields":[` + state + `,"value":{"name":"Open"}}],"customFields":[` + state + `}]}`
	var w ytIssue
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	y := &youTrack{stateField: "State"}
	if got := y.stateName(w); got != "" {
		t.Errorf("stateName = %q, want empty after the later key omitted the value", got)
	}
}

func TestCustomFieldNotObjectHasItsOwnError(t *testing.T) {
	var c ytCustomFields
	if err := c.UnmarshalJSON([]byte(`[1]`)); !errors.Is(err, errCustomFieldNotObject) {
		t.Errorf("err = %v, want errCustomFieldNotObject", err)
	}
}

func TestMoveToColumnRefusesAnIssueResolvedInTheIssueTrackerWithoutAReopen(t *testing.T) {
	y, srv := columnDriver(t, nil)
	ctx := context.Background()
	n := srv.SeedIssue("t", "")
	if err := y.SetIssueState(ctx, testProjectID, n, forge.StateClosed); err != nil {
		t.Fatalf("resolve the issue: %v", err)
	}
	before := srv.IssueState(n)
	srv.ResetRequests()
	err := y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: "In Progress"})
	if !errors.Is(err, ErrIssueResolved) {
		t.Fatalf("MoveToColumn = %v, want ErrIssueResolved", err)
	}
	if w := writesOf(srv); len(w) != 0 {
		t.Errorf("writes sent for a resolved issue: %v", w)
	}
	if got := srv.IssueState(n); got != before {
		t.Errorf("issue state = %q, want it untouched (%q)", got, before)
	}
}

func TestMoveToColumnWithReopenWritesOverAResolvedIssue(t *testing.T) {
	y, srv := columnDriver(t, nil)
	ctx := context.Background()
	n := srv.SeedIssue("t", "")
	if err := y.SetIssueState(ctx, testProjectID, n, forge.StateClosed); err != nil {
		t.Fatalf("resolve the issue: %v", err)
	}
	if err := y.MoveToColumn(ctx, testProjectID, n, forge.ColumnMove{Column: "In Progress", Reopen: true}); err != nil {
		t.Fatalf("MoveToColumn with Reopen: %v", err)
	}
	if got := srv.IssueState(n); got != "In Progress" {
		t.Errorf("issue state = %q, want In Progress", got)
	}
	iss, err := y.GetIssue(ctx, testProjectID, n)
	if err != nil {
		t.Fatal(err)
	}
	if iss.State != string(forge.StateOpened) {
		t.Errorf("issue state after the reopen = %q, want opened", iss.State)
	}
}
