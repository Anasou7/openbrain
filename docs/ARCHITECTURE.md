# Architecture

OpenBrain is a small, self-hosted service that keeps the shared state of
your projects: where each participant (you and any number of AI agents) left
off, what was decided, what is blocked, and what happened recently. People use
it through a server-rendered dashboard, AI agents through MCP, and scripts
through a JSON API.

```
   browser (you)          AI agents (any MCP client)        scripts
        │ cookie session       │ Bearer <actor token>          │ Bearer <actor or admin token>
        ▼                      ▼                               ▼
  ┌──────────────────────── openbrain (one Go binary, one process) ────────┐
  │ internal/web        dashboard + templates  │  /mcp           │  /api/...   │
  │                                            │ internal/mcpserver (SDK)      │
  │                     └───────────────┬──────┴──────────┬──────┘             │
  │ internal/store      validation, semantic operations, auth, migrations      │
  │                                     │ database/sql (pure-Go SQLite)        │
  └─────────────────────────────────────┼──────────────────────────────────────┘
                                        ▼
                           openbrain.db (+ -wal, -shm), a local file
```

## Principles

- **One binary, one file.** No runtime dependencies besides the OS. The SQLite
  driver is pure Go (`modernc.org/sqlite`), so builds need no C toolchain and
  the binary is static.
- **The application owns the database.** Nothing else opens `openbrain.db`
  for writing; there is no generic query endpoint. Every write goes through a
  semantic operation in `internal/store` that validates input, records who made
  the change, and appends to the activity log in the same transaction.
- **Agents are data.** Actors (the human, Claude, Codex, a cron job, …) are
  rows in a table. Nothing in the schema or code names a specific agent.
- **Facts are kept apart from interpretation.** A handoff stores checkable
  facts (branch, commit SHA, metadata) in their own columns, separate from the
  actor's narrative (summary, state, next steps). Every record carries the
  actor that wrote it. That actor comes from the caller's credential, not
  from anything the caller says. The activity log also records the channel
  (`dashboard`, `api`, `admin-api`, `mcp`).
- **Small and quiet.** No background workers or polling. The process sleeps
  between requests.

## Packages

| Path | Responsibility |
|------|----------------|
| `cmd/openbrain` | CLI entry point: `serve`, `migrate`, `backup`, `restore`, `user`, `actor`, `token`, `version`. Reads configuration from the environment. |
| `internal/store` | Opens SQLite, runs embedded migrations, validates input, and implements every read and write, including accounts, password hashing, sessions, token authentication and idempotency. Returns typed errors (`ErrNotFound`, `ErrConflict`, `ErrInvalidToken`, `ErrInvalidLogin`, `ErrSetupDone`, `*ValidationError`). |
| `internal/web` | HTTP: base-path routing, JSON API (bearer token), dashboard and sign-in (server-side session cookie), first-run setup, sign-in rate limiter, health endpoints, security headers, request logging. Templates, CSS, JS and fonts are embedded. Mounts `/mcp`. No SQL. |
| `internal/mcpserver` | The MCP adapter, built on the official Go SDK. Nineteen tools, each a call into `internal/store`. No SQL and no business rules of its own. |

## Data model

All tables are `STRICT`. Timestamps are UTC ISO-8601 text with millisecond
precision (`2026-01-02T15:04:05.000Z`), which sorts correctly as a string.

