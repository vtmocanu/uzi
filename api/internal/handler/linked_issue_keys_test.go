package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The key is built only for a repo linked to an issue tracker with a stored project short
// name; every other repo answers "", which is what omits the key from a payload.
func TestLinkedIssueKeyIsEmptyOffALinkedRepo(t *testing.T) {
	linked := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	short := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	for _, tc := range []struct {
		name string
		repo store.GetRepoForUserRow
		want string
	}{
		{"linked", store.GetRepoForUserRow{IssueTrackerConnectionID: linked, IssueTrackerProjectShort: short("ABC")}, "ABC-12"},
		{"unlinked", store.GetRepoForUserRow{}, ""},
		{"no connection", store.GetRepoForUserRow{IssueTrackerProjectShort: short("ABC")}, ""},
		{"empty short name", store.GetRepoForUserRow{IssueTrackerConnectionID: linked, IssueTrackerProjectShort: short("")}, ""},
		{"null short name", store.GetRepoForUserRow{IssueTrackerConnectionID: linked}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkedIssueKey(tc.repo, 12); got != tc.want {
				t.Errorf("linkedIssueKey = %q, want %q", got, tc.want)
			}
		})
	}
	if got := issueKeyFromShort("", 12); got != "" {
		t.Errorf("issueKeyFromShort of an empty short name = %q, want empty", got)
	}
	if got := issueKeyFromShort("ABC", 12); got != "ABC-12" {
		t.Errorf("issueKeyFromShort = %q, want ABC-12", got)
	}
}
