package handler

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// createdIssueURL uses stored display metadata only; it performs no forge request.
func createdIssueURL(repoURL, forgeType string, iid int64) string {
	if iid <= 0 || (forgeType != "github" && forgeType != "gitlab" && forgeType != "forgejo") {
		return ""
	}
	unsafe := func(s string) bool {
		return !utf8.ValidString(s) || strings.ContainsAny(s, "\\") ||
			strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf) })
	}
	if unsafe(repoURL) || strings.ContainsAny(repoURL, "?#") {
		return ""
	}
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	if net.ParseIP(host) == nil {
		if strings.ContainsAny(u.Host, "[]:") && strings.HasPrefix(u.Host, "[") {
			return ""
		}
		host = strings.TrimSuffix(host, ".")
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return ""
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return ""
				}
			}
		}
		if len(host) > 253 {
			return ""
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return ""
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
	}
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	if escaped == "" || !strings.HasPrefix(escaped, "/") {
		return ""
	}
	// Validate each path segment before appending; preserve valid stored escaping.
	for _, segment := range strings.Split(escaped[1:], "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." ||
			strings.ContainsAny(decoded, "/\\%") || unsafe(decoded) {
			return ""
		}
	}
	suffix := "/issues/"
	if forgeType == "gitlab" {
		suffix = "/-/issues/"
	}
	return "https://" + u.Host + escaped + suffix + strconv.FormatInt(iid, 10)
}

// overlayCreatedIssues is detail-only and follows the authorized run's owner even
// for admin viewers. One query reads the run outputs; any read failure drops the entire
// decoration, including rows read before a late Rows.Err.
func (h *Handler) overlayCreatedIssues(ctx context.Context, run store.Run, dto *apitypes.RunDTO) {
	if run.Kind != runkind.Prompt || !run.RepoID.Valid || (!run.ScheduleID.Valid && run.TriggerSource != "schedule") {
		return
	}
	rows, err := h.q.ListCreatedIssuesForRun(ctx, store.ListCreatedIssuesForRunParams{
		RunID: run.ID, RepoID: run.RepoID, UserID: run.UserID,
	})
	if err != nil {
		slog.Error("resolve run created issues", "run_id", run.ID, "error", termsafe.SanitizeBounded(err.Error(), 512))
		return
	}
	type coordinate struct {
		repo uuid.UUID
		iid  int64
	}
	seen := make(map[coordinate]bool, len(rows))
	issues := make([]apitypes.CreatedIssueDTO, 0, len(rows))
	for _, row := range rows {
		key := coordinate{row.RepoID, row.Iid}
		if seen[key] {
			continue
		}
		seen[key] = true
		issues = append(issues, apitypes.CreatedIssueDTO{IID: row.Iid, Title: row.Title, WebURL: createdIssueURL(row.WebUrl, row.ForgeType, row.Iid)})
	}
	dto.CreatedIssues = issues
}
