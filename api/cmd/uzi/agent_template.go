package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// agent_template.go is `uzi agent-template list|show|set|reset` (PRD #3264 M3, #3258): read and edit
// the Claude and Codex model and effort pins of your own agent templates. Builtin and global templates are edited
// in the web UI by an admin.

// agentTemplateHarnessFields maps --harness to the API field the pin is stored in.
var agentTemplateHarnessFields = map[string]string{"claude": "model", "codex": "codex_model"}

// agentTemplateEffortFields maps --harness to the API field the effort pin is stored in.
var agentTemplateEffortFields = map[string]string{"claude": "effort", "codex": "codex_effort"}

var agentTemplateColumns = []string{"NAME", "SCOPE", "CLAUDE PIN", "CODEX PIN", "CLAUDE EFFORT", "CODEX EFFORT", "EDITABLE", "ID"}

func newAgentTemplateCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent-template",
		Short: "List and edit the model and effort pins of your agent templates",
		Long: "Read and edit the Claude and Codex model and reasoning-effort pins of agent templates.\n\n" +
			"A pin is stored per model family: --harness claude edits the Claude pins and --harness codex the\n" +
			"Codex pins. A template with no pin inherits: it follows the model the run would otherwise use.\n" +
			"Only your own templates can be edited here; builtin and global templates are edited in the web UI\n" +
			"by an admin. A name that matches more than one template is refused: pass the id instead.",
	}
	cmd.AddCommand(newAgentTemplateListCmd(env, gf), newAgentTemplateShowCmd(env, gf),
		newAgentTemplateSetCmd(env, gf), newAgentTemplateResetCmd(env, gf))
	return cmd
}

func newAgentTemplateListCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List builtin, global and your own agent templates with their model and effort pins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			res, err := c.ListMyAgentTemplates(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rawOrMarshal(res.Raw, map[string][]uzicli.AgentTemplatePins{"templates": res.Templates}))
			}
			if len(res.Templates) == 0 {
				p.Println("No agent templates.")
				return nil
			}
			rows := make([][]string, 0, len(res.Templates))
			for _, t := range res.Templates {
				rows = append(rows, []string{t.Name, t.Scope, pinCell(t.Model), pinCell(t.CodexModel), pinCell(t.Effort), pinCell(t.CodexEffort), yesNo(t.Editable), t.ID})
			}
			return p.Table(agentTemplateColumns, rows)
		},
	}
}

func newAgentTemplateShowCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name|id>",
		Short: "Show one agent template's model and effort pins",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			res, err := c.ListMyAgentTemplates(cmd.Context())
			if err != nil {
				return err
			}
			idx, err := resolveAgentTemplate(res.Templates, args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				// show has no route of its own: --json prints {"template": <that list entry>}, the
				// same envelope shape set and reset print, with the entry exactly as the list sent it.
				var body struct {
					Templates []json.RawMessage `json:"templates"`
				}
				entry := any(res.Templates[idx])
				if json.Unmarshal(res.Raw, &body) == nil && len(body.Templates) == len(res.Templates) {
					entry = body.Templates[idx]
				}
				return p.JSON(map[string]any{"template": entry})
			}
			printAgentTemplate(p, res.Templates[idx])
			return nil
		},
	}
}

func newAgentTemplateSetCmd(env Env, gf *globalFlags) *cobra.Command {
	var harness, effort string
	cmd := &cobra.Command{
		Use:   "set <name|id> --harness claude|codex [<model>] [--effort <level>]",
		Short: "Pin the Claude or Codex model and reasoning effort of one of your agent templates",
		Long: "Pin the model, the reasoning effort, or both, that one of your own agent templates uses for one\n" +
			"model family. Give a model, --effort, or both.\n\n" +
			"  uzi agent-template set reviewer --harness claude <model-id>\n" +
			"  uzi agent-template set reviewer --harness codex <model-id>\n" +
			"  uzi agent-template set reviewer --harness codex --effort high\n\n" +
			"The effort is one of " + strings.Join(crossCheckEffortLevels, ", ") + ". The template is named by name or id; a name\n" +
			"that matches more than one template is refused and the ids are listed. Pins are stored per family\n" +
			"and the other family is left unchanged. A model id is passed through and the server validates it.\n" +
			"Builtin and global templates are edited in the web UI by an admin. Undo a model pin with\n" +
			"`uzi agent-template reset` and an effort pin with `uzi agent-template reset --effort`; the template then inherits again.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			field, err := agentTemplateField(harness)
			if err != nil {
				return err
			}
			patch := map[string]*string{}
			if len(args) == 2 {
				model, err := parseModelID(args[1])
				if err != nil {
					return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
				}
				patch[field] = model
			}
			if cmd.Flags().Changed("effort") {
				if !slices.Contains(crossCheckEffortLevels, effort) {
					return uzicli.Exitf(uzicli.ExitUsage, "invalid effort %q; use %s", cellText(effort), strings.Join(crossCheckEffortLevels, ", "))
				}
				patch[agentTemplateEffortFields[harness]] = &effort
			}
			if len(patch) == 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "agent-template set needs a model, --effort, or both")
			}
			return runAgentTemplateWrite(cmd, env, gf, args[0], patch)
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "model family the pins apply to: claude or codex")
	cmd.Flags().StringVar(&effort, "effort", "", "reasoning effort to pin: "+strings.Join(crossCheckEffortLevels, ", "))
	return cmd
}

