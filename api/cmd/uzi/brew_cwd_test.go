package main

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestBrewCommandsFromDeletedWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("fixture working directory still exists")
	}
	t.Run("quiet probe", func(t *testing.T) {
		out, err := quietProcess(func(ctx context.Context, cmd *exec.Cmd) error {
			if cmd.Dir != "/" {
				t.Errorf("probe directory=%q; want /", cmd.Dir)
			}
			return runProcess(ctx, cmd)
		}, "/bin/pwd", nil, nil)
		if err != nil || strings.TrimSpace(out) != "/" {
			t.Fatalf("probe from deleted directory: output=%q err=%v; want /", out, err)
		}
	})
	t.Run("foreground upgrade", func(t *testing.T) {
		cmd := foregroundBrewCommand("upgrade", "vtmocanu/tap/uzi-cli-rc")
		if cmd.Dir != "/" {
			t.Fatalf("foreground directory=%q; want /", cmd.Dir)
		}
		if !reflect.DeepEqual(cmd.Args, []string{"brew", "upgrade", "vtmocanu/tap/uzi-cli-rc"}) || cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr || cmd.Env != nil {
			t.Fatalf("foreground arguments or inherited environment/output changed: %+v", cmd)
		}
	})
}
