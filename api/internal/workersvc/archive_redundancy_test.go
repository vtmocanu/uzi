package workersvc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// vectorsPath is read outside the api module: run this package with -count=1 after editing it.
const vectorsPath = "../../../fixtures/recovery-archive-redundancy/vectors.json"

type vecObject struct {
	Sha        string   `json:"sha"`
	Round      int      `json:"round"`
	Batch      int      `json:"batch"`
	Parents    []string `json:"parents"`
	BodyBase64 string   `json:"bodyBase64"`
}

type vecCase struct {
	Name             string      `json:"name"`
	Roots            []string    `json:"roots"`
	CurrentSha       string      `json:"currentSha"`
	Tree             string      `json:"tree"`
	Digest           string      `json:"digest"`
	DigestInput      string      `json:"digestInput"`
	SourceSha        string      `json:"sourceSha"`
	LeafParents      []string    `json:"leafParents"`
	AggregateObjects []vecObject `json:"aggregateObjects"`
	CurrentObject    vecObject   `json:"currentObject"`
	WitnessObject    *struct {
		Sha        string `json:"sha"`
		BodyBase64 string `json:"bodyBase64"`
		Tree       string `json:"tree"`
	} `json:"witnessObject"`
}

func loadVectors(t *testing.T) map[string]vecCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(vectorsPath))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Schema int       `json:"schema"`
		Cases  []vecCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Schema != 1 || len(f.Cases) == 0 {
		t.Fatalf("vectors: schema=%d cases=%d %v", f.Schema, len(f.Cases), err)
	}
	out := map[string]vecCase{}
	for _, c := range f.Cases {
		out[c.Name] = c
	}
	return out
}

func (c vecCase) request(gen int64) apitypes.RecoveryArchiveRedundancyRequest {
	req := apitypes.RecoveryArchiveRedundancyRequest{
		Generation: gen, CoverageDigest: c.Digest, Roots: slices.Clone(c.Roots), CurrentSha: c.CurrentSha,
		Tree: c.Tree, CurrentObject: c.CurrentObject.BodyBase64,
	}
	for _, o := range c.AggregateObjects {
		req.AggregateObjects = append(req.AggregateObjects, o.BodyBase64)
	}
	if c.WitnessObject != nil {
		req.WIP = &apitypes.RecoveryArchiveRedundancyWIP{WitnessObject: c.WitnessObject.BodyBase64}
	}
	return req
}

func TestRedundancyVectorsMatchRealGit(t *testing.T) {
	for name, c := range loadVectors(t) {
		t.Run(name, func(t *testing.T) {
			if got := redundancyDigest(c.Roots, c.CurrentSha, c.Tree); got != c.Digest {
				t.Fatalf("digest %s want %s", got, c.Digest)
			}
			for _, o := range append(slices.Clone(c.AggregateObjects), c.CurrentObject) {
				body, err := base64.StdEncoding.DecodeString(o.BodyBase64)
				if err != nil {
					t.Fatal(err)
				}
				if got := gitObjectID(body); got != o.Sha {
					t.Fatalf("object id %s want %s", got, o.Sha)
				}
				cm, ok := parseStrictCommit(body)
				if !ok || cm.sha != o.Sha {
					t.Fatalf("strict parse of real git commit %s: %+v ok=%v", o.Sha, cm, ok)
				}
			}
			in, reason := verifyRedundancyInputs(c.request(1), c.SourceSha)
			if reason != "" {
				t.Fatalf("real aggregate refused: %s", reason)
			}
			if !slices.Equal(in.heads, c.LeafParents) {
				t.Fatalf("heads %v want %v", in.heads, c.LeafParents)
			}
		})
	}
}

func TestRedundancyVectorTampering(t *testing.T) {
	vec := loadVectors(t)
	small, two := vec["small"], vec["two_rounds"]
	other := base64.StdEncoding.EncodeToString([]byte("tree " + strings.Repeat("1", 40) + "\n\nnot a coverage commit\n"))
	for _, tc := range []struct {
		name   string
		c      vecCase
		edit   func(*apitypes.RecoveryArchiveRedundancyRequest)
		source string
		want   string
	}{
		{"digest_changed", small, func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.CoverageDigest = strings.Repeat("e", 64) }, "", apitypes.RecoveryRedundancyDigestMismatch},
		{"root_swapped", small, func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.Roots[0] = strings.Repeat("0", 40) }, "", apitypes.RecoveryRedundancyDigestMismatch},
		{"wrong_source", small, nil, strings.Repeat("9", 40), apitypes.RecoveryRedundancySourceMismatch},
		{"extra_object", small, func(r *apitypes.RecoveryArchiveRedundancyRequest) {
			r.AggregateObjects = append(r.AggregateObjects, other)
		}, "", apitypes.RecoveryRedundancyBadObject},
		{"missing_round_object", two, func(r *apitypes.RecoveryArchiveRedundancyRequest) {
			r.AggregateObjects = r.AggregateObjects[:2]
		}, "", apitypes.RecoveryRedundancySourceMismatch},
		{"duplicate_object", small, func(r *apitypes.RecoveryArchiveRedundancyRequest) {
			r.AggregateObjects = append(r.AggregateObjects, r.AggregateObjects[0])
		}, "", apitypes.RecoveryRedundancyBadObject},
		{"not_base64", small, func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.AggregateObjects[0] = "!!!" }, "", apitypes.RecoveryRedundancyBadObject},
		{"newline_in_base64", small, func(r *apitypes.RecoveryArchiveRedundancyRequest) {
			r.AggregateObjects[0] = r.AggregateObjects[0][:8] + "\n" + r.AggregateObjects[0][8:]
		}, "", apitypes.RecoveryRedundancyBadObject},
		{"current_object_other_commit", small, func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.CurrentObject = r.AggregateObjects[0] }, "", apitypes.RecoveryRedundancyBadObject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.c.request(1)
			if tc.edit != nil {
				tc.edit(&req)
			}
			source := tc.c.SourceSha
			if tc.source != "" {
				source = tc.source
			}
			if _, reason := verifyRedundancyInputs(req, source); reason != tc.want {
				t.Fatalf("reason %q want %q", reason, tc.want)
			}
		})
	}
}

