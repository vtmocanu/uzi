# Uzinele Întunecate (uzi)

AI dark factory. Local laptop demo: Go API + React SPA + PostgreSQL, run via docker-compose.

## Quick Start

```sh
git submodule update --init      # pulls inspiration/ (bottega, multica, dot-agent-deck)
cp .env.example .env
# set JWT_SECRET and POSTGRES_PASSWORD in .env, e.g.:
#   openssl rand -hex 64   -> JWT_SECRET
#   openssl rand -hex 24   -> POSTGRES_PASSWORD
docker compose up
```

Open <http://127.0.0.1:8080> and register. The first registered account becomes admin.

## Documentation

Full docs live in [docs/](docs/):

- [Installation](docs/installation.md)
- [Configuration](docs/configuration.md)
- [Auth design](docs/auth-design.md)

See [ARCHITECTURE.md](ARCHITECTURE.md) for the system shape, [prds/](prds/) for product specs, and [specs/](specs/) for the requirements contract ([human.md](specs/human.md)) and design decisions ([ai.md](specs/ai.md)).

## Contributing

Internal project; see [plan.md](plan.md) for direction and `prds/` for active work.

## License

Unlicensed, internal use only.
