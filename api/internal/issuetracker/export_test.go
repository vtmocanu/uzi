package issuetracker

import "net/http"

// newYouTrackOver builds a YouTrack driver whose HTTP transport is base, so a
// test can trust the self-signed certificate of a youtracktest server or an
// httptest TLS server. Options carries no TLS field by design.
func newYouTrackOver(opts Options, base http.RoundTripper) (*youTrack, error) {
	return buildYouTrack(opts, base)
}

// NewYouTrackOver is newYouTrackOver for the external test package
// issuetracker_test, which must be external because issuetrackertest imports
// issuetracker.
func NewYouTrackOver(opts Options, base http.RoundTripper) (Driver, error) {
	y, err := buildYouTrack(opts, base)
	if err != nil {
		return nil, err
	}
	return y, nil
}

// Credential and TokenForms share the internal tests' token fixtures with the
// external test package, so there is one copy of them.
var (
	Credential = credential
	TokenForms = tokenForms
)