func TestParseStrictCommit(t *testing.T) {
	tree, p1, p2 := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	ident := "author a <a@x> 946684800 +0000\ncommitter a <a@x> 946684800 +0000\n"
	ok := "tree " + tree + "\nparent " + p1 + "\nparent " + p2 + "\n" + ident + "\nmessage\n"
	cm, good := parseStrictCommit([]byte(ok))
	if !good || cm.tree != tree || !slices.Equal(cm.parents, []string{p1, p2}) || cm.message != "message\n" || cm.sha != gitObjectID([]byte(ok)) {
		t.Fatalf("valid commit: %+v %v", cm, good)
	}
	if _, good := parseStrictCommit([]byte("tree " + tree + "\n" + ident + "gpgsig -----BEGIN-----\n body\n -----END-----\n\nm\n")); !good {
		t.Fatal("a continuation line of a signature header is valid")
	}
	if cm, good := parseStrictCommit([]byte("tree " + tree + "\n" + ident + "\nroot commit\n")); !good || len(cm.parents) != 0 {
		t.Fatalf("a root commit has no parents: %+v %v", cm, good)
	}
	for name, body := range map[string]string{
		"uppercase_tree":      "tree " + strings.ToUpper(tree) + "\n" + ident + "\nm\n",
		"uppercase_parent":    "tree " + tree + "\nparent " + strings.ToUpper(p1) + "\n" + ident + "\nm\n",
		"short_parent":        "tree " + tree + "\nparent " + p1[:39] + "\n" + ident + "\nm\n",
		"duplicate_parent":    "tree " + tree + "\nparent " + p1 + "\nparent " + p1 + "\n" + ident + "\nm\n",
		"tree_not_first":      ident + "tree " + tree + "\n\nm\n",
		"later_tree":          "tree " + tree + "\n" + ident + "tree " + p1 + "\n\nm\n",
		"later_parent":        "tree " + tree + "\nparent " + p1 + "\n" + ident + "parent " + p2 + "\n\nm\n",
		"parent_after_header": "tree " + tree + "\n" + ident + "parent " + p1 + "\n\nm\n",
		"bare_parent_header":  "tree " + tree + "\n" + ident + "parent\n\nm\n",
		"nul_in_message":      "tree " + tree + "\n" + ident + "\nm\x00x\n",
		"nul_in_header":       "tree " + tree + "\nauthor a\x00\n\nm\n",
		"no_separator":        "tree " + tree + "\nparent " + p1 + "\n" + ident,
		"continuation_first":  "tree " + tree + "\n continued\n" + ident + "\nm\n",
		"parent_trailing":     "tree " + tree + "\nparent " + p1 + " extra\n" + ident + "\nm\n",
		"empty":               "",
	} {
		if cm, good := parseStrictCommit([]byte(body)); good {
			t.Errorf("%s: accepted %+v", name, cm)
		}
	}
}

// ---- synthetic claims ----

func synthCommit(tree string, parents []string, msg string) (string, string) {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author a <a@x> 946684800 +0000\ncommitter a <a@x> 946684800 +0000\n\n" + msg + "\n")
	return b.String(), gitObjectID([]byte(b.String()))
}

