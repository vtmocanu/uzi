package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// version is stamped at build time via -ldflags "-X main.version=vX.Y.Z"
// (the brew formula does this). It equals the uzi v* tag the binary was built
// from, making `uzi version` == the API version it was compiled against.
var version = "dev"

// Env holds the injectable dependencies of the CLI: where output goes, how the
// API client is built, and the config store. Tests build an Env with a fake
// client and an in-memory store; DefaultEnv wires the real ones.
type Env struct {
	Stdout    io.Writer
	Stderr    io.Writer
	Stdin     io.Reader
	StdoutTTY bool
	// StdinTTY reports whether stdin is a terminal. `uzi auth token` uses it to
	// decide between prompting (TTY) and reading a piped token (non-TTY); the
	// credential is never taken from argv (PRD #64).
	StdinTTY bool

	// NewClient builds the API client from resolved settings. M3 wires the
	// real-client stub; tests inject a fake; M7 replaces the default with the
	// live HTTP client.
	NewClient func(uzicli.Settings) uzicli.Client

	// Store reads config/credentials. May be nil (e.g. no home dir), in which
	// case only env/flags supply settings.
	Store *uzicli.Store

	// SkillHome is the base dir the bundled skill installs under
	// (SkillHome/.claude/skills/uzi-cli/). Empty means os.UserHomeDir(). This is a
	// TEST/INJECTION SEAM only — DefaultEnv leaves it empty (real home) and it is
	// never populated from a flag/env/config, so the "no user-supplied path
	// component" property (PRD #64) holds. Tests point it at a temp dir.
	SkillHome string

	// AutoUpgradeSkill enables the best-effort skill self-upgrade run before every
	// command. DefaultEnv enables it; command tests leave it false so unrelated
	// tests do no filesystem writes. UZI_SKILL_AUTO_UPGRADE=0 disables it at runtime.
	AutoUpgradeSkill bool
}

// DefaultEnv wires the real dependencies.
func DefaultEnv() Env {
	store, _ := uzicli.DefaultStore() // nil store tolerated; env/flags still work
	return Env{
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Stdin:     os.Stdin,
		StdoutTTY: uzicli.IsTerminal(os.Stdout),
		StdinTTY:  uzicli.IsTerminal(os.Stdin),
		NewClient:        func(s uzicli.Settings) uzicli.Client { return uzicli.NewHTTPClient(s) },
		Store:            store,
		AutoUpgradeSkill: true,
	}
}

// globalFlags are the persistent flags shared by every command.
type globalFlags struct {
	json    bool
	url     string
	quiet   bool
	noColor bool
}

// Main builds the command tree, runs it, prints any error to stderr, and maps
// the result to a process exit code.
func Main(env Env, args []string) int {
	root := newRootCmd(env)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		fmt.Fprintln(env.Stderr, "uzi:", err)
	}
	return uzicli.ExitCodeFor(err)
}

func newRootCmd(env Env) *cobra.Command {
	gf := &globalFlags{}

	root := &cobra.Command{
		Use:   "uzi",
		Short: "Terminal control of the uzi factory, for humans and agents",
		Long: "uzi drives the factory from the terminal: list and follow runs, approve plan\n" +
			"gates, manage workers, and read admin state. Humans get tables; agents get\n" +
			"--json and documented exit codes.",
		Version: version,
		// We print errors and map exit codes ourselves in Main; silence cobra's
		// own error/usage dumping so output stays under our control.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	// Best-effort skill self-upgrade before every command, so an agent always
	// drives the CLI with a SKILL.md matching THIS binary. It is deliberately
	// skipped for the `uzi skill ...` verbs (which manage the skill directly, and
	// would otherwise race their own report). Never fatal, never blocking: any
	// error warns on stderr and the real command still runs — a read-only $HOME in
	// CI must not break `uzi run list --json`.
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		if env.AutoUpgradeSkill && !underSkillCmd(cmd) {
			maybeAutoUpgradeSkill(env)
		}
	}
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	if env.Stdin != nil {
		root.SetIn(env.Stdin)
	}
	// Wrap cobra's flag parse errors (unknown/invalid flag) into an *ExitError so
	// they map to the usage exit code (2), keeping ExitCodeFor's default free to be
	// the generic error (1) for anything that leaks unwrapped.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
	})

	pf := root.PersistentFlags()
	pf.BoolVar(&gf.json, "json", false, "machine-readable JSON output (for agents)")
	pf.StringVar(&gf.url, "url", "", "uzi API base URL (overrides config and $UZI_URL)")
	pf.BoolVar(&gf.quiet, "quiet", false, "suppress non-essential output")
	pf.BoolVar(&gf.noColor, "no-color", false, "disable colour output")

	root.AddCommand(
		newLoginCmd(env, gf),
		newLogoutCmd(env, gf),
		newAuthCmd(env, gf),
		newWhoamiCmd(env, gf),
		newRunCmd(env, gf),
		newWorkerCmd(env, gf),
		newRepoCmd(env, gf),
		newAdminCmd(env, gf),
		newSkillCmd(env, gf),
		newVersionCmd(env, gf),
	)
	return root
}

// resolveSettings layers env vars and flags over the store-resolved context.
// Precedence: --url > $UZI_URL > config file for the URL; $UZI_TOKEN >
// credentials file for the token. There is deliberately no --token flag — a
// credential must never land on argv (PRD #64).
func resolveSettings(env Env, gf *globalFlags) (uzicli.Settings, error) {
	var s uzicli.Settings
	if env.Store != nil {
		var err error
		s, err = env.Store.Resolve("default")
		if err != nil {
			return s, uzicli.Exitf(uzicli.ExitAuth, "%v", err)
		}
	}
	if v := os.Getenv("UZI_URL"); v != "" {
		s.URL = v
	}
	if v := os.Getenv("UZI_TOKEN"); v != "" {
		s.Token = v
	}
	if gf.url != "" {
		s.URL = gf.url
	}
	return s, nil
}

// client resolves settings and builds the API client.
func (env Env) client(gf *globalFlags) (uzicli.Client, error) {
	s, err := resolveSettings(env, gf)
	if err != nil {
		return nil, err
	}
	return env.NewClient(s), nil
}

// printer builds the output renderer for the current global flags.
func (env Env) printer(gf *globalFlags) *uzicli.Printer {
	return uzicli.NewPrinter(env.Stdout, env.StdoutTTY, gf.json, gf.noColor, gf.quiet)
}
