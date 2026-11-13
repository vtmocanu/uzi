package main

// run_recovery_owner_compat_test.go pins #4198's owner-compatibility contract for
// `uzi run recovery <run-id>`: for the caller's own run, stdout, stderr and the exit status are
// exactly what main (171d368b8) printed, with the admin run debug server flag on or off (an
// owner's archives summary never carries admin_debug_read either way; the handler half is
// TestAdminRunDebugReadMatrixLiveDB). The golden strings were captured by running the same
// fixtures against a main export. The one deliberate difference is cancellation during one of
// the two reads #4198 added, which fails the command (TestRunRecoveryNewReadsPropagateContextErrors).

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const (
	ownerCompatHumanWithHold = "HOLD ID  GEN  DISPOSITION   ADMISSION  STATE  CAPTURE  WORKER\nhold-1   1    needs_action  -          open   -        alpha\nhold hold-1 captures:\ncap-1  available  H=invalid metadata  8 B\n  archive expiry: not recorded\n  prerequisites: unknown (no valid bound manifest evidence)\n  pin metadata does not release custody or verify latest local work; download/discard follow archive rules\n  restore guide: https://github.com/vtmocanu/uzi/blob/main/docs/run-recovery.md#prerequisite-pins-lifecycle-and-custody-release\n\n1 hold(s) have a recovery archive: download with `uzi run export`; thin archives also need prerequisite commits and verified import\n1 hold(s) await a decision: discard with `uzi run discard abcdefab-cdef-4abc-abcd-abcdefabcdef --hold <hold-id> --yes`\n"
	ownerCompatJSONWithHold  = "[\n  {\n    \"inventory_guarded\": false,\n    \"id\": \"hold-1\",\n    \"run_id\": \"abcdefab-cdef-4abc-abcd-abcdefabcdef\",\n    \"generation\": 1,\n    \"state\": \"open\",\n    \"attention\": \"needs_action\",\n    \"worker_id\": \"\",\n    \"worker_name\": \"alpha\",\n    \"has_available_capture\": true,\n    \"created_at\": \"0001-01-01T00:00:00Z\",\n    \"updated_at\": \"0001-01-01T00:00:00Z\",\n    \"captures\": [\n      {\n        \"id\": \"cap-1\",\n        \"state\": \"available\",\n        \"source_sha\": \"abcdef0123456789\",\n        \"byte_size\": 8,\n        \"created_at\": \"2026-09-01T12:30:00Z\"\n      }\n    ]\n  }\n]\n"
	ownerCompatHumanNoHold   = "run " + recoveryHeldRunID + " has no retained custody holds\n"
	ownerCompatJSONNoHold    = "[]\n"
)

// ownerCompatSummaries is an owner's (unmarked) archives summary: one capture under the listed
// hold and one under a hold the owner's list does not name.
func ownerCompatSummaries() map[string]apitypes.RecoveryArchiveSummaryDTO {
	return map[string]apitypes.RecoveryArchiveSummaryDTO{recoveryHeldRunID: {Supported: true,
		Archives: []apitypes.RecoveryArchiveDTO{debugCapture("cap-1", "hold-1"), debugCapture("cap-orphan", "unlisted-hold")}}}
}