func b64s(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// synthClaim builds a self-consistent claim over fabricated commits: the digest and the claimed
// tree are claimTree, the aggregate carries aggTree, and the current commit carries curTree.
func synthClaim(claimTree, aggTree, curTree string) (apitypes.RecoveryArchiveRedundancyRequest, string) {
	r1, r2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	curBody, cur := synthCommit(curTree, []string{r2}, "current")
	roots := []string{r1, r2}
	digest := redundancyDigest(roots, cur, claimTree)
	heads := []string{r1, r2, cur}
	slices.Sort(heads)
	aggBody, agg := synthCommit(aggTree, heads, "uzi recovery coverage "+digest+" round 0 batch 0")
	return apitypes.RecoveryArchiveRedundancyRequest{
		Generation: 3, CoverageDigest: digest, Roots: roots, CurrentSha: cur, Tree: claimTree,
		AggregateObjects: []string{b64s(aggBody)}, CurrentObject: b64s(curBody),
	}, agg
}

// ---- fakes ----

type redundancyForge struct {
	forgetest.BaseFake
	mu                              sync.Mutex
	branch, head, summaryHead       string
	summaryErr, headErr, compareErr error
	answers                         map[string]forge.Ancestry // by candidate; missing is NotAncestor
	unknown                         map[string]bool           // candidate -> forge.AncestryUnknown
	calls                           int
	compared                        []string
}

func (f *redundancyForge) GetMergeRequestSummary(context.Context, int64, int64) (forge.MergeRequestSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return forge.MergeRequestSummary{SourceBranch: f.branch, HeadSHA: f.summaryHead}, f.summaryErr
}

func (f *redundancyForge) BranchHead(context.Context, int64, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.head, f.headErr
}

func (f *redundancyForge) CompareAncestry(_ context.Context, _ int64, head, candidate string) (forge.Ancestry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.compared = append(f.compared, candidate)
	if f.compareErr != nil {
		return forge.AncestryUnknown, f.compareErr
	}
	if head != f.head {
		return forge.AncestryUnknown, errors.New("compared against a head other than the branch head")
	}
	if f.unknown[candidate] {
		return forge.AncestryUnknown, nil
	}
	if a, ok := f.answers[candidate]; ok {
		return a, nil
	}
	return forge.AncestryNotAncestor, nil
}

func (f *redundancyForge) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeRedundancyQ struct {
	view    store.GetArchiveRedundancyViewRow
	binding store.GetCompletedPublicationBindingRow
	memos   []string
	// bindingErr fails GetCompletedPublicationBinding when set.
	bindingErr error
}

func (q *fakeRedundancyQ) GetArchiveRedundancyView(context.Context, store.GetArchiveRedundancyViewParams) (store.GetArchiveRedundancyViewRow, error) {
	return q.view, nil
}

func (q *fakeRedundancyQ) GetCompletedPublicationBinding(context.Context, store.GetCompletedPublicationBindingParams) (store.GetCompletedPublicationBindingRow, error) {
	if q.bindingErr != nil {
		return store.GetCompletedPublicationBindingRow{}, q.bindingErr
	}
	return q.binding, nil
}

func (q *fakeRedundancyQ) RecordArchiveRedundancyRefusal(_ context.Context, a store.RecordArchiveRedundancyRefusalParams) (int64, error) {
	q.memos = append(q.memos, a.Reason)
	return 1, nil
}

type fakeRedundancyTxQ struct {
	q       *fakeRedundancyQ
	expire  func() []uuid.UUID
	expires int
	proof   []byte
	// workerErr fails the first locked read (GetWorkerForUpdate) when set.
	workerErr error
}

func (t *fakeRedundancyTxQ) GetWorkerForUpdate(context.Context, uuid.UUID) (store.Worker, error) {
	return store.Worker{}, t.workerErr
}

func (t *fakeRedundancyTxQ) GetRunOwnedByWorkerForUpdate(context.Context, store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error) {
	return store.Run{}, nil
}

func (t *fakeRedundancyTxQ) GetFinalInventoryHold(context.Context, store.GetFinalInventoryHoldParams) (store.RecoveryCustodyHold, error) {
	return store.RecoveryCustodyHold{ID: t.q.view.HoldID}, nil
}

func (t *fakeRedundancyTxQ) GetFinalInventoryCapture(context.Context, store.GetFinalInventoryCaptureParams) (store.RecoveryCapture, error) {
	return store.RecoveryCapture{}, nil
}

func (t *fakeRedundancyTxQ) GetArchiveRedundancyView(ctx context.Context, a store.GetArchiveRedundancyViewParams) (store.GetArchiveRedundancyViewRow, error) {
	return t.q.GetArchiveRedundancyView(ctx, a)
}

func (t *fakeRedundancyTxQ) LockCompletedPublicationBinding(context.Context, store.LockCompletedPublicationBindingParams) (store.LockCompletedPublicationBindingRow, error) {
	b := t.q.binding
	return store.LockCompletedPublicationBindingRow{RepoID: b.RepoID, ConnectionID: b.ConnectionID, ProjectID: b.ProjectID, ForgeType: b.ForgeType, BaseUrl: b.BaseUrl}, nil
}

func (t *fakeRedundancyTxQ) ExpireRedundantCapture(_ context.Context, a store.ExpireRedundantCaptureParams) ([]uuid.UUID, error) {
	t.expires++
	t.proof = a.Proof
	return t.expire(), nil
}

type fakeRedundancyTx struct {
	pgx.Tx
	commits, rollbacks int
}

func (t *fakeRedundancyTx) Commit(context.Context) error   { t.commits++; return nil }
func (t *fakeRedundancyTx) Rollback(context.Context) error { t.rollbacks++; return nil }

type fakeRedundancyBegin struct{ tx *fakeRedundancyTx }

func (b fakeRedundancyBegin) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

type redundancyUnit struct {
	p     *archiveRedundancyProver
	q     *fakeRedundancyQ
	txq   *fakeRedundancyTxQ
	tx    *fakeRedundancyTx
	forge *redundancyForge
	w     store.Worker
	run   uuid.UUID
	capID uuid.UUID
	req   apitypes.RecoveryArchiveRedundancyRequest
	vec   vecCase
	final string
	head  string
}

const unitNow = "2026-10-10T12:00:00Z"

func newRedundancyUnit(t *testing.T, vecName string) *redundancyUnit {
	t.Helper()
	c := loadVectors(t)[vecName]
	u := &redundancyUnit{
		w: store.Worker{ID: uuid.New(), UserID: uuid.New()}, run: uuid.New(), capID: uuid.New(), vec: c,
		final: strings.Repeat("a", 40), head: strings.Repeat("b", 40), req: c.request(3),
	}
	mr := int64(7)
	holdID, repoID, connID := uuid.New(), uuid.New(), uuid.New()
	id := apitypes.CompletionIdentity{
		HoldID: holdID.String(), RunID: u.run.String(), OwnerID: u.w.UserID.String(), WorkerID: u.w.ID.String(),
		Generation: 3, FinalHead: u.final, RepoID: repoID.String(), ConnectionID: connID.String(), ProjectID: 11,
		ForgeType: "gitlab", BaseURL: "https://forge.e2e", Branch: "agent/issue-1", MRIID: &mr,
	}
	raw, _ := json.Marshal(id)
	u.q = &fakeRedundancyQ{
		view: store.GetArchiveRedundancyViewRow{
			CaptureID: u.capID, RunID: u.run, UserID: u.w.UserID, CaptureWorkerID: pgconv.UUID(u.w.ID),
			CaptureState: "available", ManifestBound: true, SourceSha: c.SourceSha,
			CoverageDigest: pgconv.Text(c.Digest), HoldID: holdID, HoldGeneration: 3, HoldState: "released",
			InventoryGuarded: true, HoldWorkerID: u.w.ID, RepoID: pgconv.UUID(repoID), CompletionIdentity: raw,
			RunStatus: "completed", RunGeneration: 3, RunWorkerID: pgconv.UUID(u.w.ID),
			CompletionFinalHead: pgconv.Text(u.final), RunMrIid: pgtype.Int8{Int64: mr, Valid: true},
		},
		binding: store.GetCompletedPublicationBindingRow{RepoID: repoID, ConnectionID: connID, ProjectID: 11, ForgeType: "gitlab", BaseUrl: "https://forge.e2e"},
	}
	u.forge = &redundancyForge{branch: "agent/issue-1", head: u.head, summaryHead: u.head, answers: map[string]forge.Ancestry{u.final: forge.AncestryAncestor}}
	for _, r := range c.Roots {
		u.forge.answers[r] = forge.AncestryAncestor
	}
	u.forge.answers[c.CurrentSha] = forge.AncestryAncestor
	u.tx = &fakeRedundancyTx{}
	u.txq = &fakeRedundancyTxQ{q: u.q, expire: func() []uuid.UUID { return []uuid.UUID{u.capID} }}
	now, _ := time.Parse(time.RFC3339, unitNow)
	u.p = &archiveRedundancyProver{
		q: u.q, forges: settleUnitBuilder{f: u.forge}, begin: fakeRedundancyBegin{tx: u.tx},
		txq: func(pgx.Tx) archiveRedundancyTxStore { return u.txq }, now: func() time.Time { return now },
	}
	return u
}

func (u *redundancyUnit) run1(t *testing.T) apitypes.RecoveryArchiveRedundancyResponse {
	t.Helper()
	res, err := u.p.prove(context.Background(), u.w, u.run, u.capID, u.req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (u *redundancyUnit) wantRetained(t *testing.T, reason string) {
	t.Helper()
	res := u.run1(t)
	if res.Outcome != apitypes.RecoveryRedundancyRetained || res.Reason != reason {
		t.Fatalf("got %+v want retained/%s", res, reason)
	}
	if u.tx.commits != 0 || u.txq.expires > 1 {
		t.Fatalf("a retained claim must not commit: commits=%d expires=%d", u.tx.commits, u.txq.expires)
	}
}

func TestArchiveRedundancyExpiresOnCompleteProof(t *testing.T) {
	u := newRedundancyUnit(t, "small")
	res := u.run1(t)
	if res.Outcome != apitypes.RecoveryRedundancyExpired || res.Reason != "" || res.CaptureID != u.capID.String() {
		t.Fatalf("%+v", res)
	}
	if u.tx.commits != 1 || u.txq.expires != 1 {
		t.Fatalf("commits=%d expires=%d", u.tx.commits, u.txq.expires)
	}
	var proof redundancyProof
	if err := json.Unmarshal(u.txq.proof, &proof); err != nil || proof.AnchorHead != u.head || proof.FinalHead != u.final ||
		proof.CurrentSha != u.vec.CurrentSha || proof.Tree != u.vec.Tree || proof.MRIID != 7 || proof.Generation != 3 ||
		proof.CoverageDigest != u.vec.Digest || proof.SourceSha != u.vec.SourceSha || proof.ProvedAt != unitNow {
		t.Fatalf("recorded proof %s: %+v %v", u.txq.proof, proof, err)
	}
	// Each distinct commit is asked once: the anchor's final_head, then roots; the current head
	// is published so it needs no WIP witness.
	want := append([]string{u.final}, u.vec.Roots...)
	want = append(want, u.vec.CurrentSha)
	slices.Sort(want)
	got := slices.Clone(u.forge.compared)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("compared %v want %v", got, want)
	}
}

func TestArchiveRedundancyZeroOrWrongRowsIsNotExpired(t *testing.T) {
	for name, rows := range map[string]func(*redundancyUnit) []uuid.UUID{
		"zero_rows":  func(*redundancyUnit) []uuid.UUID { return nil },
		"wrong_id":   func(*redundancyUnit) []uuid.UUID { return []uuid.UUID{uuid.New()} },
		"two_rows":   func(u *redundancyUnit) []uuid.UUID { return []uuid.UUID{u.capID, uuid.New()} },
		"right_plus": func(u *redundancyUnit) []uuid.UUID { return []uuid.UUID{u.capID, u.capID} },
	} {
		t.Run(name, func(t *testing.T) {
			u := newRedundancyUnit(t, "small")
			u.txq.expire = func() []uuid.UUID { return rows(u) }
			u.wantRetained(t, apitypes.RecoveryRedundancyNotExpired)
			if u.tx.rollbacks < 1 || u.txq.expires != 1 {
				t.Fatalf("rollbacks=%d expires=%d", u.tx.rollbacks, u.txq.expires)
			}
		})
	}
}

func TestArchiveRedundancyForgeRefusalsRetainAndMemoize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vec    string
		edit   func(*redundancyUnit)
		reason string
	}{
		{"uncovered_root", "small", func(u *redundancyUnit) { u.forge.answers[u.vec.Roots[1]] = forge.AncestryNotAncestor }, apitypes.RecoveryRedundancyUncoveredRoot},
		{"root_unknown", "small", func(u *redundancyUnit) { u.forge.unknown[u.vec.Roots[0]] = true }, apitypes.RecoveryRedundancyAncestryUnknown},
		{"forge_error", "small", func(u *redundancyUnit) { u.forge.compareErr = errors.New("boom") }, apitypes.RecoveryRedundancyAncestryUnknown},
		{"forge_timeout", "small", func(u *redundancyUnit) { u.forge.compareErr = context.DeadlineExceeded }, apitypes.RecoveryRedundancyForgeTimeout},
		{"fork_pr_branch", "small", func(u *redundancyUnit) { u.forge.branch = "fork:agent/issue-1" }, apitypes.RecoveryRedundancyBranchMismatch},
		{"mr_head_differs_from_branch", "small", func(u *redundancyUnit) { u.forge.summaryHead = strings.Repeat("c", 40) }, apitypes.RecoveryRedundancyHeadMismatch},
		{"branch_deleted", "small", func(u *redundancyUnit) { u.forge.headErr = forge.ErrRefNotFound }, apitypes.RecoveryRedundancyBranchMissing},
		{"mr_missing", "small", func(u *redundancyUnit) { u.forge.summaryErr = forge.ErrMergeRequestNotFound }, apitypes.RecoveryRedundancyMRMissing},
		{"final_head_not_ancestor", "small", func(u *redundancyUnit) { u.forge.answers[u.final] = forge.AncestryNotAncestor }, apitypes.RecoveryRedundancyNotAncestor},
		{"uncovered_prerequisite", "small", func(u *redundancyUnit) {
			u.q.view.PrerequisiteShas = []string{strings.Repeat("7", 40)}
		}, apitypes.RecoveryRedundancyUncoveredPrereq},
		{"wip_without_witness", "wip", func(u *redundancyUnit) { u.req.WIP = nil }, apitypes.RecoveryRedundancyUncoveredWIP},
		{"wip_witness_unpublished", "wip", func(u *redundancyUnit) {}, apitypes.RecoveryRedundancyUncoveredWIP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newRedundancyUnit(t, tc.vec)
			u.forge.unknown = map[string]bool{}
			if tc.vec == "wip" {
				u.forge.answers[u.vec.CurrentSha] = forge.AncestryNotAncestor
				u.forge.answers[u.vec.CurrentObject.Parents[0]] = forge.AncestryAncestor
			}
			tc.edit(u)
			u.wantRetained(t, tc.reason)
			if u.txq.expires != 0 || !slices.Equal(u.q.memos, []string{tc.reason}) {
				t.Fatalf("expires=%d memos=%v", u.txq.expires, u.q.memos)
			}
		})
	}
}

