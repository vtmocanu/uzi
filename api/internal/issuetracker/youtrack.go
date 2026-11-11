package issuetracker

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// defaultStateField is the YouTrack custom field that holds the issue state.
const defaultStateField = "State"

var (
	shortNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	// entityIDRE matches a YouTrack entity id such as "2-15". Every id taken
	// from a response and placed in a path is checked against it.
	entityIDRE = regexp.MustCompile(`^\d+-\d+$`)
)

// youTrack is the JetBrains YouTrack driver. It is bound to one project when
// built (see Binding). Every forge.IssueTracker method checks its projectID
// argument against the bound id before any request: a driver with no project
// bound refuses them with ErrProjectNotBound, and a different id gets
// ErrProjectMismatch. VerifyToken, ListProjects, ListTags and ListStates need
// no binding.
type youTrack struct {
	baseURL     string // scheme and host, no trailing slash
	token       string
	project     ProjectRef
	projectID   int64  // the forge project id the driver answers to
	eligibility TagRef // the eligibility tag; its name always resolves to its id
	stateField  string
	openState   string
	closedState string
	client      *http.Client
	red         redactor

	// accountMu guards account, the token's own account. It is read from
	// users/me the first time tag ownership matters, then kept (see connAccount).
	accountMu sync.Mutex
	account   *Account
}

var _ Driver = (*youTrack)(nil)

// newYouTrack is the registry constructor: system roots, default transport.
func newYouTrack(opts Options) (Driver, error) {
	return buildYouTrack(opts, nil)
}

// buildYouTrack validates opts and builds the driver over base, which is nil
// outside tests. A build error never carries the token or any part of it.
func buildYouTrack(opts Options, base http.RoundTripper) (*youTrack, error) {
	baseURL, err := strictBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	if err := ValidateToken(opts.Token); err != nil {
		return nil, err
	}
	if err := ValidBinding(Binding{Project: opts.Project, ProjectID: opts.ProjectID, EligibilityTag: opts.EligibilityTag}); err != nil {
		return nil, err
	}
	stateField := opts.StateField
	if stateField == "" {
		stateField = defaultStateField
	}
	return &youTrack{
		baseURL:     baseURL,
		token:       opts.Token,
		project:     opts.Project,
		projectID:   opts.ProjectID,
		eligibility: opts.EligibilityTag,
		stateField:  stateField,
		openState:   opts.OpenState,
		closedState: opts.ClosedState,
		client:      newHTTPClient(opts.Timeout, base),
		red:         newRedactor(opts.Token),
	}, nil
}

// strictBaseURL accepts only https://host[:port] with an optional single
// trailing slash. A path, userinfo, query or fragment is rejected rather than
// stripped, so the URL the token is sent to is exactly the one that was
// validated.
func strictBaseURL(raw string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("issuetracker: base URL %s", why)
	}
	if strings.ContainsAny(raw, "?#") {
		return bad("must not carry a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("is not a valid URL")
	}
	switch {
	case u.Scheme != "https":
		return bad("must use https")
	case u.Opaque != "" || u.Hostname() == "":
		return bad("must name a host")
	case u.User != nil:
		return bad("must not carry credentials")
	case u.Path != "" && u.Path != "/":
		return bad("must not carry a path")
	}
	// An invisible or non-ASCII host character would look like another host
	// once shown; an internationalised name arrives as "xn--" punycode.
	for _, c := range u.Host {
		if InvisibleRune(c) || c > unicode.MaxASCII {
			return bad("host must be plain ASCII with no control or invisible character")
		}
	}
	return "https://" + u.Host, nil
}

// requireProject is the guard every forge.IssueTracker method starts with,
// before any request: it returns the bound project, ErrProjectNotBound for an
// unbound driver, or ErrProjectMismatch when projectID is not the bound one.
func (y *youTrack) requireProject(projectID int64) (ProjectRef, error) {
	if y.project == (ProjectRef{}) {
		return ProjectRef{}, y.red.sentinel(ErrProjectNotBound, 0, "")
	}
	if projectID != y.projectID {
		return ProjectRef{}, y.red.sentinel(ErrProjectMismatch, 0, "")
	}
	return y.project, nil
}

// fail returns a redacted error built from a format.
func (y *youTrack) fail(format string, args ...any) error {
	return y.red.error(fmt.Errorf(format, args...))
}
