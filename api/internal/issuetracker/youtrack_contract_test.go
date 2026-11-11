package issuetracker_test

import (
	"reflect"
	"testing"

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
		y, err := issuetracker.NewYouTrackOver(issuetracker.Options{
			BaseURL:     srv.URL,
			Token:       srv.Token,
			Project:     issuetracker.ProjectRef{ID: youtracktest.ProjectID, ShortName: youtracktest.ProjectShortName},
			ProjectID:   contractProjectID,
			OpenState:   "Open",
			ClosedState: "Fixed",
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
				TagsNamed: func(t *testing.T, name string) []issuetrackertest.TagView {
					eligibility := srv.TagByID(youtracktest.EligibilityTagID)
					var out []issuetrackertest.TagView
					for _, tag := range srv.TagsNamed(name) {
						out = append(out, issuetrackertest.TagView{
							ID:                    tag.ID,
							OwnedByAccount:        tag.OwnerLogin == youtracktest.BotLogin,
							SharedLikeEligibility: reflect.DeepEqual(tag.Read, eligibility.Read) && reflect.DeepEqual(tag.Use, eligibility.Use),
						})
					}
					return out
				},
			},
		}
	})
}
