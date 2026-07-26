package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/skilltmpl"
)

// skillReconcilerFake implements builtinSkillReconcilerQueries in memory so the
// default-allocation seed — including the zero-row warning Decision 9 requires —
// is covered without a live database, the same seam
// builtinReconcilerQueries gives the template side.
//
// existing names insert 0 rows (as ON CONFLICT DO NOTHING would). missingTemplates
// names agent templates that do not exist, so a seed against them returns 0 rows:
// exactly what the real query does when the SELECT matches nothing.
type skillReconcilerFake struct {
	existing         map[string]bool
	missingTemplates map[string]bool
	seedErr          error
	inserted         []string
	seeded           []string // "skill->template" for each seed that inserted a row
	attempted        []string // "skill->template" for every seed CALL, hit or miss
}

func (f *skillReconcilerFake) InsertBuiltinSkill(_ context.Context, arg InsertBuiltinSkillParams) (int64, error) {
	if f.existing[arg.Name] {
		return 0, nil
	}
	f.inserted = append(f.inserted, arg.Name)
	return 1, nil
}

func (f *skillReconcilerFake) SeedSharedSkillAllocationByName(_ context.Context, arg SeedSharedSkillAllocationByNameParams) (int64, error) {
	if f.seedErr != nil {
		return 0, f.seedErr
	}
	pair := arg.SkillName + "->" + arg.TemplateName
	f.attempted = append(f.attempted, pair)
	if f.missingTemplates[arg.TemplateName] {
		return 0, nil
	}
	f.seeded = append(f.seeded, pair)
	return 1, nil
}

// reconciled is the ordering token a caller holds after ReconcileBuiltinTemplates.
// Constructed directly here because these tests exercise the skills reconciler in
// isolation; the point of the unexported field is that no package OUTSIDE store
// can do this.
var reconciled = TemplatesReconciled{done: true}

// firstSkillWithDefaults returns a builtin skill name that declares at least one
// default allocation, so the tests below bind to the shipped map rather than to a
// hardcoded name that a rename would leave asserting nothing.
func firstSkillWithDefaults(t *testing.T) (string, []string) {
	t.Helper()
	for _, def := range skilltmpl.Builtins() {
		if targets := skilltmpl.DefaultAllocationsFor(def.Name); len(targets) > 0 {
			return def.Name, targets
		}
	}
	t.Fatal("no builtin skill declares a default allocation; these tests would assert nothing")
	return "", nil
}

func TestReconcileSkillsSeedsDefaultAllocationsOnFirstInsert(t *testing.T) {
	name, targets := firstSkillWithDefaults(t)
	fake := &skillReconcilerFake{}
	if err := ReconcileBuiltinSkills(context.Background(), fake, reconciled); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Every declared target got exactly one seed, addressed by (skill, template).
	for _, tmpl := range targets {
		pair := name + "->" + tmpl
		if got := countString(fake.seeded, pair); got != 1 {
			t.Errorf("expected exactly one seed for %s; got %d (seeded=%v)", pair, got, fake.seeded)
		}
	}
	// And nothing was seeded for a skill that declares no defaults.
	for _, def := range skilltmpl.Builtins() {
		if len(skilltmpl.DefaultAllocationsFor(def.Name)) > 0 {
			continue
		}
		for _, pair := range fake.attempted {
			if strings.HasPrefix(pair, def.Name+"->") {
				t.Errorf("skill %q declares no defaults but a seed was attempted: %s", def.Name, pair)
			}
		}
	}
}

func TestReconcileSkillsDoesNotReseedAnExistingBuiltin(t *testing.T) {
	// The whole reason `ci-cd-norms` needs migration 00083: an already-present
	// row inserts 0, so the reconciler must NOT seed — otherwise a default an admin
	// deleted would come back on the next boot (Decision 9).
	name, _ := firstSkillWithDefaults(t)
	fake := &skillReconcilerFake{existing: map[string]bool{name: true}}
	if err := ReconcileBuiltinSkills(context.Background(), fake, reconciled); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, pair := range fake.attempted {
		if strings.HasPrefix(pair, name+"->") {
			t.Errorf("an existing builtin must not re-seed its default allocation; got %s", pair)
		}
	}
}

