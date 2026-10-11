package apitypes

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes/apitypestest"
)

// The api ⇄ SPA JSON wire-contract (PRD #982). This is the GO HALF; the vitest
// half is web/src/lib/apiContract.test.ts. Neither reads the other: each side
// checks the SAME recorded fixtures with its OWN production definition (this Go
// struct, the TS type), so a failure names the side that drifted. The precedent
// is fixtures/run-usage — but that pins a fold; this pins the wire SHAPE.
//
// Per DTO two fixtures under fixtures/api-contract/:
//
//	<stem>.zero.json  == json.MarshalIndent(T{})            — nullability surface
//	<stem>.full.json  == json.MarshalIndent(populate(T{}))  — key set + value kinds
//
// The populator (apitypestest.Populate) sets every field non-zero so omitempty
// fields are present, which is what makes the key-set check on the TS side total.
//
// 🔴 THE FIXTURES ARE RECORDED, NOT AUTHORED, AND THERE IS NO -update FLAG. On a
// mismatch this test prints the FULL marshaled JSON so a deliberate wire change
// is a copy-paste into the fixture — a golden any run can rewrite is a snapshot,
// and a snapshot of a regression is green (the fixtures/run-usage house rule).
//
// 🔴 RUN THIS PACKAGE WITH -count=1 AFTER A FIXTURE-ONLY EDIT. fixtures/ sits
// ABOVE api/, so every byte of a fixture is outside this module and contributes
// NOTHING to this package's cache key: a fixture-only edit leaves `go test`
// printing "ok (cached)". The vitest half has no such cache. See the README.
const contractFixtureDir = "../../../fixtures/api-contract"

// contractCase is one DTO's contract row. zero/full are the recorded values;
// decode round-trips full.json back through the struct with DisallowUnknownFields,
// the request-body direction that catches the runtime-400 class the PRD names.
type contractCase struct {
	name   string
	zero   any
	full   any
	decode func([]byte) (any, error)
}

// newContractCase builds a case for DTO type T: the zero value, a fully populated
// value, and a strict decoder. Generic so adding a DTO is one line.
func newContractCase[T any](name string) contractCase {
	var zero T
	var full T
	apitypestest.Populate(&full)
	return contractCase{
		name: name,
		zero: zero,
		full: full,
		decode: func(b []byte) (any, error) {
			var v T
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			return v, nil
		},
	}
}

