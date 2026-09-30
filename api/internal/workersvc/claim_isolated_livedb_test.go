package workersvc

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_isolated_livedb_test.go pins PRD #1906 M3's claim-time mint on the REAL claim path
// (Service.Claim -> ClaimRun -> assembleClaim -> isolateClaim) against a real Postgres.
// Nothing binds a run to a site list in production until M8, so the tests bind by setting
// runs.egress_profile_id at insert. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (./e2e/run-store-it.sh).

type isoFix struct {
	env                    codexTestEnv
	userID, workerID, repo uuid.UUID
	profile                uuid.UUID
	pname                  string
	botPAT                 string
	svc                    *Service
	wkr                    store.Worker
}

func newIsoFix(t *testing.T) *isoFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	f := &isoFix{env: env}
	f.userID, f.workerID, f.repo = env.seedCodexInfra(t)
	f.botPAT = codexToken("bot-pat")
	sealed, err := env.box.Seal([]byte(f.botPAT))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, sealed, f.userID)
	anth, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, uuid.New(), f.userID, "anthropic-"+uuid.NewString(), anth)
	f.profile = uuid.New()
	f.pname = "iso-" + strings.ReplaceAll(f.profile.String(), "-", "")[:20]
	// github.com is a known multi-publisher host with no override here, so EffectiveEntries
	// drops it: the snapshot must hold the EFFECTIVE list.
	env.exec(`INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com,github.com,*.vendor.example}')`, f.profile, f.pname)
	t.Cleanup(func() {
		_, _ = env.pool.Exec(env.ctx, `DELETE FROM runs WHERE user_id = $1`, f.userID)
		_, _ = env.pool.Exec(env.ctx, `DELETE FROM egress_profiles WHERE id = $1`, f.profile)
	})
	f.svc = New(env.q, env.box, testParams())
	f.svc.SetTxBeginner(env.pool)
	f.wkr = store.Worker{ID: f.workerID, UserID: f.userID, Name: "worker-iso", Status: "online"}
	return f
}

func (f *isoFix) queuedRun(t *testing.T, iid int64, bound bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var prof pgtype.UUID
	if bound {
		prof = pgtype.UUID{Bytes: f.profile, Valid: true}
	}
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, egress_profile_id)
	            VALUES ($1, $2, $3, 'issue', $4, 'research x', 'find the datasheet', 'queued', $5)`, id, f.userID, f.repo, iid, prof)
	return id
}

func (f *isoFix) claim(t *testing.T, want uuid.UUID) *ClaimPayload {
	t.Helper()
	return f.claimAs(t, f.wkr, want)
}

// claimAs claims as wkr and requires the claim to deliver run want.
func (f *isoFix) claimAs(t *testing.T, wkr store.Worker, want uuid.UUID) *ClaimPayload {
	t.Helper()
	p, err := f.svc.Claim(f.env.ctx, wkr, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if p == nil || p.RunID != want.String() {
		t.Fatalf("Claim = %+v, want run %s", p, want)
	}
	return p
}

type isoCred struct {
	hash    []byte
	gen     int64
	revoked bool
}

func (f *isoFix) cred(t *testing.T, run uuid.UUID) isoCred {
	t.Helper()
	var c isoCred
	var rev pgtype.Timestamptz
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT token_hash, claim_generation, revoked_at FROM run_fetch_credentials WHERE run_id = $1`,
		run).Scan(&c.hash, &c.gen, &rev); err != nil {
		t.Fatalf("read credential: %v", err)
	}
	c.revoked = rev.Valid
	return c
}

