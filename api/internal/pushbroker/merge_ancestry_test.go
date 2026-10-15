package pushbroker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Native Git excludes X through ^B, leaving A's parent unavailable in the broker.
// Neither the worker's merge construction nor the origin protocol is stubbed.
func TestPublishMergeAncestryDepth1(t *testing.T) {
	for _, size := range []int{2, 7} {
		for _, first := range []bool{true, false} {
			t.Run(fmt.Sprintf("parents%d/baseFirst%t", size, first), func(t *testing.T) {
				f := newGitFixture(t)
				x := f.commit("data.txt", bigText("X"), "X")
				b := f.commit("data.txt", bigText("B"), "B")
				f.pushMain()
				f.git("push", "origin", b+":refs/heads/topic", b+":refs/uzi-checkpoints/topic")
				f.git("checkout", "--detach", x)
				a := f.commit("data.txt", bigText("A"), "A")
				parents := []string{a}
				for i := 2; i < size; i++ {
					f.git("checkout", "--detach", x)
					parents = append(parents, f.commit("data.txt", bigText(fmt.Sprint(i)), fmt.Sprint(i)))
				}
				if first {
					parents = append([]string{b}, parents...)
				} else {
					parents = append(parents, b)
				}
				tree := strings.TrimSpace(f.git("rev-parse", a+"^{tree}"))
				args := []string{"commit-tree", tree, "-m", "M"}
				for _, p := range parents {
					args = append(args, "-p", p)
				}
				m := strings.TrimSpace(f.git(args...))
				if got := strings.TrimSpace(f.git("show", "-s", "--format=%P", m)); got != strings.Join(parents, " ") {
					t.Fatalf("ordered parents = %s", got)
				}
				f.git("checkout", "--detach", m)
				tip := f.commit("data.txt", bigText("T"), "T")
				f.git("merge-base", "--is-ancestor", b, tip)
				pack := f.pack(tip, b)
				if !packHasDelta(t, pack) {
					t.Fatal("regression pack must contain delta entries")
				}

				// Independently reconstruct the actual depth-1 fetch + native worker pack.
				repo, err := git.Init(memory.NewStorage(), nil)
				if err != nil {
					t.Fatal(err)
				}
				remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{f.cloneURL()}})
				if err != nil {
					t.Fatal(err)
				}
				err = remote.FetchContext(context.Background(), &git.FetchOptions{
					Depth: 1, RefSpecs: []config.RefSpec{"+refs/heads/main:refs/heads/main", "+refs/heads/topic:refs/heads/topic", "+refs/uzi-checkpoints/topic:refs/uzi-checkpoints/topic"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err = packfile.UpdateObjectStorage(repo.Storer, bytes.NewReader(pack)); err != nil {
					t.Fatal(err)
				}
				for _, h := range []string{b, a, m} {
					if _, err = object.GetCommit(repo.Storer, plumbing.NewHash(h)); err != nil {
						t.Fatalf("missing fixture commit %s: %v", h, err)
					}
				}
				if _, err = object.GetCommit(repo.Storer, plumbing.NewHash(x)); !errors.Is(err, plumbing.ErrObjectNotFound) {
					t.Fatalf("X must be absent: %v", err)
				}
				_, err = pushbroker.Publish(context.Background(), pushbroker.Options{
					CloneURL: f.cloneURL(), Branch: "topic", DefaultBranch: "main", DeclaredTip: tip, Pack: pack,
				})
				got := f.originRef("refs/uzi-checkpoints/topic")
				t.Logf("Publish err=%v ErrNotDescendant=%t remote=%s initial=%s tip=%s", err, errors.Is(err, pushbroker.ErrNotDescendant), got, b, tip)
				if err != nil {
					if got != b {
						t.Errorf("ref moved on refusal: %s", got)
					}
					t.Fatalf("Publish: %v", err)
				}
				if got != tip {
					t.Fatalf("checkpoint=%s want %s", got, tip)
				}
				if got := f.originRef("refs/heads/topic"); got != b {
					t.Fatalf("branch moved: %s", got)
				}
			})
		}
	}
}

// Checkpoint and branch are independent proofs when their origin tips differ.
func TestPublishDistinctBranchAncestry(t *testing.T) {
	for _, mode := range []string{"later_parent", "branch_unreachable", "branch_equals_declared", "checkpoint_unreachable"} {
		t.Run(mode, func(t *testing.T) {
			f := newGitFixture(t)
			x := f.commit("data.txt", bigText("X"), "X")
			b := f.commit("data.txt", bigText("B"), "B")
			f.pushMain()
			f.git("checkout", "--detach", x)
			c := f.commit("data.txt", bigText("C"), "C")
			f.git("push", "origin", c+":refs/uzi-checkpoints/topic")
			f.git("checkout", "--detach", x)
			a := f.commit("data.txt", bigText("A"), "A")
			parents := []string{c, a, b}
			if mode == "branch_unreachable" {
				parents = []string{c, a}
			}
			if mode == "checkpoint_unreachable" {
				parents = []string{a, b}
			}
			tree := strings.TrimSpace(f.git("rev-parse", a+"^{tree}"))
			args := []string{"commit-tree", tree, "-m", "merge"}
			for _, p := range parents {
				args = append(args, "-p", p)
			}
			m := strings.TrimSpace(f.git(args...))
			f.git("checkout", "--detach", m)
			tip := f.commit("data.txt", bigText("T"), "T")
			branch := b
			if mode == "branch_equals_declared" {
				branch = tip
			}
			f.git("push", "origin", branch+":refs/heads/topic")
			pack := f.pack(tip, c)
			if !packHasDelta(t, pack) {
				t.Fatal("control pack must contain actual deltas")
			}
			_, err := pushbroker.Publish(context.Background(), pushbroker.Options{CloneURL: f.cloneURL(), Branch: "topic", DefaultBranch: "main", DeclaredTip: tip, Pack: pack})
			want := tip
			if mode == "branch_unreachable" || mode == "checkpoint_unreachable" {
				want = c
				if !errors.Is(err, pushbroker.ErrNotDescendant) {
					t.Fatalf("want ErrNotDescendant: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := f.originRef("refs/uzi-checkpoints/topic"); got != want {
				t.Fatalf("checkpoint=%s want %s", got, want)
			}
			if got := f.originRef("refs/heads/topic"); got != branch {
				t.Fatalf("branch changed=%s want %s", got, branch)
			}
			t.Logf("mode=%s err=%v final checkpoint=%s branch=%s", mode, err, want, branch)
		})
	}
}