func TestReconcileSkillsWarnsWhenTheTargetTemplateIsMissing(t *testing.T) {
	// Decision 9's first failure mode the template analogue does not have: the seed
	// targets a DIFFERENT row, so an absent template inserts nothing and returns no
	// error. Only the row count can see it.
	name, targets := firstSkillWithDefaults(t)
	absent := targets[0]
	fake := &skillReconcilerFake{missingTemplates: map[string]bool{absent: true}}

	logs := captureLogs(t, func() {
		if err := ReconcileBuiltinSkills(context.Background(), fake, reconciled); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})

	if !strings.Contains(logs, "default allocation seeded no row") {
		t.Errorf("expected a zero-row warning; logs:\n%s", logs)
	}
	// The warning must name BOTH coordinates — "which template" is its entire
	// content, and is why the query seeds one template per call.
	if !strings.Contains(logs, `"skill":"`+name+`"`) || !strings.Contains(logs, `"template":"`+absent+`"`) {
		t.Errorf("warning must name the skill and the missing template; logs:\n%s", logs)
	}
	// A miss is not fatal and does not abort the remaining targets.
	for _, tmpl := range targets[1:] {
		if countString(fake.seeded, name+"->"+tmpl) != 1 {
			t.Errorf("a missing template must not stop the other seeds; seeded=%v", fake.seeded)
		}
	}
}

func TestReconcileSkillsIsSilentWhenEverySeedLands(t *testing.T) {
	// Positive control for the test above: the warning string must not be
	// printable by the healthy path, or its presence proves nothing.
	fake := &skillReconcilerFake{}
	logs := captureLogs(t, func() {
		if err := ReconcileBuiltinSkills(context.Background(), fake, reconciled); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
	if strings.Contains(logs, "seeded no row") {
		t.Errorf("a fully-successful reconcile must not warn; logs:\n%s", logs)
	}
}

func TestReconcileSkillsFailsOnSeedError(t *testing.T) {
	fake := &skillReconcilerFake{seedErr: errors.New("boom")}
	err := ReconcileBuiltinSkills(context.Background(), fake, reconciled)
	if err == nil {
		t.Fatal("expected a seed error to fail the reconcile")
	}
	// The message must locate the failure; "boom" alone is not actionable at boot.
	if !strings.Contains(err.Error(), "seed default allocation") {
		t.Errorf("error must say what failed; got %v", err)
	}
}

func TestReconcileSkillsRefusesAZeroOrderingToken(t *testing.T) {
	// The compile-time dependency (main.go cannot call this without a token
	// ReconcileBuiltinTemplates produced) leaves exactly one hole: passing the zero
	// value explicitly. This closes it, and it is the assertion behind Decision 9's
	// "the boot ordering is asserted, not assumed".
	fake := &skillReconcilerFake{}
	err := ReconcileBuiltinSkills(context.Background(), fake, TemplatesReconciled{})
	if err == nil {
		t.Fatal("expected the zero ordering token to be rejected")
	}
	if len(fake.inserted) != 0 {
		t.Errorf("nothing must be inserted before the ordering check; inserted=%v", fake.inserted)
	}
	if !strings.Contains(err.Error(), "not been reconciled") {
		t.Errorf("error must explain the ordering requirement; got %v", err)
	}
}

func TestReconcileTemplatesReturnsAUsableOrderingToken(t *testing.T) {
	// The other end of the same contract: a successful template reconcile produces
	// a token the skills reconciler accepts. Without this, the token could be
	// permanently zero and every boot would fail — a regression the tests above
	// cannot see, since they build the token themselves.
	tmplFake := &reconcilerFake{}
	tok, err := ReconcileBuiltinTemplates(context.Background(), tmplFake)
	if err != nil {
		t.Fatalf("reconcile templates: %v", err)
	}
	if err := ReconcileBuiltinSkills(context.Background(), &skillReconcilerFake{}, tok); err != nil {
		t.Fatalf("the token from a successful template reconcile must be accepted: %v", err)
	}
}

func countString(hay []string, needle string) int {
	n := 0
	for _, s := range hay {
		if s == needle {
			n++
		}
	}
	return n
}
