---
title: Glossary
order: 11
audience: user
---

# Glossary

uzi turns an issue on your code host into a plan, waits for your approval, writes the code and opens a merge request. This page names the words that uzi uses for that work.

## Where the work comes from

**Forge**:
The code host that holds your repositories and issues: GitHub, GitLab or Forgejo.
_Avoid_: git host, provider, SCM

**Bot account**:
A separate forge account that uzi uses to read issues, push branches and open merge requests. Its token is what you connect to uzi, never your own.
_Avoid_: service user, robot

**Board**:
The uzi view of the issues of one repository, in columns. The columns between Backlog and Closed are labels on the forge.
_Avoid_: kanban, project

**Merge request**:
The proposed change that a run opens on the forge. GitHub calls it a pull request.
_Avoid_: MR and PR in the same text

## How the work runs

**Run**:
One piece of agent work, most often on one issue, from the plan to the merge request.
_Avoid_: job, task, session

**Plan gate**:
The point where a run stops and waits for a person to approve, revise or reject its plan.
_Avoid_: approval step, checkpoint

**Lead**:
The agent that plans a run and hands its parts to the other agents.
_Avoid_: main agent, orchestrator

**Harness**:
The engine that executes a run: Claude or Codex. One run uses one harness from start to end.
_Avoid_: runtime, backend

**Capability**:
A hard need of a run, such as Docker, that only some workers meet.
_Avoid_: feature, requirement

## Where the work runs

**Worker**:
The container that executes runs. It connects out to uzi and asks for work.
_Avoid_: agent container, runner

**Join token**:
The secret that lets a new worker connect to uzi. uzi shows it once, when you create the worker.
_Avoid_: worker key, API key

**Model credential**:
Your own login for the model that does the work: an Anthropic token for Claude, or a Codex login or an OpenAI API key for Codex.
_Avoid_: API key, model token

## Who sets it up

**First admin account**:
The admin account that the API creates at its first start when `UZI_SEED_EMAIL` and `UZI_SEED_PASSWORD` are set in `.env`. Without them, the first account that registers becomes the admin.
_Avoid_: seeded admin, bootstrap admin