func contractCases() []contractCase {
	return []contractCase{
		newContractCase[BuildInfoDTO]("build_info"),
		newContractCase[ReleaseCheckStatusDTO]("release_check_status"),
		newContractCase[RunDTO]("run"),
		newContractCase[RunListItemDTO]("run_list_item"),
		newContractCase[RunSummaryItemDTO]("run_summary_item"),
		// M2 — the rest of the apitypes hot set.
		newContractCase[RepoDTO]("repo"),
		newContractCase[AdminDockerAllowlistRepoDTO]("admin_docker_allowlist_repo"),
		newContractCase[AdminDockerAllowlistReposDTO]("admin_docker_allowlist_repos"),
		newContractCase[MessageDTO]("message"),
		newContractCase[ScheduleDTO]("schedule"),
		newContractCase[RunNowResponse]("schedule_run_now"),
		// ScheduleRequest is a REQUEST body: its full.json round-trips through
		// DisallowUnknownFields (the runtime-400 class the PRD names) like every other row.
		newContractCase[ScheduleRequest]("schedule_input"),
		// The pause-all singleton response (PRD #1093 M2): an exported DTO on a
		// RequireUser route (GET/PUT/DELETE /api/schedules/pause).
		newContractCase[SchedulePauseDTO]("schedule_pause"),
		newContractCase[WorkerDTO]("worker"),
		newContractCase[AdminWorkerDTO]("admin_worker"),
		newContractCase[UserDTO]("user"),
		newContractCase[AgentMemoryDTO]("agent_memory"),
		newContractCase[SecretDTO]("secret"),
		newContractCase[UsageDTO]("usage"),
		newContractCase[AdminUserUsageDTO]("admin_user_usage"),
		newContractCase[AdminUsageDTO]("admin_usage"),
		newContractCase[RunOutcomesDTO]("run_outcomes"),
		newContractCase[UserSettingsDTO]("user_settings"),
		newContractCase[CatalogEntryDTO]("catalog_entry"),
		newContractCase[AdminCLITokenDTO]("cli_token"),
		// PRD #1183 M3: the first fixture pair for the Findings backlog row, so its Go/TS
		// mirror is guarded from now on. Every nullable field on IncidentalFindingDTO is
		// omitempty (finding_id, filed_issue_iid, resolved_at, …), so its zero.json carries
		// NO null — registered {stem:"finding", nullable:false} on the TS side, the same
		// shape as AgentMemoryDTO. The zero-fixture check stays non-vacuous by pinning the
		// always-present key set.
		newContractCase[IncidentalFindingDTO]("finding"),
		// PRD #1255 M2a: the forge-view read DTOs (pulls list + drill-in, CI runs list +
		// drill-in). PullDetailDTO embeds PullDTO and CIRunDetailDTO embeds CIRunDTO, so
		// their full fixtures carry the embedded scalars inline (like RunListItemDTO).
		newContractCase[PullDTO]("pull"),
		newContractCase[PullDetailDTO]("pull_detail"),
		newContractCase[CheckDTO]("check"),
		newContractCase[PullReviewDTO]("pull_review"),
		newContractCase[MergeStateDTO]("merge_state"),
		newContractCase[CIRunDTO]("ci_run"),
		newContractCase[CIRunDetailDTO]("ci_run_detail"),
		newContractCase[CIJobDTO]("ci_job"),
		newContractCase[CIStepDTO]("ci_step"),
		// PRD #1296 M1: the owner-facing recovery DTOs. RecoveryArchiveDTO is all-omitempty
		// optionals (its zero.json carries no null, the finding shape); the summary's
		// archives slice is non-omitempty (its zero.json carries a null the mapper
		// normalizes to []). Their nested RecoveryArchiveStateCountsDTO rides inside the
		// summary fixtures (no standalone row — it is never returned alone).
		newContractCase[RecoveryArchiveDTO]("recovery_archive"),
		newContractCase[RecoveryArchiveSummaryDTO]("recovery_archive_summary"),
		// PRD #1349 M1: the owner-facing custody-hold DTOs. RecoveryCustodyHoldDTO's only
		// nullable field is released_at (*time.Time omitempty, dropped on the zero value) and
		// its worker_name/capture_state are omitempty strings, so its zero.json carries NO
		// null (the finding shape). RecoveryCustodyAggregateDTO is all ints (no null).
		// RecoveryCustodyHoldsDTO's Holds slice is non-omitempty, so its zero.json carries a
		// null the mapper normalizes to [] (the recovery_archive_summary shape).
		newContractCase[RecoveryCustodyHoldDTO]("recovery_custody_hold"),
		newContractCase[RecoveryCustodyAggregateDTO]("recovery_custody_aggregate"),
		newContractCase[RecoveryCustodyHoldsDTO]("recovery_custody_holds"),
		// PRD #1432 M1: the admin cross-user guardrail override-request row. Its
		// Findings slice is non-omitempty (its zero.json carries a null the handler
		// normalizes to []); every other field is a required scalar, so its zero.json
		// carries no other null.
		newContractCase[GuardrailOverrideRequestDTO]("guardrail_override_request"),
		// PRD #1209 M1: the Codex per-account meter and its admin row. The nested window
		// and bucket DTOs ride inside these two fixtures (no standalone row — they are
		// never returned alone). CodexAccountRateLimitDTO's aliases + buckets slices are
		// non-omitempty, so its zero.json carries the nil-slice nulls the M3 mapper
		// normalizes to []; last_success_at/stale are omitempty and drop on the zero value.
		newContractCase[CodexAccountRateLimitDTO]("codex_account_rate_limit"),
		newContractCase[CodexAdminRateLimitRowDTO]("codex_admin_rate_limit_row"),
		// PRD #1484 M1: the admin-health document. Its Checks slice is non-omitempty (its
		// zero.json carries a null the registry normalizes to []); snoozed_until and
		// episode_id are nullable pointers present-as-null on the zero value; counts is a
		// nested all-int struct (no null). The nested HealthCheckDTO / HealthEvidenceDTO
		// ride inside the full fixture's checks[0], no standalone row (never returned alone).
		newContractCase[HealthDocDTO]("health_doc"),
		// PRD #1907 M1: the product registry, product tokens and the /api/v1 whoami
		// seam. ProductDTO's deleted_at, ProductTokenDTO's last_used_*/expires_at and
		// V1WhoamiDTO's product are present-as-null pointers; the scopes slices are
		// non-omitempty (null in zero.json, never null on the real wire: the column is
		// NOT NULL and non-empty). AdminProductTokenDTO embeds ProductTokenDTO, so its
		// fixture carries the row's keys inline; MintProductTokenResponse nests it.
		newContractCase[ProductDTO]("product"),
		// PRD #1910 M1: the product's nested oauth_client (rotated_at is a present-as-null
		// pointer; the redirect_uris/scopes slices are non-omitempty, null in zero.json and
		// never null on the real wire) and the rotate-secret response that carries the
		// one-time plaintext beside the updated product.
		newContractCase[RotateProductClientSecretResponse]("rotate_product_client_secret"),
		// PRD #1910 M2: the /connect consent-screen metadata (scopes is a nil-slice null in
		// zero.json, never null on the wire) and the approve / deny redirect response.
		newContractCase[OAuthAuthorizeRequestDTO]("oauth_authorize_request"),
		newContractCase[OAuthRedirectResponse]("oauth_redirect_response"),
		// PRD #1910 M3: the token endpoint bodies. Go-only (the SPA never calls /api/oauth/token), like
		// the v1_* pairs.
		newContractCase[OAuthTokenResponse]("oauth_token_response"),
		newContractCase[OAuthRefreshResponse]("oauth_refresh_response"),
		newContractCase[OAuthErrorResponse]("oauth_error_response"),
		// PRD #1910 M3: one live OAuth connection of the caller (GET /api/me/oauth-connections;
		// last_used_at and refresh_issued_at are present-as-null pointers; scopes is a nil-slice
		// null in zero.json, never null on the wire).
		newContractCase[OAuthConnectionDTO]("oauth_connection"),
		// PRD #1910 M5: one live connection of a product in the admin list (last_used_at is a
		// present-as-null pointer; scopes is a nil-slice null in zero.json, never null on the wire).
		newContractCase[AdminOAuthConnectionDTO]("admin_oauth_connection"),
		newContractCase[ProductTokenDTO]("product_token"),
		newContractCase[AdminProductTokenDTO]("admin_product_token"),
		newContractCase[MintProductTokenResponse]("mint_product_token"),
		newContractCase[V1WhoamiDTO]("v1_whoami"),
		// PRD #1907 M4/M5: the typed admin delete response (its nested product carries the
		// present-as-null deleted_at) and the user mint picker entry (all strings, no null).
		newContractCase[AdminDeleteProductResponse]("admin_delete_product"),
		newContractCase[MintableProductDTO]("mintable_product"),
		// PRD #1906 M1w: the admin egress profile ("site list"). created_by/updated_by are
		// present-as-null pointers on the zero value; hosts, multi_publisher_override and
		// warnings are non-omitempty slices (nil-slice nulls the handler normalizes to [],
		// nonNilStrings and make(..., 0, n) in egressProfileToDTO). The nested
		// EgressProfileWarningDTO rides inside the full fixture's warnings[0].
		newContractCase[EgressProfileDTO]("egress_profile"),
		// PRD #1908 M5: the /api/v1/jobs wire. V1JobCreateRequest is a REQUEST body (its
		// full.json round-trips through DisallowUnknownFields like schedule_input). The
		// nullable pointers are present-as-null, so the response zero.json files carry nulls.
		newContractCase[V1JobCreateRequest]("v1_job_create_request"),
		newContractCase[V1JobDTO]("v1_job"),
		newContractCase[V1JobListDTO]("v1_job_list"),
		newContractCase[V1JobResultDTO]("v1_job_result"),
		newContractCase[V1JobMessagesDTO]("v1_job_messages"),
		// PRD #1909 M2: the uploaded-file DTO. expires_at is a present-as-null pointer.
		newContractCase[V1FileDTO]("v1_file"),
		// PRD #1909 M5: the job-file read DTOs. expires_at and source_url are present-as-null.
		newContractCase[V1JobFileDTO]("v1_job_file"),
		newContractCase[V1JobFilesDTO]("v1_job_files"),
		newContractCase[V1JobRefusedFileDTO]("v1_job_refused_file"),
		newContractCase[V1JobSourceDTO]("v1_job_source"),
		// PRD #1909 M6: the admin product skill-set view. applied_at/applied_by and staged are
		// present-as-null pointers; applied.skills is a nil-slice null the handler normalizes to
		// [] (make(..., 0, n) in productSkillsView). The nested staged/diff/drop DTOs ride inside
		// the full fixture, no standalone row (never returned alone).
		newContractCase[ProductSkillsDTO]("product_skills"),
		// PRD #1908 D-D: the job block of the run detail. It is omitempty on RunDTO (so
		// run.zero.json is unchanged) and nested in run.full.json; the standalone pair pins
		// its own nullability surface (origin members, result).
		newContractCase[RunJobDTO]("run_job"),
	}
}

