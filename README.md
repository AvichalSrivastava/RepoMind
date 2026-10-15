# RepoMind

**Sprint Release Notes Generator** — pulls the active sprint from Jira, gathers the PRs that landed against it from GitHub, analyses them with an LLM, and publishes structured release notes to Confluence (one child page per sprint under a parent folder, auto-created on sprint activation). Built for the **Marvelous** sub-team on the SHL "Marvels" board; designed to fan out to sibling teams (Kang, IronMan, Gladiators) in V2.

## Status

> v1 — under active development. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the per-module roadmap and decision log.

| Module | Status |
|---|---|
| 0 — Scaffolding + domain layer | ✅ |
| 1 — Jira adapter | ✅ |
| 2 — GitHub adapter + webhook verifier | ✅ |
| 3 — Postgres persistence (pgx + outbox + UoW) | ✅ done |
| 4 — OpenAI-compatible LLM adapter (OpenAI / Azure OpenAI / GitHub Models) | ✅ done |
| 5 — Risk analyzer (per-PR classification) | ✅ done |
| 6 — QA recommendations analyzer | ✅ done |
| 7 — Release-notes composer | ✅ done |
| 8 — Confluence publisher | ✅ done |
| 9 — REST API + webhook handler | ✅ |
| 10 — Sprint worker (cron + on-webhook) | ✅ |

## Architecture

Clean Architecture + DDD. Inward dependency rule: `interfaces → application → domain ← infrastructure`.

Visual reference (Mermaid — GitHub renders inline): **[docs/diagrams/](docs/diagrams/)** — layered architecture, end-to-end webhook → Confluence sequence, and outbox state machine.

Deep dive: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (module status, decisions log, 6-layer dedup matrix, open items).

```
cmd/repomind/             # entry point (composition root)
internal/
  domain/                 # entities, value objects, sentinel errors — zero deps
  application/            # use-case ports (interfaces)
  infrastructure/         # adapters: jira, github, postgres, (openai, confluence)
configs/                  # environment templates (config.dev.env, config.example.env)
deploy/                   # Dockerfile, docker-compose
docs/                     # ARCHITECTURE.md and supporting docs
migrations/               # golang-migrate SQL files
scripts/
  git-hooks/              # secret-scanning pre-commit hook (install via `make hooks`)
  run-dev.ps1
secrets/                  # gitignored — GitHub App private key lives here
```

External integrations (all driven by ports in `internal/application/`):

| System | Role | Auth |
|---|---|---|
| Jira | Sprint source of truth | Basic (email + API token) |
| GitHub | PR data + webhooks | GitHub App (`ghinstallation/v2`) + HMAC-SHA256 |
| OpenAI / Azure OpenAI / GitHub Models | LLM analysis (one OpenAI-compatible adapter, dev default = GitHub Models) | API key or GitHub PAT |
| Confluence | Publishing target | Basic (same Atlassian token as Jira) |
| Postgres | State + outbox | pgx/v5 |

## Quick start

```bash
make hooks           # install secret-scanning pre-commit hook (do this first, once per clone)
make tidy            # download Go dependencies
cp configs/config.example.env .env   # then fill in tokens (file is gitignored)
make docker-up       # start Postgres + app via docker-compose
# OR for local dev with hot env loading:
make run-dev         # loads configs/config.dev.env + .env, then `go run ./cmd/repomind`
make test            # unit tests with race detector
make test-integration  # integration tests (testcontainers)
```

## Configuration

All configuration is via environment variables (12-factor) with the `REPOMIND_` prefix.

- **Public template:** [configs/config.example.env](configs/config.example.env) — placeholder values, safe to commit
- **Dev template:** [configs/config.dev.env](configs/config.dev.env) — SHL site values pre-filled, no secrets
- **Secrets:** create `.env` at the repo root (gitignored). Holds all `*_API_TOKEN`, `*_API_KEY`, `*_SECRET`, `*_PRIVATE_KEY*` values.

The GitHub App private key file goes in `./secrets/` (also gitignored).

## Security

- Pre-commit hook (`scripts/git-hooks/pre-commit`) blocks commits that contain `-----BEGIN … PRIVATE KEY-----`, or token shapes for GitHub, OpenAI, AWS, Atlassian, Slack, and GCP — plus any staged file matching `*.pem`, `*.key`, `*.p12`, `id_rsa*`, `credentials.json`, `service-account*.json`, `.env`, etc., even if force-added past `.gitignore`.
- Install once per clone with `make hooks`. Dry-run anytime with `make scan-secrets`.
- Bypass requires explicit `git commit --no-verify`.

## License

Apache-2.0