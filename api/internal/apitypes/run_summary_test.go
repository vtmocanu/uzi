package apitypes

import (
	"reflect"
	"sort"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes/apitypestest"
)

// summaryOmitted is the exact set of RunListItemDTO keys the ?view=summary projection drops
// (issue #2661). Adding a key here is a deliberate wire decision, never a side effect.
var summaryOmitted = map[string]bool{
	"plan_md":           true,
	"repo_agents":       true,
	"issue_description": true,
	"preserved_patch":   true,
}

// TestRunSummaryItemDTOKeys pins the summary key set to the list item's key set minus
// summaryOmitted. A new RunDTO/RunListItemDTO field fails here until it is placed on the
// summary on purpose.
func TestRunSummaryItemDTOKeys(t *testing.T) {
	var full RunListItemDTO
	apitypestest.Populate(&full)

	want := make([]string, 0)
	for _, k := range tagSet(t, full) {
		if !summaryOmitted[k] {
			want = append(want, k)
		}
	}
	sort.Strings(want)
	for k := range summaryOmitted {
		if !contains(tagSet(t, full), k) {
			t.Fatalf("summaryOmitted names %q, which RunListItemDTO no longer emits", k)
		}
	}
	assertTags(t, "RunSummaryOf(populated)", RunSummaryOf(full), want...)
}

// TestRunSummaryItemRoundTrip proves ListItem is the inverse of RunSummaryOf for every field
// the summary carries: only the four omitted fields (and the embedded worker name, which the
// list path never populates) differ from the source.
func TestRunSummaryItemRoundTrip(t *testing.T) {
	var full RunListItemDTO
	apitypestest.Populate(&full)

	want := full
	want.PlanMd = nil
	want.RepoAgents = nil
	want.IssueDescription = ""
	want.PreservedPatch = nil
	want.RunDTO.WorkerName = nil

	got := RunSummaryOf(full).ListItem()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunSummaryOf(x).ListItem() != x minus heavy fields\n got: %+v\nwant: %+v", got, want)
	}
}