func TestArchiveRedundancyWIPProvedByWitness(t *testing.T) {
	u := newRedundancyUnit(t, "wip")
	u.forge.unknown = map[string]bool{}
	u.forge.answers[u.vec.CurrentSha] = forge.AncestryNotAncestor
	u.forge.answers[u.vec.CurrentObject.Parents[0]] = forge.AncestryAncestor
	u.forge.answers[u.vec.WitnessObject.Sha] = forge.AncestryAncestor
	if res := u.run1(t); res.Outcome != apitypes.RecoveryRedundancyExpired {
		t.Fatalf("%+v", res)
	}
}

func TestArchiveRedundancyCooldownMakesNoForgeCall(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, unitNow)
	u := newRedundancyUnit(t, "small")
	u.q.view.RedundancyRefusedAt = pgtype.Timestamptz{Time: now.Add(-10 * time.Minute), Valid: true}
	u.q.view.RedundancyRefusal = pgconv.Text(apitypes.RecoveryRedundancyUncoveredRoot)
	u.wantRetained(t, apitypes.RecoveryRedundancyCoolingDown)
	if u.forge.callCount() != 0 || len(u.q.memos) != 0 || u.txq.expires != 0 {
		t.Fatalf("cool-down must be answered locally: calls=%d memos=%v", u.forge.callCount(), u.q.memos)
	}
	u.q.view.RedundancyRefusedAt = pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	if res := u.run1(t); res.Outcome != apitypes.RecoveryRedundancyExpired || u.forge.callCount() == 0 {
		t.Fatalf("a stale memo must not block: %+v", res)
	}
}

