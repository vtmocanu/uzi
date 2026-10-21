package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// TestPolicyHoldCLICancelLiveDB proves M1 Cancel only, through the built CLI,
// bearer-authenticated Routes, owner service and committed Postgres rows. Policy
// source/session/scope are seeded evidence, not a worker-delivery proof. Stop
// negotiation remains off in newSettleEnv; custody is independent of cancellation.
func TestPolicyHoldCLICancelLiveDB(t *testing.T) {
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	scratch, err := filepath.Abs("../../../.uzi/scratch")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(scratch, "policy-cancel-cli.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	binary := inventoryBuildCLI(t, dir)

	// The cases run serially; each command gets the existing inventory CLI
	// invocation's 15-second bound and one attempt. A failed case does not prevent
	// sibling subtests from running. There are no retries or readiness sleeps.
	for _, tc := range []struct {
		name    string
		reason  string
		pending bool
		foreign bool
	}{
		{name: "paused_held_plain_cancel"},
		{name: "paused_held_cancel_with_reason", reason: "owner aborts this run"},
		{name: "pending_outcome_refusal_then_explicit_discard_preserves_custody", pending: true},
		{name: "foreign_owner_refused", foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSettleEnv(t)
			e.wsvc.SetTxBeginner(e.pool)
			// A well-shaped frozen source without a primary revision, using the V1
			// shape in newPolicyStopConfirmNoFeedbackEnv. No Stop receipt is seeded.
			presentation := uuid.New()
			payload := `{"plan_md":"preserved plan","milestones":[{"id":"m1","title":"one"},{"id":"m2","title":"two"}],"required_capabilities":[],"required_tools":[],"size_class":"s"}`
			digest := sha256.Sum256([]byte(payload))
			source, err := json.Marshal(map[string]any{
				"version":          1,
				"gate":             map[string]string{"revision": "3", "presentation_id": presentation.String(), "payload_digest": hex.EncodeToString(digest[:])},
				"primary_revision": nil, "associated_cross_checks": []any{},
			})
			if err != nil {
				t.Fatal(err)
			}
			e.exec(`UPDATE runs SET status='paused',harness='codex',interactive=false,
 hold_reason='provider_policy_refusal',policy_refusal_count=1,claim_generation=2,
 session_id='preserved-cancel-thread',policy_last_outcome_id=$2,policy_last_source_generation=2,
 policy_last_tag='cyberPolicy',policy_last_phase='planning',policy_last_refused_at=now(),
 policy_last_correlation_id=$3,policy_hold_provenance='verified',policy_hold_published=false,
 gate_revision=3,gate_presentation_id=$4,gate_presented_payload=$5::jsonb,gate_payload_digest=$6,
 policy_revision_source=$7::jsonb,plan_md='preserved plan',size_class='s',
 milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_candidate='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_completed='["m1"]',started_at=now()-interval '1 minute'
 WHERE id=$1`, e.run, uuid.New(), "pr-"+strings.Repeat("a", 32)+"-2", presentation, payload, digest[:], string(source))
			if tc.pending {
				// Same exact-generation lease shape as seedTerminalPendingLease.
				e.exec(`INSERT INTO worker_active_runs
 (worker_id,run_id,claim_generation,phase,terminal_pending,terminal_pending_until,snapshot_epoch,reported_at)
 VALUES($1,$2,2,'running',true,now()+interval '1 hour',1,now())`, e.workerA, e.run)
			}
			owner := e.user
			if tc.foreign {
				owner = uuid.New()
				e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, "cancel-"+owner.String()+"@e2e")
			}
			token := cliMintToken(t, e.pool, owner, clitoken.ScopeUser)
			var mu sync.Mutex
			var observations []policyCancelCLIObservation
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				rec := httptest.NewRecorder()
				e.router.ServeHTTP(rec, r)
				headers := r.Header.Clone()
				authenticated := headers.Get("Authorization") == "Bearer "+token
				headers.Del("Authorization")
				mu.Lock()
				observations = append(observations, policyCancelCLIObservation{
					method: r.Method, path: r.URL.RequestURI(), headers: headers,
					body: string(body), authenticated: authenticated, readErr: readErr,
					status: rec.Code, response: rec.Body.String(),
				})
				mu.Unlock()
				for key, values := range rec.Header() {
					w.Header()[key] = append([]string(nil), values...)
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(rec.Body.Bytes())
			}))
			t.Cleanup(server.Close)
			snapshot := func(t *testing.T) map[string]json.RawMessage {
				t.Helper()
				var raw []byte
				err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_object(
 'run',to_jsonb(r),
 'inputs',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.id),'[]'::jsonb) FROM run_user_inputs i WHERE i.run_id=r.id),
 'custody',(SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY h.id),'[]'::jsonb) FROM recovery_custody_holds h WHERE h.run_id=r.id),
 'leases',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.worker_id),'[]'::jsonb) FROM worker_active_runs a WHERE a.run_id=r.id))
 FROM runs r WHERE r.id=$1`, e.run).Scan(&raw)
				if err != nil {
					t.Fatal(err)
				}
				var result map[string]json.RawMessage
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			invoke := func(t *testing.T, discard bool, wantStatus int) {
				t.Helper()
				mu.Lock()
				start := len(observations)
				mu.Unlock()
				args := []string{"--url", server.URL, "run", "cancel", e.run.String()}
				if tc.reason != "" {
					args = append(args, "-m", tc.reason)
				}
				if discard {
					args = append(args, "--discard-pending-outcome")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // G204: test-built CLI, fixed command and test-owned URL/ID; no shell.
				for _, entry := range os.Environ() {
					key, _, _ := strings.Cut(entry, "=")
					switch key {
					case "UZI_TOKEN", "UZI_URL", "UZI_CONTEXT", "UZI_SKILL_AUTO_UPGRADE", "HOME", "XDG_CONFIG_HOME", "TMPDIR":
						continue
					}
					cmd.Env = append(cmd.Env, entry)
				}
				cmd.Env = append(cmd.Env, "UZI_TOKEN="+token, "UZI_SKILL_AUTO_UPGRADE=0",
					"HOME="+dir, "XDG_CONFIG_HOME="+dir, "TMPDIR="+dir)
				cmd.Dir = dir
				cmd.WaitDelay = time.Second
				out, cliErr := cmd.CombinedOutput()
				// Never include a plaintext credential in assertion output.
				output := strings.ReplaceAll(string(out), token, "[redacted]")
				if ctx.Err() != nil {
					t.Fatalf("CLI deadline: %v", ctx.Err())
				}
				if (cliErr == nil) != (wantStatus == http.StatusAccepted) {
					t.Fatalf("CLI success mismatch: %v %s", cliErr, output)
				}
				mu.Lock()
				got := append([]policyCancelCLIObservation(nil), observations[start:]...)
				mu.Unlock()
				if len(got) != 1 {
					t.Fatalf("CLI made %d requests, want one POST without refresh or retry", len(got))
				}
				o := got[0]
				if o.readErr != nil || o.method != http.MethodPost || o.path != "/api/runs/"+e.run.String()+"/inputs" ||
					!o.authenticated || o.headers.Get("Content-Type") != "application/json" || o.headers.Get("If-Match") != "" {
					t.Fatal("Cancel changed its authenticated POST path/header contract or body could not be observed")
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(o.body), &body); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{"kind": "cancel", "body": tc.reason, "selection": nil, "override_capabilities": false}
				if discard {
					want["discard_pending_outcome"] = true
				}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("unexpected Cancel request body: %s", o.body)
				}
				if o.status != wantStatus {
					t.Fatalf("HTTP status=%d want=%d response=%s", o.status, wantStatus, o.response)
				}
				if wantStatus == http.StatusAccepted {
					if strings.TrimSpace(o.response) != `{"server_side":true}` {
						t.Fatalf("Cancel receipt fabricated extra state: %s", o.response)
					}
					if strings.TrimSpace(output) != e.run.String()+": run cancelled" {
						t.Fatalf("CLI did not report committed cancellation: %s", output)
					}
				} else if strings.Contains(output, "run cancelled") || strings.Contains(output, "cancellation sent") {
					t.Fatalf("refused Cancel reported success: %s", output)
				}
				if tc.pending && !discard && wantStatus == http.StatusConflict {
					var refusal struct {
						Reason string `json:"reason"`
					}
					if err := json.Unmarshal([]byte(o.response), &refusal); err != nil || refusal.Reason != "outcome_pending_confirmation_required" {
						t.Fatalf("pending refusal lost typed reason: %s", o.response)
					}
					if !strings.Contains(output, "--discard-pending-outcome") {
						t.Fatalf("pending refusal lost explicit confirmation guidance: %s", output)
					}
				}
			}
			before := snapshot(t)
			if tc.foreign {
				invoke(t, false, http.StatusNotFound)
				policyCancelEqualJSON(t, "foreign refusal", before, snapshot(t))
				return
			}
			if tc.pending {
				if !t.Run("plain_refusal_unchanged", func(t *testing.T) {
					invoke(t, false, http.StatusConflict)
					policyCancelEqualJSON(t, "pending refusal", before, snapshot(t))
				}) {
					return
				}
			}
			invoke(t, tc.pending, http.StatusAccepted)
			after := snapshot(t)
			for _, key := range []string{"custody", "inputs", "leases"} {
				policyCancelEqualJSON(t, key, before[key], after[key])
			}
			var prior, current map[string]json.RawMessage
			if err := json.Unmarshal(before["run"], &prior); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(after["run"], &current); err != nil {
				t.Fatal(err)
			}
			if string(current["status"]) != `"cancelled"` || string(current["stop_kind"]) != `"cancelled"` ||
				string(current["finished_at"]) == "null" {
				t.Fatal("Cancel did not commit terminal cancelled status/stop_kind/finished_at")
			}
			wantReason := "null"
			if tc.reason != "" {
				reasonJSON, err := json.Marshal(tc.reason)
				if err != nil {
					t.Fatal(err)
				}
				wantReason = string(reasonJSON)
			}
			if string(current["stop_reason"]) != wantReason {
				t.Fatalf("committed optional reason=%s want=%s", current["stop_reason"], wantReason)
			}
			for _, key := range []string{"plan_cross_check_gate_reason", "milestones_in_progress", "milestones_agents",
				"pause_requested_at", "pause_mode", "pause_after_count", "credential_switch_requested_at",
				"credential_switch_generation", "health_reason", "health_since"} {
				if string(current[key]) != "null" {
					t.Fatalf("terminal Cancel did not clear %s", key)
				}
			}
			if string(current["health"]) != `"ok"` {
				t.Fatal("terminal Cancel retained a health flag")
			}
			// All other columns must survive byte-for-byte JSON comparison, including
			// session, frozen/completed scope, policy count/source/fresh grant, approval
			// and Stop resolution. These are the legitimate terminal Cancel writes.
			for _, key := range []string{"status", "status_since", "stop_kind", "stop_reason", "finished_at", "move_pending_since", "updated_at",
				"plan_cross_check_gate_reason", "milestones_in_progress", "milestones_agents", "pause_requested_at", "pause_mode", "pause_after_count",
				"credential_switch_requested_at", "credential_switch_generation", "health", "health_reason", "health_since"} {
				delete(prior, key)
				delete(current, key)
			}
			policyCancelEqualJSON(t, "preserved run columns", prior, current)
			t.Run("duplicate_terminal_cancel_no_second_mutation", func(t *testing.T) {
				invoke(t, tc.pending, http.StatusConflict)
				policyCancelEqualJSON(t, "terminal retry", after, snapshot(t))
			})
			if head, compare := e.fake.calls(); head != 0 || compare != 0 {
				t.Fatal("Cancel attempted forge ancestry resolution")
			}
		})
	}
}

type policyCancelCLIObservation struct {
	method, path, body, response string
	headers                      http.Header
	authenticated                bool
	readErr                      error
	status                       int
}

// Compare complete JSON without allocating an aggregate assertion diff. On a
// mismatch only the named snapshot is reported, including for the whole run.
func policyCancelEqualJSON(t *testing.T, name string, want, got any) {
	t.Helper()
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w, g) {
		t.Fatalf("%s changed", name)
	}
}
