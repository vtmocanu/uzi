package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These are constructor/input persistence tests, not provider claim or delivery proofs.
// They use the existing LiveDB fixtures; without UZI_TEST_DATABASE_URL they skip.
func TestCodexProviderFreshConstructorPathsLiveDB(t *testing.T) {
	for _, path := range []string{"manual-ci-fix", "auto-ci-fix", "handoff"} {
		t.Run(path, func(t *testing.T) {
			for _, change := range []string{"changed", "cleared", "disabled"} {
				t.Run(change, func(t *testing.T) {
					fx := newProvFix(t)
					fx.codexDefault() // Vendor remains available even in the refusal case.
					seedCodexSource(t, fx)
					source, err := fx.svc.CreateTaskRun(fx.env.ctx, fx.userID, fx.repoID, "source A", "main", false, false, false, false, nil)
					if err != nil {
						t.Fatal(err)
					}
					source = mustRun(t, fx.env, source.ID)
					wantCodexBinding(t, fx, source, 1, fx.keyRev())
					fx.cancelRun(source.ID)
					seedCodexSource(t, fx) // B is distinct from the earlier run's A.
					if fx.provider == uuid.UUID(source.ModelProviderID.Bytes) {
						t.Fatal("fixture did not change provider")
					}
					switch change {
					case "cleared":
						fx.env.exec("UPDATE users SET codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1", fx.userID)
					case "disabled":
						fx.env.exec("UPDATE user_secrets SET disabled_at=now() WHERE id=$1", fx.key)
					}
					if path == "auto-ci-fix" {
						// The poller needs a qualifying run on this branch. The cancelled task
						// above retains binding A but cannot serve as that candidate.
						const mrIID int64 = 900
						ref := source.Branch.String
						seedRecordedPR(t, fx.env, fx.userID, fx.repoID, ref, mrIID, time.Now())
						fx.svc.SetForges(bindingBuilder{&bindingForge{answers: map[int64]bindingAnswer{
							mrIID: bindSum(mrIID, forge.MRStateOpened, ref),
						}}})
					}
					before := fx.runCount()
					var got store.Run
					switch path {
					case "manual-ci-fix":
						got, err = fx.svc.CreateCIFixRun(fx.env.ctx, fx.userID, fx.repoID, source.Branch.String, "fix", "d", sampleSnapshot(), nil)
					case "auto-ci-fix":
						got, err = fx.svc.CreateAutoCIFixRun(fx.env.ctx, fx.userID, fx.repoID, source.Branch.String, "fix", "d", sampleSnapshot(), nil)
					case "handoff":
						got, err = fx.svc.CreateTaskRun(fx.env.ctx, fx.userID, fx.repoID, "fresh task", "main", false, false, false, false, nil)
					}
					if change == "disabled" {
						if !errors.Is(err, ErrProviderSourceUnusable) {
							t.Fatalf("unusable B with vendor available: %v", err)
						}
						if fx.runCount() != before {
							t.Fatal("refusal created a run")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					got = mustRun(t, fx.env, got.ID)
					if fx.runCount() != before+1 {
						t.Fatal("constructor did not create exactly one run")
					}
					if change == "cleared" {
						fx.wantVendor(got)
						if got.Harness != "codex" || !got.CodexSecretID.Valid {
							t.Fatal("cleared source did not freeze vendor Codex credential")
						}
					} else {
						wantCodexBinding(t, fx, got, 1, fx.keyRev())
					}
					if bindingOfRun(mustRun(t, fx.env, source.ID)) != bindingOfRun(source) {
						t.Fatal("fresh constructor changed source A")
					}
				})
			}
		})
	}
}

func TestCodexProviderReliableSuccessorPathsLiveDB(t *testing.T) {
	for _, change := range []string{"changed", "cleared"} {
		t.Run(change, func(t *testing.T) {
			fx := newProvFix(t)
			fx.codexDefault()
			seedCodexSource(t, fx)
			source, err := fx.svc.CreateTaskRun(fx.env.ctx, fx.userID, fx.repoID, "source A", "main", false, false, false, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			source = mustRun(t, fx.env, source.ID)
			wantCodexBinding(t, fx, source, 1, fx.keyRev())
			fx.cancelRun(source.ID)
			seedCodexSource(t, fx)
			if change == "cleared" {
				fx.env.exec("UPDATE users SET codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1", fx.userID)
			}
			constructors := []struct {
				name   string
				create func() (store.Run, error)
			}{
				{"mr-rework", func() (store.Run, error) {
					return fx.svc.CreateAutoMRReworkRun(fx.env.ctx, fx.userID, fx.repoID, source.Branch.String, 900, source.ID, "rework", "d", sampleReviewSnapshot())
				}},
				{"task-review", func() (store.Run, error) {
					return fx.svc.CreateTaskReviewRun(fx.env.ctx, fx.userID, fx.repoID, source.ID, source.Branch.String, "main", HarnessCodex)
				}},
				{"then-fix", func() (store.Run, error) {
					return fx.svc.CreateThenFixRun(fx.env.ctx, fx.userID, fx.repoID, source.ID, source.Branch.String, "main", "fix", pgtype.Int4{}, pgtype.Int4{}, HarnessCodex)
				}},
			}
			for _, constructor := range constructors {
				t.Run(constructor.name, func(t *testing.T) {
					child, err := constructor.create()
					if err != nil {
						t.Fatal(err)
					}
					child = mustRun(t, fx.env, child.ID)
					if child.Harness != "codex" || bindingOfRun(child) != bindingOfRun(source) || child.CodexSecretID.Valid || child.CodexAuthMode.Valid {
						t.Fatalf("successor did not inherit A: %+v", bindingOfRun(child))
					}
					fx.cancelRun(child.ID)
				})
			}
		})
	}
}

func TestCodexProviderSameRunFollowupPathLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.codexDefault()
	seedCodexSource(t, fx)
	source, err := fx.svc.CreateTaskRun(fx.env.ctx, fx.userID, fx.repoID, "source A", "main", false, false, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	source = mustRun(t, fx.env, source.ID)
	wantCodexBinding(t, fx, source, 1, fx.keyRev())
	seedCodexSource(t, fx)
	before := fx.runCount()
	input, err := fx.svc.SubmitInput(fx.env.ctx, fx.userID, source.ID, "follow_up", "keep working", nil)
	if err != nil {
		t.Fatal(err)
	}
	var runID uuid.UUID
	var kind, body string
	if err := fx.env.pool.QueryRow(fx.env.ctx, "SELECT run_id, kind, body FROM run_user_inputs WHERE id=$1", input.ID).Scan(&runID, &kind, &body); err != nil {
		t.Fatal(err)
	}
	if runID != source.ID || kind != "follow_up" || body != "keep working" {
		t.Fatal("follow-up was not persisted on the original run")
	}
	got := mustRun(t, fx.env, source.ID)
	if fx.runCount() != before || got.Harness != "codex" || bindingOfRun(got) != bindingOfRun(source) {
		t.Fatal("same-run follow-up changed source A or created a successor")
	}
}
