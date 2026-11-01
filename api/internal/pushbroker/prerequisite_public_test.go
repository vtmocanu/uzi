package pushbroker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// This test intentionally uses only pre-M2 public symbols, for baseline execution.
func TestPrereqPublicListDelete(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("base.txt", "base\n", "base")
	f.pushMain()
	url := f.cloneURL()
	ref := "refs/uzi-prereq/3c1d7e52-9a4b-4f68-8d21-5e0b6a7c9d13"
	f.git("push", "origin", base+":"+ref)
	t.Run("list", func(t *testing.T) {
		tips, err := pushbroker.ListRefTips(context.Background(), pushbroker.ListRefsOptions{CloneURL: url}, ref)
		if err != nil || tips[ref] != base {
			t.Fatalf("ListRefTips = %v, %v", tips, err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		for _, old := range []string{"", strings.Repeat("0", 40)} {
			err := pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: url, Ref: ref, ExpectedOldTip: old})
			if !errors.Is(err, pushbroker.ErrInvalidRef) {
				t.Fatalf("old %q: %v", old, err)
			}
		}
		if err := pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: url, Ref: ref, ExpectedOldTip: base}); err != nil {
			t.Fatal(err)
		}
		if got := f.originRef(ref); got != "" {
			t.Fatalf("not deleted: %s", got)
		}
	})
}
