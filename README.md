# Uzinele Întunecate (uzi)

AI dark factory. Local laptop demo: Go API + React SPA + PostgreSQL, run via docker-compose.

## Quick Start

```sh
git submodule update --init      # pulls inspiration/ (bottega, multica, dot-agent-deck)
cp .env.example .env
# set JWT_SECRET, UZI_SECRET_KEY and POSTGRES_PASSWORD in .env, e.g.:
#   openssl rand -hex 64      -> JWT_SECRET
#   openssl rand -base64 32   -> UZI_SECRET_KEY
#   openssl rand -hex 24      -> POSTGRES_PASSWORD
docker compose up
```

Open <http://127.0.0.1:8080> and register. The first registered account becomes admin — or set `UZI_SEED_EMAIL`/`UZI_SEED_PASSWORD` in `.env` to provision an admin automatically at startup (see [configuration.md](docs/configuration.md)). Admins can define agent templates under **Agents**; any user can connect their own Anthropic token under **Settings** (see [docs/anthropic-token.md](docs/anthropic-token.md)).

Each git checkout/worktree gets its own isolated compose stack and `pgdata` volume (no shared `name:` in `docker-compose.yml`), so running the stack from a PRD worktree never touches another checkout's data.

### Connect a forge

The board works against GitLab issues, via a per-user bot account:

1. Create a bot account and an `api`-scoped PAT, add it as Developer to a project — see [docs/gitlab-bot-setup.md](docs/gitlab-bot-setup.md) (`scripts/create-gitlab-bot.sh` automates the admin path).
2. In uzi: **Forge** (top nav), pick a base URL and paste the PAT.
3. **Repos**, enable the project you added the bot to.
4. Open its board from the sidebar — `PRD`-labeled issues appear as cards; drag between columns to relabel on the forge, or let a run move them for you (see [docs/board.md](docs/board.md)).

### Run an agent

With a forge connected and your Anthropic token saved (**Settings**), have uzi work an issue end to end:

1. **Settings → Workers**, generate a join token, then run `docker compose --profile agent up` (or `docker run`) with it: see [docs/worker-setup.md](docs/worker-setup.md). The worker shows **online**.
2. On a board, **Create issue** to open a PRD-shaped issue in GitLab, or pick an existing `PRD`-labeled card. Click its title (or a card) to open the issue in-app.
3. **Start run** — the card moves to **In Progress** and a worker claims it; the run view streams the lead agent's progress live.
4. It pauses at **awaiting approval** with a plan: read it, approve (or reject with a reason). Any awaiting run also shows up in the board's attention strip.
5. Watch the implement ⇄ review loop stream per agent; send a follow-up message if it needs steering.
6. On completion, a branch and MR are open, linked from the run view and the card, and the issue moves to **Human Review**. `main` is never touched: see [ARCHITECTURE.md](ARCHITECTURE.md#guardrail-layers-the-primary-directive) for why that holds even under an adversarial prompt.

## Documentation

Full docs live in [docs/](docs/), and the same golden-path pages are
browsable in-app under **Docs**:

- [Getting started](docs/getting-started.md)
- [GitLab bot setup](docs/gitlab-bot-setup.md)
- [Board](docs/board.md)
- [Anthropic token](docs/anthropic-token.md)
- [Agent templates](docs/agent-templates.md)
- [Worker setup](docs/worker-setup.md)
- [Installation](docs/installation.md)
- [Configuration](docs/configuration.md)
- [Auth design](docs/auth-design.md)
- [Proc hardening](docs/proc-hardening.md)
- [Developer conventions](docs/dev-conventions.md)

See [ARCHITECTURE.md](ARCHITECTURE.md) for the system shape, [prds/](prds/) for product specs, and [specs/](specs/) for the requirements contract ([human.md](specs/human.md)) and design decisions ([ai.md](specs/ai.md)).

## Contributing

Internal project; see [plan.md](plan.md) for direction and `prds/` for active work.

## License

Unlicensed, internal use only.
