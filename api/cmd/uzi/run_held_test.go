package main

// run_held_test.go covers `uzi run held` and `uzi run held expire` (issue #2545): the list
// render (human and JSON), the untrusted-field sanitizing, the expire confirmation contract
// (interactive prompt, a declined prompt performing NO mutation, the non-TTY hard refusal
// without --yes, --yes mapping straight to the expiry), and the held receipt in `run recovery`.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func heldFixture() apitypes.HeldPublicationsDTO {
	exp := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return apitypes.HeldPublicationsDTO{Publications: []apitypes.HeldPublicationDTO{
		{ID: "p2", RunID: "run1", Generation: 2, Ref: "refs/uzi-held/run1/2", Tip: "2222222222222222222222222222222222222222",
			State: "acknowledged", ExpiresAt: &exp},
		{ID: "p1", RunID: "run1", Generation: 1, Ref: "refs/uzi-held/run1/1", Tip: "1111111111111111111111111111111111111111",
			State: "deleted", LastError: "boom"},
	}}
}

func TestRunHeldRenders(t *testing.T) {
	fc := &uzicli.FakeClient{HeldPublicationsResult: heldFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "held", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"refs/uzi-held/run1/2", "acknowledged", "222222222222", "2026-10-20T12:00:00Z", "refs/uzi-held/run1/1", "deleted", "gen 1: boom", "uzi run held expire run1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "22222222222222222222") {
		t.Errorf("the tip must be cut to 12 characters:\n%s", out)
	}
}

func TestRunHeldEmptyAndJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "held", "run1")
	if code != uzicli.ExitOK || !strings.Contains(out, "run run1 has no held publications") {
		t.Fatalf("empty = %d %q", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(&uzicli.FakeClient{}), "--json", "run", "held", "run1")
	if code != uzicli.ExitOK || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty --json = %d %q, want []", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(&uzicli.FakeClient{HeldPublicationsResult: heldFixture()}), "--json", "run", "held", "run1")
	var got []apitypes.HeldPublicationDTO
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || len(got) != 2 || got[0].Ref != "refs/uzi-held/run1/2" {
		t.Fatalf("--json = %d %q", code, out)
	}
}

// Server-supplied text cannot smuggle a newline or an escape sequence into the rendered rows.
func TestRunHeldSanitizesUntrustedFields(t *testing.T) {
	dto := apitypes.HeldPublicationsDTO{Publications: []apitypes.HeldPublicationDTO{{
		ID: "p", RunID: "run1", Generation: 1, State: "deleted\x1b[31m",
		Ref: "refs/uzi-held/run1/1\nforged line", Tip: "\x1b[2Jabc", LastError: "x\ny\x1b]0;title\x07", RefusalReason: "r\nforged",
	}}}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{HeldPublicationsResult: dto}), "run", "held", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("a control byte reached the terminal: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "forged") {
			t.Errorf("a newline in a server field forged a line: %q", line)
		}
	}
}

func TestRunHeldExpireYes(t *testing.T) {
	fc := &uzicli.FakeClient{HeldExpireResult: apitypes.HeldPublicationsExpireDTO{Expired: 2}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "held", "expire", "run1", "--yes")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if len(fc.HeldExpireCalls) != 1 || fc.HeldExpireCalls[0] != "run1" {
		t.Fatalf("expire calls = %v, want exactly [run1]", fc.HeldExpireCalls)
	}
	if !strings.Contains(out, "2 held publication(s) of run run1 are due for deletion") {
		t.Errorf("output = %q", out)
	}
	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "held", "expire", "run1", "--yes")
	if !strings.Contains(out, "no acknowledged held publication") {
		t.Errorf("zero-expiry output = %q", out)
	}
}

func TestRunHeldExpireNonTTYRefusesWithoutYes(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = false
	_, errb, code := runCLI(t, env, "run", "held", "expire", "run1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if len(fc.HeldExpireCalls) != 0 {
		t.Errorf("a non-TTY refusal must mutate nothing; got %v", fc.HeldExpireCalls)
	}
	if !strings.Contains(errb, "--yes") {
		t.Errorf("stderr = %q, want a --yes hint", errb)
	}
}

func TestRunHeldExpireConfirm(t *testing.T) {
	for _, tc := range []struct {
		in    string
		calls int
	}{{"n\n", 0}, {"\n", 0}, {"", 0}, {"y\n", 1}, {"YES\n", 1}} {
		fc := &uzicli.FakeClient{}
		env := fakeEnv(fc)
		env.StdinTTY = true
		env.Stdin = strings.NewReader(tc.in)
		_, errb, code := runCLI(t, env, "run", "held", "expire", "run1")
		if code != uzicli.ExitOK {
			t.Fatalf("input %q: exit = %d\nstderr=%q", tc.in, code, errb)
		}
		if len(fc.HeldExpireCalls) != tc.calls {
			t.Errorf("input %q: expire calls = %v, want %d", tc.in, fc.HeldExpireCalls, tc.calls)
		}
		if tc.calls == 0 && !strings.Contains(errb, "aborted") {
			t.Errorf("input %q: a declined prompt should say it aborted; stderr=%q", tc.in, errb)
		}
	}
}

func TestRunHeldExpireNotFoundMapsExit(t *testing.T) {
	fc := &uzicli.FakeClient{HeldErr: uzicli.Exitf(uzicli.ExitNotFound, "run not found")}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "held", "expire", "run1", "--yes")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

// `uzi run recovery` shows a released hold's held receipt: the ref and the commit it holds.
func TestRunRecoveryRendersHeldReceipt(t *testing.T) {
	dto := apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{
		{ID: "hold-1", RunID: "run1", Generation: 1, State: "released", Attention: "released", WorkerID: "w1",
			FinalPublicationID: "pub-1", HeldRef: "refs/uzi-held/run1/1",
			FinalReceipt: &apitypes.RecoveryFinalDisposition{Kind: "held_publication", PublicationID: "pub-1",
				SourceSha: "3333333333333333333333333333333333333333", CoverageDigest: "c"}},
		{ID: "hold-2", RunID: "run1", Generation: 2, State: "open", Attention: "active", WorkerID: "w1"},
	}}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	const want = "hold hold-1 held: refs/uzi-held/run1/1 @ 333333333333\n"
	if !strings.Contains(out, want) {
		t.Errorf("output missing %q:\n%s", want, out)
	}
	if strings.Count(out, " held: ") != 1 {
		t.Errorf("only the released hold carries a held receipt:\n%s", out)
	}
	if got := heldReceiptLine(apitypes.RecoveryCustodyHoldDTO{ID: "h", HeldRef: "refs/uzi-held/r\nforged"}); strings.Contains(got, "\n") {
		t.Errorf("heldReceiptLine leaked a newline: %q", got)
	}
}
