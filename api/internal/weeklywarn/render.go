package weeklywarn

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxRunLines caps the per-run lines of the DM; the total is always stated.
const maxRunLines = 5

// Payload is the notifications row's event-log body.
type Payload struct {
	CredentialKey  string     `json:"credential_key"`
	Harness        string     `json:"harness"`
	Pct            int        `json:"pct"`
	EffectiveLimit int        `json:"effective_limit"`
	ResetsAt       *time.Time `json:"resets_at,omitempty"`
	Runs           int        `json:"runs"`
}

// runLine is one affected run as the DM shows it.
type runLine struct {
	ID                 uuid.UUID
	Reason             string
	Command            string
	CommandUnavailable bool
}

// dm is everything the Slack render is built from.
type dm struct {
	Label    string
	Pct      int
	Limit    int
	ResetsAt *time.Time
	At       bool
	Total    int
	Runs     []runLine
}

// build collects the affected runs and renders the notification. A failure to read the runs or build
// their blocks leaves the lines out (the DM still names the credential): the claim has already
// established that a non-terminal run exists.
func (w *Warner) build(ctx context.Context, c *credential, near usagereserve.NearVerdict) notifysvc.Notification {
	d := dm{Label: c.label, Pct: near.Pct, Limit: near.EffectiveLimit, ResetsAt: near.ResetsAt, At: near.State == usagereserve.NearAt}
	if w.blocks != nil {
		if runs, err := w.runs(ctx, c.userID); err != nil {
			w.logger.Warn("weeklywarn: list runs for the warning", "error", err)
		} else {
			var open []store.Run
			for _, r := range runs {
				if !apitypes.IsTerminalRunStatus(r.Status) && r.Harness == c.harness {
					open = append(open, r)
				}
			}
			blocks := w.blocks.ForRuns(ctx, c.userID, open)
			for _, r := range open {
				dto := blocks[r.ID]
				if dto == nil || dto.CredentialKey != c.key {
					continue
				}
				if d.Total == 0 && dto.CredentialLabel != "" {
					d.Label = dto.CredentialLabel
				}
				d.Total++
				if len(d.Runs) < maxRunLines {
					d.Runs = append(d.Runs, lineOf(r.ID, dto))
				}
			}
		}
	}
	slack := renderSlack(d)
	c.fence(&slack)
	payload := Payload{CredentialKey: c.key, Harness: c.harness, Pct: near.Pct, EffectiveLimit: near.EffectiveLimit, ResetsAt: near.ResetsAt, Runs: d.Total}
	return notifysvc.Notification{UserID: c.userID, Kind: notifysvc.KindWeeklyLimitWarning, Payload: payload, Slack: &slack}
}

func lineOf(id uuid.UUID, dto *apitypes.WeeklyLimitDTO) runLine {
	l := runLine{ID: id, Reason: dto.Reason}
	for _, f := range dto.Fix {
		if f.Command != "" {
			l.Command = f.Command
			return l
		}
		if f.CommandUnavailable != "" {
			l.CommandUnavailable = true
		}
	}
	return l
}

// reasonText turns a NextClaimReason into the DM's wording; an unknown value shows as generic.
func reasonText(r string) string {
	switch r {
	case workersvc.NextClaimCanSwitch:
		return "can switch to another token"
	case workersvc.NextClaimSwitchPending:
		return "a switch is pending"
	case workersvc.NextClaimSelectionSame:
		return "auto-select keeps this token"
	case workersvc.NextClaimHeadroomOutside:
		return "another token has room but is not pooled"
	case workersvc.NextClaimPoolExhausted:
		return "every pooled token is exhausted"
	case workersvc.NextClaimNoAlternative:
		return "no other token to switch to"
	case workersvc.NextClaimAccountBound:
		return "bound to this Codex account"
	case workersvc.NextClaimNotAuto:
		return "not on auto-select"
	case workersvc.NextClaimPinned:
		return "pinned to this token"
	}
	return "will wait for the reset"
}

// ownerTextStrip removes what could turn owner-authored text into markup once the body is parsed as
// CommonMark: emphasis and code delimiters, link brackets and the backslash that escapes a delimiter.
// The label is shown without them, so it can never close the bold span it sits in, open another, form
// a link or swallow the closing delimiter.
var ownerTextStrip = strings.NewReplacer("*", "", "_", "", "~", "", "`", "", "[", "", "]", "", "\\", "")

// ownerLabel is an owner-authored credential label made safe for the body: terminal and layout
// control characters folded (termsafe.CellText), then the markup characters stripped. Slack's own
// &, < and > escaping is not applied here: the body reaches Slack through slacksvc.SlackMrkdwnBlock,
// which escapes every text node itself, so escaping here would double-escape ("R&D").
func ownerLabel(s string) string {
	return strings.TrimSpace(ownerTextStrip.Replace(termsafe.CellText(s)))
}

// resetFact is the window reset as a Slack date token. It is a Fact because Facts are trusted markup
// that no markdown walk touches; in the body the walker would escape the token into literal text.
func resetFact(t time.Time) string {
	return fmt.Sprintf("Resets <!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format("2006-01-02 15:04 MST"))
}

// renderSlack builds the render. The Body is untrusted CommonMark text (the notifier renders it with
// slacksvc.SlackMrkdwnBlock): bold is "**", and every owner-authored string is passed through
// unescaped except for the markup stripping above.
func renderSlack(d dm) notifysvc.SlackRender {
	title, verb := "WEEKLY LIMIT NEARLY REACHED", "is at"
	if d.At {
		title, verb = "WEEKLY LIMIT REACHED", "has reached"
	}
	label := "A credential"
	if l := ownerLabel(d.Label); l != "" {
		label = "**" + l + "**"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %d%% of its %d%% weekly limit.", label, verb, d.Pct, d.Limit)
	if d.Total > 0 {
		noun := "runs use"
		if d.Total == 1 {
			noun = "run uses"
		}
		fmt.Fprintf(&b, "\n%d %s this credential:", d.Total, noun)
		for _, r := range d.Runs {
			fmt.Fprintf(&b, "\n• `%s` %s", r.ID.String()[:8], reasonText(r.Reason))
			switch {
			case r.Command != "" && !strings.ContainsAny(r.Command, "`\n\r"):
				fmt.Fprintf(&b, ": `%s`", r.Command)
			case r.Command != "" || r.CommandUnavailable:
				b.WriteString(": rename the token in Settings to get a copyable command")
			}
		}
		if d.Total > len(d.Runs) {
			fmt.Fprintf(&b, "\n…and %d more", d.Total-len(d.Runs))
		}
	}
	r := notifysvc.SlackRender{Emoji: "⚠️", Title: title, Body: b.String()}
	if d.ResetsAt != nil {
		r.Facts = []string{resetFact(*d.ResetsAt)}
	}
	return r
}
