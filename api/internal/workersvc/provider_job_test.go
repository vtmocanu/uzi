package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_job_test.go is PRD #4322 M1b's database-free seams: the strict job model, the provider job
// claim assembly against fakes (custody, parity with the vendor job, the unmapped refusal and the model
// settings read settled as transient or terminal) and the job limit sentence. The LiveDB twins are in
// provider_job_livedb_test.go.

const (
	pjuPlain = "provider-job-" + "fixture-key"
	// pvJobNoSonnetMap maps opus and haiku only: the default "sonnet" alias is unmapped.
	pvJobNoSonnetMap = `{"opus":{"model":"corp-opus"},"haiku":{"model":"corp-haiku"}}`
)

type jobModelStub struct {
	lane pgtype.Text
	err  error
}

func (s jobModelStub) GetUserHarnessModelDefaults(context.Context, uuid.UUID) (store.GetUserHarnessModelDefaultsRow, error) {
	return store.GetUserHarnessModelDefaultsRow{DefaultClaudeModel: s.lane}, s.err
}

// TestResolveJobModelStrictCanonicalization pins the one canonicalization the create check and the
// claim share (PRD #4322 M1b A5): trim, and an empty or literal "inherit" at ANY level is absent and
// falls through, so the result is never "inherit" and a map target literally named "inherit" can
// never be matched by it.
func TestResolveJobModelStrictCanonicalization(t *testing.T) {
	codexRole := ""
	for m := range agenttmpl.CodexRoleModels {
		codexRole = m
		break
	}
	if codexRole == "" {
		t.Fatal("no Codex role model to exercise")
	}
	svc := &Service{}
	for _, tc := range []struct {
		name string
		run  pgtype.Text
		lane pgtype.Text
		want string
	}{
		{"nothing set falls to the sonnet alias", pgtype.Text{}, pgtype.Text{}, "sonnet"},
		{"run model wins", txt("opus"), txt("haiku"), "opus"},
		{"run model is trimmed", txt("  opus \n"), pgtype.Text{}, "opus"},
		{"run inherit falls to the lane", txt("inherit"), txt("opus"), "opus"},
		{"run inherit with padding falls to the lane", txt(" inherit "), txt("opus"), "opus"},
		{"lane inherit falls to sonnet", pgtype.Text{}, txt("inherit"), "sonnet"},
		{"both inherit fall to sonnet", txt("inherit"), txt("inherit"), "sonnet"},
		{"empty run model falls to the lane", txt("   "), txt("haiku"), "haiku"},
		{"a Codex role token in the lane is dropped", pgtype.Text{}, txt(codexRole), "sonnet"},
		{"an incompatible run model falls to the lane", txt(codexRole), txt("opus"), "opus"},
		{"a custom full id passes through", pgtype.Text{}, txt("claude-opus-4-8"), "claude-opus-4-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := store.Run{ID: uuid.New(), UserID: uuid.New(), Harness: harnessClaude, Model: tc.run}
			got, err := svc.resolveJobModel(context.Background(), jobModelStub{lane: tc.lane}, run, true)
			if err != nil || got == nil || *got != tc.want {
				t.Fatalf("resolveJobModel = (%v, %v), want %q", got, err, tc.want)
			}
			if *got == "inherit" {
				t.Fatal("the canonical model is never inherit")
			}
		})
	}
	boom := errors.New("lane read failed")
	if got, err := svc.resolveJobModel(context.Background(), jobModelStub{err: boom}, store.Run{Model: txt("opus")}, true); !errors.Is(err, boom) || got != nil {
		t.Fatalf("a strict read error = (%v, %v), want the error and no guessed model", got, err)
	}
}

