package workersvc

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

const (
	// RunKindJudge is the run-judge kind (PRD #46 Decision 1): a worker-executed
	// retrospective of another finished run. It has no repo, no issue, no branch, no
	// MR — it rides the run machinery only for token delivery and message
	// persistence, and points at the reviewed run via target_run_id.
	RunKindJudge = "judge"
	// RunKindSelfImprove is the self-improvement kind (PRD #46 Decision 10): an
	// autonomous improvement run against uzi's own repo. It is issue-shaped and flows
	// through the ordinary repo-ful claim path — no fork needed here.
	RunKindSelfImprove = "self_improve"
)

// Judge scan caps (PRD #46 Decision 4 + audit L1): the command-not-found pre-scan
// runs off the hot request path (at claim assembly) and bounds what it inspects so a
// pathological run can't make it expensive.
const (
	judgeToolResultRowCap int32 = 2000       // tool_result rows fetched for the scan
	judgeScanByteBudget         = 512 * 1024 // bytes of tool output inspected
	judgeMaxMissingTools        = 20         // distinct missing tools reported
	judgeEvidenceMaxLen         = 200        // per-hit evidence cap (chars)
)

// JudgeSignal is the API-side deterministic pre-scan the judge claim carries (PRD #46
// Decision 4): command-not-found / missing-executable hits found in the reviewed
// run's tool_result output. The judge interprets them (which tool, which agent) and,
// if the model call fails, they are the deterministic fallback recommendation.
type JudgeSignal struct {
	MissingTools []ToolMiss `json:"missing_tools"`
}

// ToolMiss is one deterministic command-not-found hit: the executable a shell could
// not find and the trimmed line that flagged it (bounded — never raw output wholesale).
type ToolMiss struct {
	Command  string `json:"command"`
	Evidence string `json:"evidence"`
}

// Command-not-found patterns, ordered high- to lower-confidence. They only FLAG
// missing-executable evidence for the judge to interpret; none decides anything.
var (
	reCmdNotFound    = regexp.MustCompile(`([A-Za-z0-9_.+-]+): command not found`)                    // bash: `foo: command not found`
	reCmdNotFoundZsh = regexp.MustCompile(`command not found: ([A-Za-z0-9_.+-]+)`)                    // zsh: `command not found: foo`
	reExecNotFound   = regexp.MustCompile(`exec: "?([A-Za-z0-9_./+-]+)"?: executable file not found`) // Go exec.LookPath
	reShNotFound     = regexp.MustCompile(`([A-Za-z0-9_.+-]+): not found\b`)                          // dash/busybox: `foo: not found`
)

// shellNames are the interpreters that REPORT a missing command; they are never the
// missing command themselves, so the bash/zsh forms (`zsh: command not found: foo`)
// would otherwise flag the shell prefix. Filtered out of the results.
var shellNames = map[string]bool{
	"bash": true, "zsh": true, "sh": true, "dash": true, "ash": true, "ksh": true,
	"fish": true, "/bin/sh": true, "/bin/bash": true, "/usr/bin/env": true, "env": true,
}

// noisyShToken matches tokens the low-confidence `X: not found` (sh/busybox) form
// commonly mis-flags but that are never a missing WORKER TOOL: HTTP status / line
// numbers (404, 1) and shared-object / archive / header files (libssl.so.1, foo.o,
// bar.h). Applied ONLY to reShNotFound; the explicit "command not found" forms are
// high-confidence and unfiltered. A generic English word ("key: not found") can still
// slip through — distinguishing it needs context this flag-only scan lacks, so the
// judge interprets it.
var noisyShToken = regexp.MustCompile(`(?i)^\d+$|\.(so|a|o|h|dll|dylib|la|lo)(\.\d+)*$`)

// scanCommandNotFound flags missing-executable evidence in a run's tool_result
// payloads (PRD #46 Decision 4). It bounds the bytes inspected (judgeScanByteBudget),
// dedupes by command keeping the first evidence line, and caps the distinct count.
// Pure over its input so it is unit-testable without a DB.
func scanCommandNotFound(payloads [][]byte) []ToolMiss {
	var out []ToolMiss
	seen := map[string]bool{}
	scanned := 0

	add := func(cmd, evidence string) {
		cmd = strings.Trim(strings.TrimSpace(cmd), `"'`)
		if cmd == "" || shellNames[cmd] || seen[cmd] || len(out) >= judgeMaxMissingTools {
			return
		}
		seen[cmd] = true
		evidence = strings.TrimSpace(evidence)
		if len(evidence) > judgeEvidenceMaxLen {
			evidence = evidence[:judgeEvidenceMaxLen]
		}
		out = append(out, ToolMiss{Command: cmd, Evidence: evidence})
	}

	for _, p := range payloads {
		if scanned >= judgeScanByteBudget || len(out) >= judgeMaxMissingTools {
			break
		}
		text := string(p)
		if scanned+len(text) > judgeScanByteBudget {
			text = text[:judgeScanByteBudget-scanned]
		}
		scanned += len(text)
		// tool_result payloads are jsonb, so embedded quotes arrive escaped as \" —
		// unescape them so the exec: "cmd" form (Go's exec.LookPath error) matches.
		text = strings.ReplaceAll(text, `\"`, `"`)

		for _, m := range reCmdNotFound.FindAllStringSubmatch(text, -1) {
			add(m[1], m[0])
		}
		for _, m := range reCmdNotFoundZsh.FindAllStringSubmatch(text, -1) {
			add(m[1], m[0])
		}
		for _, m := range reExecNotFound.FindAllStringSubmatch(text, -1) {
			add(m[1], m[0])
		}
		for _, m := range reShNotFound.FindAllStringSubmatch(text, -1) {
			if noisyShToken.MatchString(m[1]) {
				continue
			}
			add(m[1], m[0])
		}
	}
	return out
}

