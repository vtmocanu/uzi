package main

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// oscLink wraps visible text in an OSC-8 hyperlink so a command-click opens url in
// the browser. url is forge-authored and untrusted, so EVERY control/format rune is
// stripped here — including \n and \t, which sanitizeTTY deliberately spares for
// flowing text but which have no place in a single-line URL. Uses the ST form (ESC \)
// rather than BEL. Returns bare text when url is empty (nothing to link to).
func oscLink(url, text string) string {
	if url == "" {
		return text
	}
	// The URL is an OSC-8 escape PARAMETER, not flowing text: per the spec its URI is
	// single-line printable, and a raw newline/tab is a framing break, not content.
	// sanitizeTTY (tuned for multi-line free text) SPARES \n and \t, which here would
	// forge a row in the frame (#169 class), so strip every control/format rune instead.
	// This also removes ESC/BEL, so a hostile forge URL cannot forge its own ST
	// terminator or open a new OSC.
	url = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, url)
	if url == "" {
		return text
	}
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
		// oscLink is the sanitizing sink for the URL: it strips every control/format
		// rune (incl. \n and \t) from the OSC-8 target, which is the real defense here
		// (with the hostile-URL case in TestTUIViewsStripControlBytesFromUntrustedText).
		// IssueWebURL stays in d7UntrustedFields as a tripwire for any DIRECT draw of it,
		// but the D7 AST guard does not gate THIS path — oscLink is not a recognized
		// writer, so the guard reads the field mention as plumbing either way.
		return oscLink(strOr(r.IssueWebURL, ""), styledIID)
	}
	return styledIID
}
