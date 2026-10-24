package pushbroker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// A review publishes to its own key while its pack and ancestry use the root head.
func TestPublishNamedCoordinates(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("base.txt", "base", "base")
	f.pushMain()
	head := f.commit("root.txt", "root", "root")
	f.git("push", "origin", head+":refs/heads/work/root")
	first := f.commit("review.txt", "first", "first")
	pack := f.pack(first, head)
	result, err := pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), Branch: "legacy/ignored", WorkBranch: "work/root",
		CheckpointKey: "review/child", DefaultBranch: "main", DeclaredTip: first, Pack: pack,
	})
	if err != nil || result.Ref != "refs/uzi-checkpoints/review/child" {
		t.Fatalf("publish result=%+v err=%v", result, err)
	}
	// The second delta excludes the child checkpoint, which only the key fetch supplies.
	second := f.commit("review.txt", "second", "second")
	_, err = pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), WorkBranch: "work/root", CheckpointKey: "review/child",
		DefaultBranch: "main", DeclaredTip: second, Pack: f.pack(second, first),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.originRef("refs/uzi-checkpoints/review/child"); got != second {
		t.Fatalf("checkpoint=%s", got)
	}
	if got := f.originRef("refs/heads/work/root"); got != head {
		t.Fatalf("work head moved: %s", got)
	}
	if got := f.originRef("refs/uzi-checkpoints/work/root"); got != "" {
		t.Fatalf("root checkpoint created: %s", got)
	}

	// A separate key cannot bypass the actual root head's ancestry fence.
	f.git("checkout", "--detach", base)
	sibling := f.commit("sibling.txt", "sibling", "sibling")
	_, err = pushbroker.Publish(context.Background(), pushbroker.Options{
		CloneURL: f.cloneURL(), WorkBranch: "work/root", CheckpointKey: "review/diverged",
		DefaultBranch: "main", DeclaredTip: sibling, Pack: f.pack(sibling, base),
	})
	if !errors.Is(err, pushbroker.ErrNotDescendant) {
		t.Fatalf("divergent publish err=%v", err)
	}

	// The explicit ref wins over both the key and the legacy fallback.
	err = pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{
		CloneURL: f.cloneURL(), Branch: "legacy/ignored", CheckpointKey: "wrong/key",
		Ref: "refs/uzi-checkpoints/review/child", ExpectedOldTip: second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.originRef("refs/uzi-checkpoints/review/child"); got != "" {
		t.Fatalf("explicit ref survives: %s", got)
	}
}

func TestHeldPackNamedCoordinates(t *testing.T) {
	f := newGitFixture(t)
	f.commit("base.txt", "base", "base")
	f.pushMain()
	head := f.commit("root.txt", "root", "root")
	f.git("push", "origin", head+":refs/heads/work/root")
	checkpoint := f.commit("child.txt", "child", "child")
	f.git("push", "origin", checkpoint+":refs/uzi-checkpoints/review/child")
	tip := f.commit("held.txt", "held", "held")
	_, err := pushbroker.PrepareHeldPack(context.Background(), pushbroker.HeldPackOptions{
		CloneURL: f.cloneURL(), RunID: uuid.New(), Generation: 1, WorkBranch: "work/root",
		CheckpointKey: "review/child", DefaultBranch: "main", Tip: tip, Pack: f.pack(tip, checkpoint),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteNamedCheckpointKey(t *testing.T) {
	f := newGitFixture(t)
	tip := f.commit("base.txt", "base", "base")
	f.pushMain()
	f.git("push", "origin", tip+":refs/uzi-checkpoints/review/child")
	err := pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{
		CloneURL: f.cloneURL(), Branch: "legacy/ignored", CheckpointKey: "review/child", ExpectedOldTip: tip,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.originRef("refs/uzi-checkpoints/review/child"); got != "" {
		t.Fatalf("key survives: %s", got)
	}
}
