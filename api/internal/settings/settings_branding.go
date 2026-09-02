package settings

// This file holds the instance-branding accessor/type, their bounds consts and
// their write-time validators (PRD #1021 M2, split verbatim from settings.go).

import (
	"context"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// BrandingConfig is the allowlisted instance-branding config (PRD #685 M1, extended
// by PRD #780 M1): EXACTLY the seven branding keys, coerced to their typed form. It is
// the only thing the public GET /api/branding reads from settings — built key-by-key
// here rather than from All/AdminView so that anonymous read cannot leak any other
// settings key (Risk R1).
type BrandingConfig struct {
	AppLogoMode     string
	AppLogoPreset   string
	AppLogoKeepName bool
	BrandMode       string
	BrandCompany    string
	BrandPlacement  string
	BrandPlaque     bool
}

// Branding returns the effective branding config (PRD #685 M1), reading each of the
// seven keys individually through the same ENV-over-DB-over-default precedence every
// other accessor uses. The two bools apply the same junk-tolerance: only
// "true"/"false" are honored and any other stored value falls back to the compiled-in
// default rather than silently reading false. A cold-refresh error is returned
// alongside a defaults-filled struct so a best-effort caller can ignore err.
//
// It DELIBERATELY does not range over Defaults (as All/AdminView do): the public
// endpoint that consumes this serves anonymous callers, so it must expose only these
// seven fields and never the rest of the non-secret settings surface (Risk R1).
func (c *Cache) Branding(ctx context.Context) (BrandingConfig, error) {
	m, err := c.snapshot(ctx)
	boolOf := func(key string) bool {
		switch c.effective(key, m) {
		case "true":
			return true
		case "false":
			return false
		default:
			return Defaults[key] == "true"
		}
	}
	return BrandingConfig{
		AppLogoMode:     c.effective(KeyAppLogoMode, m),
		AppLogoPreset:   c.effective(KeyAppLogoPreset, m),
		AppLogoKeepName: boolOf(KeyAppLogoKeepName),
		BrandMode:       c.effective(KeyBrandMode, m),
		BrandCompany:    c.effective(KeyBrandCompany, m),
		BrandPlacement:  c.effective(KeyBrandPlacement, m),
		BrandPlaque:     boolOf(KeyBrandPlaque),
	}, err
}

// maxBrandCompanyLen caps the POWERED BY company text (PRD #685 M1). 64 runes, the
// same visual cap as a label; unlike ValidateLabel this is measured in RUNES via
// utf8.RuneCountInString so a multibyte name is not undercounted.
const maxBrandCompanyLen = 64

// validateBrandCompany is the DEDICATED write-time gate for brand_company (PRD #685
// M1). It deliberately does NOT reuse ValidateLabel: the branding company text may be
// empty (the default) and may contain commas ("Acme, Inc."), both of which
// ValidateLabel rejects. It DOES enforce a 64-rune cap and — because this text is
// admin-authored yet rendered into every user's chrome, including signed-out
// (the "rendered to a principal other than the author" class .claude/rules/web.md
// governs) — it rejects control and Unicode-format runes via termsafe.Validate, so an
// RTL-override or zero-width rune cannot mangle the chrome for everyone. The empty
// value passes termsafe.Validate (no runes to reject), so no special case is needed.
func validateBrandCompany(value string) error {
	if utf8.RuneCountInString(value) > maxBrandCompanyLen {
		return fmt.Errorf("must be at most %d characters", maxBrandCompanyLen)
	}
	return termsafe.Validate("brand_company", value)
}

// brandingSlugRE is the SHAPE gate for app_logo_preset (PRD #780 M1): a short,
// lowercase web-catalog slug. Empty is handled by the caller (means "no preset");
// a non-empty value must start with a-z and contain only a-z, 0-9 and hyphen, up to
// 32 chars total.
const maxBrandingSlugLen = 32

var brandingSlugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// validateBrandingSlug is the write-time gate for app_logo_preset (PRD #780 M1). It is
// a SHAPE check only: the empty string is allowed (it means "no preset" / leaving
// preset mode, and is also the compiled-in default), and any other value must be a
// short lowercase slug. It DELIBERATELY does not check the slug against any catalog —
// the web catalog is the source of truth and an unknown slug degrades gracefully in
// the UI, so validating membership here would couple the backend to that catalog.
func validateBrandingSlug(value string) error {
	if value == "" {
		return nil
	}
	if !brandingSlugRE.MatchString(value) {
		return fmt.Errorf("app_logo_preset must be a short lowercase slug (a-z, 0-9, hyphen; %d chars max)", maxBrandingSlugLen)
	}
	return nil
}
