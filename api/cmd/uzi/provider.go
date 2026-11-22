package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newProviderCmd — `uzi provider` manages the caller's model providers (PRD #4320): a named
// Anthropic-compatible gateway the Claude engine can be pointed at.
//
// The CLI covers list / create / edit / test / retire / use, which are RequireUser routes. The KEY
// (add, replace, remove, enable/disable), a destination change (base URL, auth style) and
// deleting a provider are cookie-only web actions on purpose: they mint or redirect a credential,
// which a stolen CLI token must not be able to do (the same reasoning as `uzi token`). So there is
// no key verb here and no flag that would accept a key.
func newProviderCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "List, create, edit, test, retire and select your model providers",
		Long: "Manage model providers: named Anthropic-compatible gateways that Claude can be pointed at.\n\n" +
			"The base URL must be one your operator allowed (UZI_PROVIDER_ALLOWED_BASE_URLS); ask\n" +
			"your operator which ones. Adding or replacing the provider's API key, changing its\n" +
			"base URL or auth style, and deleting it are web-only (Settings), because they handle\n" +
			"a credential or redirect where it is sent and must not be reachable from a CLI token.\n" +
			"A provider without a key cannot be used until you add one in the web Settings.",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List your model providers (never keys)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			out, err := c.ListProviders(cmd.Context())
			if err != nil {
				return providerErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				if out.Providers == nil {
					out.Providers = []apitypes.ProviderDTO{}
				}
				if out.AllowedBaseURLs == nil {
					out.AllowedBaseURLs = []string{}
				}
				return p.JSON(out)
			}
			if !out.Enabled {
				if !gf.quiet {
					p.Printf("Model providers are not enabled on this server. Ask your operator to set UZI_PROVIDER_ALLOWED_BASE_URLS.\n")
				}
				return nil
			}
			if len(out.Providers) == 0 {
				if !gf.quiet {
					p.Printf("No model providers yet. Create one with `uzi provider create`.\n")
					if len(out.AllowedBaseURLs) > 0 {
						p.Printf("Base URLs your operator allows:\n")
						for _, u := range out.AllowedBaseURLs {
							p.Printf("  %s\n", cellText(u))
						}
					}
				}
				return nil
			}
			rows := make([][]string, 0, len(out.Providers))
			for _, d := range out.Providers {
				rows = append(rows, []string{
					cellText(d.Name), cellText(d.BaseURL), cellText(d.AuthStyle),
					modelsCell(d.ModelMap), providerKeyCell(d), providerTestCell(d.Probe), boolStr(d.Retired),
				})
			}
			return p.Table([]string{"NAME", "URL", "AUTH", "MODELS", "KEY", "TEST", "RETIRED"}, rows)
		},
	}

	var cName, cBaseURL, cAuthStyle string
	var cModels []string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a model provider (add its key in the web Settings afterwards)",
		Long: "Create a model provider. --base-url must be one of the URLs your operator allowed, exactly\n" +
			"as listed (path included). --model maps a Claude alias to a gateway model and is repeatable:\n" +
			"alias=model[:context_window], with alias one of haiku, sonnet, opus, fable. haiku is required.\n" +
			"Set the context window to the gateway model's real window. The smallest window set on any\n" +
			"model applies to all of them; with none set, uzi assumes 200000.\n" +
			"Caveat: the Claude CLI may apply its own window to model names it recognizes as Anthropic\n" +
			"models (for example claude-sonnet-4-5), ignoring both a configured window and uzi's 200000\n" +
			"default (measured at Claude Code 2.1.325).\n\n" +
			"The API key is added in the web Settings, never from the CLI.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(cName) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--name is required")
			}
			if strings.TrimSpace(cBaseURL) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--base-url is required; `uzi provider list --json` shows the URLs your operator allows")
			}
			if cAuthStyle != "bearer" && cAuthStyle != "x_api_key" {
				return uzicli.Exitf(uzicli.ExitUsage, "invalid --auth-style %q; use bearer or x_api_key", cAuthStyle)
			}
			mm, err := parseModelFlags(cModels)
			if err != nil {
				return err
			}
			if mm.Haiku.Model == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--model haiku=<model> is required (Claude's background calls use the haiku slot)")
			}
			warnClaudeWindow(env, mm)
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			d, err := c.CreateProvider(cmd.Context(), apitypes.ProviderCreateRequest{
				Name: strings.TrimSpace(cName), Engine: "claude", Protocol: "anthropic_messages",
				BaseURL: strings.TrimSpace(cBaseURL), AuthStyle: cAuthStyle, ModelMap: mm,
			})
			if err != nil {
				return providerErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(d)
			}
			if !gf.quiet {
				printProvider(p, d)
				p.Printf("\nNext: add the provider's API key in the web Settings (keys are web-only), then run `uzi provider test %s`.\n", cellText(d.Name))
			}
			return nil
		},
	}
	create.Flags().StringVar(&cName, "name", "", "provider name (unique among your providers, case-insensitively)")
	create.Flags().StringVar(&cBaseURL, "base-url", "", "gateway base URL, exactly as your operator allowed it")
	create.Flags().StringVar(&cAuthStyle, "auth-style", "bearer", "how the key is sent: bearer or x_api_key")
	create.Flags().StringArrayVar(&cModels, "model", nil, "alias=model[:context_window], repeatable; haiku is required (digits after the last colon are the window; a name ending in :0 or containing other colons stays whole)")

	var eName, eBaseURL, eAuthStyle string
	var eModels []string
	edit := &cobra.Command{
		Use:   "edit <name|id>",
		Short: "Rename a provider and/or replace its model map",
		Long: "Rename a provider (--name) and/or replace its model map (--model, repeatable, same form as\n" +
			"`create`). Giving any --model replaces the whole map, so repeat every alias you want to keep,\n" +
			"context windows included. The smallest window set on any model applies to all of them; with\n" +
			"none set, uzi assumes 200000. The Claude CLI may\n" +
			"apply its own window to model names it recognizes as Anthropic models (for example\n" +
			"claude-sonnet-4-5), ignoring both a configured window and uzi's 200000 default (measured\n" +
			"at Claude Code 2.1.325).\n\n" +
			"Changing the base URL or auth style is web-only (Settings).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if eBaseURL != "" || eAuthStyle != "" {
				return uzicli.Exitf(uzicli.ExitUsage, "changing the base URL or auth style is web-only: use Settings in the web UI")
			}
			if eName == "" && len(eModels) == 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "pass --name and/or --model")
			}
			req := apitypes.ProviderPatchRequest{}
			if eName != "" {
				n := strings.TrimSpace(eName)
				req.Name = &n
			}
			if len(eModels) > 0 {
				mm, err := parseModelFlags(eModels)
				if err != nil {
					return err
				}
				if mm.Haiku.Model == "" {
					return uzicli.Exitf(uzicli.ExitUsage, "--model haiku=<model> is required: a new model map replaces the old one")
				}
				warnClaudeWindow(env, mm)
				req.ModelMap = &mm
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			target, err := resolveProvider(cmd, c, args[0])
			if err != nil {
				return err
			}
			d, err := c.EditProvider(cmd.Context(), target.ID, req)
			if err != nil {
				return providerErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(d)
			}
			if !gf.quiet {
				printProvider(p, d)
			}
			return nil
		},
	}
	edit.Flags().StringVar(&eName, "name", "", "new provider name")
	edit.Flags().StringArrayVar(&eModels, "model", nil, "alias=model[:context_window], repeatable; replaces the whole model map (digits after the last colon are the window)")
	// Offered only to explain: a destination change is a web-only action.
	edit.Flags().StringVar(&eBaseURL, "base-url", "", "web-only: change the base URL in Settings")
	edit.Flags().StringVar(&eAuthStyle, "auth-style", "", "web-only: change the auth style in Settings")
	_ = edit.Flags().MarkHidden("base-url")
	_ = edit.Flags().MarkHidden("auth-style")

	test := &cobra.Command{
		Use:   "test <name|id>",
		Short: "Probe a provider's gateway (auth and tool calls); neither result certifies compatibility",
		Long: "Send a small probe to the provider's gateway from the api and print two separate results:\n" +
			"auth (ok, rejected, inconclusive) and tool calls (passed, not_supported, inconclusive, not_run).\n" +
			"Neither result certifies that the gateway is fully compatible with Claude. The provider needs its\n" +
			"key (added in the web Settings) to be tested. Tests are rate limited per user.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			target, err := resolveProvider(cmd, c, args[0])
			if err != nil {
				return err
			}
			probe, err := c.TestProvider(cmd.Context(), target.ID)
			if err != nil {
				return providerErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(struct {
					ID    string                     `json:"id"`
					Name  string                     `json:"name"`
					Probe *apitypes.ProviderProbeDTO `json:"probe"`
				}{target.ID, target.Name, probe})
			}
			if !gf.quiet {
				p.Printf("provider %q\n", cellText(target.Name))
				if probe == nil {
					p.Printf("no probe result was returned\n")
				} else {
					p.Printf("auth:  %s\n", cellText(probe.AuthResult))
					p.Printf("tools: %s\n", dashOr(cellText(probe.ToolResult)))
					if hint := probeHint(probe); hint != "" {
						p.Printf("%s\n", hint)
					}
				}
				p.Printf("Neither result certifies full compatibility with Claude.\n")
			}
			return nil
		},
	}

	retire := &cobra.Command{
		Use:   "retire <name|id>",
		Short: "Retire a provider so it can no longer be used",
		Long: "Retire a provider. A retired provider stays in your list (and counts toward the 20-provider\n" +
			"limit) and cannot be edited, tested or selected. Deleting one is web-only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			target, err := resolveProvider(cmd, c, args[0])
			if err != nil {
				return err
			}
			d, err := c.RetireProvider(cmd.Context(), target.ID)
			if err != nil {
				return providerErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(d)
			}
			if !gf.quiet {
				p.Printf("Retired provider %q\n", cellText(d.Name))
			}
			return nil
		},
	}

	var useNone bool
	use := &cobra.Command{
		Use:   "use <name|id> | --none",
		Short: "Run your Claude runs on a model provider, or back on Anthropic with --none",
		Long: "Select the model provider your Claude runs use, by name or id, or clear the selection with\n" +
			"--none so Claude runs use Anthropic directly again. A provider must not be retired, must have\n" +
			"an enabled key (added in the web Settings) and must have a base URL your operator still allows.\n" +
			"The selection applies to runs you start afterwards; a run already started keeps what it was\n" +
			"bound to. Codex runs are unaffected.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if useNone == (len(args) == 1) {
				return uzicli.Exitf(uzicli.ExitUsage, "pass a provider name or id, or --none (not both)")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			var want *string
			var name string
			if !useNone {
				target, err := resolveProvider(cmd, c, args[0])
				if err != nil {
					return err
				}
				if err := providerUsableRefusal(target); err != nil {
					return err
				}
				want, name = &target.ID, target.Name
			}
			got, err := c.SetClaudeProviderSource(cmd.Context(), want)
			if err != nil {
				return providerUseErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(struct {
					ClaudeProviderID *string `json:"claude_provider_id"`
					Name             *string `json:"name"`
				}{got, nilIfEmpty(name)})
			}
			if !gf.quiet {
				if got == nil {
					p.Printf("Claude runs now use Anthropic directly (no model provider selected).\n")
				} else {
					p.Printf("Claude runs you start now use provider %q.\n", cellText(name))
				}
			}
			return nil
		},
	}
	use.Flags().BoolVar(&useNone, "none", false, "clear the selection: Claude runs use Anthropic directly")

	cmd.AddCommand(list, create, edit, test, retire, use)
	return cmd
}