| Table | Purpose |
|-------|---------|
| `actors` | Who writes: `slug` (stable handle), `display_name`, `kind` (`human`, `agent`, `automation`), `metadata` JSON. Migration 1 seeds one human actor, `user`; the first account claims it (see below). |
| `projects` | Numeric `id` (identity) plus unique `slug` (handle). Name, description, `status` (`active`, `paused`, `done`, `archived`), `current_focus`, `repo_url`, `default_branch`, timestamps, `archived_at`. Identity never depends on a filesystem path. |
| `notes` | Free-form notes on a project by an actor, with an optional `kind` category and `pinned` flag. Only the author may edit a note's content; anyone may pin it. |
| `handoffs` | Immutable records of where an actor left a project after a session. Interpretation: `summary`, `current_state`, `current_task`, `blockers`, `next_steps`. Facts: `branch`, `commit_sha`, `metadata`. History is never overwritten. |
| `decisions` | Durable decisions with optional reasoning. |
| `blockers` | Open/resolved items. Resolved state is derived from `resolved_at`; `resolved_by` and `resolution` record who closed it and how. |
| `actor_states` | One row per (project, actor): `state` (`idle`, `working`, `blocked`, `waiting`, `finished`), `current_task`, `started_at`, `finished_at`, `updated_at`. |
| `activity` | Append-only timeline: `kind` (`note.added`, `handoff.created`, `blocker.resolved`, …), `summary`, `subject_id`, `actor_id`, `via` (`dashboard`, `api`, `admin-api`, `mcp`). Written in the same transaction as the change it describes. |
| `actor_tokens` | Per-actor credentials (migration 2): `actor_id`, `token_hash` (SHA-256, unique), `prefix` (first 12 characters, for recognition), `label`, `created_at`, `last_used_at` (updated at most once a minute), `revoked_at`. The plaintext is never stored. |
| `idempotency_keys` | (migration 2) `(actor_id, key)` mapped to the operation and the record it created, so retried creates return the original record. Pruned after 7 days. |
| `users` | People who sign in (migration 3): `username` (unique, case-insensitive), `actor_id` (unique: one human actor per account), `password_hash` (Argon2id PHC string), `role` (`admin` or `user`), `created_at`, `password_changed_at`, `last_login_at`, `disabled_at`. |
| `user_sessions` | (migration 3) Browser sessions: `user_id`, `token_hash` (SHA-256, unique), `created_at`, `last_seen_at`, `expires_at`, `revoked_at`. Deleted with their user. |
| `project_updates` | (migration 4) Human-readable statements of what meaningfully changed, written for the project owner: `title`, `summary`, optional `details` and `task_reference`, `category` (`new`, `improved`, `fixed`, `changed`, `important`), the author `actor_id`, and `handoff_id` when published with a handoff (at most one update per handoff). Append-only: a correction is a newer update. Indexed by (project, id), (project, actor, id) and (actor, id) for history and latest-per-project and latest-per-actor lookups. |
| `profile_sections` | (migration 5) The project profile: durable, plain-text knowledge of what a project is and does, for people and assistants without the code. One row per (project, `key`), unique; keys are canonical (`overview`, `icp`, `limitations`, ...) or custom, with no migration needed for new ones. `status` is `known`, `unknown` or `not_applicable`, so gaps are explicit (a `known` section must have content). Provenance: `created_by`, `updated_by`, timestamps, and an optional `source_branch` and `source_commit`. |
| `features` | (migration 5) User-facing features: `slug` (unique per project), `name`, `summary`, `description`, `target_users`, `use_cases` (one per line), `limitations`, and `status` (`planned`, `experimental`, `available`, `deprecated`, `removed`). Never deleted; removed features are hidden by default. Same provenance as sections. Indexed by (project, status). |

### Key queries

- **Latest handoff per actor** for a project:
  `WHERE id IN (SELECT max(id) FROM handoffs WHERE project_id = ? GROUP BY actor_id)`,
  served by the index `handoffs (project_id, actor_id, id)`. IDs are monotonic,
  so `max(id)` is the newest row without comparing timestamps.
- **Project context** runs ~9 small indexed queries inside one read
  transaction, so the bundle is a consistent snapshot.

### Stale states

An agent that crashes leaves its state at `working` forever. Every state row
has `updated_at` (the last time the actor reported, even if nothing changed).
Reads compute `stale = state ∈ {working, waiting, blocked} AND now − updated_at > OPENBRAIN_STALE_AFTER`
(default 1h) and return it alongside the raw timestamp, so clients can apply
their own threshold. Heartbeats that change nothing update `updated_at` but do
not add activity entries.

## Migrations

SQL files in `internal/store/migrations/` named `NNNN_description.sql` are
embedded in the binary. The database's `PRAGMA user_version` records how many
have been applied. Each pending migration runs in its own transaction together
with the `user_version` bump, so a failed migration leaves the database
unchanged. `serve` applies pending migrations at startup; `migrate` does it
explicitly. A binary refuses to run against a database with a newer schema
than it knows.

