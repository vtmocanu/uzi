package main

import (
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// reviewBotsSettingKey is the instance setting holding the trusted review-bot allowlist
// (issue #2347). It mirrors settings.KeyMrReviewTrustedBots; the CLI does not import the
// server settings package.
const reviewBotsSettingKey = "mr_review_trusted_bots"

// reviewBotEntry is one allowlist token as displayed: a well-formed
// `<base_url>#<forge_user_id>` pair, or the raw token flagged Malformed.
type reviewBotEntry struct {
	BaseURL     string `json:"base_url,omitempty"`
	ForgeUserID string `json:"forge_user_id,omitempty"`
	Raw         string `json:"raw,omitempty"`
	Malformed   bool   `json:"malformed"`
}

// parseReviewBotsForDisplay is deliberately lenient: it never rejects, it only flags.
// The server's write-time validator is the real gate.
func parseReviewBotsForDisplay(value string) []reviewBotEntry {
	out := []reviewBotEntry{}
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		i := strings.LastIndex(tok, "#")
		if i <= 0 {
			out = append(out, reviewBotEntry{Raw: tok, Malformed: true})
			continue
		}
		base, id := strings.TrimSpace(tok[:i]), strings.TrimSpace(tok[i+1:])
		n, err := strconv.ParseInt(id, 10, 64)
		u, uerr := url.Parse(base)
		if err != nil || n <= 0 || uerr != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			out = append(out, reviewBotEntry{Raw: tok, Malformed: true})
			continue
		}
		out = append(out, reviewBotEntry{BaseURL: base, ForgeUserID: id})
	}
	return out
}

// newAdminReviewBotsCmd — `uzi admin review-bots`. Read-only view of the trusted
// review-bot allowlist; the write endpoint is cookie-only.
func newAdminReviewBotsCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "review-bots",
		Short: "List the trusted review bots whose MR comments the rework lane ingests",
		Long: "List the instance setting " + reviewBotsSettingKey + ": review bots (a forge base URL " +
			"plus the bot's forge user id) whose merge-request review comments the mr_rework lane " +
			"may ingest even though the bot is not a repo collaborator. Comments from any other " +
			"non-collaborator are withheld. This command is read-only: edit the list in the web " +
			"Admin Settings page or through the admin settings API (cookie-authenticated).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			view, err := c.AdminSettings(cmd.Context())
			if err != nil {
				return err
			}
			entries := parseReviewBotsForDisplay(view.Settings[reviewBotsSettingKey])
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(struct {
					Source  string           `json:"source,omitempty"`
					Entries []reviewBotEntry `json:"entries"`
				}{view.Sources[reviewBotsSettingKey], entries})
			}
			if len(entries) == 0 {
				p.Printf("no trusted review bots: third-party review bots' comments are withheld\n")
				return nil
			}
			sort.SliceStable(entries, func(i, j int) bool { return !entries[i].Malformed && entries[j].Malformed })
			rows := make([][]string, 0, len(entries))
			for _, e := range entries {
				if e.Malformed {
					rows = append(rows, []string{cellText(e.Raw), "-", "malformed"})
					continue
				}
				rows = append(rows, []string{cellText(e.BaseURL), e.ForgeUserID, "ok"})
			}
			return p.Table([]string{"BASE URL", "FORGE USER ID", "STATUS"}, rows)
		},
	}
}
