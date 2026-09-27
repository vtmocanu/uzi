package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// credential_disabled_harness_livedb_test.go pins PRD #1732 D15 and the schedule half of D2
// against real Postgres: harness usability reads ENABLED credentials only, an implicit request
// falls back past a slot with no enabled default, an explicit harness with no enabled credential
// is refused with the distinct errHarnessCredentialDisabled (no run, no fallback), and a
// schedule's stored pin that has since been disabled starts no run (ErrCredentialDisabled, the
// credential_disabled skip). Skipped unless UZI_TEST_DATABASE_URL is set.

// disableSecret disables a credential; clearDefault also drops its default flag (the state D4
// leaves after the slot's last enabled credential is disabled).
func disableSecret(env codexTestEnv, id uuid.UUID, clearDefault bool) {
	env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1,
	    is_default = CASE WHEN $2 THEN false ELSE is_default END WHERE id = $1`, id, clearDefault)
}

func runCountForUser(t *testing.T, env codexTestEnv, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestResolveHarnessImplicitDisabledCodexLiveDB (D15): with users.default_harness = codex, an
// implicit request freezes the slot's ENABLED default and never a disabled sibling; once the
// slot has no enabled default (the last Codex credential disabled, whether D4 cleared the flag
// or a disabled row still carries it) the implicit request falls back to Claude.
//
// MUTATION: drop the api.Disabled check in resolveUsableCodexCredential; the "enabled
// non-default sibling" case then freezes the disabled default key and this test fails.
func TestResolveHarnessImplicitDisabledCodexLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	seed := func(t *testing.T) (uuid.UUID, uuid.UUID) {
		userID, _, _ := env.seedCodexInfra(t)
		seedAnthropicToken(t, env, userID)
		setDefaultHarness(t, env, userID, "codex")
		key := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
		makeCodexDefault(t, env, key)
		return userID, key
	}

	t.Run("enabled default, disabled sibling", func(t *testing.T) {
		userID, key := seed(t)
		sibling := env.seedStaticAPIKey(t, userID, "old-key-"+uuid.NewString(), codexToken("sk"))
		disableSecret(env, sibling, false)
		got, err := svc.resolveRunHarnessQ(env.ctx, userID, nil, env.q)
		if err != nil || got.Harness != HarnessCodex || got.Codex == nil || got.Codex.SecretID != key {
			t.Fatalf("resolve = (%+v, %v), want Codex on the enabled default %s", got, err, key)
		}
	})
	for _, tc := range []struct {
		name           string
		clearDefault   bool
		enabledSibling bool
	}{
		{"last Codex credential disabled", true, false},
		{"disabled row still default", false, false},
		{"disabled default with an enabled non-default sibling", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userID, key := seed(t)
			if tc.enabledSibling {
				env.seedStaticAPIKey(t, userID, "sibling-"+uuid.NewString(), codexToken("sk"))
			}
			disableSecret(env, key, tc.clearDefault)
			got, err := svc.resolveRunHarnessQ(env.ctx, userID, nil, env.q)
			if err != nil || got.Harness != HarnessClaude || got.Codex != nil {
				t.Fatalf("resolve = (%+v, %v), want the implicit fallback to Claude", got, err)
			}
		})
	}
}

// TestExplicitHarnessNoEnabledCredentialRefusedLiveDB (D15): an EXPLICIT harness whose
// credentials are all disabled is refused with errHarnessCredentialDisabled (which still wraps
// ErrNoCredentialForHarness: no fallback) and creates no run; an explicit harness with no
// credential at all keeps the plain refusal.
//
// MUTATION: read UserHasAnthropicToken for Claude usability; the explicit-Claude case then
// creates a Claude run on an all-disabled slot and this test fails.
func TestExplicitHarnessNoEnabledCredentialRefusedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	claude, codex := HarnessClaude, HarnessCodex

	t.Run("explicit Claude, every token disabled", func(t *testing.T) {
		userID, _, repoID := env.seedCodexInfra(t)
		tok := env.seedAnthropicSecret(t, userID, "a-"+uuid.NewString(), false)
		disableSecret(env, tok, true)
		key := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
		makeCodexDefault(t, env, key)
		schedID := seedPromptScheduleRow(t, env, userID, repoID)
		_, err := svc.CreatePromptRun(env.ctx, userID, repoID, schedID, "t", "p", false, false, nil, nil, false, nil, &claude)
		if !errors.Is(err, ErrHarnessCredentialDisabled) || !errors.Is(err, ErrNoCredentialForHarness) {
			t.Fatalf("CreatePromptRun err = %v, want ErrHarnessCredentialDisabled (wrapping ErrNoCredentialForHarness)", err)
		}
		if n := runCountForUser(t, env, userID); n != 0 {
			t.Fatalf("runs = %d, want 0", n)
		}
	})
	t.Run("explicit Codex, every Codex credential disabled", func(t *testing.T) {
		userID, _, repoID := env.seedCodexInfra(t)
		env.seedAnthropicSecret(t, userID, "a-"+uuid.NewString(), true)
		key := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
		disableSecret(env, key, true)
		schedID := seedPromptScheduleRow(t, env, userID, repoID)
		_, err := svc.CreatePromptRun(env.ctx, userID, repoID, schedID, "t", "p", false, false, nil, nil, false, nil, &codex)
		if !errors.Is(err, ErrHarnessCredentialDisabled) {
			t.Fatalf("CreatePromptRun err = %v, want ErrHarnessCredentialDisabled", err)
		}
		if n := runCountForUser(t, env, userID); n != 0 {
			t.Fatalf("runs = %d, want 0 (no fallback to Claude)", n)
		}
	})
	t.Run("explicit Codex, no Codex credential at all", func(t *testing.T) {
		userID, _, repoID := env.seedCodexInfra(t)
		env.seedAnthropicSecret(t, userID, "a-"+uuid.NewString(), true)
		schedID := seedPromptScheduleRow(t, env, userID, repoID)
		_, err := svc.CreatePromptRun(env.ctx, userID, repoID, schedID, "t", "p", false, false, nil, nil, false, nil, &codex)
		if !errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) {
			t.Fatalf("CreatePromptRun err = %v, want the plain ErrNoCredentialForHarness", err)
		}
	})
}

// TestScheduledRunDisabledStoredPinStartsNoRunLiveDB (D2): a schedule's stored token pin that
// was disabled after the schedule was saved starts no run on either scheduled seam (prompt and
// issue) and never inherits another credential; the scheduler maps the error to the
// credential_disabled skip. An enabled stored pin still creates the run.
//
// MUTATION: drop checkStoredOverrideEnabled from CreatePromptRun / createRun; the fires then
// create runs pinned to the disabled token and this test fails.
func TestScheduledRunDisabledStoredPinStartsNoRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	userID, _, repoID := env.seedCodexInfra(t)
	env.seedAnthropicSecret(t, userID, "def-"+uuid.NewString(), true)
	pin := env.seedAnthropicSecret(t, userID, "pin-"+uuid.NewString(), false)
	override := &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &pin}
	disableSecret(env, pin, false)

	schedID := seedPromptScheduleRow(t, env, userID, repoID)
	if _, err := svc.CreatePromptRun(env.ctx, userID, repoID, schedID, "t", "p", false, false, nil, nil, false, override, nil); !errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("prompt fire err = %v, want ErrCredentialDisabled", err)
	}
	seedEligibleIssue(t, env, repoID, 4101)
	if _, err := svc.CreateScheduledRun(env.ctx, userID, repoID, 4101, "desc", nil, nil, nil, false, nil, override, nil); !errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("issue fire err = %v, want ErrCredentialDisabled", err)
	}
	if n := runCountForUser(t, env, userID); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}

	env.exec(`UPDATE user_secrets SET disabled_at = NULL WHERE id = $1`, pin)
	run, err := svc.CreatePromptRun(env.ctx, userID, repoID, schedID, "t", "p", false, false, nil, nil, false, override, nil)
	if err != nil || uuid.UUID(run.CredentialOverrideSecretID.Bytes) != pin {
		t.Fatalf("enabled pin fire = (%v, %v), want a run pinned to %s", run.CredentialOverrideSecretID, err, pin)
	}
}

// TestScheduleCredentialDisabledPrecheckLiveDB (D2/D15): the scheduler's pre-fire check reads the
// same facts the fire seams refuse on, with no run and no write. A disabled stored pin answers
// ErrCredentialDisabled; a pinned harness whose credentials are all disabled answers
// ErrHarnessCredentialDisabled; an enabled pin, an unpinned schedule, and a pinned harness with
// no credential at all (the fire's own hard refusal, not a hold) answer nil.
//
// MUTATION: drop either arm of ScheduleCredentialDisabled; the matching case then answers nil
// and this test fails.
func TestScheduleCredentialDisabledPrecheckLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	claude, codex := HarnessClaude, HarnessCodex
	userID, _, _ := env.seedCodexInfra(t)
	tok := env.seedAnthropicSecret(t, userID, "a-"+uuid.NewString(), true)
	pin := &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &tok}

	if err := svc.ScheduleCredentialDisabled(env.ctx, userID, pin, &claude); err != nil {
		t.Fatalf("enabled pin and harness: err = %v, want nil", err)
	}
	if err := svc.ScheduleCredentialDisabled(env.ctx, userID, nil, &codex); err != nil {
		t.Fatalf("pinned Codex with no Codex credential: err = %v, want nil (the fire refuses it, no hold)", err)
	}
	disableSecret(env, tok, true)
	if err := svc.ScheduleCredentialDisabled(env.ctx, userID, pin, nil); !errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("disabled pin: err = %v, want ErrCredentialDisabled", err)
	}
	if err := svc.ScheduleCredentialDisabled(env.ctx, userID, nil, &claude); !errors.Is(err, ErrHarnessCredentialDisabled) {
		t.Fatalf("pinned Claude, every token disabled: err = %v, want ErrHarnessCredentialDisabled", err)
	}
	if err := svc.ScheduleCredentialDisabled(env.ctx, userID, nil, nil); err != nil {
		t.Fatalf("unpinned schedule: err = %v, want nil", err)
	}
	if n := runCountForUser(t, env, userID); n != 0 {
		t.Fatalf("runs = %d, want 0 (the check writes nothing)", n)
	}
}
