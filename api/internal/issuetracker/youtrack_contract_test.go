package issuetracker_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/issuetrackertest"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// contractProjectID is the forge project id the contract's YouTrack driver is
// bound to; it is deliberately not 1, the id the forge fake's first project has.
const contractProjectID = 4173

// TestYouTrackContract runs the shared issue tracker contract against the
// YouTrack driver over the in-process fake.
func TestYouTrackContract(t *testing.T) {
	issuetrackertest.RunContract(t, func(t *testing.T) issuetrackertest.Subject {
		srv := youtracktest.NewServer(t)
		const openState = "Open"
		// Two columns mapped to unresolved State values the fake's bundle holds.
		columns := []string{"contract-column-a", "contract-column-b"}
		stateOf := map[string]string{columns[0]: "Contract State A", columns[1]: "Contract State B"}
		for _, state := range stateOf {
			srv.SeedState(state, false)
		}
		y, err := issuetracker.NewYouTrackOver(issuetracker.Options{
			BaseURL:      srv.URL,
			Token:        srv.Token,
			Project:      issuetracker.ProjectRef{ID: youtracktest.ProjectID, ShortName: youtracktest.ProjectShortName},
			ProjectID:    contractProjectID,
			OpenState:    openState,
			ClosedState:  "Fixed",
			ColumnStates: stateOf,
			EligibilityTag: issuetracker.TagRef{
				ID: youtracktest.EligibilityTagID, Name: youtracktest.EligibilityTagName,
			},
		}, srv.Client().Transport)
		if err != nil {
			t.Fatal(err)
		}
		return issuetrackertest.Subject{
			IssueTracker: y,
			ProjectID:    contractProjectID,
			Columns:      columns,
			PlanMove: func(_ *testing.T, _ int64, column string) forge.ColumnMove {
				return forge.ColumnMove{Column: column}
			},
			ColumnOf: func(_ context.Context, iid int64) (string, error) {
				state := srv.IssueState(iid)
				for column, name := range stateOf {
					if name == state {
						return column, nil
					}
				}
				if state == openState {
					return "", nil
				}
				// Any other unmapped State is a failure, not the Open column.
				return "unmapped State " + state, nil
			},
			SeedHumanComment: func(t *testing.T, iid int64, username, body string) {
				srv.SeedComment(iid, username, body)
			},
			SeedLabelEvent: func(t *testing.T, iid int64, action, label, username string) {
				// The contract made the label through EnsureLabels, so it is the
				// tag the bot owns.
				var id string
				for _, tag := range srv.TagsNamed(label) {
					if tag.OwnerLogin == youtracktest.BotLogin {
						id = tag.ID
					}
				}
				if id == "" {
					t.Fatalf("no tag %q owned by the bot", label)
				}
				if action == "add" {
					srv.SeedTagActivity(iid, username, []string{id}, nil)
				} else {
					srv.SeedTagActivity(iid, username, nil, []string{id})
				}
			},
			SeedDeletedComment: func(t *testing.T, iid int64, username, body string) {
				srv.SeedDeletedComment(iid, username, body)
			},
			MoveIssue: func(t *testing.T, iid int64) {
				srv.MoveIssue(iid, youtracktest.OtherProjectID)
			},
			OtherProjectID:      contractProjectID + 1,
			WantProjectGuardErr: issuetracker.ErrProjectMismatch,
			NoteIDUnset:         true,
			SameName: &issuetrackertest.SameNameSeams{
				EligibilityName:  youtracktest.EligibilityTagName,
				EligibilityTagID: youtracktest.EligibilityTagID,
				SameNameTagID:    youtracktest.SameNameTagID,
				SeedIssue: func(t *testing.T, tagIDs ...string) int64 {
					return srv.SeedIssue("seeded", "", tagIDs...)
				},
				IssueTagIDs: func(t *testing.T, iid int64) []string { return srv.IssueTagIDs(iid) },
				SeedForeignTag: func(t *testing.T, name string) string {
					everyone := youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}}
					return srv.SeedTag(name, youtracktest.OtherLogin, everyone, youtracktest.Sharing{})
				},
				TagsNamed: func(t *testing.T, name string) []issuetrackertest.TagView {
					// The fake's eligibility tag is owned by the other user, so a
					// tag shared like it also names that owner.
					eligibility := srv.TagByID(youtracktest.EligibilityTagID)
					want := func(s youtracktest.Sharing) youtracktest.Sharing {
						return youtracktest.Sharing{Groups: s.Groups, Users: append(slices.Clone(s.Users), youtracktest.OtherUserID)}
					}
					var out []issuetrackertest.TagView
					for _, tag := range srv.TagsNamed(name) {
						out = append(out, issuetrackertest.TagView{
							ID:                    tag.ID,
							OwnedByAccount:        tag.OwnerLogin == youtracktest.BotLogin,
							SharedLikeEligibility: reflect.DeepEqual(tag.Read, want(eligibility.Read)) && reflect.DeepEqual(tag.Use, want(eligibility.Use)),
						})
					}
					return out
				},
				RenameTag: func(t *testing.T, tagID, name string) { srv.RenameTag(tagID, name) },
			},
		}
	})
}
