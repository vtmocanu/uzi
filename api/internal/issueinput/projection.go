package issueinput

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

const Placeholder = "[Issue content withheld]"
const NotEligible = "author_not_eligible"
const Unknown = "permission_unknown"
const SnapshotVersion = 2

// Comment contains only assessed content; withheld source bodies never enter it.
type Comment struct {
	AuthorUsername    string    `json:"author_username"`
	AuthorForgeUserID int64     `json:"author_forge_user_id"`
	CreatedAt         time.Time `json:"created_at"`
	Body              string    `json:"body"`
	Reason            string    `json:"reason,omitempty"`
}
type Thread struct {
	Version   int       `json:"version"`
	Comments  []Comment `json:"comments"`
	Truncated bool      `json:"truncated"`
	Withheld  bool      `json:"withheld"`
	Unknown   bool      `json:"unknown"`
}

// Capture binds the safe projection to the raw issue's identity and digest.
type Capture struct {
	ProjectID int64
	Issue     forge.Issue
	Digest    string
	Reason    string
	Thread    *Thread
	Withheld  bool
	Unknown   bool
}

func Reason(d forge.AuthorEligibility) string {
	switch d {
	case forge.AuthorEligible:
		return ""
	case forge.AuthorNotEligible:
		return NotEligible
	default:
		return Unknown
	}
}

// Digest length-prefixes both fields so even empty fields and embedded separators
// have an unambiguous representation.
func Digest(title, body string) string {
	h := sha256.New()
	for _, s := range []string{title, body} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

var ansi = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)

// AuthorName removes terminal controls, formatting controls (including bidi),
// and newlines before bounding display metadata to 200 runes.
func AuthorName(s string) string {
	s = ansi.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

// ProjectThread classifies the entire thread before applying the existing tail
// bounds. A failed author lookup does not block siblings; Assessment bounds the
// operation by its deadline and distinct-author budget.
func ProjectThread(a *Assessment, in []forge.IssueComment, botID int64) *Thread {
	out := &Thread{Version: SnapshotVersion, Comments: []Comment{}}
	if botID <= 0 {
		out.Unknown = true
		return out
	}
	for _, c := range in {
		if c.AuthorForgeUserID == botID {
			continue
		}
		d, _ := a.Author(forge.Issue{AuthorForgeUserID: c.AuthorForgeUserID, Author: c.AuthorUsername})
		reason := Reason(d)
		body := c.Body
		if reason != "" {
			body = Placeholder
		}
		out.Withheld = out.Withheld || reason == NotEligible
		out.Unknown = out.Unknown || reason == Unknown
		out.Comments = append(out.Comments, Comment{AuthorName(c.AuthorUsername), c.AuthorForgeUserID, c.CreatedAt, body, reason})
	}
	if len(out.Comments) > 200 {
		out.Comments = out.Comments[len(out.Comments)-200:]
		out.Truncated = true
	}
	total := 0
	for _, c := range out.Comments {
		total += len(c.Body)
	}
	if total > 32768 {
		out.Truncated = true
		start := len(out.Comments) - 1
		sum := len(out.Comments[start].Body)
		for i := start - 1; i >= 0; i-- {
			if sum+len(out.Comments[i].Body) > 32768 {
				break
			}
			sum += len(out.Comments[i].Body)
			start = i
		}
		out.Comments = out.Comments[start:]
		if len(out.Comments) == 1 && len(out.Comments[0].Body) > 32768 {
			body := out.Comments[0].Body[:32768]
			for !utf8.ValidString(body) {
				body = body[:len(body)-1]
			}
			out.Comments[0].Body = body
		}
	}
	return out
}

// Fetch begins the deadline before the raw fetch and closes the assessment after
// projecting comments. Valid raw capture survives child expiry, but parent
// cancellation never becomes a successful capture.
func Fetch(parent context.Context, f forge.Forge, project, iid, botID int64) (*Capture, error) {
	a := NewAssessment(parent, f, project)
	defer a.Close()
	raw, err := f.GetIssue(a.Context(), project, iid)
	if err != nil {
		return nil, err
	}
	if err := a.Context().Err(); err != nil {
		return nil, err
	}
	c := &Capture{ProjectID: project, Issue: raw, Digest: Digest(raw.Title, raw.Description)}
	d, _ := a.Author(raw)
	c.Reason = Reason(d)
	c.Issue.Author = AuthorName(raw.Author)
	comments, err := f.ListIssueComments(a.Context(), project, iid)
	if err != nil {
		c.Thread = &Thread{Version: SnapshotVersion, Comments: []Comment{}, Unknown: true}
	} else {
		c.Thread = ProjectThread(a, comments, botID)
	}
	c.Withheld = c.Reason == NotEligible || c.Thread.Withheld
	c.Unknown = c.Reason == Unknown || c.Thread.Unknown
	if err := parent.Err(); err != nil {
		return nil, err
	}
	return c, nil
}
func (c *Capture) Reasons() []string {
	out := []string{}
	if c.Withheld {
		out = append(out, NotEligible)
	}
	if c.Unknown {
		out = append(out, Unknown)
	}
	return out
}

type captureKey struct{}

func WithCapture(ctx context.Context, c *Capture) context.Context {
	return context.WithValue(ctx, captureKey{}, c)
}
func FromContext(ctx context.Context, project, iid int64) *Capture {
	c, _ := ctx.Value(captureKey{}).(*Capture)
	if c == nil || c.ProjectID != project || c.Issue.IID != iid {
		return nil
	}
	return c
}
