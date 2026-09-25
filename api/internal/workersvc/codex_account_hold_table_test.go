package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexHoldCase is one alias/account state in the PRD #1590 M2 shared fixture table. The same
// table drives the pure Go classifier (TestCodexHoldTableGoClassifier, below) and, in
// codex_account_gate_livedb_test.go, the SQL side: ClaimRun's gate, the
// park_codex_account_unavailable page and the health projection's codex_account_gated. Every
// field is relative to a run whose binding was frozen against a linked, idle account, so the
// LiveDB side can reach each state with plain UPDATEs from one seeded baseline.
type codexHoldCase struct {
	name string
	// harness/mode/kind of the run; empty means codex / subscription / issue.
	harness, mode, kind string
	// unfrozen: the run has no frozen identity (codex_account_key and
	// codex_account_revision NULL). Only FreezeCodexBinding, at create, freezes one, and only
	// when the alias is already linked, so such a run fails every release predicate with
	// ErrCodexAccountKeyUnfrozen: it is never held (D1 holds only what can resume).
	unfrozen bool
	// alias is the codex_credential_state status: linked, staging, failed or static.
	alias string
	// materialAhead: the alias material_revision is past the run's frozen one.
	materialAhead bool
	// account is what the alias links to: "" (none), "same" (the frozen identity) or "other"
	// (a different identity).
	account string
	// revBumped: the linked account's credential_revision is past the frozen one.
	revBumped bool
	// coord is the linked account's coord_state.
	coord string
	// hold is the expected answer of every side.
	hold bool
	// goNA names why the Go claim classifier is not asked for this row ("" = it is).
	goNA string
}

// codexHoldTable covers every coord_state the codex_provider_account CHECK allows (idle,
// in_progress, committed, quarantined) and every alias status the codex_credential_state CHECK
// allows (staging, linked, failed, static).
var codexHoldTable = []codexHoldCase{
	{name: "linked idle", alias: "linked", account: "same", coord: "idle"},
	{name: "linked in_progress is not a hold (D1)", alias: "linked", account: "same", coord: "in_progress"},
	{name: "linked committed", alias: "linked", account: "same", coord: "committed"},
	{name: "linked quarantined", alias: "linked", account: "same", coord: "quarantined", hold: true},
	{name: "quarantined other identity is terminal", alias: "linked", account: "other", coord: "quarantined"},
	{name: "quarantined and revoked is terminal", alias: "linked", account: "same", revBumped: true, coord: "quarantined"},
	{name: "revoked idle is terminal", alias: "linked", account: "same", revBumped: true, coord: "idle"},
	{name: "other identity idle is terminal", alias: "linked", account: "other", coord: "idle"},
	{name: "staging relogin", alias: "staging", materialAhead: true, hold: true},
	{name: "failed relogin", alias: "failed", materialAhead: true, hold: true},
	{name: "staging without newer material", alias: "staging"},
	{name: "A1 same identity relink idle", alias: "linked", materialAhead: true, account: "same", coord: "idle", hold: true},
	{name: "A1 same identity relink in_progress", alias: "linked", materialAhead: true, account: "same", coord: "in_progress", hold: true},
	{name: "A1 same identity relink committed", alias: "linked", materialAhead: true, account: "same", coord: "committed", hold: true},
	{name: "A1 same identity relink quarantined", alias: "linked", materialAhead: true, account: "same", coord: "quarantined", hold: true},
	{name: "relink to other identity is terminal", alias: "linked", materialAhead: true, account: "other", coord: "idle"},
	{name: "relink with bumped revision is terminal", alias: "linked", materialAhead: true, account: "same", revBumped: true, coord: "idle"},
	// No frozen identity: nothing freezes it after create, so these runs can never pass the
	// predicate and none is held; the claim fails them credential_unavailable as before M2.
	{name: "unfrozen staging", unfrozen: true, alias: "staging", materialAhead: true},
	{name: "unfrozen idle", unfrozen: true, alias: "linked", account: "same", coord: "idle"},
	{name: "unfrozen quarantined is terminal", unfrozen: true, alias: "linked", account: "same", coord: "quarantined"},
	{name: "api_key static alias", mode: codexAuthModeAPIKey, alias: "static"},
	{name: "api_key mode on a quarantined login", mode: codexAuthModeAPIKey, alias: "linked", account: "same", coord: "quarantined"},
	// A Claude run cannot carry a Codex binding (runs_codex_harness_coherence_check): this is a
	// Claude run of an owner whose Codex login is quarantined.
	{name: "claude run of an owner with a quarantined login", harness: "claude", alias: "linked", account: "same", coord: "quarantined",
		goNA: "not a Codex claim"},
	{name: "chat run on a quarantined login (D7)", kind: "chat", alias: "linked", account: "same", coord: "quarantined",
		goNA: "the chat lane keeps the terminal classification"},
}

