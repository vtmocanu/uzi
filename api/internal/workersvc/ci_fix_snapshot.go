package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// snapshotSecretPatterns is the dedicated log-tail scrubber for the failure
// snapshot (PRD #6). A job trace is a SUCCESS body, so the forge driver's
// error-only, connection-PAT-only redactor does not cover it — this second,
// pattern-based pass runs BEFORE any tail is frozen into failure_snapshot or a
// claim payload. It targets the token SHAPES a teammate's pipeline might print:
//   - GitLab token families (glpat-, gloas-, glrt-, glcbt-, glptt-, glsoat-,
//     glimt-, glagent-, gldt- deploy tokens) — a long base62/underscore/dash body.
//   - GitHub token families (PRD #954 M1): classic PATs (ghp_/gho_/ghu_/ghs_/ghr_)
//     and fine-grained github_pat_ — the GitHub driver has been live since
//     2026-08-08 and parses github_pat_ itself.
//   - Anthropic keys (sk-ant-...), the shape of a printed per-user token.
//   - Auth header lines a `curl -v` / `set -x` echo would emit: PRIVATE-TOKEN,
//     Authorization (Bearer ...), and a bare "Bearer <token>".
//   - uzi's own Bearer credentials (uzw_/uzc_/uza_, PRD #64 Risk 14). The CLI PRD
//     tells users to put UZI_TOKEN in a GitLab CI variable, so a `uzi ...` invocation
//     that echoes its token into a trace is exactly the path this snapshot ingests —
//     a uzc_/uza_ minted by this API must never freeze into a failure_snapshot.
//
// Arbitrary third-party secrets with no recognizable shape remain the documented
// residual risk (docs/configuration.md); the snapshot is owner/admin-visible only.
var snapshotSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gl(pat|oas|rt|cbt|ptt|soat|imt|agent|dt)-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`uz[caw]_[A-Za-z0-9_\-]{16,}`),
	// Header lines a `curl -v` / `set -x` echoes — redact the WHOLE value to EOL
	// (`.` excludes newline, so `.*` stops at the line end), never just the first word.
	regexp.MustCompile(`(?i)(private-token|authorization)\s*[:=].*`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
}

