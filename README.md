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
2. In uzi: **Settings → Forge**, pick a base URL and paste the PAT.
3. **Repos**, enable the project you added the bot to.
4. Open its board from the sidebar — `PRD`-labeled issues appear as cards; drag between columns to relabel on the forge.

## Documentation

Full docs live in [docs/](docs/):

- [Installation](docs/installation.md)
- [Configuration](docs/configuration.md)
- [Auth design](docs/auth-design.md)
- [GitLab bot setup](docs/gitlab-bot-setup.md)
- [Agent templates](docs/agent-templates.md)
- [Anthropic token](docs/anthropic-token.md)

See [ARCHITECTURE.md](ARCHITECTURE.md) for the system shape, [prds/](prds/) for product specs, and [specs/](specs/) for the requirements contract ([human.md](specs/human.md)) and design decisions ([ai.md](specs/ai.md)).

## Contributing

Internal project; see [plan.md](plan.md) for direction and `prds/` for active work.

## License

Unlicensed, internal use only.