func TestArchiveRedundancyLocalRefusalsMakeNoForgeCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vec    string
		edit   func(*redundancyUnit)
		reason string
	}{
		{"digest", "small", func(u *redundancyUnit) { u.q.view.CoverageDigest = pgconv.Text(strings.Repeat("d", 64)) }, apitypes.RecoveryRedundancyDigestMismatch},
		{"source", "small", func(u *redundancyUnit) { u.q.view.SourceSha = strings.Repeat("5", 40) }, apitypes.RecoveryRedundancySourceMismatch},
		{"ttl_expired_first", "small", func(u *redundancyUnit) { u.q.view.CaptureState = "expired" }, apitypes.RecoveryRedundancyNotAvailable},
		{"discarded", "small", func(u *redundancyUnit) { u.q.view.CaptureState = "discarded" }, apitypes.RecoveryRedundancyNotAvailable},
		{"unbound_manifest", "small", func(u *redundancyUnit) { u.q.view.ManifestBound = false }, apitypes.RecoveryRedundancyNotAvailable},
		{"generation", "small", func(u *redundancyUnit) { u.req.Generation = 4 }, apitypes.RecoveryRedundancyBindingMismatch},
		{"older_generation_hold", "small", func(u *redundancyUnit) { u.req.Generation = 2 }, apitypes.RecoveryRedundancyBindingMismatch},
		{"other_worker_hold", "small", func(u *redundancyUnit) { u.q.view.HoldWorkerID = uuid.New() }, apitypes.RecoveryRedundancyBindingMismatch},
		{"unguarded_hold", "small", func(u *redundancyUnit) { u.q.view.InventoryGuarded = false }, apitypes.RecoveryRedundancyBindingMismatch},
		{"open_hold", "small", func(u *redundancyUnit) { u.q.view.HoldState = "open" }, apitypes.RecoveryRedundancyNotCompleted},
		{"failed_run", "small", func(u *redundancyUnit) { u.q.view.RunStatus = "failed" }, apitypes.RecoveryRedundancyNotCompleted},
		{"parked_run", "small", func(u *redundancyUnit) { u.q.view.RunStatus = "recovery_wait" }, apitypes.RecoveryRedundancyNotCompleted},
		{"successor_generation", "small", func(u *redundancyUnit) { u.q.view.RunGeneration = 4 }, apitypes.RecoveryRedundancyBindingMismatch},
		{"final_head_differs", "small", func(u *redundancyUnit) { u.q.view.CompletionFinalHead = pgconv.Text(strings.Repeat("e", 40)) }, apitypes.RecoveryRedundancyBindingMismatch},
		{"no_identity", "small", func(u *redundancyUnit) { u.q.view.CompletionIdentity = nil }, apitypes.RecoveryRedundancyIdentityMissing},
		{"identity_hold_differs", "small", func(u *redundancyUnit) { u.q.view.HoldID = uuid.New() }, apitypes.RecoveryRedundancyIdentityChanged},
		{"null_mr_iid", "small", func(u *redundancyUnit) {
			var id apitypes.CompletionIdentity
			_ = json.Unmarshal(u.q.view.CompletionIdentity, &id)
			id.MRIID = nil
			u.q.view.CompletionIdentity, _ = json.Marshal(id)
		}, apitypes.RecoveryRedundancyMRMissing},
		{"run_mr_differs", "small", func(u *redundancyUnit) { u.q.view.RunMrIid = pgtype.Int8{Int64: 8, Valid: true} }, apitypes.RecoveryRedundancyIdentityChanged},
		{"too_many_heads", "small", func(u *redundancyUnit) {
			u.req.Roots = nil
			for i := 1; i <= 65; i++ {
				u.req.Roots = append(u.req.Roots, fmt.Sprintf("%040x", i))
			}
		}, apitypes.RecoveryRedundancyInventoryTooLarge},
		{"too_many_objects", "small", func(u *redundancyUnit) {
			for i := 0; i < 8; i++ {
				u.req.AggregateObjects = append(u.req.AggregateObjects, u.req.AggregateObjects[0])
			}
		}, apitypes.RecoveryRedundancyInventoryTooLarge},
		{"oversized_object", "small", func(u *redundancyUnit) { u.req.CurrentObject = strings.Repeat("A", 120000) }, apitypes.RecoveryRedundancyInventoryTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newRedundancyUnit(t, tc.vec)
			tc.edit(u)
			u.wantRetained(t, tc.reason)
			if u.forge.callCount() != 0 || len(u.q.memos) != 0 || u.txq.expires != 0 {
				t.Fatalf("local refusal must touch neither forge nor memo: calls=%d memos=%v", u.forge.callCount(), u.q.memos)
			}
		})
	}
}