// TestResolveJobModelVendorPostureUnchanged: the vendor job keeps its best-effort read, its lane-then-run
// precedence and NO fallback model (the runner uses its own default).
func TestResolveJobModelVendorPostureUnchanged(t *testing.T) {
	svc := &Service{}
	run := store.Run{ID: uuid.New(), UserID: uuid.New(), Harness: harnessClaude}
	if got, err := svc.resolveJobModel(context.Background(), jobModelStub{}, run, false); err != nil || got != nil {
		t.Fatalf("no lane, no model = (%v, %v), want nil and no error", got, err)
	}
	if got, _ := svc.resolveJobModel(context.Background(), jobModelStub{lane: txt("inherit")}, run, false); got == nil || *got != "inherit" {
		t.Fatalf("the vendor lane value is passed through unchanged, got %v", got)
	}
	run.Model = txt("opus")
	if got, _ := svc.resolveJobModel(context.Background(), jobModelStub{lane: txt("haiku")}, run, false); got == nil || *got != "opus" {
		t.Fatalf("a compatible run model overrides the lane, got %v", got)
	}
	if got, err := svc.resolveJobModel(context.Background(), jobModelStub{err: errors.New("boom"), lane: txt("haiku")}, run, false); err != nil || got == nil || *got != "opus" {
		t.Fatalf("a vendor read error is tolerated, got (%v, %v)", got, err)
	}
}

// providerJobStore is the fake a provider or vendor job assembles against: the matrix store's binding
// reads and key open, plus the job reads.
type providerJobStore struct {
	*providerClaimMatrixStore
	inputs []store.ListJobInputsForClaimRow
	files  []store.ListJobInputFilesForClaimRow
	skills []store.ListProductSkillsForRunRow
	clears int
}

func (s *providerJobStore) ListProductSkillsForRun(context.Context, uuid.UUID) ([]store.ListProductSkillsForRunRow, error) {
	return s.skills, nil
}

func (s *providerJobStore) ClearJobResultForRun(context.Context, uuid.UUID) error {
	s.clears++
	return nil
}

func (s *providerJobStore) ListJobInputsForClaim(context.Context, uuid.UUID) ([]store.ListJobInputsForClaimRow, error) {
	return s.inputs, nil
}

func (s *providerJobStore) ListJobInputFilesForClaim(context.Context, pgtype.UUID) ([]store.ListJobInputFilesForClaimRow, error) {
	return s.files, nil
}

// vendorJobStore is the vendor job's fake: the plain fake store (the vendor ladder reads its token)
// with the same job reads.
type vendorJobStore struct {
	*fakeStore
	inputs []store.ListJobInputsForClaimRow
	files  []store.ListJobInputFilesForClaimRow
	skills []store.ListProductSkillsForRunRow
}

func (s *vendorJobStore) ListProductSkillsForRun(context.Context, uuid.UUID) ([]store.ListProductSkillsForRunRow, error) {
	return s.skills, nil
}

func (s *vendorJobStore) ClearJobResultForRun(context.Context, uuid.UUID) error { return nil }
func (s *vendorJobStore) ListJobInputsForClaim(context.Context, uuid.UUID) ([]store.ListJobInputsForClaimRow, error) {
	return s.inputs, nil
}

func (s *vendorJobStore) ListJobInputFilesForClaim(context.Context, pgtype.UUID) ([]store.ListJobInputFilesForClaimRow, error) {
	return s.files, nil
}

// vendorTwin is the same job without its binding, over a service whose store holds a vendor token and
// the provider fixture's job reads and settings.
func (fx *providerJobFixture) vendorTwin(t *testing.T) (*Service, store.Run, store.Worker) {
	t.Helper()
	box := newBox(t)
	token, err := box.Seal([]byte("fixture-vendor-token"))
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{anthropic: token, defaultEffort: fx.store.defaultEffort, defaultModel: fx.store.defaultModel, defaultModelErr: fx.store.defaultModelErr}
	svc := newOffModeService(&vendorJobStore{fakeStore: fs, inputs: fx.store.inputs, files: fx.store.files, skills: fx.store.skills}, box, fx.svc.p)
	vendor := fx.run
	vendor.ModelProviderID, vendor.ModelProviderRevision, vendor.ModelProviderKeyMode = pgtype.UUID{}, pgtype.Int4{}, pgtype.Text{}
	vendor.ModelProviderKeySecretID, vendor.ModelProviderKeyRev = pgtype.UUID{}, pgtype.Int8{}
	return svc, vendor, store.Worker{ID: fx.worker.ID, UserID: vendor.UserID}
}

type providerJobFixture struct {
	svc    *Service
	store  *providerJobStore
	run    store.Run
	worker store.Worker
	ctx    context.Context
}

