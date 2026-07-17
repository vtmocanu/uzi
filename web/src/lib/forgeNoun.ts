// The SINGLE per-forge terminology mapping site (PRD #65 D2). uzi boards are
// mixed-forge — one board can carry a GitLab card (Merge Request) beside a Forgejo
// card (Pull Request) — so the noun is chosen per card/run from its forge_type,
// never scattered as ternaries across ~8 components. Its Go twin is
// slacksvc/notifier.go (forgeMrNoun), kept adjacent-in-review with the same mapping.
//
// Acceptance (D2): a grep for the Title-Case GitLab-noun literal across web/src
// returns exactly one non-test hit — MR_NOUN below. Every other exported form here
// DERIVES from MR_NOUN rather than carrying its own copy of that literal, so the
// invariant holds.
//
// Shared / forge-less chrome (nav, app-shell, admin, docs) has no forge_type to key
// off and MUST NOT default to one forge's word: it names both or restructures past
// the noun. Those surfaces do not call this helper.

const FORGEJO = "forgejo";

// MR_NOUN is the one place the GitLab noun is spelled. The default (unknown / absent
// forge_type) is GitLab's noun — every connection that can exist today is GitLab
// (the forgejo handler gate flips last, PRD #65 M6b), so this preserves the exact
// pre-Forgejo rendering for existing cards, never a shared-chrome guess.
const MR_NOUN: Record<string, string> = {
  gitlab: "Merge Request",
  [FORGEJO]: "Pull Request",
};

// forgeNoun is the Title-Case noun for a button/label (GitLab vs Forgejo phrasing).
export function forgeNoun(forgeType: string | null | undefined): string {
  return MR_NOUN[forgeType ?? ""] ?? MR_NOUN.gitlab;
}

// forgeNounLower is the noun for mid-sentence prose ("open the merge request").
export function forgeNounLower(forgeType: string | null | undefined): string {
  return forgeNoun(forgeType).toLowerCase();
}

// forgeNounSentence capitalizes the first letter for a sentence-initial noun
// ("Merge request merged"), derived from the lower form so no second literal exists.
export function forgeNounSentence(forgeType: string | null | undefined): string {
  const n = forgeNounLower(forgeType);
  return n.charAt(0).toUpperCase() + n.slice(1);
}

// mrAbbrev is the compact prefix a meta chip shows before the number: GitLab "MR",
// Forgejo "PR".
export function mrAbbrev(forgeType: string | null | undefined): string {
  return forgeType === FORGEJO ? "PR" : "MR";
}

// mrRefSymbol is the forge's reference sigil for the request number: GitLab writes
// "!42" for a merge request, Forgejo "#42" for a pull request.
export function mrRefSymbol(forgeType: string | null | undefined): string {
  return forgeType === FORGEJO ? "#" : "!";
}