func TestArchiveRedundancyMalformedClaimIsInvalid(t *testing.T) {
	for name, edit := range map[string]func(*apitypes.RecoveryArchiveRedundancyRequest){
		"zero_generation": func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.Generation = 0 },
		"short_digest":    func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.CoverageDigest = r.CoverageDigest[:63] },
		"upper_digest": func(r *apitypes.RecoveryArchiveRedundancyRequest) {
			r.CoverageDigest = strings.ToUpper(r.CoverageDigest)
		},
		"upper_current":    func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.CurrentSha = strings.ToUpper(r.CurrentSha) },
		"upper_tree":       func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.Tree = strings.ToUpper(r.Tree) },
		"no_roots":         func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.Roots = nil },
		"unsorted_roots":   func(r *apitypes.RecoveryArchiveRedundancyRequest) { slices.Reverse(r.Roots) },
		"duplicate_roots":  func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.Roots = []string{r.Roots[0], r.Roots[0]} },
		"short_root":       func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.Roots[0] = r.Roots[0][:39] },
		"no_aggregate":     func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.AggregateObjects = nil },
		"no_current":       func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.CurrentObject = "" },
		"empty_witness":    func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.WIP = &apitypes.RecoveryArchiveRedundancyWIP{} },
		"upper_root_alone": func(r *apitypes.RecoveryArchiveRedundancyRequest) { r.Roots[1] = strings.ToUpper(r.Roots[1]) },
	} {
		t.Run(name, func(t *testing.T) {
			u := newRedundancyUnit(t, "small")
			edit(&u.req)
			if _, err := u.p.prove(context.Background(), u.w, u.run, u.capID, u.req); !errors.Is(err, ErrInvalidRedundancyRequest) {
				t.Fatalf("err %v", err)
			}
			if u.forge.callCount() != 0 || u.txq.expires != 0 {
				t.Fatal("a malformed claim must not reach the forge or the store")
			}
		})
	}
}