var providerJobWorkerCaps = []string{capability.ClaudeProviderV1, capability.ClaudeProviderJobV1}

// newProviderJobFixture builds a provider-bound Claude job over a gateway key that is really sealed.
func newProviderJobFixture(t *testing.T, modelMap string) *providerJobFixture {
	t.Helper()
	box := newBox(t)
	run := providerBoundRow(runkind.Job)
	run.JobType = pgconv.Text("research")
	run.IssueTitle, run.IssueDescription = "Summarize", "read the inputs"
	sealed, err := box.Seal([]byte(pjuPlain))
	if err != nil {
		t.Fatal(err)
	}
	token, err := box.Seal([]byte("fixture-vendor-token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &providerJobStore{providerClaimMatrixStore: &providerClaimMatrixStore{
		fakeStore: &fakeStore{claimRun: run, anthropic: token},
		rev: store.GetModelProviderRevisionForOwnerRow{ProviderID: uuid.UUID(run.ModelProviderID.Bytes), Revision: 1, Engine: harnessClaude,
			Protocol: providers.ProtocolAnth, AuthStyle: "bearer", BaseUrl: pvBase, ModelMap: []byte(modelMap)},
		key:    store.GetProviderBindingKeyByIDRow{Kind: providerKeyKind(harnessClaude), ProviderID: run.ModelProviderID, EnablementRev: 1},
		cipher: store.GetUserSecretCiphertextByIDRow{ID: uuid.UUID(run.ModelProviderKeySecretID.Bytes), UserID: run.UserID, Kind: providerKeyKind(harnessClaude), SealedWith: store.SealedWithMaster, Ciphertext: sealed},
	}}
	svc := newOffModeService(f, box, testParams())
	svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	return &providerJobFixture{
		svc: svc, store: f, run: run,
		worker: store.Worker{ID: uuid.New(), UserID: run.UserID, ProtocolCapabilities: providerJobWorkerCaps},
		ctx:    context.Background(),
	}
}

// TestProviderJobClaimAssemblesOnlyTheFrozenBinding: the claim carries exactly one provider binding and
// no vendor credential, opens the frozen key once, and never touches the vendor ladder or its journal.
func TestProviderJobClaimAssemblesOnlyTheFrozenBinding(t *testing.T) {
	fx := newProviderJobFixture(t, pvDefaultMap)
	fx.store.inputs = []store.ListJobInputsForClaimRow{{Name: "notes.md", ContentMd: "alpha"}}
	payload, err := fx.svc.assembleJobClaim(fx.ctx, fx.worker, fx.run)
	if err != nil || payload == nil {
		t.Fatalf("assembleJobClaim = (%v, %v)", payload != nil, err)
	}
	b := payload.Secrets.Provider
	if b == nil || payload.Secrets.AnthropicOAuthToken != "" || payload.Secrets.Codex != nil || payload.Secrets.ForgePAT != "" {
		t.Fatalf("secrets = %+v, want only a provider binding", payload.Secrets)
	}
	if b.Credential != pjuPlain || b.Engine != harnessClaude || b.BaseURL != pvBase || b.ModelMap["sonnet"].Model != "corp-sonnet" {
		t.Fatalf("binding = %+v", b)
	}
	if fx.store.opens != 1 {
		t.Fatalf("secret ciphertext reads = %d, want exactly the frozen key's", fx.store.opens)
	}
	if n := len(fx.store.recordedCreds) + len(fx.store.defaultMetaLookups); n != 0 {
		t.Fatalf("a provider job touched the vendor credential ladder %d times", n)
	}
	if payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != "sonnet" {
		t.Fatalf("default_model = %v, want the mapped sonnet alias", payload.Config.DefaultModel)
	}
	if payload.Job == nil || len(payload.Job.Inputs) != 1 || payload.Job.Inputs[0].Name != "notes.md" {
		t.Fatalf("the job block lost its inputs: %+v", payload.Job)
	}
	if fx.store.clears != 1 {
		t.Fatalf("earlier-result clears = %d, want 1", fx.store.clears)
	}
}