// ScrubKnownTokens redacts known token shapes from snapshot log-tail content.
func ScrubKnownTokens(s string) string {
	for _, re := range snapshotSecretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// BuildFailureSnapshot freezes the failed pipeline into a self-contained
// FailureSnapshot: up to maxJobs failed jobs, each with a logTailBytes tail of its
// trace. Log tails pass the driver's connection-PAT scrub (M1) plus a known-token
// scrub here. A single unreadable trace degrades to an empty tail, not a
// whole-snapshot failure.
func BuildFailureSnapshot(ctx context.Context, f forge.Forge, projectID int64, ps store.PipelineStatus, maxJobs, logTailBytes int) (FailureSnapshot, error) {
	snap := FailureSnapshot{
		PipelineID: ps.PipelineID,
		Ref:        ps.Ref,
		SHA:        ps.Sha,
		WebURL:     ps.WebUrl,
	}
	jobs, err := f.ListPipelineJobs(ctx, projectID, ps.PipelineID)
	if err != nil {
		return FailureSnapshot{}, err
	}
	for _, j := range jobs {
		if !pipelinestatus.IsFailed(j.Status) {
			continue
		}
		if len(snap.FailedJobs) >= maxJobs {
			break
		}
		tail, err := f.JobLogTail(ctx, projectID, j.ID, logTailBytes)
		if err != nil {
			slog.Warn("ci-fix: job log tail", "job", j.ID, "error", err)
			tail = ""
		}
		snap.FailedJobs = append(snap.FailedJobs, SnapshotJob{
			Name:    j.Name,
			Stage:   j.Stage,
			WebURL:  j.WebURL,
			LogTail: ScrubKnownTokens(tail),
		})
	}
	return snap, nil
}

// signatureLogLines caps how many trailing non-empty log-tail lines feed the
// signature per job — a failure's cause concludes its log, and a bounded window
// keeps the fingerprint stable against a longer preamble.
const signatureLogLines = 20

// Normalization regexes for normalizeLogLine, applied in order. Each strips a
// class of token that varies run-to-run for the SAME underlying failure, so two
// reruns of one broken build normalize to the same fingerprint. Ordered so a
// broader class (paths, timestamps, durations) is collapsed before the residual
// digit-run sweep can nibble at its interior.
var (
	// ANSI/VT100 escape sequences a colorized CI log emits.
	reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	// Absolute runner paths whose leading segments are run/host specific.
	rePath = regexp.MustCompile(`(/builds/|/tmp/)[^\s]*`)
	// ISO-8601 timestamps (date, optional time, optional fractional/zone).
	reISOTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2}([T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?)?`)
	// Hex / pointer addresses (0xDEADBEEF).
	reAddr = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	// Durations: 1m2s, 1m2.5s, 1.23s, 500ms, 12µs/us/ns.
	reDuration = regexp.MustCompile(`\b(\d+h)?(\d+m)?\d+(\.\d+)?(ms|µs|us|ns|s)\b`)
	// Any remaining run of two or more digits (line numbers, counts, ids).
	reDigits = regexp.MustCompile(`\d{2,}`)
	// Internal whitespace runs, collapsed last.
	reWS = regexp.MustCompile(`\s+`)
)

// normalizeLogLine strips the volatile tokens that differ between two runs of the
// SAME failure — ANSI escapes, ISO-8601 timestamps, durations, hex/pointer
// addresses, absolute /builds and /tmp paths, and long digit runs — replacing each
// with a fixed placeholder, then lowercases and collapses internal whitespace. It
// biases toward "same": aggressive normalization means a rerun with fresh
// timestamps/durations/ids yields the same fingerprint, at the cost of occasionally
// treating two genuinely-different lines as one.
func normalizeLogLine(s string) string {
	s = reANSI.ReplaceAllString(s, "")
	s = rePath.ReplaceAllString(s, "<PATH>")
	s = reISOTime.ReplaceAllString(s, "<TS>")
	s = reAddr.ReplaceAllString(s, "<ADDR>")
	s = reDuration.ReplaceAllString(s, "<DUR>")
	s = reDigits.ReplaceAllString(s, "<NUM>")
	s = strings.ToLower(s)
	s = reWS.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// lastNonEmptyLogLines returns up to n trailing lines of s that are non-empty
// after trimming, oldest-first within the retained window.
func lastNonEmptyLogLines(s string, n int) []string {
	var kept []string
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append(kept, lines[i])
	}
	// Reverse into oldest-first order so the canonical string reads top-to-bottom.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept
}

// FailureSignature returns a hex SHA-256 over a canonical, normalized rendering of
// the snapshot's failed jobs (PRD #71 design (b)): a stable fingerprint that stays
// the same across a rerun of the identical failure (fresh timestamps, durations,
// ids) yet differs for a different failing job set. Jobs are sorted by name|stage;
// each contributes its name|stage plus the normalized last ~20 non-empty log-tail
// lines. Used by the server-side auto-fix guard to detect "the same failure again".
func FailureSignature(snap FailureSnapshot) string {
	jobs := make([]SnapshotJob, len(snap.FailedJobs))
	copy(jobs, snap.FailedJobs)
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].Name+"|"+jobs[i].Stage < jobs[j].Name+"|"+jobs[j].Stage
	})
	var b strings.Builder
	for _, j := range jobs {
		b.WriteString(j.Name)
		b.WriteByte('|')
		b.WriteString(j.Stage)
		b.WriteByte('\n')
		for _, line := range lastNonEmptyLogLines(j.LogTail, signatureLogLines) {
			b.WriteString(normalizeLogLine(line))
			b.WriteByte('\n')
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// MergeCIConfigPaths unions the configured default CI-config globs with a project's
// own configured CI config path (GitLab ci_config_path), de-duplicated with empties
// dropped and a stable order: the defaults first, then projectPath appended only if
// it is non-empty and not already present. This is the guard's watch set — the paths
// a change must touch for the auto-fix guard to treat it as a CI-config edit.
func MergeCIConfigPaths(defaults []string, projectPath string) []string {
	out := make([]string, 0, len(defaults)+1)
	seen := map[string]struct{}{}
	for _, p := range defaults {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if projectPath != "" {
		if _, dup := seen[projectPath]; !dup {
			out = append(out, projectPath)
		}
	}
	return out
}
