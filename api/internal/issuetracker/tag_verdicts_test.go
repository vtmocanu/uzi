package issuetracker

import "testing"

// TagVerdicts is the kind's tag judgment for a link (PRD #4765 A1). For YouTrack it
// is exactly AccountCanApply and OthersCanApply; an unknown kind fails closed, even
// for a tag that YouTrack's rules would let everyone apply.
func TestTagVerdictsByKind(t *testing.T) {
	me := Account{ID: "1-1", Login: "uzi"}
	other := Account{ID: "1-2", Login: "ann"}
	allUsers := SharingSettings{Groups: []Group{{ID: "3-0", AllUsers: true}}}
	for _, tc := range []struct {
		name string
		tag  Tag
	}{
		{"owned, private", Tag{ID: "6-1", Owner: me}},
		{"owned, shared with all", Tag{ID: "6-2", Owner: me, Use: allUsers}},
		{"another's, shared with me", Tag{ID: "6-3", Owner: other, Use: SharingSettings{Users: []Account{me}}}},
		{"another's, private", Tag{ID: "6-4", Owner: other}},
	} {
		acc, oth := TagVerdicts(KindYouTrack, tc.tag, me)
		if acc != AccountCanApply(tc.tag, me) || oth != OthersCanApply(tc.tag, me) {
			t.Errorf("%s: YouTrack verdicts = %v, %v; want the YouTrack rules %v, %v",
				tc.name, acc, oth, AccountCanApply(tc.tag, me), OthersCanApply(tc.tag, me))
		}
		if acc, oth := TagVerdicts(Kind("nope"), tc.tag, me); acc || oth {
			t.Errorf("%s: unknown kind verdicts = %v, %v; want false, false", tc.name, acc, oth)
		}
	}
}

// ValidIDFor is the stored shape plus the kind's grammar; an unknown kind accepts no id.
func TestValidIDForByKind(t *testing.T) {
	for id, want := range map[string]bool{"0-1": true, "12-345": true, "abc": false, "1-": false, "1208765432109876": false, "0-1/x": false, "": false} {
		if got := ValidIDFor(KindYouTrack, id); got != want {
			t.Errorf("ValidIDFor(youtrack, %q) = %v, want %v", id, got, want)
		}
	}
	if ValidIDFor(Kind("nope"), "0-1") {
		t.Error("an unknown kind accepted an id")
	}
}