// TestProviderJobClaimParityWithVendorJob: apart from the secrets and the canonical model, a provider
// job's claim is byte-identical to a vendor job's over the same inputs, files, effort and wall.
func TestProviderJobClaimParityWithVendorJob(t *testing.T) {
	fx := newProviderJobFixture(t, pvDefaultMap)
	fx.store.inputs = []store.ListJobInputsForClaimRow{{Name: "a.md", ContentMd: "alpha"}, {Name: "b.md", ContentMd: "beta"}}
	fx.store.files = []store.ListJobInputFilesForClaimRow{{
		ID: uuid.New(), DisplayName: "data.csv", ByteSize: 12,
		StorageName: txt("abc.csv"), Sha256: txt("abc"), ContentType: txt("text/csv"),
	}}
	fx.store.defaultEffort = txt("high")
	// An approved product origin: two delivered skills (served in name order) and one dropped for size.
	fx.svc.p.ProductSkillsEnabled = true
	fx.store.skills = []store.ListProductSkillsForRunRow{
		{ID: uuid.New(), Name: "zeta", Description: "zeta description.", Body: "z body"},
		{ID: uuid.New(), Name: "alpha", Description: "alpha description.", Body: "a body"},
		{ID: uuid.New(), Name: "huge", Description: "too big.", Body: strings.Repeat("x", fx.svc.p.SkillMaxBytes+1)},
	}
	wall := int32(900)
	fx.run.BudgetWallSeconds = pgtype.Int4{Int32: wall, Valid: true}
	vendorSvc, vendor, vendorWorker := fx.vendorTwin(t)

	pp, err := fx.svc.assembleJobClaim(fx.ctx, fx.worker, fx.run)
	if err != nil {
		t.Fatalf("provider claim: %v", err)
	}
	vp, err := vendorSvc.assembleJobClaim(fx.ctx, vendorWorker, vendor)
	if err != nil {
		t.Fatalf("vendor claim: %v", err)
	}
	if vp.Secrets.AnthropicOAuthToken == "" || vp.Secrets.Provider != nil {
		t.Fatalf("the vendor control did not take the vendor path: %+v", vp.Secrets)
	}
	if pp.Secrets.Provider == nil || pp.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("the provider claim did not take the provider path: %+v", pp.Secrets)
	}
	for name, p := range map[string]*ClaimPayload{"provider": pp, "vendor": vp} {
		if len(p.Skills) != 2 || p.Skills[0].Name != "alpha" || p.Skills[1].Name != "zeta" || len(p.SkillsDropped) != 1 || p.SkillsDropped[0].Name != "huge" || p.SkillsDropped[0].Reason != DropTooLarge {
			t.Fatalf("%s claim skills = %+v drops = %+v, want alpha+zeta delivered and huge dropped", name, p.Skills, p.SkillsDropped)
		}
	}
	pp.RunID, vp.RunID = "", ""
	pp.Secrets, vp.Secrets = ClaimSecrets{}, ClaimSecrets{}
	if vp.Config.DefaultModel != nil {
		t.Fatalf("the vendor control carries a default model %v; the parity premise needs none", *vp.Config.DefaultModel)
	}
	if pp.Config.DefaultModel == nil || *pp.Config.DefaultModel != "sonnet" {
		t.Fatalf("provider default model = %v", pp.Config.DefaultModel)
	}
	pp.Config.DefaultModel = nil
	pj, _ := json.Marshal(pp)
	vj, _ := json.Marshal(vp)
	if string(pj) != string(vj) {
		t.Fatalf("provider and vendor job claims differ beyond secrets and the canonical model:\nprovider %s\nvendor   %s", pj, vj)
	}
}

