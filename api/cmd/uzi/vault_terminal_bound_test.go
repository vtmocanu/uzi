package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestVaultTerminalLineBound(t *testing.T) {
	ending, ignored := "\n", "\r"
	if runtime.GOOS == "windows" {
		ending, ignored = "\r", "\n"
	}
	for _, tc := range []struct {
		name, input, want string
		err               error
	}{
		{"editing", "ab\bc" + ignored + ending, "ac", nil},
		{"empty backspace", "\ba" + ending, "a", nil},
		{"EOF", "abc", "abc", nil},
		{"empty EOF", "", "", io.EOF},
		{"exact limit", strings.Repeat("x", maxVaultPasswordBytes) + ending, strings.Repeat("x", maxVaultPasswordBytes), nil},
		{"overflow", strings.Repeat("x", maxVaultPasswordBytes+1), "", errVaultPasswordTooLong},
		{"ignored budget", strings.Repeat(ignored, maxVaultPasswordBytes+1), "", errVaultPasswordTooLong},
		{"editing budget", strings.Repeat("\b", maxVaultPasswordBytes+1), "", errVaultPasswordTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.NewReader(tc.input)
			password, err := readVaultPasswordLine(input)
			defer clear(password)
			if !errors.Is(err, tc.err) || !bytes.Equal(password, []byte(tc.want)) {
				t.Fatalf("result length=%d err=%v; want length=%d err=%v", len(password), err, len(tc.want), tc.err)
			}
			if err != nil && password != nil {
				t.Fatal("failure returned password storage")
			}
		})
	}
	t.Run("empty read budget", func(t *testing.T) {
		reads := 0
		password, err := readVaultPasswordLine(vaultInputReader(func([]byte) (int, error) {
			reads++
			return 0, nil
		}))
		if password != nil || !errors.Is(err, errVaultPasswordTooLong) || reads != maxVaultPasswordBytes+1 {
			t.Fatalf("length=%d err=%v reads=%d", len(password), err, reads)
		}
	})
	t.Run("read error", func(t *testing.T) {
		reads := 0
		password, err := readVaultPasswordLine(vaultInputReader(func(p []byte) (int, error) {
			reads++
			if reads == 1 {
				p[0] = 'x'
				return 1, nil
			}
			return 0, io.ErrUnexpectedEOF
		}))
		if password != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("length=%d err=%v", len(password), err)
		}
	})
}

func TestVaultTerminalBoundErrorMapping(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		env := fakeEnv(&uzicli.FakeClient{})
		env.StdinTTY = true
		env.Stderr = io.Discard
		storage := []byte(vaultTestSecret)
		env.ReadVaultPassword = func(context.Context, io.Reader) ([]byte, error) {
			if cancelled {
				cancel()
			}
			return storage, errVaultPasswordTooLong
		}
		password, err := readVaultPassword(ctx, env)
		cancel()
		wantCode, wantMessage := uzicli.ExitUsage, "vault password input is too long"
		if cancelled {
			wantCode, wantMessage = uzicli.ExitGeneric, "vault password input cancelled"
		}
		if password != nil || uzicli.ExitCodeFor(err) != wantCode || err == nil || err.Error() != wantMessage {
			t.Fatalf("cancelled=%t length=%d err=%v", cancelled, len(password), err)
		}
		if !bytes.Equal(storage, make([]byte, len(storage))) {
			t.Fatal("reader storage was not cleared")
		}
	}
}