// (3i) The claim's tree must be the tree of the captured head. A forged aggregate can carry any
// tree with valid hashes, a valid digest and published parents, so the check is on
// current_object, before any forge call, even though every parent here is an ancestor.
func TestArchiveRedundancyTreeBindingRefusedBeforeForge(t *testing.T) {
	treeT, treeU := strings.Repeat("7", 40), strings.Repeat("8", 40)
	for _, tc := range []struct {
		name  string
		claim func() (apitypes.RecoveryArchiveRedundancyRequest, string)
		want  string // "" is accepted
		err   error
	}{
		{"consistent_claim_is_accepted", func() (apitypes.RecoveryArchiveRedundancyRequest, string) { return synthClaim(treeT, treeT, treeT) }, "", nil},
		{"unrelated_tree_valid_current_object", func() (apitypes.RecoveryArchiveRedundancyRequest, string) { return synthClaim(treeT, treeT, treeU) }, apitypes.RecoveryRedundancyTreeMismatch, nil},
		{"aggregate_tree_differs_from_claim", func() (apitypes.RecoveryArchiveRedundancyRequest, string) { return synthClaim(treeT, treeU, treeT) }, apitypes.RecoveryRedundancyTreeMismatch, nil},
		{"current_object_hash_mismatch", func() (apitypes.RecoveryArchiveRedundancyRequest, string) {
			r, agg := synthClaim(treeT, treeT, treeT)
			r.CurrentObject = b64s("tree " + treeT + "\nparent " + strings.Repeat("2", 40) + "\nauthor a <a@x> 1 +0000\ncommitter a <a@x> 1 +0000\n\nother\n")
			return r, agg
		}, apitypes.RecoveryRedundancyBadObject, nil},
		{"current_object_missing", func() (apitypes.RecoveryArchiveRedundancyRequest, string) {
			r, agg := synthClaim(treeT, treeT, treeT)
			r.CurrentObject = ""
			return r, agg
		}, "", ErrInvalidRedundancyRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newRedundancyUnit(t, "small")
			req, source := tc.claim()
			// Every commit the claim names is published, so only the tree binding can refuse it.
			for _, h := range append(slices.Clone(req.Roots), req.CurrentSha) {
				u.forge.answers[h] = forge.AncestryAncestor
			}
			u.q.view.SourceSha = source
			u.q.view.CoverageDigest = pgconv.Text(req.CoverageDigest)
			u.req = req
			res, err := u.p.prove(context.Background(), u.w, u.run, u.capID, u.req)
			switch {
			case tc.err != nil:
				if !errors.Is(err, ErrInvalidRedundancyRequest) {
					t.Fatalf("err %v", err)
				}
			case err != nil:
				t.Fatal(err)
			case tc.want == "":
				if res.Outcome != apitypes.RecoveryRedundancyExpired {
					t.Fatalf("%+v", res)
				}
				return
			case res.Outcome != apitypes.RecoveryRedundancyRetained || res.Reason != tc.want:
				t.Fatalf("%+v want retained/%s", res, tc.want)
			}
			if u.forge.callCount() != 0 || u.txq.expires != 0 || len(u.q.memos) != 0 {
				t.Fatalf("refused after touching forge/store: calls=%d expires=%d memos=%v", u.forge.callCount(), u.txq.expires, u.q.memos)
			}
		})
	}
}

func TestArchiveRedundancyReplayReturnsExpired(t *testing.T) {
	u := newRedundancyUnit(t, "small")
	u.q.view.CaptureState = "expired"
	u.q.view.CaptureReason = pgconv.Text("published_redundant")
	u.q.view.RedundancyProof = []byte(`{}`)
	if res := u.run1(t); res.Outcome != apitypes.RecoveryRedundancyExpired || u.forge.callCount() != 0 || u.txq.expires != 0 {
		t.Fatalf("%+v", res)
	}
	// A TTL expiry (no recorded proof) or another digest is not a replay.
	u.q.view.RedundancyProof = nil
	u.wantRetained(t, apitypes.RecoveryRedundancyNotAvailable)
}

// Only refusals derived from forge evidence are remembered: a database error, a binding mismatch,
// a missing forge or a client that went away says nothing about the forge.
func TestArchiveRedundancyOnlyForgeEvidenceIsMemoized(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*redundancyUnit)
		ctx    func() (context.Context, context.CancelFunc)
		reason string
	}{
		{"binding_db_error", func(u *redundancyUnit) { u.q.bindingErr = errors.New("db down") }, nil, apitypes.RecoveryRedundancyAncestryUnknown},
		{"binding_mismatch", func(u *redundancyUnit) { u.q.binding.ProjectID = 12 }, nil, apitypes.RecoveryRedundancyIdentityChanged},
		{"nil_forges", func(u *redundancyUnit) { u.p.forges = nil }, nil, apitypes.RecoveryRedundancyAncestryUnknown},
		{"cancelled_request", func(u *redundancyUnit) { u.forge.compareErr = context.Canceled }, func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, apitypes.RecoveryRedundancyAncestryUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newRedundancyUnit(t, "small")
			tc.edit(u)
			ctx := context.Background()
			if tc.ctx != nil {
				var cancel context.CancelFunc
				ctx, cancel = tc.ctx()
				defer cancel()
			}
			res, err := u.p.prove(ctx, u.w, u.run, u.capID, u.req)
			if err != nil || res.Outcome != apitypes.RecoveryRedundancyRetained || res.Reason != tc.reason {
				t.Fatalf("%+v %v want retained/%s", res, err, tc.reason)
			}
			if len(u.q.memos) != 0 || u.txq.expires != 0 {
				t.Fatalf("a refusal not derived from forge evidence was memoized: %v", u.q.memos)
			}
		})
	}
}