// TestProviderJobClaimModelPrecedence: the claim's model is the same canonical model the create check
// validated, from the frozen run model, else the owner's lane, else sonnet, and an unmapped one is the
// typed terminal refusal with nothing delivered.
func TestProviderJobClaimModelPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modelMap string
		run      pgtype.Text
		lane     pgtype.Text
		want     string
	}{
		{"run inherit, mapped owner default", pvDefaultMap, txt("inherit"), txt("opus"), "opus"},
		{"sonnet target literally inherit, no model", `{"opus":{"model":"corp-opus"},"sonnet":{"model":"inherit"},"haiku":{"model":"corp-haiku"}}`, pgtype.Text{}, pgtype.Text{}, "sonnet"},
		{"opus maps to inherit, run inherit, no default: not opus", `{"opus":{"model":"inherit"},"sonnet":{"model":"x"},"haiku":{"model":"corp-haiku"}}`, txt("inherit"), pgtype.Text{}, "sonnet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newProviderJobFixture(t, tc.modelMap)
			fx.run.Model = tc.run
			fx.store.defaultModel = tc.lane
			payload, err := fx.svc.assembleJobClaim(fx.ctx, fx.worker, fx.run)
			if err != nil || payload.Config.DefaultModel == nil || *payload.Config.DefaultModel != tc.want {
				t.Fatalf("assembleJobClaim default model = (%v, %v), want %q", payload, err, tc.want)
			}
		})
	}
	fx := newProviderJobFixture(t, pvJobNoSonnetMap)
	payload, err := fx.svc.assembleJobClaim(fx.ctx, fx.worker, fx.run)
	var refusal *providerClaimRefusal
	if payload != nil || !errors.As(err, &refusal) || refusal.Reason != holdProviderModelUnmapped || !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("unmapped default = (%v, %v), want the typed provider_model_unmapped refusal and no payload", payload != nil, err)
	}
	fx = newProviderJobFixture(t, pvDefaultMap)
	fx.store.defaultModel = txt("claude-not-in-the-map")
	if _, err := fx.svc.assembleJobClaim(fx.ctx, fx.worker, fx.run); !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("an unmapped owner default: err = %v, want provider_model_unmapped", err)
	}
}

// TestProviderJobClaimUnmappedModelRefusalNamesNoModel: the owner's default model is any single-token
// string, so a pasted credential must never reach the refusal text (it becomes failure_reason).
func TestProviderJobClaimUnmappedModelRefusalNamesNoModel(t *testing.T) {
	secretShaped := strings.Join([]string{"sk", "proj", "abcdefghijklmnopqrstuvwxyz0123456789"}, "-")
	fx := newProviderJobFixture(t, pvDefaultMap)
	fx.store.defaultModel = txt(secretShaped)
	_, err := fx.svc.assembleJobClaim(fx.ctx, fx.worker, fx.run)
	if !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("err = %v, want provider_model_unmapped", err)
	}
	if strings.Contains(err.Error(), secretShaped) || strings.Contains(err.Error(), secretShaped[:12]) {
		t.Fatalf("the refusal text %q carries the model value", err.Error())
	}
}

