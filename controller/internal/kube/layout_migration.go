package kube

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Run-workdir layout migration guard (issue #3153).
//
// A Docker worker's pods used to keep /data/runner on a pod-lifetime emptyDir named
// legacyRunWorkdirVolume; they now mount the `data` PVC subPath dindWorkdirSubPath
// there. Replacing an old-layout pod destroys the in-pod run workdir, which for a
// worker holding a durable-recovery custody hold (DesiredWorker.CustodyHeld) is
// custody-held run state. migrationHeld is the ONE predicate the controller's
// pod-replacing roll and recycle paths ask; none of them may re-derive it. (Fenced
// DinD maintenance and teardown are guarded by CustodyHeld directly instead.)
//
// This is a narrow exception to the operator's emergency DrainPolicy.ForceRoll: a
// force-roll is consent to requeue a busy run, not to discard custody-held state.
// It cannot guard what the controller does not do: kubelet eviction, node drain and
// a manual pod delete replace pods without passing through this controller.

// legacyRunWorkdirVolume is the removed emptyDir volume name of the old layout.
const legacyRunWorkdirVolume = "run-workdir"

// runnerLayout aliases the reconcile type so ObservedWorker carries it without a
// mapping step. Its zero value is layoutUnknown.
type runnerLayout = reconcile.RunnerLayout

const (
	layoutUnknown = reconcile.RunnerLayoutUnknown
	layoutNone    = reconcile.RunnerLayoutNone
	layoutNew     = reconcile.RunnerLayoutNew
	layoutOld     = reconcile.RunnerLayoutOld
)

// observeRunnerLayout classifies a worker's pods, terminating ones included. Callers
// invoke it only after a successful pod list; a failed list leaves the observed
// layout at its zero value, layoutUnknown. New requires EVERY pod's worker and dind containers to mount the runner-shared
// subPath and NO pod to carry the legacy volume; any other non-empty set, mixed
// ones included, is old.
func observeRunnerLayout(pods []corev1.Pod) runnerLayout {
	if len(pods) == 0 {
		return layoutNone
	}
	for i := range pods {
		if !podHasNewLayout(&pods[i]) {
			return layoutOld
		}
	}
	return layoutNew
}

func podHasNewLayout(p *corev1.Pod) bool {
	for _, v := range p.Spec.Volumes {
		if v.Name == legacyRunWorkdirVolume {
			return false
		}
	}
	// dind is a native sidecar (an init container with restartPolicy Always), so both
	// lists are searched.
	containers := append(append([]corev1.Container{}, p.Spec.Containers...), p.Spec.InitContainers...)
	for _, name := range []string{workerContainerName, dindContainerName} {
		found := false
		for _, c := range containers {
			if c.Name != name {
				continue
			}
			found = true
			if !mountsRunnerShared(c.VolumeMounts) {
				return false
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func mountsRunnerShared(mounts []corev1.VolumeMount) bool {
	want := runnerWorkdirMount()
	for _, m := range mounts {
		if m.MountPath == want.MountPath && m.Name == want.Name && m.SubPath == want.SubPath {
			return true
		}
	}
	return false
}

// migrationHeld reports that replacing w's pods would destroy custody-held state.
// Unknown holds too: not knowing the layout is not evidence it is safe.
func migrationHeld(w protocol.DesiredWorker, layout runnerLayout) bool {
	return w.Docker && w.CustodyHeld && (layout == layoutOld || layout == layoutUnknown)
}

// layoutMigrationReason is the pacing BlockingReason of a held worker.
const layoutMigrationReason = "layout_migration_custody"
