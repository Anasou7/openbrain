# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.1.0] - Unreleased

The first public release.

### Shared project state
- Projects with status, current focus, repository URL and default branch.
- Notes (pinnable), immutable handoffs, decisions, blockers, per-actor working
  states with staleness detection, and an append-only activity log recording
  who made each change and through which channel.
- Actors are data: register any number of people, agents and automations.
- Repository lookup that matches SSH and HTTPS spellings of the same remote.

### Project profile
- A durable **project profile**: the non-code description of what a project
  is and does, so an assistant that never sees the repository can explain the
  product, answer customers and draft marketing.
  - **Knowledge sections** use canonical keys (overview, audience, ICP,
    problem, use cases, positioning, integrations, limitations, FAQ,
    terminology, pricing, …) or custom keys, which need no migration. Each is
    marked `known`, `unknown` or `not_applicable`, so gaps are explicit.
  - **Features** are first class: summary, description, target users, use
    cases, limitations, and a status (`planned`, `experimental`, `available`,
    `deprecated`, `removed`). Features are never deleted.
  - **Provenance:** who wrote and last changed each section and feature, when,
    and optionally the branch and commit it was derived from.
  - **Completeness:** a derived checklist of which canonical sections are
    known, unknown, not applicable or missing.
- MCP tools:
  - `get_project_profile`: the whole profile unclipped, one section, one
    feature, or features only.
  - `sync_project_profile`: comprehensive onboarding in one transactional,
    merging call; nothing omitted is deleted.
  - `update_project_knowledge` and `upsert_project_feature`: focused changes.
- REST:
  - `GET`, `PATCH /api/projects/{ref}/profile`
  - `GET`, `PUT`, `DELETE /api/projects/{ref}/profile/{section}`
  - `GET`, `POST /api/projects/{ref}/features`, plus `/features/{slug}`
  - `PATCH /api/features/{id}`
- Dashboard: a Profile page laid out as an editorial dossier, editable by
  signed-in people, and a product snapshot on each project page.
- `get_project_context` reports only the profile's size and freshness, not its
  content.

### History
- `get_project_updates` (category and actor filters) and `get_project_history`
  (handoffs, decisions, notes, activity) page through a project's full
  history, beyond the bounded context bundle.
- The REST list endpoints for notes, handoffs and decisions accept `before`;
  updates also accept `category`.

### Project updates
- A first-class **project update**: a short, patch-note-style statement of
  what meaningfully changed, written for the project owner. Each has a title,
  a summary, optional details and task reference, and a category (`new`,
  `improved`, `fixed`, `changed`, `important`). Updates are attributed to the
  publishing actor and append-only.
- Agents publish with the `publish_project_update` MCP tool, or with
  `user_update` on `record_handoff`. That creates the handoff and a linked
  update in one transaction.
- People publish from a project's Updates page; the signed-in user is the
  author.
- The dashboard overview shows each agent's latest meaningful work in the
  project grid and in a "Latest agent work" list. Every project page shows its
  latest update, and its Updates page groups updates by day, like patch notes.
- `get_project_context` includes recent updates; `get_overview` includes each
  project's latest update and the newest updates across projects.
- REST: `GET` and `POST /api/projects/{ref}/updates`, and
  `GET /api/projects/{ref}/updates/latest`.

### Interfaces
- **Dashboard:** server-rendered, no JavaScript framework, self-hosted fonts,
  and a strict Content Security Policy.
- **MCP server** at `/mcp`: Streamable HTTP, 19 tools, the stateless
  2026-07-28 protocol plus the legacy `initialize` handshake. Agents
  authenticate with their own actor tokens, so every write is attributable.
- **REST API** under `/api/`, with idempotency keys for retried creates.

### Accounts and security
- First-run setup at `/setup` creates the administrator. It is race-safe, and
  becomes unavailable once an account exists.
- Administrator and user roles, user management, password changes, and
  recovery from the command line.
- Argon2id password hashing, server-side sessions stored as hashes, HttpOnly
  and SameSite cookies (Secure over HTTPS), sign-in rate limiting, and
  same-origin checks on every form.
- Hashed actor tokens (`obt_…`), and an optional administrative API token
  (`OPENBRAIN_TOKEN`) for scripts.
- Sign-in logs record the client address and internal user IDs, never the
  submitted username.

### Operations
- One static Go binary and one SQLite file (WAL mode), with embedded
  migrations.
- A configurable base path (`OPENBRAIN_BASE_PATH`) for serving at the root of a
  host or under a prefix such as `/openbrain`, and trusted-proxy handling.
- `openbrain backup` and `restore` (consistent online snapshots), a hardened
  systemd unit and a nightly backup timer.
- Example Caddy and nginx configurations, including a raw-IP HTTPS variant.
- About 20 MB of RAM when idle, and no background CPU use.

### Upgrading from the pre-release "Project Brain"
- The old `BRAIN_*` environment variables are still read, with a warning.
- Actor tokens issued as `pbt_…` remain valid.
- The schema migrates automatically.
