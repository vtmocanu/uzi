package workersvc

import (
	"errors"
	"github.com/google/uuid"
	"testing"
)

func TestLateSoleStoredOverrideResolverRefusalLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, repo := seedCodexOnlyOriginUser(t, env)
	seedAnthropicToken(t, env, user)
	var alias uuid.UUID
	if err := env.pool.QueryRow(env.ctx, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", user).Scan(&alias); err != nil {
		t.Fatal(err)
	}
	svc := atomicSvc(env)
	override := &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &alias}
	if err := svc.ScheduleCredentialDisabled(env.ctx, user, override, nil); err != nil {
		t.Fatal(err)
	}
	disableSecret(env, alias, true)
	claude := HarnessClaude
	seedEligibleIssue(t, env, repo, 96000)
	_, err := svc.CreateScheduledAutopilotRun(env.ctx, user, repo, 96000, "body", nil, nil, nil, false, override, &claude)
	if !errors.Is(err, ErrHarnessCredentialDisabled) || errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("expected harness refusal before stored-token closure, got %v", err)
	}
	// Old unlabelled selection would resolve usable Codex and reach the stored-token
	// check; the scheduler treats that refusal as schedule-wide rather than a label skip.
	seedEligibleIssue(t, env, repo, 96001)
	_, err = svc.CreateScheduledAutopilotRun(env.ctx, user, repo, 96001, "body", nil, nil, nil, false, override, nil)
	if !errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("old selection refusal=%v", err)
	}
}
