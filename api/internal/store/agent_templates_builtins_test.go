package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// reconcilerFake implements builtinReconcilerQueries in memory so the collision
// and read-back-error branches of ReconcileBuiltinTemplates are covered without
// a live database. A name in existing or getErr makes the insert a no-op (0 rows,
// as ON CONFLICT DO NOTHING would); everything else inserts (1 row).
type reconcilerFake struct {
	existing  map[string]AgentTemplate // name -> the row GetSharedAgentTemplateByName returns
	getErr    map[string]error         // name -> a read-back error
	inserted  []string
	seeded    []string // names passed to SeedSharedTemplateAllocationByName
	refreshed []string // names passed to RefreshPristineBuiltin (no-insert path only)
}

func (f *reconcilerFake) InsertBuiltinAgentTemplate(_ context.Context, arg InsertBuiltinAgentTemplateParams) (int64, error) {
	if _, ok := f.existing[arg.Name]; ok {
		return 0, nil
	}
	if _, ok := f.getErr[arg.Name]; ok {
		return 0, nil
	}
	f.inserted = append(f.inserted, arg.Name)
	return 1, nil
}

func (f *reconcilerFake) SeedSharedTemplateAllocationByName(_ context.Context, name string) error {
	f.seeded = append(f.seeded, name)
	return nil
}

// RefreshPristineBuiltin records the call; the in-memory fake cannot model the
// customized/content-guard WHERE (that is LiveDB coverage), it only proves the
// reconciler routes the no-insert path here. Returns 0 rows (a no-op refresh).
func (f *reconcilerFake) RefreshPristineBuiltin(_ context.Context, arg RefreshPristineBuiltinParams) (int64, error) {
	f.refreshed = append(f.refreshed, arg.Name)
	return 0, nil
}

func (f *reconcilerFake) GetSharedAgentTemplateByName(_ context.Context, name string) (AgentTemplate, error) {
	if err := f.getErr[name]; err != nil {
		return AgentTemplate{}, err
	}
	if row, ok := f.existing[name]; ok {
		return row, nil
	}
	return AgentTemplate{}, errors.New("not found")
}

// captureLogs redirects slog (at debug level) to a buffer for the duration of fn.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestReconcileWarnsOnCustomRowShadowingBuiltin(t *testing.T) {
	fake := &reconcilerFake{
		existing: map[string]AgentTemplate{
			"lead": {Name: "lead", IsBuiltin: false}, // an admin created a custom "lead"
		},
	}
	logs := captureLogs(t, func() {
		if _, err := ReconcileBuiltinTemplates(context.Background(), fake); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})

	if !strings.Contains(logs, "shadowed by a custom row") || !strings.Contains(logs, `"name":"lead"`) {
		t.Errorf("expected a shadow warning naming lead; logs:\n%s", logs)
	}
	if strings.Count(logs, "shadowed by a custom row") != 1 {
		t.Errorf("expected exactly one shadow warning; logs:\n%s", logs)
	}
	// The other builtins still seed normally.
	if len(fake.inserted) == 0 {
		t.Error("expected the non-shadowed builtins to be inserted")
	}
	for _, name := range fake.inserted {
		if name == "lead" {
			t.Error("lead must not be reported inserted when a custom row shadows it")
		}
	}
	// A global-default allocation is seeded exactly for the builtins that were
	// actually inserted — never for the shadowed one (PRD #18 M7: no re-adding a
	// default an admin removed).
	if strings.Join(fake.seeded, ",") != strings.Join(fake.inserted, ",") {
		t.Errorf("seeded (%v) must match inserted (%v)", fake.seeded, fake.inserted)
	}
}

func TestReconcileSilentWhenBuiltinAlreadySeeded(t *testing.T) {
	// An already-seeded builtin row (is_builtin=true) is the normal every-boot
	// case and must NOT warn.
	fake := &reconcilerFake{
		existing: map[string]AgentTemplate{
			"coder": {Name: "coder", IsBuiltin: true},
		},
	}
	logs := captureLogs(t, func() {
		if _, err := ReconcileBuiltinTemplates(context.Background(), fake); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
	if strings.Contains(logs, "shadowed by a custom row") {
		t.Errorf("an already-seeded builtin must not warn; logs:\n%s", logs)
	}
	// The already-seeded builtin took the no-insert path, so the reconciler must
	// route it through RefreshPristineBuiltin (the boot-time pristine refresh),
	// while a freshly-INSERTED builtin must NOT be refreshed (PRD #275). This pins
	// the boot wiring the LiveDB test cannot reach — that test calls the query
	// directly, so without this assertion deleting the reconciler's refresh call
	// would leave the whole suite green.
	if !slices.Contains(fake.refreshed, "coder") {
		t.Errorf("an already-seeded builtin must be refreshed on boot; refreshed=%v", fake.refreshed)
	}
	for _, name := range fake.inserted {
		if slices.Contains(fake.refreshed, name) {
			t.Errorf("a freshly-inserted builtin %q must not also be refreshed", name)
		}
	}
}

func TestReconcileLogsReadBackErrorAtDebug(t *testing.T) {
	fake := &reconcilerFake{
		getErr: map[string]error{"lead": errors.New("boom")},
	}
	logs := captureLogs(t, func() {
		if _, err := ReconcileBuiltinTemplates(context.Background(), fake); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
	if !strings.Contains(logs, "could not read back the shadowing row") {
		t.Errorf("expected a debug log on the read-back error path; logs:\n%s", logs)
	}
	if strings.Contains(logs, "shadowed by a custom row") {
		t.Errorf("a read-back error must not be reported as a shadow; logs:\n%s", logs)
	}
}
