package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const planCompareLimit = 200 * 1024

type runPlanOutput struct {
	Version                int     `json:"version"`
	BaseVersion            *int    `json:"base_version"`
	PlanMD                 string  `json:"plan_md"`
	Diff                   *string `json:"diff"`
	IdenticalAfterFeedback bool    `json:"identical_after_feedback"`
}

func newRunPlanCmd(env Env, gf *globalFlags) *cobra.Command {
	var version int
	var diff bool
	cmd := &cobra.Command{
		Use:   "plan <run-id>",
		Short: "Print a plan version or compare a revision with its feedback base",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			feed, err := c.RunLogs(cmd.Context(), args[0], 0)
			if err != nil {
				return err
			}
			msgs := append([]apitypes.MessageDTO(nil), feed...)
			sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Seq < msgs[j].Seq })
			var plans []int
			for i, m := range msgs {
				if m.Kind == "plan" {
					plans = append(plans, i)
				}
			}
			if len(plans) == 0 {
				return uzicli.Exitf(uzicli.ExitNotFound, "run has no plan")
			}
			n := version
			if !cmd.Flags().Changed("version") {
				n = len(plans)
			}
			if n < 1 || n > len(plans) {
				return uzicli.Exitf(uzicli.ExitUsage, "run has %d plan version(s)", len(plans))
			}
			idx := plans[n-1]
			target, err := planMarkdown(msgs[idx])
			if err != nil {
				return err
			}
			result := runPlanOutput{Version: n, PlanMD: target}
			var base string
			if b := planBase(msgs, idx); b >= 0 {
				base, err = planMarkdown(msgs[b])
				if err != nil {
					return err
				}
				bv := n - 1
				result.BaseVersion = &bv
				result.IdenticalAfterFeedback = strings.TrimRight(base, "\r\n") == strings.TrimRight(target, "\r\n")
			}
			if diff && (len(target) > planCompareLimit || len(base) > planCompareLimit) {
				return uzicli.Exitf(uzicli.ExitUsage, "plan too large to compare (limit 200 KiB)")
			}
			if diff {
				if result.BaseVersion == nil {
					if _, err := io.WriteString(env.Stderr, "no revision base (no feedback preceded this version)\n"); err != nil {
						return err
					}
				} else {
					d := planUnifiedDiff(base, target, *result.BaseVersion, n)
					result.Diff = &d
					if result.IdenticalAfterFeedback {
						if _, err := fmt.Fprintf(env.Stderr, "This revision is identical to v%d. Your requested changes were not applied.\n", *result.BaseVersion); err != nil {
							return err
						}
					}
				}
			}
			if gf.json {
				return json.NewEncoder(env.Stdout).Encode(result)
			}
			if result.Diff != nil {
				_, err = io.WriteString(env.Stdout, *result.Diff)
			} else {
				_, err = io.WriteString(env.Stdout, target)
			}
			return err
		},
	}
	cmd.Flags().IntVar(&version, "version", 0, "Plan version (default: latest)")
	cmd.Flags().BoolVar(&diff, "diff", false, "Compare with the immediately preceding plan when feedback intervened")
	return cmd
}

// planBase takes an index into the entire seq-ordered feed, not a plan ordinal.
// Only the nearest preceding plan qualifies, and only with feedback strictly
// between its seq and the target's seq. Later feedback cannot revise this base.
func planBase(msgs []apitypes.MessageDTO, idx int) int {
	if idx < 0 || idx >= len(msgs) || msgs[idx].Kind != "plan" {
		return -1
	}
	for q := idx - 1; q >= 0; q-- {
		if msgs[q].Kind != "plan" {
			continue
		}
		for f := q + 1; f < idx; f++ {
			if msgs[f].Kind == "plan_feedback" && msgs[q].Seq < msgs[f].Seq && msgs[f].Seq < msgs[idx].Seq {
				return q
			}
		}
		return -1
	}
	return -1
}

func planMarkdown(msg apitypes.MessageDTO) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return "", fmt.Errorf("invalid plan payload at seq %d: %w", msg.Seq, err)
	}
	raw := payload["plan_md"]
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "", fmt.Errorf("invalid plan payload at seq %d: plan_md must be a string", msg.Seq)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", fmt.Errorf("invalid plan payload at seq %d: plan_md must be a string: %w", msg.Seq, err)
	}
	return text, nil
}

func planDiffText(text string) string {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return ""
	}
	return text + "\n"
}

type planDiffLine struct {
	prefix byte
	text   string
}

// planUnifiedDiff keeps decoded line strings intact, including multibyte text
// and interior CR bytes. Only trailing CR/LF bytes are normalized.
func planUnifiedDiff(base, target string, bv, version int) string {
	dmp := diffmatchpatch.New()
	dmp.DiffTimeout = 0
	c1, c2, lines := dmp.DiffLinesToChars(planDiffText(base), planDiffText(target))
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(c1, c2, false), lines)
	var rows []planDiffLine
	for _, d := range diffs {
		prefix := byte(' ')
		switch d.Type {
		case diffmatchpatch.DiffDelete:
			prefix = '-'
		case diffmatchpatch.DiffInsert:
			prefix = '+'
		}
		for _, line := range strings.SplitAfter(d.Text, "\n") {
			if line != "" {
				rows = append(rows, planDiffLine{prefix, line})
			}
		}
	}
	type window struct{ start, end int }
	var windows []window
	for i := 0; i < len(rows); {
		if rows[i].prefix == ' ' {
			i++
			continue
		}
		start := i
		for i < len(rows) && rows[i].prefix != ' ' {
			i++
		}
		w := window{max(0, start-3), min(len(rows), i+3)}
		if len(windows) > 0 && w.start <= windows[len(windows)-1].end {
			windows[len(windows)-1].end = w.end
		} else {
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return ""
	}
	old, next := make([]int, len(rows)+1), make([]int, len(rows)+1)
	for i, r := range rows {
		old[i+1], next[i+1] = old[i], next[i]
		if r.prefix != '+' {
			old[i+1]++
		}
		if r.prefix != '-' {
			next[i+1]++
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- v%d\n+++ v%d\n", bv, version)
	for _, w := range windows {
		a, b := old[w.start]+1, old[w.end]-old[w.start]
		c, d := next[w.start]+1, next[w.end]-next[w.start]
		if b == 0 {
			a--
		}
		if d == 0 {
			c--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", a, b, c, d)
		for _, r := range rows[w.start:w.end] {
			out.WriteByte(r.prefix)
			out.WriteString(r.text)
		}
	}
	return out.String()
}
