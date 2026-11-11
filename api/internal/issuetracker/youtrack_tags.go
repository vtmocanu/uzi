package issuetracker

import "context"

// ListTags implements Driver. It lists the tags the token can see with their
// owners and sharing settings; it needs no bound project. Every id taken from
// the response is checked, and one tag's users and groups are capped at
// maxNestedItems and count toward the listing ceiling.
func (y *youTrack) ListTags(ctx context.Context) ([]Tag, error) {
	wire, err := y.listTags(ctx, sharedTagFields)
	if err != nil {
		return nil, err
	}
	out := make([]Tag, 0, len(wire))
	for _, w := range wire {
		if w.nestedCount() > maxNestedItems {
			return nil, y.red.error(errNestedCeiling)
		}
		t := Tag{ID: w.ID, Name: w.Name}
		if w.Owner != nil {
			if t.Owner, err = y.accountOf(*w.Owner); err != nil {
				return nil, err
			}
		}
		if t.Read, err = y.sharingOf(w.Read); err != nil {
			return nil, err
		}
		if t.Use, err = y.sharingOf(w.Use); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// accountOf maps a wire user, refusing a malformed id.
func (y *youTrack) accountOf(u ytUser) (Account, error) {
	if !entityIDRE.MatchString(u.ID) {
		return Account{}, y.fail("youtrack: user with a malformed id in the response")
	}
	return Account(u), nil
}

// sharingOf maps wire sharing settings; absent settings are empty.
func (y *youTrack) sharingOf(s *ytSharing) (SharingSettings, error) {
	var out SharingSettings
	if s == nil {
		return out, nil
	}
	for _, g := range s.Groups {
		if !entityIDRE.MatchString(g.ID) {
			return SharingSettings{}, y.fail("youtrack: group with a malformed id in the response")
		}
		out.Groups = append(out.Groups, Group(g))
	}
	for _, u := range s.Users {
		a, err := y.accountOf(u)
		if err != nil {
			return SharingSettings{}, err
		}
		out.Users = append(out.Users, a)
	}
	return out, nil
}
