package workersvc

import "testing"

// The ladder's precedence holds pairwise: a disposition always outranks the filed axis,
// and dismissed outranks done. filed(settled) outranks todo.
func TestBucketOfPrecedence(t *testing.T) {
	cases := []struct {
		status string
		filed  bool
		want   string
	}{
		{"dismissed", true, "dismissed"}, // dismissed beats filed
		{"dismissed", false, "dismissed"},
		{"done", true, "done"}, // done beats filed
		{"done", false, "done"},
		{"", true, "filed"}, // no disposition, settled link → filed
		{"", false, "todo"}, // nothing → todo
	}
	for _, c := range cases {
		if got := BucketOf(c.status, c.filed); got != c.want {
			t.Errorf("BucketOf(%q, %v) = %q, want %q", c.status, c.filed, got, c.want)
		}
	}
}

// An unsettled claim (filed axis false) is NOT filed — it buckets as todo, matching the
// per-review path that skips claims without filed_at.
func TestBucketTriageUnsettledClaimIsNotFiled(t *testing.T) {
	got := BucketTriage([]TriageRow{{FiledSettled: false}})
	if got.Todo != 1 || got.Filed != 0 || got.Total != 1 {
		t.Fatalf("unsettled claim should be todo, got %+v", got)
	}
}

// FalsePositives counts only not_an_issue dismissals; a wont_do dismissal counts as
// Dismissed but not a false positive.
func TestBucketTriageCounts(t *testing.T) {
	rows := []TriageRow{
		{Status: "", FiledSettled: false},                                 // todo
		{Status: "", FiledSettled: true},                                  // filed
		{Status: "done"},                                                  // done
		{Status: "done", FiledSettled: true},                              // done (beats filed)
		{Status: "dismissed", Reason: "wont_do"},                          // dismissed, not FP
		{Status: "dismissed", Reason: "not_an_issue"},                     // dismissed + FP
		{Status: "dismissed", Reason: "not_an_issue", FiledSettled: true}, // dismissed + FP (beats filed)
	}
	got := BucketTriage(rows)
	want := struct{ total, todo, filed, done, dismissed, fp int }{7, 1, 1, 2, 3, 2}
	if got.Total != want.total || got.Todo != want.todo || got.Filed != want.filed ||
		got.Done != want.done || got.Dismissed != want.dismissed || got.FalsePositives != want.fp {
		t.Fatalf("BucketTriage = %+v, want %+v", got, want)
	}
}
