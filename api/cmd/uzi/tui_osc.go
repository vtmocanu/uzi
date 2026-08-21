package main

import (
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// oscLink wraps visible text in an OSC-8 hyperlink so a command-click opens url in
// the browser. url is forge-authored and untrusted, so control bytes are stripped
// (D7) — sanitizeTTY also removes the ESC that would otherwise let a hostile url
// forge its own ST terminator. Uses the ST form (ESC \) rather than BEL. Returns
// bare text when url is empty (nothing to link to).
func oscLink(url, text string) string {
	if url == "" {
		return text
	}
	url = sanitizeTTY(url)
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// linksEnabled reports whether the terminal can render OSC-8 hyperlinks. Ascii /
// NO_COLOR (and the never-reached NoTTY/Unknown) get plain text instead.
func (m tuiModel) linksEnabled() bool { return m.profile >= colorprofile.ANSI }

// issueLink hyperlinks an already-styled #<iid> segment to the run's forge issue
// URL when one exists and the terminal supports links; otherwise returns the plain
// styled segment (still a useful, non-clickable label). Call only when the caller
// has already confirmed r.IssueIID != nil and built styledIID.
func (m tuiModel) issueLink(r apitypes.RunDTO, styledIID string) string {
	if r.IssueWebURL != nil && m.linksEnabled() {
		return oscLink(sanitizeTTY(strOr(r.IssueWebURL, "")), styledIID)
	}
	return styledIID
}