Never edit a released migration; add a new file.

## Concurrency

SQLite allows one writer at a time. The store opens two `database/sql` pools
on the same file:

- **writer** — one connection, `BEGIN IMMEDIATE` transactions. Writers queue
  in-process for the connection instead of racing for the file lock and
  failing with `SQLITE_BUSY`.
- **readers** — up to four connections with `query_only`. In WAL mode they read
  concurrently with the writer.

Pragmas: `journal_mode=WAL`, `synchronous=NORMAL` (durable across application
crashes; a power loss can drop the last few commits but never corrupts),
`foreign_keys=ON`, `busy_timeout=5000` (covers other processes such as the
backup CLI).

## Authentication

People and machines authenticate differently, and the two never mix: a
browser session is accepted only by dashboard pages, and bearer tokens only by
`/api/` and `/mcp`.

### Accounts

- **Users map to actors.** Every account owns exactly one human actor
  (`users.actor_id` is unique), and everything a person does on the dashboard
  is attributed to that actor with `via=dashboard`. Creating an account claims
  an unclaimed human actor with the same slug if one exists; otherwise the
  **first** account claims the seeded `user` actor and renames it to the
  username, so history written before setup becomes theirs; otherwise a new
  human actor is created. Agent slugs can't be taken as usernames.
- **First-run setup.** While `users` is empty, every page redirects to
  `/setup`. Submitting it hashes the password first, then opens a
  `BEGIN IMMEDIATE` transaction, re-counts users and inserts only if there are
  still none. Two simultaneous submissions therefore produce exactly one
  administrator; the other gets `ErrSetupDone`. After that `/setup` is a 403
  page. The first account is always `admin`.
- **Roles.** `admin` manages users, actors and tokens; `user` works with
  projects and their own account. The last active administrator can't be
  disabled.
- **Passwords** are hashed with Argon2id (m = 12 MiB, t = 3, p = 1, 16-byte
  salt, 32-byte key) in PHC format, so the parameters travel with each hash
  and outdated hashes are replaced on the next successful sign-in. Hashing is
  serialised to one at a time, with a GC straight after, which keeps a burst of
  sign-ins inside the memory budget. Unknown usernames are checked against a
  dummy hash, so timing doesn't reveal which accounts exist. Passwords are
  8–1024 bytes and can't equal the username.
- **Sessions** are 32 random bytes in a cookie (`openbrain_session`,
  `HttpOnly`, `SameSite=Lax`, `Secure` over HTTPS, `Path` = the base path).
  Only the SHA-256 hash is stored. They slide: idle sessions expire after 30
  days, and every session after 90. `last_seen_at` is written at most hourly.
  Signing out revokes the row; changing your password can sign out your other
  sessions; an administrator's password reset or disabling an account revokes
  all of that user's sessions, and disabled users are rejected on every
  request. Dashboard form posts are refused unless `Sec-Fetch-Site` (or
  `Origin`) says they came from this site.
- **Sign-in rate limiting** is in memory: 5 failures per (username, client)
  and 20 per client in 15 minutes, then `429` with `Retry-After`. A successful
  sign-in clears that pair. The table is capped at 10,000 keys. The client
  address is the TCP peer unless the peer is in `OPENBRAIN_TRUSTED_PROXIES`, in
  which case it is the right-most untrusted `X-Forwarded-For` entry. The same
  trust rule decides whether `X-Forwarded-Proto: https` marks cookies
  `Secure`. All failures show one message, "Invalid username or password."

### Tokens

- **Admin token** (optional): `OPENBRAIN_TOKEN` (at least 32 characters) from
  the environment, for scripts that administer through the API. Unset means
  there is no admin API credential at all. The comparison hashes both sides
  with SHA-256 and uses `crypto/subtle`, so it is constant-time regardless of
  length. Writes made with it name their actor in the body and are logged as
  `via=admin-api`.