// resolveProvider lists the caller's providers and picks the one an argument names: an exact id,
// else a name matched case-insensitively (names are unique per owner case-insensitively). A
// feature-off server and an unknown name are distinct, readable errors raised before any write.
func resolveProvider(cmd *cobra.Command, c uzicli.Client, arg string) (apitypes.ProviderDTO, error) {
	arg = strings.TrimSpace(arg)
	out, err := c.ListProviders(cmd.Context())
	if err != nil {
		return apitypes.ProviderDTO{}, providerErr(err)
	}
	if !out.Enabled {
		return apitypes.ProviderDTO{}, uzicli.Exitf(uzicli.ExitConflict,
			"model providers are not enabled on this server; ask your operator to set UZI_PROVIDER_ALLOWED_BASE_URLS")
	}
	for _, d := range out.Providers {
		if d.ID == arg {
			return d, nil
		}
	}
	for _, d := range out.Providers {
		if strings.EqualFold(d.Name, arg) {
			return d, nil
		}
	}
	return apitypes.ProviderDTO{}, uzicli.Exitf(uzicli.ExitNotFound,
		"no provider named or with id %q; `uzi provider list` shows yours", cellText(arg))
}

// providerUsableRefusal refuses, before any write, a provider the list already shows cannot be the
// Claude source, with the one step that fixes it. The server applies the same rules (and the
// operator allowlist, which the list does not show), so this only saves a round trip and names
// the fix; it is never the authority.
func providerUsableRefusal(d apitypes.ProviderDTO) error {
	switch {
	case d.Retired:
		return uzicli.Exitf(uzicli.ExitConflict, "provider %q is retired and cannot be used; pick another with `uzi provider list`", cellText(d.Name))
	case !d.HasKey:
		return uzicli.Exitf(uzicli.ExitConflict, "provider %q has no key; add its API key in the web Settings (keys are web-only), then run `uzi provider use %s`", cellText(d.Name), cellText(d.ID))
	case !d.KeyEnabled:
		return uzicli.Exitf(uzicli.ExitConflict, "provider %q has its key disabled; enable it in the web Settings, then run `uzi provider use %s`", cellText(d.Name), cellText(d.ID))
	}
	return nil
}