// readContractFixture is fatal on any failure, never a skip. A skipped contract
// check is indistinguishable from a passing one (the false-green shape this repo
// documents repeatedly), so a missing or unreadable fixture must FAIL loudly.
func readContractFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractFixtureDir, name)) //nolint:gosec // G304: test reads a fixture from the fixed contractFixtureDir path
	if err != nil {
		t.Fatalf("fixture unreadable: %s: %v -- this contract asserts nothing without it, "+
			"and skipping would look identical to passing", name, err)
	}
	return b
}

func mustMarshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

// assertFixtureEqual compares a freshly marshaled value against the recorded
// fixture byte-for-byte and, on mismatch, prints the full marshaled JSON so
// re-recording is a copy-paste.
func assertFixtureEqual(t *testing.T, name string, got []byte) {
	t.Helper()
	want := readContractFixture(t, name)
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(got, "\n")) {
		t.Errorf("fixture %s is stale -- re-record it from this exact output "+
			"(recorded, not authored; there is no -update flag):\n%s", name, got)
	}
}

// TestContractFixturesMatchMarshal is assertion (a): the zero and populated
// values must marshal byte-equal to the recorded fixtures.
func TestContractFixturesMatchMarshal(t *testing.T) {
	for _, c := range contractCases() {
		t.Run(c.name, func(t *testing.T) {
			assertFixtureEqual(t, c.name+".zero.json", mustMarshalIndent(t, c.zero))
			assertFixtureEqual(t, c.name+".full.json", mustMarshalIndent(t, c.full))
		})
	}
}

// TestContractFullFixtureDecodesStrict is assertion (b): full.json must decode
// into the struct with DisallowUnknownFields (a wire key the struct lacks is the
// runtime-400 class) AND re-marshal byte-equal to what it decoded.
func TestContractFullFixtureDecodesStrict(t *testing.T) {
	for _, c := range contractCases() {
		t.Run(c.name, func(t *testing.T) {
			raw := readContractFixture(t, c.name+".full.json")
			v, err := c.decode(raw)
			if err != nil {
				t.Fatalf("full.json for %s did not decode with DisallowUnknownFields: %v -- "+
					"a wire key the struct lacks is a runtime 400", c.name, err)
			}
			assertFixtureEqual(t, c.name+".full.json", mustMarshalIndent(t, v))
		})
	}
}