func (c codexHoldCase) runKind() string {
	if c.kind == "" {
		return "issue"
	}
	return c.kind
}

func (c codexHoldCase) authMode() string {
	if c.mode == "" {
		return codexAuthModeSubscription
	}
	return c.mode
}

// goInputs builds the claim-time classifier inputs for the case: the run's frozen binding and
// the GetRunCodexAuthContext row as the final classification reads it (status claimed).
func (c codexHoldCase) goInputs(t *testing.T) (store.Run, store.GetRunCodexAuthContextRow, string, int64) {
	t.Helper()
	identity := func(which string) (string, string) {
		if which == "other" {
			return "provider-other", "workspace-other"
		}
		return "provider-same", "workspace-same"
	}
	const frozenMaterial, frozenRev = 5, 3
	material := int64(frozenMaterial)
	if c.materialAhead {
		material++
	}
	credRev := int64(frozenRev)
	if c.revBumped {
		credRev++
	}
	run := store.Run{
		Harness:               harnessCodex,
		Kind:                  c.runKind(),
		CodexAuthMode:         pgtype.Text{String: c.authMode(), Valid: true},
		CodexMaterialRevision: pgtype.Int8{Int64: frozenMaterial, Valid: true},
	}
	if !c.unfrozen {
		key, err := codexAccountKey(identity("same"))
		if err != nil {
			t.Fatal(err)
		}
		run.CodexAccountKey = pgtype.Text{String: key, Valid: true}
		run.CodexAccountRevision = pgtype.Int8{Int64: frozenRev, Valid: true}
	}
	boundKind := store.KindCodexAuth
	if c.alias == "static" {
		boundKind = store.KindOpenAIAPIKey
	}
	row := store.GetRunCodexAuthContextRow{
		CodexAuthMode:           run.CodexAuthMode,
		CodexAccountKey:         run.CodexAccountKey,
		CodexMaterialRevision:   run.CodexMaterialRevision,
		CodexAccountRevision:    run.CodexAccountRevision,
		Status:                  "claimed",
		CurrentMaterialRevision: material,
		BoundKind:               boundKind,
	}
	if c.account != "" {
		provider, workspace := identity(c.account)
		row.ProviderUserID = pgtype.Text{String: provider, Valid: true}
		row.WorkspaceAccountID = pgtype.Text{String: workspace, Valid: true}
		row.CurrentCredentialRevision = pgtype.Int8{Int64: credRev, Valid: true}
		row.CurrentCoordState = pgtype.Text{String: c.coord, Valid: true}
	}
	return run, row, c.alias, material
}

// TestCodexHoldTableGoClassifier pins the Go half of the shared table: the claim-time
// classifier (classifyCodexClaimAuthority over evalCodexReleasePredicate) answers hold exactly
// where the SQL gate and the sweeper park do.
func TestCodexHoldTableGoClassifier(t *testing.T) {
	seenCoord := map[string]bool{}
	seenAlias := map[string]bool{}
	for _, tc := range codexHoldTable {
		seenAlias[tc.alias] = true
		if tc.coord != "" {
			seenCoord[tc.coord] = true
		}
		if tc.goNA != "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			run, row, state, material := tc.goInputs(t)
			hold, err := classifyCodexClaimAuthority(run, row, state, material)
			if hold != tc.hold {
				t.Fatalf("classifyCodexClaimAuthority hold = %v (err %v), want %v", hold, err, tc.hold)
			}
		})
	}
	for _, c := range []string{"idle", "in_progress", "committed", codexCoordQuarantined} {
		if !seenCoord[c] {
			t.Errorf("the table has no row with coord_state %q", c)
		}
	}
	for _, a := range []string{"staging", "linked", "failed", "static"} {
		if !seenAlias[a] {
			t.Errorf("the table has no row with alias status %q", a)
		}
	}
}
