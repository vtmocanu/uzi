package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Limit transient input without applying password-creation minimums to existing vaults.
const maxVaultPasswordBytes = 1 << 20

func newVaultCmd(env Env, gf *globalFlags) *cobra.Command {
	noArgs := func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return uzicli.Exitf(uzicli.ExitUsage, "vault commands do not accept positional arguments")
		}
		return nil
	}
	cmd := &cobra.Command{Use: "vault", Short: "Check or unlock your vault", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error {
		return uzicli.Exitf(uzicli.ExitUsage, "invalid vault command flags; use --help")
	})
	status := &cobra.Command{Use: "status", Short: "Show whether your vault is locked", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			result, err := c.VaultStatus(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(result)
			}
			if result.Unlocked {
				p.Println("unlocked")
			} else {
				p.Println("locked")
			}
			return nil
		}}
	var passwordStdin bool
	unlock := &cobra.Command{Use: "unlock", Short: "Unlock with your login password or OIDC vault passphrase", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if env.StdinTTY && passwordStdin {
				return uzicli.Exitf(uzicli.ExitUsage, "--password-stdin requires non-terminal stdin")
			}
			if !env.StdinTTY && !passwordStdin {
				return uzicli.Exitf(uzicli.ExitUsage, "use --password-stdin with non-terminal stdin")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			password, err := readVaultPassword(cmd.Context(), env)
			defer clear(password)
			if err != nil {
				return err
			}
			if cmd.Context().Err() != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "vault password input cancelled")
			}
			if err := c.VaultUnlock(cmd.Context(), string(password)); err != nil {
				return uzicli.SafeVaultUnlockError(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(uzicli.VaultStatusResult{Unlocked: true})
			}
			if !gf.quiet {
				p.Println("Vault unlocked.")
			}
			return nil
		}}
	unlock.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read one password line from non-terminal stdin")
	cmd.AddCommand(status, unlock)
	return cmd
}

func readVaultPassword(ctx context.Context, env Env) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, uzicli.Exitf(uzicli.ExitGeneric, "vault password input cancelled")
	}
	var password []byte
	var err error
	if env.StdinTTY {
		if env.ReadVaultPassword == nil {
			return nil, uzicli.Exitf(uzicli.ExitGeneric, "terminal password reader unavailable")
		}
		if _, err = fmt.Fprint(env.Stderr, "Login password or OIDC vault passphrase: "); err != nil {
			return nil, uzicli.Exitf(uzicli.ExitGeneric, "cannot write vault password prompt")
		}
		password, err = env.ReadVaultPassword(ctx, env.Stdin)
		_, _ = fmt.Fprintln(env.Stderr)
	} else {
		if env.Stdin == nil {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "no vault password provided")
		}
		password, err = bufio.NewReader(io.LimitReader(env.Stdin, maxVaultPasswordBytes+3)).ReadBytes('\n')
		if err == io.EOF {
			err = nil
		}
		if len(password) > 0 && password[len(password)-1] == '\n' {
			password = password[:len(password)-1]
			if len(password) > 0 && password[len(password)-1] == '\r' {
				password = password[:len(password)-1]
			}
		}
	}
	if ctx.Err() != nil {
		clear(password)
		return nil, uzicli.Exitf(uzicli.ExitGeneric, "vault password input cancelled")
	}
	if errors.Is(err, errVaultPasswordTooLong) {
		clear(password)
		return nil, uzicli.Exitf(uzicli.ExitUsage, "vault password input is too long")
	}
	if err != nil {
		clear(password)
		return nil, uzicli.Exitf(uzicli.ExitGeneric, "cannot read vault password")
	}
	if len(password) == 0 {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "no vault password provided")
	}
	if len(password) > maxVaultPasswordBytes {
		clear(password)
		return nil, uzicli.Exitf(uzicli.ExitUsage, "vault password input is too long")
	}
	return password, nil
}

// The bounded reader attempts to restore terminal state before returning.
// If overflow cleanup cannot discard pending input, echo stays disabled.
// Context cancellation is checked before and after the blocking terminal read;
// no password is submitted after cancellation.
func readVaultTerminalPassword(ctx context.Context, input io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, ok := input.(*os.File)
	if !ok {
		return nil, uzicli.Exitf(uzicli.ExitGeneric, "password input is not a terminal file")
	}
	password, err := readVaultHiddenPassword(f.Fd())
	if ctx.Err() != nil {
		clear(password)
		return nil, ctx.Err()
	}
	return password, err
}
