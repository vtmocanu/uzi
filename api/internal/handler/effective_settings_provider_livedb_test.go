package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4320 M4: the read-only schedule preview of an owner whose Claude source is a model provider.
// Every provider refusal a fire records must answer 200 with its own refusal token, never a 500.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

const effProviderBase = "https://gw.example.com/anthropic"

// selectProvider seeds a provider (revision 1 at effProviderBase) with an enabled key and selects it
// as the owner's Claude source.
func (f *effFix) selectProvider() uuid.UUID {
	f.t.Helper()
	p, err := f.h.q.InsertModelProvider(f.ctx, store.InsertModelProviderParams{OwnerUserID: f.user, Name: "p-" + uuid.NewString()[:8], Engine: "claude"})
	if err != nil {
		f.t.Fatalf("insert provider: %v", err)
	}
	if err := f.h.q.InsertModelProviderRevision(f.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: effProviderBase, AuthStyle: "bearer",
		ModelMap: []byte(`{"opus":{"model":"corp-opus"},"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku"}}`),
	}); err != nil {
		f.t.Fatalf("insert revision: %v", err)
	}
	if _, err := f.h.q.InsertProviderKeySecret(f.ctx, store.InsertProviderKeySecretParams{
		UserID: f.user, Label: p.ID.String(), Ciphertext: []byte("ct"), SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true},
	}); err != nil {
		f.t.Fatalf("insert key: %v", err)
	}
	f.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, f.user, p.ID)
	// Registered after the fixture's user delete, so it runs first: a selected provider cannot be
	// deleted (RESTRICT).
	f.t.Cleanup(func() {
		if _, err := f.pool.Exec(f.ctx, `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, f.user); err != nil {
			f.t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	return p.ID
}

// TestSetTokenOnAProviderBoundRunNeverUsesCodexWordingLiveDB: set-token on a provider-bound run is
// refused with the provider wording, including for a run in an open policy episode, whose Codex
// continuation hint would otherwise be appended.
func TestSetTokenOnAProviderBoundRunNeverUsesCodexWordingLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.selectProvider()
	run, err := f.create(nil, nil)
	if err != nil {
		t.Fatalf("create provider run: %v", err)
	}
	worker := uuid.New()
	f.exec(`INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`, worker, f.user, worker.String(), worker[:])
	for _, policyEpisode := range []bool{false, true} {
		if policyEpisode {
			// An unresolved policy episode (the shape runs_policy_hold_shape accepts).
			f.exec(`UPDATE runs SET policy_outcome_id = gen_random_uuid(), policy_continuation_mode = 'same_conversation',
				policy_applying_worker_id = $2, policy_source_generation = 1, policy_applying_generation = 1,
				policy_preserve_required = true WHERE id = $1`, run.ID, worker)
		}
		rec := httptest.NewRecorder()
		f.h.SetRunCredential(rec, userReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/credential", `{"mode":"auto"}`, f.user, map[string]string{"id": run.ID.String()}))
		body := rec.Body.String()
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "model provider") || strings.Contains(strings.ToLower(body), "codex") {
			t.Fatalf("set-token (policy episode %v) = %d %s, want the 422 provider-source wording", policyEpisode, rec.Code, body)
		}
	}
}

// TestSchedulePreviewProviderSourceRefusalsLiveDB: (a) an owner whose source is set but unusable
// previews any schedule as provider_source_unusable; (b) an owner with a usable source previewing a
// schedule that stores a credential override gets credential_override_provider_source, not the Codex
// token it wraps; and the real save of that override is refused with the provider wording.
func TestSchedulePreviewProviderSourceRefusalsLiveDB(t *testing.T) {
	t.Run("unusable source", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		f.selectProvider()
		f.exec(`UPDATE model_providers SET retired_at = now() WHERE owner_user_id = $1`, f.user)
		for _, body := range []string{`{"target":"sweep"}`, `{"target":"prompt","harness":"claude"}`} {
			rec := f.schedulePreviewRaw(f.user, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("preview %s = %d %s, want 200 with a refusal (not a 500)", body, rec.Code, rec.Body.String())
			}
			dto, raw := f.schedulePreview(body)
			wantRefusal(t, dto, raw, "provider_source_unusable")
		}
		id := f.insertSweep("", "", false, "", nil)
		dto, raw := f.schedulePreview(`{"schedule_id":"` + id + `"}`)
		wantRefusal(t, dto, raw, "provider_source_unusable")
	})

	t.Run("usable source with a stored override", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.anthropic()
		f.selectProvider()
		id := f.insertSweep("", "", false, "auto", nil)
		rec := f.schedulePreviewRaw(f.user, `{"schedule_id":"`+id+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview = %d %s, want 200 with a refusal (not a 500)", rec.Code, rec.Body.String())
		}
		dto, raw := f.schedulePreview(`{"schedule_id":"` + id + `"}`)
		wantRefusal(t, dto, raw, "credential_override_provider_source")

		// The save step answers the same token and the real save the provider wording.
		dto, raw = f.schedulePreview(`{"target":"sweep","credential_override":{"mode":"auto"}}`)
		wantRefusal(t, dto, raw, "credential_override_provider_source")
		_, code, resp := f.saveSchedule(sweepBody(`,"credential_override":{"mode":"auto"}`))
		if code != http.StatusUnprocessableEntity || !strings.Contains(resp, "model provider") || strings.Contains(resp, "codex") {
			t.Fatalf("POST /schedules = %d %s, want the 422 provider-source refusal", code, resp)
		}
	})
}
