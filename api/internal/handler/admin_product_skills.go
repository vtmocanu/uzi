package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Product skill sets (PRD #1909 M6, D9): the admin routes that configure a product's skills
// repo, sync it into a STAGED snapshot, and approve that snapshot into the product's applied
// skills. Routing (routes_admin.go) carries the authorization: GET is in the admin READ group
// (session or uza_ token), the sync and apply writes are cookie-only (RequireAuth +
// RequireAdmin), and the PATCH of the source fields is AdminPatchProduct's (cookie-only).
//
// THE CLONE TOKEN. It is sealed with secretbox under the AAD productSkillsTokenAAD(product id)
// (so a sealed token cannot be copied onto another product's row), written only by PATCH
// skills_token, never returned by any route (the DTO says set or not set), decrypted only inside
// the sync handler, handed to agentsource as CloneOptions.Token (BasicAuth username oauth2, kept
// on the starting origin across redirects, scrubbed from every error agentsource returns), and
// never logged. No response or error text here contains it or the agentsource error text.

// Bounds for the source fields. The token cap matches the agent-source credential cap (a GitHub
// fine-grained PAT is ~93 characters); settings' own constant is unexported.
const (
	maxProductSkillsTokenLen = 1024
	maxProductSkillsURLLen   = 2048
	maxProductSkillsRefLen   = 256
)

// productSkillsTokenAADPrefix binds a sealed token to its product: the AAD is
// "product_skills_token|<product id>".
const productSkillsTokenAADPrefix = "product_skills_token|"

func productSkillsTokenAAD(productID uuid.UUID) []byte {
	return []byte(productSkillsTokenAADPrefix + productID.String())
}

// Reason codes of a skill a sync did not stage (apitypes.ProductSkillDropDTO.Reason). The
// agentsource note vocabulary supplies invalid, too_large, duplicate and over_limit; "secret" is
// a skill carrying a full provider token.
const (
	productSkillDropInvalid   = "invalid"
	productSkillDropTooLarge  = "too_large"
	productSkillDropDuplicate = "duplicate"
	productSkillDropOver      = "over_limit"
	productSkillDropSecret    = "secret"
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// validateProductSkillsURL is the write-time gate for skills_repo_url. Empty clears the source. A
// non-empty value must be an absolute https URL with a host and NO userinfo (a credential belongs
// in skills_token, never in a URL that is stored in clear, shown to admins and embedded in clone
// errors), no query or fragment, and its scheme+host must be on
// UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS (an empty allowlist means the feature is off and refuses
// every URL). The allowlist check runs on the parsed URL, so a host that only LOOKS allowlisted
// through userinfo (https://allowed.example@evil.example/) is refused by the userinfo rule first.
// The error text never echoes the value.
func (h *Handler) validateProductSkillsURL(raw string) error {
	if raw == "" {
		return nil
	}
	if raw != strings.TrimSpace(raw) {
		return errors.New("skills_repo_url must not have leading or trailing whitespace")
	}
	if len(raw) > maxProductSkillsURLLen {
		return fmt.Errorf("skills_repo_url must be at most %d bytes", maxProductSkillsURLLen)
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return errors.New("skills_repo_url must not contain whitespace or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("skills_repo_url must be a valid URL")
	}
	if u.Scheme != "https" {
		return errors.New("skills_repo_url must use https")
	}
	if u.Host == "" {
		return errors.New("skills_repo_url must include a host")
	}
	if u.User != nil {
		return errors.New("skills_repo_url must not embed credentials; use skills_token")
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		return errors.New("skills_repo_url must not carry a query or fragment")
	}
	if len(h.cfg.ProductSkillsAllowedBaseURLs) == 0 {
		return errors.New("product skill sets are not enabled on this instance (UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS is empty)")
	}
	if !h.cfg.ProductSkillsBaseURLAllowed(raw) {
		return errors.New("skills_repo_url is not on the UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS allowlist")
	}
	return nil
}

// validateProductSkillsRef gates skills_ref: empty (the default branch), or one token of at most
// maxProductSkillsRefLen characters with no whitespace or control characters (a branch, a tag or a
// 40-hex SHA; agentsource validates it against git's ref rules again before any network call).
func validateProductSkillsRef(raw string) error {
	if raw == "" {
		return nil
	}
	if utf8.RuneCountInString(raw) > maxProductSkillsRefLen {
		return fmt.Errorf("skills_ref must be at most %d characters", maxProductSkillsRefLen)
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("skills_ref must not contain whitespace or control characters")
		}
	}
	return nil
}

// validateProductSkillsToken gates skills_token: one opaque token, non-empty, at most
// maxProductSkillsTokenLen characters, with no whitespace or control characters. The error never
// echoes the value.
func validateProductSkillsToken(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("skills_token must not be empty (use clear_skills_token to remove it)")
	}
	if utf8.RuneCountInString(raw) > maxProductSkillsTokenLen {
		return fmt.Errorf("skills_token must be at most %d characters", maxProductSkillsTokenLen)
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("skills_token must not contain whitespace or control characters")
		}
	}
	return nil
}