// A store error on the locked reads is an infrastructure failure, not an identity change.
func TestArchiveRedundancyExpireStoreErrorIsAnError(t *testing.T) {
	u := newRedundancyUnit(t, "small")
	u.txq.workerErr = errors.New("connection reset")
	res, err := u.p.prove(context.Background(), u.w, u.run, u.capID, u.req)
	if err == nil || res.Outcome != "" {
		t.Fatalf("got %+v %v want an error", res, err)
	}
	if u.tx.commits != 0 || u.txq.expires != 0 {
		t.Fatalf("commits=%d expires=%d", u.tx.commits, u.txq.expires)
	}
	// A missing row is still the bounded identity_changed reason.
	u = newRedundancyUnit(t, "small")
	u.txq.workerErr = pgx.ErrNoRows
	u.wantRetained(t, apitypes.RecoveryRedundancyIdentityChanged)
}

// A forged current_object can list far more parents than a real commit; the proof must not turn
// that into one forge call per parent.
func TestArchiveRedundancyParentAndProvedCapsRefuseBeforeForge(t *testing.T) {
	tree := strings.Repeat("7", 40)
	shas := func(base, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%040x", base+i)
		}
		return out
	}
	build := func(nRoots, nParents int) (apitypes.RecoveryArchiveRedundancyRequest, string) {
		roots := shas(0x1000, nRoots)
		curBody, cur := synthCommit(tree, shas(0x9000, nParents), "current")
		witBody, _ := synthCommit(tree, shas(0xa000, 1), "witness")
		digest := redundancyDigest(roots, cur, tree)
		heads := append(slices.Clone(roots), cur)
		slices.Sort(heads)
		aggBody, agg := synthCommit(tree, heads, "uzi recovery coverage "+digest+" round 0 batch 0")
		return apitypes.RecoveryArchiveRedundancyRequest{
			Generation: 3, CoverageDigest: digest, Roots: roots, CurrentSha: cur, Tree: tree,
			AggregateObjects: []string{b64s(aggBody)}, CurrentObject: b64s(curBody),
			WIP: &apitypes.RecoveryArchiveRedundancyWIP{WitnessObject: b64s(witBody)},
		}, agg
	}
	for _, tc := range []struct {
		name           string
		roots, parents int
		prerequisites  int
		wantTooLarge   bool
	}{
		{"parents_at_cap", 2, redundancyMaxParents, 0, false},
		{"parents_over_cap", 2, redundancyMaxParents + 1, 0, true},
		// 31 roots + the head + 32 parents + 64 prerequisites + the witness is 129 distinct commits.
		{"proved_over_cap", 31, redundancyMaxParents, redundancyMaxPrerequisite, true},
		{"proved_at_cap", 30, redundancyMaxParents, redundancyMaxPrerequisite, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newRedundancyUnit(t, "small")
			req, source := build(tc.roots, tc.parents)
			u.req = req
			u.q.view.SourceSha = source
			u.q.view.CoverageDigest = pgconv.Text(req.CoverageDigest)
			u.q.view.PrerequisiteShas = shas(0x5000, tc.prerequisites)
			res, err := u.p.prove(context.Background(), u.w, u.run, u.capID, u.req)
			if err != nil {
				t.Fatal(err)
			}
			tooLarge := res.Outcome == apitypes.RecoveryRedundancyRetained && res.Reason == apitypes.RecoveryRedundancyInventoryTooLarge
			if tooLarge != tc.wantTooLarge {
				t.Fatalf("%+v wantTooLarge=%v", res, tc.wantTooLarge)
			}
			if tc.wantTooLarge && (u.forge.callCount() != 0 || len(u.q.memos) != 0 || u.txq.expires != 0) {
				t.Fatalf("over-cap claim reached forge/store: calls=%d memos=%v", u.forge.callCount(), u.q.memos)
			}
		})
	}
}

// The captured head's own parents are part of the WIP proof. In this vector the parent is not a
// root, so the parent loop is the only thing that can notice it is unpublished.
func TestArchiveRedundancyWIPParentMustBePublished(t *testing.T) {
	setup := func(parentPublished bool) *redundancyUnit {
		u := newRedundancyUnit(t, "wip_unrooted_parent")
		u.forge.unknown = map[string]bool{}
		parent := u.vec.CurrentObject.Parents[0]
		if slices.Contains(u.vec.Roots, parent) {
			t.Fatalf("vector parent %s must not be a root", parent)
		}
		u.forge.answers[u.vec.CurrentSha] = forge.AncestryNotAncestor
		u.forge.answers[u.vec.WitnessObject.Sha] = forge.AncestryAncestor
		if parentPublished {
			u.forge.answers[parent] = forge.AncestryAncestor
		} else {
			u.forge.answers[parent] = forge.AncestryNotAncestor
		}
		return u
	}
	u := setup(false)
	u.wantRetained(t, apitypes.RecoveryRedundancyUncoveredWIP)
	if u.txq.expires != 0 || !slices.Equal(u.q.memos, []string{apitypes.RecoveryRedundancyUncoveredWIP}) {
		t.Fatalf("expires=%d memos=%v", u.txq.expires, u.q.memos)
	}
	if res := setup(true).run1(t); res.Outcome != apitypes.RecoveryRedundancyExpired {
		t.Fatalf("%+v", res)
	}
}
