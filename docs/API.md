# HTTP API

All endpoints under `/api/` take and return JSON. This is the interface
scripts use. The dashboard and the MCP server call the same store operations
internally.

**Base path.** Paths below are relative to the base path. With
`OPENBRAIN_BASE_PATH=/openbrain`, `/api/projects` is served at
`/openbrain/api/projects`, and `/health` at `/openbrain/health`. Anything
outside the base path is `404`.

## Conventions

**Authentication.** Every `/api/` request needs `Authorization: Bearer <token>`,
using one of two kinds of credential:

| Credential | Who holds it | Writes are attributed to | Activity `via` |
|------------|--------------|--------------------------|----------------|
| **Actor token** (`obt_…`, from `openbrain token create`) | One agent or person on one machine | The token's actor, always. `actor` in the body may be omitted; if present it must match, otherwise `403`. | `api` |
| **Admin token** (`OPENBRAIN_TOKEN`, optional) | The operator | The actor named in the body's `actor` field (required) | `admin-api` |

Only the admin token can register actors through the API. It exists only if
the operator sets `OPENBRAIN_TOKEN`; otherwise administrators use the
dashboard or the `openbrain` command. It is meant for administrative scripts.
Anything whose writes should be attributable, including every AI agent,
should use its own actor token instead
([Credentials](DEPLOYMENT.md#credentials)). A missing, wrong or revoked token
returns `401`. `/health` and `/ready` need no token. The MCP endpoint accepts
actor tokens only; see [MCP.md](MCP.md).

**Browser sessions don't authenticate the API.** The dashboard's session
cookie is ignored on `/api/` and `/mcp`, so another site can't ride on your
signed-in browser to call them. Scripts always present a bearer token.

**Attribution.** Every write is recorded with its actor, and the activity log
also records the channel: `dashboard`, `api`, `admin-api` or `mcp`. Each
person with an account writes as their own human actor, whose slug is their
username.

**Idempotency.** The create endpoints (projects, notes, handoffs, decisions,
blockers) accept an optional `idempotency_key`, any string up to 128 bytes. A
retry with the same key from the same actor returns the original record
instead of creating another. Keys are kept for 7 days.

**Project references.** `{ref}` is a project's numeric `id` or its `slug`.
Slugs can't be all digits, so the two never collide.

**Strict input.** Bodies must be one JSON object of at most 1 MiB. Unknown
fields are rejected, so a typo fails loudly instead of being silently dropped.
Text is trimmed. Long text fields allow 64 KiB, and short ones (names, tasks,
branches, URLs) allow 1000 bytes.

**Responses.** `POST` returns `201` with the created object. `GET`, `PATCH`
and `PUT` return `200`. List endpoints wrap results in a named key
(`{"notes": [...]}`) and return newest first. The `limit` query parameter
defaults to 50 and is capped at 500.

**Timestamps** are RFC 3339 in UTC with millisecond precision, for example
`2026-01-02T15:04:05.123Z`. Empty optional timestamps are omitted.

**Errors** have this shape:

```json
{"error": {"code": "invalid", "message": "summary is required"}}
```

| Status | `code` | When |
|--------|--------|------|
| 400 | `invalid` | A field failed validation (including an unknown `actor`). |
| 400 | `bad_request` | Malformed JSON, unknown field, bad query parameter. |
| 401 | `unauthorized` | Missing, wrong or revoked token. |
| 403 | `forbidden` | An actor token tried to write as another actor, or to do something only the admin token may do. |
| 404 | `not_found` | No such project, note, blocker, update, or endpoint. |
| 409 | `conflict` | Slug already taken. |
| 413 | `too_large` | Body over 1 MiB. |
| 500 | `internal` | Server error (details are in the server log only). |

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/health` | Liveness: `{"status":"ok"}`. No auth. |
| GET | `/ready` | Database reachable and migrated: `200` or `503`. No auth. |
| POST | `/mcp` | MCP endpoint (Streamable HTTP, actor tokens only). See [MCP.md](MCP.md). |
| GET | `/api/whoami` | Which credential this is: `{"kind":"admin"}` or the actor and token metadata |
| GET | `/api/actors` | List actors |
| POST | `/api/actors` | Register an actor (admin token only) |
| GET | `/api/overview` | Every project × every actor's latest position (the dashboard grid) |
| GET | `/api/activity` | Timeline across all projects |
| GET | `/api/projects` | List projects, or find them by `?repo_url=` |
| POST | `/api/projects` | Create a project |
| GET | `/api/projects/{ref}` | One project |
| PATCH | `/api/projects/{ref}` | Update a project |
| GET | `/api/projects/{ref}/context` | **The resume-work bundle** |
| GET | `/api/projects/{ref}/profile` | **The project profile**: knowledge sections, features, completeness |
| PATCH | `/api/projects/{ref}/profile` | Merge many sections and features in one transaction (nothing omitted is deleted) |
| GET | `/api/projects/{ref}/profile/{section}` | One knowledge section |
| PUT | `/api/projects/{ref}/profile/{section}` | Create or replace one knowledge section |
| DELETE | `/api/projects/{ref}/profile/{section}` | Remove one knowledge section (explicit; syncing never deletes) |
| GET | `/api/projects/{ref}/features` | List features (`?include_removed=true` for removed ones) |
| GET | `/api/projects/{ref}/features/{slug}` | One feature |
| POST | `/api/projects/{ref}/features` | Add a feature (`409` if the slug exists) |
| PATCH | `/api/features/{id}` | Change a feature |
| GET | `/api/projects/{ref}/notes` | List notes (paged with `before`) |
| POST | `/api/projects/{ref}/notes` | Add a note |
| PATCH | `/api/notes/{id}` | Edit, pin or unpin a note |
| GET | `/api/projects/{ref}/handoffs` | Handoff history (paged with `before`) |
| GET | `/api/projects/{ref}/handoffs/latest` | Latest handoff from each actor |
| POST | `/api/projects/{ref}/handoffs` | Record a handoff (optionally with a project update) |
| GET | `/api/projects/{ref}/updates` | Project updates, newest first |
| GET | `/api/projects/{ref}/updates/latest` | The newest project update |
| POST | `/api/projects/{ref}/updates` | Publish a project update |
| GET | `/api/projects/{ref}/decisions` | List decisions (paged with `before`) |
| POST | `/api/projects/{ref}/decisions` | Record a decision |
| GET | `/api/projects/{ref}/blockers` | List blockers |
| POST | `/api/projects/{ref}/blockers` | Raise a blocker |
| PATCH | `/api/blockers/{id}` | Resolve, reopen or edit a blocker |
| GET | `/api/projects/{ref}/states` | Every actor's state on the project |
| PUT | `/api/projects/{ref}/states/{actor}` | Set or refresh an actor's state |
| GET | `/api/projects/{ref}/activity` | Timeline |

The examples below assume:

```sh
API=http://127.0.0.1:8080/api
AUTH="Authorization: Bearer $OPENBRAIN_TOKEN"
```

### Actors

`POST /api/actors`

| Field | Required | Notes |
|-------|----------|-------|
| `slug` | yes | 1–64 chars of `a-z 0-9 - _`, starting with a letter or digit, not all digits. |
| `display_name` | no | Defaults to the slug. |
| `kind` | no | `human`, `agent` (default) or `automation`. |
| `metadata` | no | A JSON object, for example model or host. |

```sh
curl -s -H "$AUTH" $API/actors -d '{"slug":"codex","display_name":"Codex","metadata":{"host":"laptop"}}'
```

Returns `409` if the slug exists. The `openbrain actor create` command
does the same from the server's shell and treats an existing slug as success.

### Projects

`POST /api/projects`: `actor` and `name` are required. Optional fields are
`slug` (derived from the name if omitted), `description`, `status` (`active`
by default, also `paused`, `done`, `archived`), `current_focus`, `repo_url`
and `default_branch`.

`PATCH /api/projects/{ref}`: `actor` plus any subset of the fields above.
Omitted fields are unchanged. Archiving sets `archived_at`, and archived
projects are hidden from lists unless `?include_archived=true` is passed.
Status and focus changes each get their own timeline entry.

`GET /api/projects` returns projects with derived `last_activity_at` and
`open_blockers`, most recently active first.

`GET /api/projects?repo_url=<git remote>` returns the projects, archived ones
included, that track that repository. SSH and HTTPS spellings of one remote
match: `git@github.com:Acme/W.git` matches `https://github.com/acme/w`.
`repo_url` values containing credentials are rejected on create and update.

### Context

`GET /api/projects/{ref}/context` is one consistent snapshot. Add
`?compact=true` for the smaller agent-oriented form: fewer notes and decisions,
no `recent_notes`, and long text clipped at 1,500 characters, with
`truncated: true` when anything was cut. Add `activity_limit=N` to change the
number of activity entries (at most 200). [MCP.md](MCP.md#context-bundle) has
the exact limits.

```json
{
  "project": { "id": 1, "slug": "my-app", "status": "active", "current_focus": "auth", "...": "..." },
  "profile": { "sections": 12, "features": 8, "updated_at": "…" },
  "generated_at": "2026-01-02T15:04:05.000Z",
  "stale_after_seconds": 3600,
  "compact": false,
  "truncated": false,
  "latest_handoffs":  [ "newest handoff from each actor" ],
  "recent_updates":   [ "latest 10 project updates (5 compact)" ],
  "actor_states":     [ "every actor's state, with a stale flag" ],
  "open_blockers":    [ "all open blockers (up to 100)" ],
  "pinned_notes":     [ "up to 50" ],
  "human_notes":      [ "latest 10 notes written by human actors" ],
  "recent_notes":     [ "latest 10 notes from anyone" ],
  "recent_decisions": [ "latest 10" ],
  "recent_activity":  [ "latest 25 timeline entries" ]
}
```

### Profile

The profile is durable, non-code knowledge of what a project is and does,
for people and assistants who never see the repository. See
[MCP.md](MCP.md#project-profile) for the model: knowledge sections with a
status of `known`, `unknown` or `not_applicable`, features with a life-cycle
status, and provenance on both.

`GET /api/projects/{ref}/profile` returns everything in one snapshot. Nothing
is clipped:

```json
{
  "project":  { "slug": "my-app", "...": "..." },
  "sections": [ { "key": "overview", "title": "What it is", "content": "…", "status": "known", "canonical": true,
                  "source_branch": "main", "source_commit": "a81c9e3",
                  "created_by": {"slug": "claude"}, "updated_by": {"slug": "claude"},
                  "created_at": "…", "updated_at": "…" } ],
  "features": [ { "id": 1, "slug": "team-workspaces", "name": "Team workspaces", "summary": "…", "status": "available",
                  "target_users": "…", "use_cases": ["…"], "limitations": "…", "...": "provenance as above" } ],
  "completeness": { "known": ["overview"], "unknown": ["pricing"], "not_applicable": [], "missing": ["…"], "custom": [], "features": 1 },
  "updated_at": "…"
}
```

Use `?include_sections=false`, `?include_features=false` or
`?include_removed=true` to trim or widen it.

`PATCH /api/projects/{ref}/profile` merges, in one transaction:

| Field | Notes |
|-------|-------|
| `actor` | Admin token only. Actor tokens write as their own actor. |
| `sections` | `[{key, title?, content?, status?}]`. Matched by key; only the fields you send change. A new section with status `known` needs content. |
| `features` | `[{slug?, name?, summary?, description?, status?, target_users?, use_cases?, limitations?}]`. Matched by slug (derived from the name if omitted). A new feature needs a name and a summary. |
| `source_branch`, `source_commit` | Applied to every item that does not carry its own. |

It returns which keys and slugs were `created`, `updated` or `unchanged`,
plus `completeness`. Nothing omitted is deleted, and repeating a call changes
nothing.

`PUT /api/projects/{ref}/profile/{section}` takes
`{title?, content?, status?, source_branch?, source_commit?}` and creates or
replaces that one section. `DELETE` removes it; with the admin token, name the
actor as `?actor=`. Section keys are lowercase letters, digits, `_` and `-`;
the canonical ones are listed in [MCP.md](MCP.md#project-profile), and any
other key is a custom section.

Text is stored and shown as plain text, never as HTML.

### Features

`GET /api/projects/{ref}/features` lists features in the order they were
added (removed ones only with `?include_removed=true`).
`GET .../features/{slug}` returns one.

`POST /api/projects/{ref}/features` adds one: `name` and `summary` are
required; `slug` defaults from the name; `status` defaults to `available`. It
returns `409` if the slug exists. `PATCH /api/features/{id}` changes only the
fields you send. The slug cannot change. Features are never deleted: set
`status` to `deprecated` or `removed`.

```sh
curl -s -H "Authorization: Bearer $OPENBRAIN_MCP_TOKEN" $API/projects/my-app/features -d '{
  "name":"Team workspaces","summary":"Manage several client projects from one account.",
  "target_users":"Agencies and consultants","use_cases":["Separate client work"],
  "limitations":"No workspace-level billing yet."}'
```

### Notes

`POST /api/projects/{ref}/notes`

| Field | Required | Notes |
|-------|----------|-------|
| `actor`, `content` | yes | |
| `kind` | no | A free-form category (same rules as a slug). Defaults to `note`. Examples: `idea`, `warning`, `ops`. |
| `pinned` | no | Boolean. |

`GET .../notes?actor=claude&human=true&pinned=true&limit=20&before={id}` filters notes and pages backwards. `GET .../handoffs` and `GET .../decisions` also take `limit` and `before`.

`PATCH /api/notes/{id}` takes `actor` plus any of `content`, `kind` and
`pinned`. Only the note's author may change `content` or `kind`. Any actor may
pin or unpin it.

### Handoffs

`POST /api/projects/{ref}/handoffs`

| Field | Kind | Notes |
|-------|------|-------|
| `actor`, `summary` | required | What was done and where it stands. |
| `current_state`, `current_task`, `blockers`, `next_steps` | interpretation | The actor's own account. |
| `branch`, `commit_sha` | fact | `commit_sha` must be 4–64 hex characters and is stored lowercase. |
| `metadata` | fact | A JSON object for anything else checkable (test counts, PR URL, and so on). |
| `state` | optional | Also sets the actor's state on the project, in the same transaction. |
| `user_update` | optional | A project update for the project owner (`title`, `summary`, `category`, optional `details` and `task_reference`; see below). Published in the same transaction, by the same actor, and linked to the handoff. It cannot name a different actor. |

```sh
curl -s -H "$AUTH" $API/projects/my-app/handoffs -d '{
  "actor":"claude","summary":"Login API done.","next_steps":"Form UI.",
  "branch":"feature/auth","commit_sha":"9ad281c","metadata":{"tests":"142 passing"},"state":"idle"}'
```

Handoffs are never edited or deleted. `GET .../handoffs?actor=claude&limit=10`
returns history. `GET .../handoffs/latest` returns one per actor. The response
to recording a handoff includes `user_update` when one was published.

### Project updates

A project update says what meaningfully changed, in plain English, for the
person who owns the project. A handoff, by contrast, tells the next agent where
the work stands.

`POST /api/projects/{ref}/updates`

| Field | Required | Notes |
|-------|----------|-------|
| `actor` | admin token only | Actor tokens publish as their own actor. |
| `title` | yes | One line, up to 200 bytes. State the outcome: "Login is now faster", not "Task 4 complete". |
| `summary` | yes | Up to 4,000 bytes. What changed, why it matters, what to expect now. Blank lines separate paragraphs. |
| `category` | yes | `new`, `improved`, `fixed`, `changed` or `important`. |
| `details` | no | Up to 8,000 bytes of slightly more technical context. |
| `task_reference` | no | One line, such as `Task 3` or `Network setup`. |
| `idempotency_key` | no | As for other creates. |

```sh
curl -s -H "Authorization: Bearer $OPENBRAIN_MCP_TOKEN" $API/projects/my-app/updates -d '{
  "title":"Login is now faster","category":"fixed","task_reference":"Task 3",
  "summary":"The delay noticed during task 3 is fixed, so signing in should feel immediate."}'
```

`GET .../updates?actor=claude&category=fixed&limit=20&before={id}` lists updates newest first,
paged like activity. `GET .../updates/latest` returns the newest one, or `404`
if the project has none. Each update carries `project_slug`, `project_name` and
its `actor`. Updates published with a handoff also carry `handoff_id`,
`branch` and `commit_sha`.

Updates are append-only: there is no edit or delete. To correct one, publish a
newer update. Text is stored and shown as plain text, never as HTML.

### States

`PUT /api/projects/{ref}/states/{actor}` takes `{"state": "working", "current_task": "…"}`.

- `state` is one of `idle`, `working`, `blocked`, `waiting`, `finished`.
- If `current_task` is omitted, the previous task is kept.
- Moving into `working` sets `started_at`, and moving into `finished` sets
  `finished_at`.
- A call that changes nothing still refreshes `updated_at`. That makes it a
  heartbeat, and heartbeats don't clutter the timeline.
- `stale` is `true` when the state is `working`, `waiting` or `blocked` and
  `updated_at` is older than `stale_after_seconds`. The raw `updated_at` is
  always returned, so clients can apply their own threshold.

### Decisions

`POST /api/projects/{ref}/decisions` takes `{"actor": "...", "decision": "...", "reasoning": "..."}`.
`reasoning` is optional.

### Blockers

`POST /api/projects/{ref}/blockers` takes `{"actor": "...", "description": "..."}`.

`PATCH /api/blockers/{id}` takes `actor` plus any of:

- `resolved: true` resolves the blocker and records the caller as resolver.
- `resolved: false` reopens it and clears the resolution.
- `resolution`, a note on how it was resolved.
- `description`.

`GET .../blockers?state=open|resolved|all` filters by state. The default is
`open`.

### Activity

`GET /api/projects/{ref}/activity?limit=50&before={id}` returns entries newest
first. To page backwards, pass the smallest `id` you have seen as `before`.

`GET /api/activity?limit=50&before={id}` is the same timeline across every
project, paged the same way. Each entry also carries `project_slug` and
`project_name`.

Entry kinds: `project.created`, `project.updated`, `project.status_changed`,
`project.focus_changed`, `note.added`, `note.edited`, `note.pinned`,
`note.unpinned`, `handoff.created`, `update.published`, `decision.recorded`,
`blocker.created`, `blocker.edited`, `blocker.resolved`, `blocker.reopened`,
`state.changed`, `profile.section_updated`, `profile.section_removed`,
`profile.synced`, `feature.added`, `feature.updated`.

### Overview

`GET /api/overview?include_archived=true` returns the dashboard grid as data:

```json
{
  "actors": [ {"slug": "alice", "display_name": "Alice", "kind": "human"}, "..." ],
  "projects": [
    { "project": { "...": "..." },
      "latest_update": {},
      "actors": { "claude": { "state": {}, "latest_update": {}, "latest_handoff": {}, "latest_note": {} } } }
  ]
}
```

Actor columns are people plus every actor that has touched a listed project.