func TestRunRecoveryOwnerOutputMatchesMain(t *testing.T) {
	serverErr := uzicli.Exitf(uzicli.ExitGeneric, "server error (500)")
	unauthorized := uzicli.Exitf(uzicli.ExitAuth, "unauthorized")
	notFound := uzicli.Exitf(uzicli.ExitNotFound, "run not found")
	scopedHolds := apitypes.RecoveryCustodyHoldsDTO{OwnerID: debugOwnerID, Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-raced", recoveryHeldRunID)}}
	for _, tc := range []struct {
		name    string
		fc      func() *uzicli.FakeClient
		human   string
		jsonOut string
	}{
		{"a own hold plus a capture under an unlisted hold", func() *uzicli.FakeClient {
			return &uzicli.FakeClient{
				RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-1", recoveryHeldRunID)}},
				RunByID:             map[string]apitypes.RunDTO{recoveryHeldRunID: {ID: recoveryHeldRunID, Status: "completed"}},
				RecoverySummaries:   ownerCompatSummaries(),
			}
		}, ownerCompatHumanWithHold, ownerCompatJSONWithHold},
		{"b no own hold, scoped none, captures present", func() *uzicli.FakeClient {
			return &uzicli.FakeClient{RecoverySummaries: ownerCompatSummaries()}
		}, ownerCompatHumanNoHold, ownerCompatJSONNoHold},
		{"c1 race: scoped holds, archives unmarked", func() *uzicli.FakeClient {
			return &uzicli.FakeClient{RecoveryHoldsScopedResult: scopedHolds, RecoverySummaries: ownerCompatSummaries()}
		}, ownerCompatHumanNoHold, ownerCompatJSONNoHold},
		{"c2 race: scoped holds, archives 404", func() *uzicli.FakeClient {
			// A hold opened between the reads and the run then vanished: the 404 names the
			// prerequisites, so this case is checked for stdout only below.
			return &uzicli.FakeClient{RecoveryHoldsScopedResult: scopedHolds, RecoveryArchivesErr: notFound}
		}, ownerCompatHumanNoHold, ownerCompatJSONNoHold},
		{"c3 race: scoped holds, archives 500", func() *uzicli.FakeClient {
			return &uzicli.FakeClient{RecoveryHoldsScopedResult: scopedHolds, RecoveryArchivesErr: serverErr}
		}, ownerCompatHumanNoHold, ownerCompatJSONNoHold},
		{"d1 scoped 500", func() *uzicli.FakeClient {
			return &uzicli.FakeClient{RecoveryHoldsScopedErr: serverErr, RecoverySummaries: ownerCompatSummaries()}
		}, ownerCompatHumanNoHold, ownerCompatJSONNoHold},
		{"d2 scoped and archives 401", func() *uzicli.FakeClient {
			return &uzicli.FakeClient{RecoveryHoldsScopedErr: unauthorized, RecoveryArchivesErr: unauthorized}
		}, ownerCompatHumanNoHold, ownerCompatJSONNoHold},
	} {
		for _, mode := range []string{"human", "json"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				args := []string{"run", "recovery", recoveryHeldRunID}
				want := tc.human
				if mode == "json" {
					args = append(args, "--json")
					want = tc.jsonOut
				}
				out, errb, code := runCLI(t, fakeEnv(tc.fc()), args...)
				if code != uzicli.ExitOK || out != want {
					t.Errorf("exit=%d stdout=%q\nwant exit 0 and main's stdout %q", code, out, want)
				}
				// main printed nothing on stderr for every case; c2's 404 is the one a
				// foreign-run read also gets, so it keeps the prerequisites hint.
				if strings.HasPrefix(tc.name, "c2") {
					if errb != adminRunDebugHint+"\n" {
						t.Errorf("stderr=%q, want only the prerequisites hint", errb)
					}
				} else if errb != "" {
					t.Errorf("stderr=%q, want main's empty stderr", errb)
				}
			})
		}
	}
}

// ctxClient cancels or outlives the command context inside one chosen read, then answers with
// the context's own error, as a real HTTP client does.
type ctxClient struct {
	uzicli.Client
	seam   string // "scoped" or "archives"
	cancel context.CancelFunc
}

func (c *ctxClient) block(ctx context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *ctxClient) RecoveryHoldsScoped(ctx context.Context, userID, runID string) (apitypes.RecoveryCustodyHoldsDTO, error) {
	if c.seam == "scoped" {
		return apitypes.RecoveryCustodyHoldsDTO{}, c.block(ctx)
	}
	return c.Client.RecoveryHoldsScoped(ctx, userID, runID)
}

func (c *ctxClient) RecoveryArchives(ctx context.Context, runID string) (apitypes.RecoveryArchiveSummaryDTO, error) {
	if c.seam == "archives" {
		return apitypes.RecoveryArchiveSummaryDTO{}, c.block(ctx)
	}
	return c.Client.RecoveryArchives(ctx, runID)
}

func TestRunRecoveryNewReadsPropagateContextErrors(t *testing.T) {
	for _, seam := range []string{"scoped", "archives"} {
		for _, kind := range []string{"canceled", "deadline"} {
			for _, mode := range []string{"human", "json"} {
				t.Run(seam+"/"+kind+"/"+mode, func(t *testing.T) {
					var ctx context.Context
					var cancel context.CancelFunc
					c := &ctxClient{Client: &uzicli.FakeClient{RecoverySummaries: ownerCompatSummaries()}, seam: seam}
					wantErr := context.Canceled
					if kind == "canceled" {
						ctx, cancel = context.WithCancel(context.Background())
						c.cancel = cancel
					} else {
						ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
						wantErr = context.DeadlineExceeded
					}
					defer cancel()
					env := fakeEnv(c)
					var out, errb bytes.Buffer
					env.Stdout, env.Stderr = &out, &errb
					t.Setenv("UZI_URL", "")
					t.Setenv("UZI_TOKEN", "")
					args := []string{"run", "recovery", recoveryHeldRunID}
					if mode == "json" {
						args = append(args, "--json")
					}
					// Main runs with Cobra's background context, so execute the root command
					// with the controlled one (as vault_test.go does).
					root := newRootCmd(env)
					root.SetArgs(args)
					err := root.ExecuteContext(ctx)
					if !errors.Is(err, wantErr) {
						t.Fatalf("err = %v, want %v", err, wantErr)
					}
					if uzicli.ExitCodeFor(err) == uzicli.ExitOK {
						t.Errorf("exit code for %v is 0", err)
					}
					if strings.Contains(out.String(), "no retained custody holds") || strings.TrimSpace(out.String()) == "[]" || out.Len() != 0 {
						t.Errorf("stdout = %q, want no empty result rendered", out.String())
					}
				})
			}
		}
	}
}