func newAgentTemplateResetCmd(env Env, gf *globalFlags) *cobra.Command {
	var harness string
	var effort bool
	cmd := &cobra.Command{
		Use:   "reset <name|id> --harness claude|codex [--effort]",
		Short: "Clear the Claude or Codex model pin, or with --effort the effort pin, of one of your agent templates",
		Long: "Clear one pin of one of your own agent templates so that family inherits again. Without --effort\n" +
			"the model pin is cleared; with --effort only the effort pin is cleared and the model pin is kept.\n" +
			"Takes the same name or id and --harness as `uzi agent-template set`. Builtin and global templates\n" +
			"are edited in the web UI by an admin.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			field, err := agentTemplateField(harness)
			if err != nil {
				return err
			}
			if effort {
				field = agentTemplateEffortFields[harness]
			}
			return runAgentTemplateWrite(cmd, env, gf, args[0], map[string]*string{field: nil})
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "model family whose pin is cleared: claude or codex")
	cmd.Flags().BoolVar(&effort, "effort", false, "clear the effort pin instead of the model pin")
	return cmd
}

func agentTemplateField(harness string) (string, error) {
	field, ok := agentTemplateHarnessFields[harness]
	if !ok {
		return "", uzicli.Exitf(uzicli.ExitUsage, "agent-template needs --harness claude or --harness codex")
	}
	return field, nil
}

// runAgentTemplateWrite resolves the template through the list, sends the patch (a nil pin is a
// clear) and prints the template the server answered. Arguments are validated by the
// caller, so a usage error never reaches the list.
func runAgentTemplateWrite(cmd *cobra.Command, env Env, gf *globalFlags, ref string, patch map[string]*string) error {
	c, err := env.client(gf)
	if err != nil {
		return err
	}
	list, err := c.ListMyAgentTemplates(cmd.Context())
	if err != nil {
		return err
	}
	idx, err := resolveAgentTemplate(list.Templates, ref)
	if err != nil {
		return err
	}
	res, err := c.PutAgentTemplateModelPins(cmd.Context(), list.Templates[idx].ID, patch)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(rawOrMarshal(res.Raw, map[string]uzicli.AgentTemplatePins{"template": res.Template}))
	}
	printAgentTemplate(p, res.Template)
	return nil
}

// resolveAgentTemplate finds the template ref names: an exact id match wins, else an exact name
// match. More than one name match is a usage error listing the ids; none is a not-found error.
func resolveAgentTemplate(ts []uzicli.AgentTemplatePins, ref string) (int, error) {
	for i, t := range ts {
		if strings.EqualFold(t.ID, ref) {
			return i, nil
		}
	}
	var matches []int
	for i, t := range ts {
		if t.Name == ref {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return 0, uzicli.Exitf(uzicli.ExitNotFound, "template not found: %s", cellText(ref))
	case 1:
		return matches[0], nil
	}
	lines := make([]string, 0, len(matches))
	for _, i := range matches {
		lines = append(lines, fmt.Sprintf("  %s (%s)", cellText(ts[i].ID), cellText(ts[i].Scope)))
	}
	return 0, uzicli.Exitf(uzicli.ExitUsage, "%d templates are named %s; pass the id of one:\n%s",
		len(matches), cellText(ref), strings.Join(lines, "\n"))
}

// printAgentTemplate prints one template's stored model and effort pins and whether each family inherits. It states
// only what is stored: never a resolved model or where one would come from.
func printAgentTemplate(p *uzicli.Printer, t uzicli.AgentTemplatePins) {
	p.Println("Template: " + cellText(t.Name))
	p.Println("ID: " + cellText(t.ID))
	p.Println("Scope: " + cellText(t.Scope))
	p.Println("Claude pin: " + pinLine(t.Model))
	p.Println("Codex pin: " + pinLine(t.CodexModel))
	p.Println("Claude effort: " + pinLine(t.Effort))
	p.Println("Codex effort: " + pinLine(t.CodexEffort))
	p.Println("Editable: " + yesNo(t.Editable))
}

func pinCell(pin *string) string {
	if pin == nil {
		return "inherit"
	}
	return *pin
}

func pinLine(pin *string) string {
	if pin == nil {
		return "inherit (no pin stored)"
	}
	return cellText(*pin) + " (pinned)"
}

// rawOrMarshal returns the server's envelope bytes when the client kept them, else the re-encoded
// decoded value.
func rawOrMarshal(raw json.RawMessage, decoded any) any {
	if len(raw) > 0 {
		return raw
	}
	return decoded
}