// TestIsolatedClaimMintsCredentialLiveDB: a profile-bound run's claim carries the uzf_
// credential (stored only as its sha256 under the claim's generation), the EFFECTIVE site
// list snapshotted at the first claim, no forge credential, repo, agents or skills, and no
// Codex block. An admin edit of the profile then does not change the run: a re-claim keeps
// the snapshot, rotates the token (the old one no longer resolves), clears the revocation
// the requeue set, stamps the new generation, and releases the earlier claim's open
// reservation.
func TestIsolatedClaimMintsCredentialLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 301, true)
	// PRD #1906 M5: only a lane worker (server-set isolated_lane, isolated_fetch_v1) claims it.
	lane := f.laneWorker(t, run)

	p := f.claimAs(t, lane, run)
	g := p.IsolatedFetch
	if g == nil || !strings.HasPrefix(g.Credential, fetchctl.CredentialPrefix) {
		t.Fatalf("isolated_fetch = %+v, want a uzf_ credential", g)
	}
	if g.Profile != f.pname || strings.Join(g.Hosts, ",") != "docs.example.com,*.vendor.example" {
		t.Fatalf("grant profile=%q hosts=%v, want %q and the effective entries (github.com dropped)", g.Profile, g.Hosts, f.pname)
	}
	if p.Secrets.ForgePAT != "" || p.Secrets.ForgeUsername != "" || p.Secrets.Codex != nil {
		t.Fatalf("secrets carry forge_pat=%v username=%q codex=%v, want none", p.Secrets.ForgePAT != "", p.Secrets.ForgeUsername, p.Secrets.Codex != nil)
	}
	if p.Secrets.AnthropicOAuthToken == "" {
		t.Fatal("the research claim lost its model token")
	}
	if p.Repo != (ClaimRepo{}) || len(p.Agents) != 0 || len(p.Skills) != 0 || len(p.Config.ToolPackages) != 0 {
		t.Fatalf("repo=%+v agents=%d skills=%d tools=%v, want all stripped", p.Repo, len(p.Agents), len(p.Skills), p.Config.ToolPackages)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), f.botPAT) || !strings.Contains(string(raw), `"isolated_fetch":{"credential":"uzf_`) || strings.Contains(string(raw), `"codex"`) {
		t.Fatalf("claim JSON leaks the PAT, lacks the grant, or has a codex block: %s", raw)
	}
	c1 := f.cred(t, run)
	sum := sha256.Sum256([]byte(g.Credential))
	if string(c1.hash) != string(sum[:]) || c1.gen != p.ClaimGeneration || c1.gen != 1 || c1.revoked {
		t.Fatalf("credential row = %+v, want sha256 of the token, generation 1, not revoked", c1)
	}

	// An admin edit after the claim, an open reservation of this claim, then a requeue.
	f.env.exec(`UPDATE egress_profiles SET hosts = '{evil.example.com}' WHERE id = $1`, f.profile)
	f.env.exec(`INSERT INTO run_fetch_reservations (run_id, claim_generation, bytes) VALUES ($1, 1, 10)`, run)
	f.env.exec(`UPDATE run_fetch_credentials SET reserved_bytes = 10, inflight = 1, files = 1, attempts = 1 WHERE run_id = $1`, run)
	f.env.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, run)
	if !f.cred(t, run).revoked {
		t.Fatal("the requeue did not revoke the credential")
	}

	p2 := f.claimAs(t, lane, run)
	g2 := p2.IsolatedFetch
	if g2 == nil || g2.Credential == g.Credential {
		t.Fatalf("re-claim grant = %+v, want a rotated credential", g2)
	}
	if strings.Join(g2.Hosts, ",") != "docs.example.com,*.vendor.example" {
		t.Fatalf("re-claim hosts = %v, want the first claim's snapshot (the admin edit must not reach a claimed run)", g2.Hosts)
	}
	c2 := f.cred(t, run)
	sum2 := sha256.Sum256([]byte(g2.Credential))
	if string(c2.hash) != string(sum2[:]) || c2.gen != 2 || p2.ClaimGeneration != 2 || c2.revoked {
		t.Fatalf("re-claimed credential row = %+v, want the new token's hash, generation 2, not revoked", c2)
	}
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_fetch_credentials WHERE token_hash = $1`, sum[:]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the first claim's token still resolves (%d rows, %v)", n, err)
	}
	var reserved, inflight, files int64
	var open int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT reserved_bytes, inflight, files,
	        (SELECT count(*) FROM run_fetch_reservations WHERE run_id = $1 AND NOT settled)
	        FROM run_fetch_credentials WHERE run_id = $1`, run).Scan(&reserved, &inflight, &files, &open); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 || inflight != 0 || open != 0 || files != 1 {
		t.Fatalf("after re-claim reserved=%d inflight=%d open=%d files=%d, want the old reservation released and the run totals kept", reserved, inflight, open, files)
	}
}

// TestUnboundClaimUnchangedLiveDB: an unbound run's claim is untouched by M3: no grant,
// its PAT and repo delivered, no credential row, no snapshot written.
func TestUnboundClaimUnchangedLiveDB(t *testing.T) {
	f := newIsoFix(t)
	run := f.queuedRun(t, 302, false)
	p := f.claim(t, run)
	if p.IsolatedFetch != nil {
		t.Fatalf("an unbound claim carries isolated_fetch %+v", p.IsolatedFetch)
	}
	if p.Secrets.ForgePAT != f.botPAT || p.Repo.ID != f.repo.String() || p.Repo.CloneURL == "" {
		t.Fatalf("unbound claim lost its PAT or repo: pat=%v repo=%+v", p.Secrets.ForgePAT == f.botPAT, p.Repo)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["isolated_fetch"]; ok {
		t.Fatalf("unbound claim JSON has an isolated_fetch key: %s", raw)
	}
	var n int
	var snap []byte
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (SELECT count(*) FROM run_fetch_credentials WHERE run_id = $1), egress_snapshot FROM runs WHERE id = $1`,
		run).Scan(&n, &snap); err != nil {
		t.Fatal(err)
	}
	if n != 0 || snap != nil {
		t.Fatalf("unbound claim wrote credential rows=%d snapshot=%s", n, snap)
	}
}
