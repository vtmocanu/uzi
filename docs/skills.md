---
title: Agent skills
order: 55
audience: user
---

# Agent skills

A skill is a named Markdown playbook an agent can pull into a run: reusable
domain knowledge like "how CI/CD works here" without bloating every agent
template's prompt. Only the skill's name and one-line description sit in an
agent's context all the time; the full body loads on demand, when the model
decides the skill is relevant (progressive disclosure).

## Where a skill comes from

Four sources, in this precedence order (a name collision keeps the highest
and drops the rest):

1. **Your own skills** ("Mine"): private playbooks only you can see or
   allocate.
2. **Global**: admin-authored, visible to everyone.
3. **Builtin**: shipped with uzi, starting with `ci-cd-norms` (below).
4. **Repo skills**: opt-in per repo, lowest precedence (see below).

## Create or edit a skill

1. Open **Skills** and click **New skill**, or pick an existing one you own
   (or, as an admin, any global/builtin skill).
2. Set its name: kebab-case, lowercase letters/digits/hyphens only, up to 64
   characters. **The name is permanent**: it is the skill's identity and how
   it is addressed in an allocation; renaming means creating a new skill.
3. Write a single-line description. This is what the model routes on, so
   keep it to one focused sentence about when and why an agent should reach
   for the skill.
4. Write the body in Markdown (up to 64 KiB). Anyone can create a personal
   skill; only an admin can create or edit a global one. Builtin skills can
   be edited by an admin and reset to their shipped default, but never
   deleted.

## Allocate a skill to an agent

Skills only reach a run if they are allocated to the template that run uses.
Open an agent template's detail page for the allocation panel:

- **Shared** (admin-managed): applies to everyone's runs on that template.
- **Mine** (self-service): your own overlay on top of the shared set,
  visible only in your own runs.

Your runs get the **union** of the two, shown at the top of the panel. A
shared allocation may only reference builtin or global skills; your overlay
may also reference your own user skills.

## Name shadowing

If your overlay allocates a skill with the same name as one already shadowed
by precedence, say a personal `ci-cd-norms` next to the builtin one, the
higher-precedence skill's **body** replaces the lower one's for every run of
yours, not just this template. The lower-precedence skill is dropped from
your run entirely: only one body per name ever loads. This is intended
run-wide shadowing (user beats global beats builtin beats repo skill), not a
per-template override.

## Limits and drops

A run can carry at most 32 skills (`SKILLS_MAX_PER_RUN`) across all its
templates combined, and each skill's body is capped at 64 KiB
(`SKILL_MAX_BYTES`); an admin can raise either instance-wide. A skill that
loses a name collision, or that would push a run over the count cap, is
dropped rather than failing the run; both kinds of drop are logged as a
message in the run's transcript so it is never a silent surprise. When
skills and repo skills combined exceed the cap, repo skills (lowest
precedence) are the ones dropped first.

## Repo skills (opt-in, default off)

A repo can carry its own skills in `.claude/skills/*/SKILL.md`, the same
layout Claude Code itself uses. uzi never loads these automatically: on the
**Repos** page, an owner (or an admin) must click **Load repo skills** for
that specific repo and accept the warning.

When enabled, uzi loads **only** `name` and `description` plus the body from
each `SKILL.md` in that repo, at the **lowest** precedence (a delivered
skill of the same name always wins), for every agent template in the run.
Nothing else under the repo's `.claude/` is ever read: no hooks, no
settings, no commands, no `CLAUDE.md`, and no other frontmatter key (a key
like `allowed-tools` would grant capabilities, so it is stripped rather than
carried through). Enable this only for a repo whose merge-request review
discipline you actually trust: a hostile skill body still cannot push code
or bypass any other guardrail, but it does get the same trusted framing as
any other skill.

## The `ci-cd-norms` builtin

Ships with uzi: the example CI/CD norm (`myorg/pipelines` includes, Harbor,
ArgoCD GitOps via `argo-apps`), how to recognize a repo that
deviates from it, and example-app as the worked exception. Allocate it to `coder`
or `reviewer` so agents extend a pipeline the example way instead of
fighting it. Like any builtin, an admin can edit it in place or reset it to
the shipped version.

## Security notes

- A skill body is **not** scrubbed beyond the same high-confidence Anthropic
  token check used for agent template prompts: never paste a credential
  into a skill's description or body.
- The `name`/`description` frontmatter a run actually sees is synthesized
  and escaped by uzi at delivery time; the body you write is never
  reinterpreted as frontmatter, so it cannot forge a second skill entry.

## Verifying a skill actually loads (manual check)

Automated tests cannot prove a real Claude session loads a skill's body
under uzi's lockdown settings, since that needs a live Anthropic session
uzi's test suite is not allowed to run. To confirm it yourself: start a run
against a throwaway repo with one skill allocated whose body contains a
unique sentinel phrase, and a prompt that requires consulting that skill.
The run's transcript should show the agent expanding `uzi:<skill-name>` and
acting on the sentinel, confirming the skill actually reached the model, not
just its listing.

See [ARCHITECTURE.md](../ARCHITECTURE.md#agent-skills) for how skills are
stored, assembled, and delivered to the worker.
