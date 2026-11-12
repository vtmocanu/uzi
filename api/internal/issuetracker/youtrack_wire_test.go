package issuetracker

import (
	"context"
	"testing"
)

func TestIssueMapping(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("title", "body", srv.SeedOwnTag("x"), srv.SeedOwnTag("y"))
	got, err := y.GetIssue(context.Background(), testProjectID, n)
	if err != nil {
		t.Fatal(err)
	}
	if got.IID != n || got.Title != "title" || got.Description != "body" || got.State != "opened" ||
		got.Author != youtracktestBot || got.AuthorForgeUserID != 0 || got.Assignees == nil || len(got.Assignees) != 0 ||
		got.WebURL != srv.URL+"/issue/UZI-1" || len(got.Labels) != 2 || got.UpdatedAt.IsZero() {
		t.Fatalf("mapped issue = %+v", got)
	}
}
