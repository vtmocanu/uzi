package issuetracker

import "fmt"

// Constructor builds a driver from options. A build error never carries the
// token, a part of it or any other remote content, because callers show it to
// the user.
type Constructor func(Options) (Driver, error)

// constructors maps each Kind to its driver constructor. A driver adds one
// entry here; there is no init-time registration.
var constructors = map[Kind]Constructor{KindYouTrack: newYouTrack}

// tagVerdicts maps each Kind to how it judges a tag for a link: whether the
// connection's account can apply it, and whether someone else can. Who may apply
// a tag is a property of each issue tracker's sharing model, so the rule lives
// with the kind, not in the link (PRD #4765 A1). A kind with no entry judges
// every tag unusable.
var tagVerdicts = map[Kind]func(Tag, Account) (accountCanApply, othersCanApply bool){
	KindYouTrack: youTrackTagVerdicts,
}

// TagVerdicts judges tag t for account a under kind's sharing rules. An unknown
// kind fails closed: neither verdict holds.
func TagVerdicts(kind Kind, t Tag, a Account) (accountCanApply, othersCanApply bool) {
	judge, ok := tagVerdicts[kind]
	if !ok {
		return false, false
	}
	return judge(t, a)
}

// New constructs the driver for kind. An unknown kind wraps ErrUnsupportedKind.
// Like every Constructor, it never puts the token in an error.
func New(kind Kind, opts Options) (Driver, error) {
	ctor, ok := constructors[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q (supported: %s)", ErrUnsupportedKind, string(kind), SupportedKindsList())
	}
	return ctor(opts)
}