// productSkillsFetcher is the skills-repo reader. Production uses agentsource.FetchSkillFiles;
// a test injects a fake through Handler.productSkillsFetch.
type productSkillsFetcher func(ctx context.Context, opts agentsource.CloneOptions, maxFileBytes int) (string, []agentsource.SkillFile, []agentsource.Note, error)

func (h *Handler) fetchProductSkills() productSkillsFetcher {
	if h.productSkillsFetch != nil {
		return h.productSkillsFetch
	}
	return agentsource.FetchSkillFiles
}

// stagedSkill is one skill of a staged snapshot as stored in product_skill_staged.skills.
type stagedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// AdminGetProductSkills returns the admin view of one product's skill set: its source, the
// applied (approved) set and the staged snapshot with its diff. A soft-deleted product is still
// readable (the registry lists it). An unknown id is a 404.
func (h *Handler) AdminGetProductSkills(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	dto, err := h.productSkillsView(r.Context(), h.q, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "product not found")
			return
		}
		slog.Error("admin get product skills", "product_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

// productSkillsView assembles the ProductSkillsDTO for a product on q (the pool-bound queries, or
// a transaction-bound one so a write responds with the state it just committed... before commit).
func (h *Handler) productSkillsView(ctx context.Context, q *store.Queries, id uuid.UUID) (apitypes.ProductSkillsDTO, error) {
	p, err := q.GetProduct(ctx, id)
	if err != nil {
		return apitypes.ProductSkillsDTO{}, err
	}
	applied, err := q.ListProductSkills(ctx, pgconv.UUID(id))
	if err != nil {
		return apitypes.ProductSkillsDTO{}, fmt.Errorf("list product skills: %w", err)
	}
	dto := apitypes.ProductSkillsDTO{
		Config: apitypes.ProductSkillsConfigDTO{
			SkillsRepoURL:  p.SkillsRepoUrl,
			SkillsRef:      p.SkillsRef,
			SkillsTokenSet: len(p.SkillsTokenSealed) > 0,
			Enabled:        len(h.cfg.ProductSkillsAllowedBaseURLs) > 0,
		},
		Applied: apitypes.ProductSkillsAppliedDTO{
			SHA:       p.SkillsAppliedSha,
			AppliedAt: timePtr(p.SkillsAppliedAt.Valid, p.SkillsAppliedAt.Time),
			AppliedBy: uuidPtr(p.SkillsAppliedBy),
			Skills:    make([]apitypes.ProductSkillDTO, 0, len(applied)),
		},
	}
	current := make(map[string]stagedSkill, len(applied))
	for _, s := range applied {
		dto.Applied.Skills = append(dto.Applied.Skills, apitypes.ProductSkillDTO{Name: s.Name, Description: s.Description, Body: s.Body})
		current[s.Name] = stagedSkill{Name: s.Name, Description: s.Description, Body: s.Body}
	}
	stage, err := q.GetProductSkillStage(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return dto, nil
	}
	if err != nil {
		return apitypes.ProductSkillsDTO{}, fmt.Errorf("get product skill stage: %w", err)
	}
	var skills []stagedSkill
	if err := json.Unmarshal(stage.Skills, &skills); err != nil {
		return apitypes.ProductSkillsDTO{}, fmt.Errorf("decode staged skills: %w", err)
	}
	var dropped []apitypes.ProductSkillDropDTO
	if err := json.Unmarshal(stage.Dropped, &dropped); err != nil {
		return apitypes.ProductSkillsDTO{}, fmt.Errorf("decode staged drops: %w", err)
	}
	if dropped == nil {
		dropped = []apitypes.ProductSkillDropDTO{}
	}
	staged := &apitypes.ProductSkillsStagedDTO{
		SHA:      stage.SourceSha,
		StagedAt: stage.StagedAt.Time,
		StagedBy: uuidPtr(stage.StagedBy),
		Skills:   make([]apitypes.ProductSkillDTO, 0, len(skills)),
		Dropped:  dropped,
		Diff:     diffProductSkills(current, skills),
	}
	for _, s := range skills {
		staged.Skills = append(staged.Skills, apitypes.ProductSkillDTO{Name: s.Name, Description: s.Description, Body: s.Body})
	}
	dto.Staged = staged
	return dto, nil
}

// uuidPtr is the JSON-null-or-string form of a nullable uuid column.
func uuidPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// diffProductSkills compares a staged set with the applied one by name.
func diffProductSkills(current map[string]stagedSkill, staged []stagedSkill) apitypes.ProductSkillsDiffDTO {
	d := apitypes.ProductSkillsDiffDTO{Added: []string{}, Changed: []string{}, Removed: []string{}, Unchanged: []string{}}
	seen := make(map[string]struct{}, len(staged))
	for _, s := range staged {
		seen[s.Name] = struct{}{}
		cur, ok := current[s.Name]
		switch {
		case !ok:
			d.Added = append(d.Added, s.Name)
		case cur.Description != s.Description || cur.Body != s.Body:
			d.Changed = append(d.Changed, s.Name)
		default:
			d.Unchanged = append(d.Unchanged, s.Name)
		}
	}
	for name := range current {
		if _, ok := seen[name]; !ok {
			d.Removed = append(d.Removed, name)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Changed)
	sort.Strings(d.Removed)
	sort.Strings(d.Unchanged)
	return d
}

// AdminSyncProductSkills reads the product's skills repo and STAGES what it finds. Nothing
// reaches a job: staged skills live in product_skill_staged and only AdminApplyProductSkills
// moves them into skills. The clone runs against the source as it is configured now (the
// allowlist is re-checked here, before any network call), authenticated with the stored token if
// there is one. Each SKILL.md is reduced to name, description and body, validated with the rules
// a global skill is saved under (description shape, SKILL_MAX_BYTES body cap, the full-token
// guardrail), de-duplicated by name and capped at SKILLS_MAX_PER_RUN; what was dropped, and why,
// is recorded beside the staged set. A fetch failure is a 502 with a fixed message: the
// scrubbed error goes to the log only, never to the response.
func (h *Handler) AdminSyncProductSkills(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	p, err := h.q.GetProduct(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "product not found")
			return
		}
		slog.Error("admin sync product skills: get product", "product_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if p.DeletedAt.Valid {
		httpx.Error(w, http.StatusConflict, "product is deleted")
		return
	}
	if p.SkillsRepoUrl == "" {
		httpx.Error(w, http.StatusConflict, "this product has no skills repo configured")
		return
	}
	// Re-check the allowlist NOW: it is instance config that may have changed since the URL was
	// saved. An off-allowlist source causes zero egress.
	if err := h.validateProductSkillsURL(p.SkillsRepoUrl); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	var token string
	if len(p.SkillsTokenSealed) > 0 {
		if h.box == nil {
			slog.Error("admin sync product skills: no secret box", "product_id", id)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		plain, oerr := h.box.OpenWithAAD(p.SkillsTokenSealed, productSkillsTokenAAD(id))
		if oerr != nil {
			slog.Error("admin sync product skills: open sealed token", "product_id", id)
			httpx.Error(w, http.StatusConflict, "the stored clone token cannot be decrypted; set it again")
			return
		}
		token = string(plain)
	}

	sha, files, notes, ferr := h.fetchProductSkills()(r.Context(), agentsource.CloneOptions{
		CloneURL:        p.SkillsRepoUrl,
		Ref:             p.SkillsRef,
		Token:           token,
		RedirectAllowed: h.cfg.ProductSkillsBaseURLAllowed,
	}, h.cfg.SkillMaxBytes)
	if ferr != nil {
		// agentsource scrubs the token from its errors; even so the text is for the log only.
		slog.Error("admin sync product skills: fetch", "product_id", id, "error", ferr)
		httpx.Error(w, http.StatusBadGateway, "could not read the skills repo (check the URL, ref and token)")
		return
	}
	if !shaRe.MatchString(sha) {
		slog.Error("admin sync product skills: source returned a malformed commit id", "product_id", id)
		httpx.Error(w, http.StatusBadGateway, "could not read the skills repo (check the URL, ref and token)")
		return
	}

	skills, dropped := h.reduceProductSkills(files, notes)
	skillsJSON, _ := json.Marshal(skills)
	droppedJSON, _ := json.Marshal(dropped)

	var view apitypes.ProductSkillsDTO
	err = h.inTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.GetProductForUpdate(r.Context(), id)
		if err != nil {
			return err
		}
		// The source may have changed, or the product been deleted, while the clone ran: staging
		// a snapshot of a source that is no longer configured would show the admin something the
		// product does not point at.
		if cur.DeletedAt.Valid || cur.SkillsRepoUrl != p.SkillsRepoUrl || cur.SkillsRef != p.SkillsRef {
			return errProductSkillsSourceChanged
		}
		if err := q.UpsertProductSkillStage(r.Context(), store.UpsertProductSkillStageParams{
			ProductID: id,
			SourceSha: sha,
			StagedBy:  pgconv.UUID(actor.ID),
			Skills:    skillsJSON,
			Dropped:   droppedJSON,
		}); err != nil {
			return err
		}
		view, err = h.productSkillsView(r.Context(), q, id)
		return err
	})
	switch {
	case errors.Is(err, errProductSkillsSourceChanged):
		httpx.Error(w, http.StatusConflict, "the skills source changed while syncing; sync again")
		return
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "product not found")
		return
	case err != nil:
		slog.Error("admin sync product skills: stage", "product_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("admin staged product skills", "actor_id", actor.ID, "product_id", id, "sha", sha, "skills", len(skills), "dropped", len(dropped))
	httpx.JSON(w, http.StatusOK, view)
}

var errProductSkillsSourceChanged = errors.New("product skills source changed")

// reduceProductSkills turns the fetched SKILL.md files into the staged set: parse (keeping only
// name, description, body), validate with validateSkillFields, refuse a duplicate name, then cap
// at SKILLS_MAX_PER_RUN (lowest names kept). The staged set is sorted by name. Every skill not
// kept is returned as a drop with its reason, and the agentsource read notes are folded in.
func (h *Handler) reduceProductSkills(files []agentsource.SkillFile, notes []agentsource.Note) ([]stagedSkill, []apitypes.ProductSkillDropDTO) {
	dropped := []apitypes.ProductSkillDropDTO{}
	for _, n := range notes {
		switch n.Reason {
		case agentsource.NoteTooLarge:
			dropped = append(dropped, apitypes.ProductSkillDropDTO{Name: n.Name, Reason: productSkillDropTooLarge})
		case agentsource.NoteDuplicate:
			dropped = append(dropped, apitypes.ProductSkillDropDTO{Name: n.Name, Reason: productSkillDropDuplicate})
		case agentsource.NoteOverLimit:
			dropped = append(dropped, apitypes.ProductSkillDropDTO{Reason: productSkillDropOver, Count: n.Count})
		}
	}
	byName := make(map[string]stagedSkill, len(files))
	for _, f := range files {
		res := agentsource.ParseSkillFile(f.Data, f.Dir)
		if !res.OK {
			dropped = append(dropped, apitypes.ProductSkillDropDTO{Name: res.Name, Reason: productSkillDropInvalid})
			continue
		}
		if _, err := validateSkillFields(skillWriteRequest{Description: res.Description, Body: res.Body}, h.cfg.SkillMaxBytes); err != nil {
			reason := productSkillDropInvalid
			switch {
			case strings.Contains(err.Error(), "maximum allowed size"):
				reason = productSkillDropTooLarge
			case strings.Contains(err.Error(), "full Anthropic token"):
				reason = productSkillDropSecret
			}
			dropped = append(dropped, apitypes.ProductSkillDropDTO{Name: res.Name, Reason: reason})
			continue
		}
		if _, dup := byName[res.Name]; dup {
			dropped = append(dropped, apitypes.ProductSkillDropDTO{Name: res.Name, Reason: productSkillDropDuplicate})
			continue
		}
		byName[res.Name] = stagedSkill{Name: res.Name, Description: res.Description, Body: res.Body}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	if limit := h.cfg.SkillsMaxPerRun; limit > 0 && len(names) > limit {
		for _, n := range names[limit:] {
			dropped = append(dropped, apitypes.ProductSkillDropDTO{Name: n, Reason: productSkillDropOver})
		}
		names = names[:limit]
	}
	out := make([]stagedSkill, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	sort.SliceStable(dropped, func(i, j int) bool {
		if dropped[i].Name != dropped[j].Name {
			return dropped[i].Name < dropped[j].Name
		}
		return dropped[i].Reason < dropped[j].Reason
	})
	return out, dropped
}

// AdminApplyProductSkills approves the staged snapshot: in ONE transaction it replaces the
// product's applied skills with the staged set, records the approving admin and the commit SHA,
// and discards the staged snapshot. The body {expected_sha} must equal the staged SHA, so an
// admin approves exactly the set they reviewed: a sync that replaced the snapshot in between is a
// 409, and nothing is applied. A claim reads either the old set or the new one, never a mix.
func (h *Handler) AdminApplyProductSkills(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	var req struct {
		ExpectedSHA string `json:"expected_sha"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !shaRe.MatchString(req.ExpectedSHA) {
		httpx.Error(w, http.StatusBadRequest, "expected_sha must be the 40-hex commit id of the staged set")
		return
	}
	var (
		view    apitypes.ProductSkillsDTO
		applied int
	)
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.GetProductForUpdate(r.Context(), id)
		if err != nil {
			return err
		}
		if cur.DeletedAt.Valid {
			return errProductDeleted
		}
		stage, err := q.GetProductSkillStage(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNothingStaged
		}
		if err != nil {
			return err
		}
		if stage.SourceSha != req.ExpectedSHA {
			return errStagedShaMismatch
		}
		var skills []stagedSkill
		if err := json.Unmarshal(stage.Skills, &skills); err != nil {
			return fmt.Errorf("decode staged skills: %w", err)
		}
		if _, err := q.DeleteProductSkills(r.Context(), pgconv.UUID(id)); err != nil {
			return err
		}
		for _, s := range skills {
			if err := q.InsertProductSkill(r.Context(), store.InsertProductSkillParams{
				Name:        s.Name,
				Description: s.Description,
				Body:        s.Body,
				ProductID:   pgconv.UUID(id),
				UpdatedBy:   pgconv.UUID(actor.ID),
			}); err != nil {
				return err
			}
		}
		if err := q.MarkProductSkillsApplied(r.Context(), store.MarkProductSkillsAppliedParams{
			Sha:       stage.SourceSha,
			AppliedBy: pgconv.UUID(actor.ID),
			ID:        id,
		}); err != nil {
			return err
		}
		if err := q.DeleteProductSkillStage(r.Context(), id); err != nil {
			return err
		}
		applied = len(skills)
		view, err = h.productSkillsView(r.Context(), q, id)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "product not found")
		return
	case errors.Is(err, errProductDeleted):
		httpx.Error(w, http.StatusConflict, "product is deleted")
		return
	case errors.Is(err, errNothingStaged):
		httpx.Error(w, http.StatusConflict, "no skills are staged for this product; sync first")
		return
	case errors.Is(err, errStagedShaMismatch):
		httpx.Error(w, http.StatusConflict, "the staged set changed since you reviewed it; review it again")
		return
	case err != nil:
		slog.Error("admin apply product skills", "product_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("admin applied product skills", "actor_id", actor.ID, "product_id", id, "sha", req.ExpectedSHA, "skills", applied)
	httpx.JSON(w, http.StatusOK, view)
}

var (
	errNothingStaged     = errors.New("no staged skills")
	errStagedShaMismatch = errors.New("staged sha mismatch")
)

// skillsSourcePatch is the validated skills-source part of an AdminPatchProduct body. Nil fields
// are untouched.
type skillsSourcePatch struct {
	repoURL    *string
	ref        *string
	sealed     []byte
	clearToken bool
}

func (p skillsSourcePatch) set() bool {
	return p.repoURL != nil || p.ref != nil || p.sealed != nil || p.clearToken
}

// parseSkillsSourcePatch validates the skills fields of a product PATCH and seals the token. The
// returned error is a client-facing message that never echoes a value.
func (h *Handler) parseSkillsSourcePatch(id uuid.UUID, repoURL, ref, token *string, clearToken *bool) (skillsSourcePatch, error) {
	var p skillsSourcePatch
	if repoURL != nil {
		if err := h.validateProductSkillsURL(*repoURL); err != nil {
			return p, err
		}
		p.repoURL = repoURL
	}
	if ref != nil {
		if err := validateProductSkillsRef(*ref); err != nil {
			return p, err
		}
		p.ref = ref
	}
	if clearToken != nil && *clearToken {
		if token != nil {
			return p, errors.New("set skills_token or clear_skills_token, not both")
		}
		p.clearToken = true
	}
	if token != nil {
		if err := validateProductSkillsToken(*token); err != nil {
			return p, err
		}
		if h.box == nil {
			return p, errors.New("secret storage is not configured")
		}
		sealed, err := h.box.SealWithAAD([]byte(*token), productSkillsTokenAAD(id))
		if err != nil {
			slog.Error("seal product skills token", "product_id", id)
			return p, errors.New("could not store the token")
		}
		p.sealed = sealed
	}
	return p, nil
}

// applySkillsSourcePatch writes the source fields on q (inside the PATCH transaction) and
// discards the staged snapshot when the repo URL or ref actually changed: a snapshot of the old
// source must not stay approvable under the new one. The applied skills are untouched: they keep
// serving until a new apply.
func applySkillsSourcePatch(ctx context.Context, q *store.Queries, cur store.Product, p skillsSourcePatch) (store.Product, error) {
	params := store.UpdateProductSkillsSourceParams{ID: cur.ID, ClearToken: p.clearToken, SkillsTokenSealed: p.sealed}
	if p.repoURL != nil {
		params.SkillsRepoUrl = pgtype.Text{String: *p.repoURL, Valid: true}
	}
	if p.ref != nil {
		params.SkillsRef = pgtype.Text{String: *p.ref, Valid: true}
	}
	updated, err := q.UpdateProductSkillsSource(ctx, params)
	if err != nil {
		return cur, err
	}
	if updated.SkillsRepoUrl != cur.SkillsRepoUrl || updated.SkillsRef != cur.SkillsRef {
		if err := q.DeleteProductSkillStage(ctx, cur.ID); err != nil {
			return cur, err
		}
	}
	return updated, nil
}