// assembleJudgeClaim builds the claim payload for a judge run (PRD #46 Decisions 1, 3
// & 4). Unlike the ordinary run lane it does NOT call GetRunClaimContext (no repo, no
// forge connection) and NEVER opens the bot PAT: least privilege, and a judge must
// not spuriously fail when the reviewed run's forge connection is gone (audit H2). It
// delivers the run owner's Anthropic token (vault-aware openAnthropic), the reviewed
// run's id (its trace is fetched out-of-band), the judge model, and the deterministic
// command-not-found pre-scan. The trace itself never rides the claim (it can be MB).
func (s *Service) assembleJudgeClaim(ctx context.Context, run store.Run) (*ClaimPayload, error) {
	// nil: the default token — and deliberately NOT the claiming worker's binding,
	// even though a judge run is claimed by an ordinary worker through the same
	// ClaimRun lane. Under PRD #104 D1 the judge lane is bound per USER, not per
	// worker: which credential reviews your work is a property of you, not of
	// whichever worker happened to pick the retrospective up. M4 replaces this with
	// the owner's judge_anthropic_secret_id when set, so retrospectives can bill a
	// different credential from the runs they review.
	anthropic, err := s.openAnthropic(ctx, run.UserID, nil)
	if err != nil {
		return nil, err
	}

	var targetRunID *string
	var signal *JudgeSignal
	if run.TargetRunID.Valid {
		id := uuid.UUID(run.TargetRunID.Bytes).String()
		targetRunID = &id
		signal = s.judgeSignal(ctx, uuid.UUID(run.TargetRunID.Bytes))
	}

	var judgeModel *string
	if s.settings != nil {
		if m, err := s.settings.JudgeModel(ctx); err == nil && strings.TrimSpace(m) != "" {
			judgeModel = &m
		}
	}

	return &ClaimPayload{
		RunID:            run.ID.String(),
		Kind:             run.Kind,
		IssueTitle:       run.IssueTitle,
		IssueDescription: run.IssueDescription,
		Status:           run.Status,
		TargetRunID:      targetRunID,
		JudgeModel:       judgeModel,
		JudgeSignal:      signal,
		SessionID:        textPtr(run.SessionID),
		LastSeq:          run.LastSeq,
		IterationCount:   run.IterationCount,
		RequeueCount:     run.RequeueCount,
		Secrets: ClaimSecrets{
			// ForgeUsername/ForgePAT are left empty by design: a judge never touches a
			// repo. The wire still carries the (empty) forge_pat key because a judge run
			// rides the ordinary ClaimPayload; the no-PAT guarantee is that assembly
			// never decrypts one, asserted in judge_test.go.
			AnthropicOAuthToken: string(anthropic),
		},
		Agents:        []ClaimAgent{},
		Skills:        []ClaimSkill{},
		SkillsDropped: []ClaimSkillDrop{},
		Config: ClaimConfig{
			RunTimeoutSeconds:  int(s.p.RunTimeout.Seconds()),
			IdleTimeoutSeconds: int(s.p.RunIdleTimeout.Seconds()),
			MaxIterations:      s.p.RunMaxIterations,
			PlanMaxRevisions:   s.p.PlanMaxRevisions,
			ToolPackages:       []string{},
		},
	}, nil
}

// judgeSignal runs the deterministic command-not-found scan over the reviewed run's
// tool_result output (PRD #46 Decision 4), off the hot request path (it runs at claim
// assembly, a separate worker poll). Best-effort — a scan error never fails the claim;
// the judge still runs, it just loses the deterministic hint. Returns nil when there
// is nothing to report so the claim omits the signal entirely.
func (s *Service) judgeSignal(ctx context.Context, targetID uuid.UUID) *JudgeSignal {
	rows, err := s.q.ListToolResultPayloadsForRun(ctx, store.ListToolResultPayloadsForRunParams{
		RunID: targetID,
		Lim:   judgeToolResultRowCap,
	})
	if err != nil {
		slog.Warn("judge signal: list tool results", "target", targetID, "error", err)
		return nil
	}
	misses := scanCommandNotFound(rows)
	if len(misses) == 0 {
		return nil
	}
	return &JudgeSignal{MissingTools: misses}
}