- **Actor tokens**: `obt_` plus 43 base64url characters (256 random bits from
  `crypto/rand`), issued by `openbrain token create` or the dashboard's
  Agents page. Each is bound to exactly one actor. Tokens issued before the
  rename (`pbt_…`) remain valid.
  - Only the SHA-256 hash is stored. With 256-bit secrets there is nothing to
    brute-force, so a slow password hash would only cost CPU.
  - Lookups go by hash. Revoked or unknown tokens get the same
    `ErrInvalidToken`.
  - **The actor is taken from the token and never from the request.** On the
    REST API, a body naming a different actor gets 403. On MCP, no tool has an
    actor argument, and extra arguments fail schema validation.

**MCP** uses the SDK's bearer middleware with a verifier that calls
`store.Authenticate`, and puts the actor slug in the request's `TokenInfo`.
The admin token and session cookies are refused there. Go's
`http.CrossOriginProtection` provides the Origin validation the MCP spec
requires.

**Evolving it.** Per-token scopes would add a column to `actor_tokens` and a
check in the two places that build a principal. OAuth would replace the
verifier function. Neither touches the tools or the store operations.

## Base path

`OPENBRAIN_BASE_PATH` (default `/`) is normalised once at startup to `""` or
`"/prefix"` (leading slash, no trailing slash, no `..`). The top-level handler
strips the prefix before routing: the bare prefix redirects to `prefix/`, and
anything outside it is `404`, so the internal routes never know about it.
Everything that emits a URL goes through one helper (`Server.url`, the `u`
template function): links, form actions, redirects, the `Path` of the session
cookie, static assets and the MCP URL shown on the Agents page. Static assets
get a content-hash query (`?v=`) and are cached as immutable.

## Dashboard

Server-rendered `html/template` pages, with no JavaScript framework and no
build step. All data and forms work without JavaScript; `app.js` (about 2 KB)
adds the collapsible rail, copy buttons and the phone-width menu sheet (the
one piece that needs it). The visual
system follows [DESIGN.md](../DESIGN.md): monochrome tokens as CSS custom
properties, a framed shell with a left rail, a right context rail that
collapses below 1400 px, and a bottom bar with a menu sheet below 1024 px.
Status is never colour-only: each state has a glyph and a word. The fonts,
League Gothic and Geist Mono (SIL OFL), are embedded and self-hosted, and the
Content Security Policy allows nothing from other origins.

## MCP

`/mcp` is served by the same process through the official Go SDK
(`github.com/modelcontextprotocol/go-sdk`).

- **Protocols.** It runs Streamable HTTP in stateless mode, which serves the
  2026-07-28 protocol and also answers the legacy `initialize` handshake, so
  clients from 2024-11 to 2025-11 keep working. Nothing is held between
  requests: no sessions, no SSE streams.
- **Responses** are plain JSON.
- **Schemas.** Tool input and output schemas are inferred from Go types. Store
  types are reused for outputs, so MCP and REST return the same shapes. The
  SDK validates inputs before a handler runs and outputs before they are sent.
- **Compact context** lives in the store (`ContextOptions`), not the adapter,
  so REST (`?compact=true`) and MCP share it.
- **Repository lookup** normalises git remotes (`RepoKey`) and compares them
  in Go. A linear scan is fine for hundreds of projects; an indexed column
  would be the upgrade path.

## Backups

`openbrain backup` runs `VACUUM INTO`, which writes a transactionally
consistent, compacted copy while the server keeps running, then runs
`PRAGMA integrity_check` on the copy. `openbrain restore` verifies the
backup, saves the current database next to itself, copies the backup in with
SQLite's online backup API, and applies any migrations the backup predates.
Copying `openbrain.db` with `cp` while the server runs is unsafe (the WAL file
holds recent commits) and is not offered.

## What is deliberately absent

The following are left out on purpose:
- agent-specific integrations (there is one MCP server for all clients)
- repository sync or scanning
- embeddings and vector search
- LLM calls
- background jobs and heartbeat daemons
- OAuth, SSO, two-factor authentication, email and password-reset mail
  (administrators reset passwords; `openbrain user` recovers from the shell)
- external services