// providerUseErr maps the server's typed refusal of the source selection onto a message that
// names the fix. provider_source_unusable already carries the cause (retired, no key, key
// disabled, base URL not on the operator allowlist, provider gone); the next step is appended.
func providerUseErr(err error) error {
	var ee *uzicli.ExitError
	if errors.As(err, &ee) && ee.Reason == "provider_source_unusable" {
		return providerErr(&uzicli.ExitError{Code: ee.Code, Reason: ee.Reason, RetryAfter: ee.RetryAfter,
			Err: fmt.Errorf("%s; pick another provider or fix this one in the web Settings", strings.TrimSuffix(ee.Error(), "."))})
	}
	return providerErr(err)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// providerErr appends the server's machine-readable reason to an error that carries one, so a
// refusal such as provider_base_url_not_allowed reads the same on a terminal as in a script.
func providerErr(err error) error {
	var ee *uzicli.ExitError
	if !errors.As(err, &ee) || ee.Reason == "" || strings.Contains(ee.Error(), ee.Reason) {
		return err
	}
	out := &uzicli.ExitError{Code: ee.Code, Err: fmt.Errorf("%s (reason: %s)", ee.Error(), ee.Reason),
		RetryAfter: ee.RetryAfter, Reason: ee.Reason}
	return out
}

// parseModelFlags turns repeated alias=model[:context_window] values into a model map. The
// context window is the part after the LAST colon, taken only when it is all digits, because a
// gateway model name may itself contain colons (for example a Bedrock id ending in ":0", which
// is kept whole; an id that ends in a non-zero ":<digits>" needs an explicit window too).
func parseModelFlags(vals []string) (apitypes.ModelMapDTO, error) {
	var mm apitypes.ModelMapDTO
	seen := map[string]bool{}
	for _, v := range vals {
		alias, rest, ok := strings.Cut(v, "=")
		alias = strings.ToLower(strings.TrimSpace(alias))
		rest = strings.TrimSpace(rest)
		if !ok || alias == "" || rest == "" {
			return mm, uzicli.Exitf(uzicli.ExitUsage, "invalid --model %q; use alias=model[:context_window], e.g. haiku=corp-haiku:200000", cellText(v))
		}
		entry := apitypes.ModelEntryDTO{Model: rest}
		if i := strings.LastIndex(rest, ":"); i > 0 && i < len(rest)-1 {
			// A zero suffix is never a window: it is the tail of an id such as "...-v1:0".
			if suffix := rest[i+1:]; isAllDigits(suffix) {
				n, err := strconv.Atoi(suffix)
				if err != nil {
					return mm, uzicli.Exitf(uzicli.ExitUsage, "invalid --model %q; the context window after the last colon is too large", cellText(v))
				}
				if n > 0 {
					entry = apitypes.ModelEntryDTO{Model: rest[:i], ContextWindow: &n}
				}
			}
		}
		if seen[alias] {
			return mm, uzicli.Exitf(uzicli.ExitUsage, "--model %s given more than once", cellText(alias))
		}
		seen[alias] = true
		switch alias {
		case "haiku":
			mm.Haiku = entry
		case "sonnet":
			mm.Sonnet = &entry
		case "opus":
			mm.Opus = &entry
		case "fable":
			mm.Fable = &entry
		default:
			return mm, uzicli.Exitf(uzicli.ExitUsage, "unknown model alias %q; use haiku, sonnet, opus or fable", cellText(alias))
		}
	}
	return mm, nil
}

// warnClaudeWindow prints a one-line plain-text stderr warning per mapped model name that contains
// "claude" (any case), whether or not a context window is set: the Claude CLI may apply its own
// window to names it recognizes as Anthropic models, ignoring both a configured window and uzi's
// 200000 default (measured at Claude Code 2.1.325). Only the fixed alias is echoed, never the
// untrusted model name.
func warnClaudeWindow(env Env, mm apitypes.ModelMapDTO) {
	h := mm.Haiku
	for _, e := range []struct {
		alias string
		m     *apitypes.ModelEntryDTO
	}{{"haiku", &h}, {"sonnet", mm.Sonnet}, {"opus", mm.Opus}, {"fable", mm.Fable}} {
		if e.m == nil || !strings.Contains(strings.ToLower(e.m.Model), "claude") {
			continue
		}
		_, _ = fmt.Fprintf(env.Stderr, "warning: --model %s: the Claude CLI may ignore the context window (configured or uzi's 200000 default) for model names it recognizes as Anthropic models, such as claude-sonnet-4-5 (measured at Claude Code 2.1.325)\n", e.alias)
	}
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// modelsCell renders the model map for the MODELS column: alias=model[:window] in the order
// haiku, sonnet, opus, fable, skipping unmapped aliases.
func modelsCell(m apitypes.ModelMapDTO) string {
	var parts []string
	add := func(alias string, e *apitypes.ModelEntryDTO) {
		if e == nil || e.Model == "" {
			return
		}
		s := alias + "=" + cellText(e.Model)
		if e.ContextWindow != nil {
			s += ":" + strconv.Itoa(*e.ContextWindow)
		}
		parts = append(parts, s)
	}
	h := m.Haiku
	add("haiku", &h)
	add("sonnet", m.Sonnet)
	add("opus", m.Opus)
	add("fable", m.Fable)
	return strings.Join(parts, " ")
}

// providerKeyCell renders KEY: "no" without a key, "disabled" with a key switched off, else "yes".
func providerKeyCell(d apitypes.ProviderDTO) string {
	switch {
	case !d.HasKey:
		return "no"
	case !d.KeyEnabled:
		return "disabled"
	default:
		return "yes"
	}
}

// providerTestCell renders TEST as "<auth>/<tool>", or "-" when the provider was never tested.
func providerTestCell(p *apitypes.ProviderProbeDTO) string {
	if p == nil {
		return "-"
	}
	return cellText(p.AuthResult) + "/" + dashOr(cellText(p.ToolResult))
}

// probeHint explains the two outcomes a user can act on; the result strings themselves are
// printed verbatim above it.
func probeHint(p *apitypes.ProviderProbeDTO) string {
	switch p.AuthResult {
	case "rejected":
		return "The gateway rejected the key: check the key and the auth style in the web Settings."
	case "inconclusive":
		return "The auth check was inconclusive: the gateway could not be judged (network error, timeout or unexpected reply)."
	}
	return ""
}

func printProvider(p *uzicli.Printer, d apitypes.ProviderDTO) {
	p.Printf("Provider %q (%s)\n", cellText(d.Name), cellText(d.ID))
	p.Printf("  url:    %s\n", cellText(d.BaseURL))
	p.Printf("  auth:   %s\n", cellText(d.AuthStyle))
	p.Printf("  models: %s\n", dashOr(modelsCell(d.ModelMap)))
	p.Printf("  key:    %s\n", providerKeyCell(d))
}
