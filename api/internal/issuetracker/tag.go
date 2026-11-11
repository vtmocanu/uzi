package issuetracker

// Tag is a YouTrack tag as ListTags reports it: only tags the token can see
// come back, so a Tag in hand is one the account can see.
type Tag struct {
	ID    string
	Name  string
	Owner Account
	// Read says who can see the tag; Use says who can apply it to an issue.
	Read SharingSettings
	Use  SharingSettings
}

// SharingSettings is who a tag is shared with: user groups and single users.
type SharingSettings struct {
	Groups []Group
	Users  []Account
}

// Group is a YouTrack user group. UsersCount is 0 when the response did not
// carry a count, which the rules below read as "not known to have members".
type Group struct {
	ID         string
	Name       string
	AllUsers   bool // the built-in group of every user
	UsersCount int
}

// AccountCanApply reports whether account a can apply tag t to an issue. It is
// true when a owns the tag, is named in the tag's use settings, or a use group
// covers all users. A tag's settings do not list the members of a named group,
// so membership in one cannot be read from them: a named group alone does not
// count, and the answer fails closed.
//
// The tag must come from ListTags (or a read of the tag by id), which return
// only tags the token can see. This function judges the apply half only; it does
// not know whether a can see t.
func AccountCanApply(t Tag, a Account) bool {
	if a.ID == "" {
		return false
	}
	if t.Owner.ID == a.ID {
		return true
	}
	for _, u := range t.Use.Users {
		if u.ID == a.ID {
			return true
		}
	}
	for _, g := range t.Use.Groups {
		if g.AllUsers {
			return true
		}
	}
	return false
}

// OthersCanApply reports whether someone other than account a can apply tag t.
// It is true when the tag has another owner (an owner can always apply their own
// tag), when a user other than a is named in the use settings, or when a use
// group covers all users or has at least two members. A named group of one
// member does not count, because that member may be a. An unknown owner or
// account never counts, so the answer fails closed.
//
// The tag must come from ListTags, as for AccountCanApply.
func OthersCanApply(t Tag, a Account) bool {
	if a.ID == "" {
		return false
	}
	if t.Owner.ID != "" && t.Owner.ID != a.ID {
		return true
	}
	for _, u := range t.Use.Users {
		if u.ID != "" && u.ID != a.ID {
			return true
		}
	}
	for _, g := range t.Use.Groups {
		if g.AllUsers || g.UsersCount >= 2 {
			return true
		}
	}
	return false
}