// TestProviderJobClaimModelReadFailure: a transient database error while reading the model settings
// leaves the claim for the never-started sweep (no settlement, no payload), while any other error is the
// terminal credential_unavailable with a FIXED sentence that carries none of the raw error.
func TestProviderJobClaimModelReadFailure(t *testing.T) {
	t.Run("transient: wrapped, classifiable, claim untouched", func(t *testing.T) {
		fx := newProviderJobFixture(t, pvDefaultMap)
		pgErr := &pgconn.PgError{Code: "40001", Message: "could not serialize access"}
		fx.store.defaultModelErr = pgErr
		payload, err := fx.svc.Claim(fx.ctx, fx.worker, nil)
		if payload != nil || err == nil {
			t.Fatalf("Claim = (%v, %v), want an error and no payload", payload != nil, err)
		}
		var got *pgconn.PgError
		if !errors.As(err, &got) || got != pgErr || !isTransientClaimDBError(err) {
			t.Fatalf("err = %v, want the wrapped serialization failure that finishRunClaim treats as transient", err)
		}
		if claimAssemblyTerminal(errors.New("x")) || !errors.Is(err, errCredentialUnavailable) {
			t.Fatalf("err = %v, want it to keep the credential_unavailable family", err)
		}
		// The claim stays exactly as ClaimRun left it: no fenced terminal, requeue or park was written.
		if fs := fx.store.fakeStore; fs.claimFailed != nil || fs.claimRequeued != nil || fs.claimParked != nil || fs.markedFailed != nil || fs.requeuedRun != nil || fs.claimCredParked != nil {
			t.Fatalf("a transient model-read error wrote a settlement: failed=%+v requeued=%+v parked=%+v marked=%+v", fs.claimFailed, fs.claimRequeued, fs.claimParked, fs.markedFailed)
		}
	})
	t.Run("non-transient: terminal credential_unavailable with a fixed reason", func(t *testing.T) {
		fx := newProviderJobFixture(t, pvDefaultMap)
		fx.store.defaultModelErr = errors.New("relation users: secret-raw-detail-7731")
		payload, cerr := fx.svc.Claim(fx.ctx, fx.worker, nil)
		if payload != nil || cerr != nil {
			t.Fatalf("Claim = (%v, %v), want idle after the terminal settlement", payload != nil, cerr)
		}
		got := fx.store.claimFailed
		if got == nil || got.FailOrigin.String != "credential_unavailable" {
			t.Fatalf("fenced terminal = %+v, want origin credential_unavailable", got)
		}
		if got.FailureReason.String == "" || strings.Contains(got.FailureReason.String, "secret-raw-detail-7731") || !strings.HasSuffix(got.FailureReason.String, "the job's model settings could not be read") {
			t.Fatalf("stored failure_reason %q must be the fixed sentence without the raw error", got.FailureReason.String)
		}
		_, err := fx.svc.assembleJobClaim(fx.ctx, fx.worker, fx.run)
		if err == nil || isTransientClaimDBError(err) || claimAssemblyOrigin(err) != "credential_unavailable" {
			t.Fatalf("err = %v, want a terminal credential_unavailable", err)
		}
		if strings.Contains(err.Error(), "secret-raw-detail-7731") || !strings.HasSuffix(err.Error(), "the job's model settings could not be read") {
			t.Fatalf("stored reason %q must be the fixed sentence without the raw error", err)
		}
	})
	t.Run("vendor job keeps the best-effort read", func(t *testing.T) {
		fx := newProviderJobFixture(t, pvDefaultMap)
		fx.store.defaultModelErr = errors.New("boom")
		vendorSvc, vendor, vendorWorker := fx.vendorTwin(t)
		payload, err := vendorSvc.assembleJobClaim(fx.ctx, vendorWorker, vendor)
		if err != nil || payload == nil || payload.Config.DefaultModel != nil {
			t.Fatalf("vendor job = (%v, %v), want a claim with no default model despite the read error", payload != nil, err)
		}
	})
}

// TestProviderJobLimitReasonIsTheJobSentence: a provider-bound job that reports any limit fact is
// stored with the job sentence (never the vendor window text, never the judge's), while a vendor job, a
// provider judge and a provider job's plain failure keep their own paths.
func TestProviderJobLimitReasonIsTheJobSentence(t *testing.T) {
	typ := "five_hour"
	limited := StateRequest{State: "failed", RateLimitType: &typ, FailureReason: strPtr("boom")}
	provider := func(kind string) store.Run { return providerBoundRow(kind) }
	if got := failedReportReason(provider(runkind.Job), limited); got.String != providerJobGatewayLimitReason {
		t.Fatalf("provider job limit reason = %q, want %q", got.String, providerJobGatewayLimitReason)
	}
	if !strings.Contains(providerJobGatewayLimitReason, "provider job does not wait") {
		t.Fatalf("the job sentence must say a provider job does not wait: %q", providerJobGatewayLimitReason)
	}
	if got := failedReportReason(provider(runkind.Judge), limited); got.String != providerJudgeGatewayLimitReason {
		t.Fatalf("provider judge limit reason = %q, want the judge sentence", got.String)
	}
	vendor := store.Run{ID: uuid.New(), Kind: runkind.Job, Harness: harnessClaude}
	if got := failedReportReason(vendor, limited); got.String == providerJobGatewayLimitReason || got.String == providerJudgeGatewayLimitReason {
		t.Fatalf("a vendor job took a provider sentence: %q", got.String)
	}
	plain := StateRequest{State: "failed", FailureReason: strPtr("the runner exited")}
	if got := failedReportReason(provider(runkind.Job), plain); got.String == providerJobGatewayLimitReason {
		t.Fatalf("a provider job's plain failure took the limit sentence: %q", got.String)
	}
}
