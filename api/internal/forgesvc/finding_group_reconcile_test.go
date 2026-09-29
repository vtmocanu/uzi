package forgesvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestMatchFindingGroupIssue(t *testing.T) {
	id := uuid.New()
	marker := "<!-- uzi-finding-group-operation: " + id.String() + " -->"
	valid := forge.Issue{IID: 42, WebURL: "https://example.com/issues/42", Description: "body\n" + marker}
	tests := []struct {
		name   string
		issues []forge.Issue
		want   bool
	}{
		{"unique", []forge.Issue{{Description: "<!-- uzi-finding-group-operation: " + uuid.NewString() + " -->"}, valid}, true},
		{"absent", []forge.Issue{{IID: 42, WebURL: valid.WebURL}}, false},
		{"altered marker", []forge.Issue{{IID: 42, WebURL: valid.WebURL, Description: "<!-- uzi-finding-group-operation:" + id.String() + " -->"}}, false},
		{"two issues", []forge.Issue{valid, valid}, false},
		{"repeated in issue", []forge.Issue{{IID: 42, WebURL: valid.WebURL, Description: marker + marker}}, false},
		{"invalid identity", []forge.Issue{{Description: marker}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issue, ok := matchFindingGroupIssue(id, tc.issues)
			if ok != tc.want {
				t.Fatalf("match = %v, want %v", ok, tc.want)
			}
			if ok && issue.IID != 42 {
				t.Fatalf("matched iid = %d", issue.IID)
			}
		})
	}
}
