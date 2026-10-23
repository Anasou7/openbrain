# MCP: connecting AI agents

OpenBrain includes a [Model Context Protocol](https://modelcontextprotocol.io)
server, so coding agents such as Claude Code, Codex, OpenCode and Hermes can
read and write the shared project state themselves. At the start of a session
they read where everyone left off. While they work they record decisions,
blockers and notes. When they stop, they record a handoff.

## One server for every agent

There is **one** MCP endpoint, `/mcp`, served by the same `openbrain`
process as the dashboard and the REST API. Each agent connects with **its own
token**, and the token decides who the agent is:

```
Claude ── token bound to "claude" ──┐
Codex ─── token bound to "codex" ───┤
Hermes ── token bound to "hermes" ──┼──▶ /mcp ──▶ store (same operations as REST) ──▶ SQLite
OpenCode ─ token bound to "opencode"┘
```

- **No agent-specific code.** Nothing in the server knows what Claude or
  Codex is. Agents are rows in the `actors` table, and any MCP client works.
- **Thin adapter.** Every tool calls the same operation the REST API uses,
  with the same validation, transactions and activity log.
- **Small and stateless.** The protocol is Streamable HTTP in stateless mode,
  so nothing is kept between requests and idle memory doesn't grow with the
  number of agents. See [DEPLOYMENT.md](DEPLOYMENT.md#resource-use) for
  measurements.

## Identity: the token decides who writes

**A client cannot choose its own actor.** No tool has an `actor`, `author` or
similar argument. Extra arguments are rejected by the input schema. The server
resolves the bearer token to exactly one actor and records every write as that
actor, with channel `mcp` in the activity log. The dashboard shows this as,
for example, "Claude … via MCP".

- **Tokens are high-entropy and shown once.** They are 256-bit random secrets
  (`obt_…`), printed only at creation. Only a SHA-256 hash is stored, and
  secrets are never logged or returned by any list.
- **Revocation is immediate.** The next request with a revoked token gets 401.
- **The admin token (`OPENBRAIN_TOKEN`) is not accepted on `/mcp`.** MCP writes
  always need an actor identity.
- **Browser sessions don't authenticate MCP either.** Signing in to the
  dashboard and connecting an agent are separate credentials. To use MCP from
  your own tools, create a token for your own human actor (its slug is your
  username).
- **Actor tokens are also valid on the REST API** under the same rule: they
  can only write as their own actor. A request naming a different actor gets
  403.

What an actor token can do today:

| Allowed | Not allowed |
|---------|-------------|
| Read every project, note, handoff, decision, blocker and state | Write as another actor |
| Create projects, notes, handoffs, decisions and blockers as itself | Create actors or tokens |
| Set its own state | Set another actor's state |
| Pin or unpin any note, resolve any blocker (recorded as itself) | Edit the text of another actor's note |

The token table can later grow per-token scopes, and OAuth could replace
static tokens, without touching the tools or the domain logic.

## Set up an agent

An administrator can do this on the dashboard's **Agents** page, or on the
server as the service user:

```sh
# 1. Register the agent. The slug is how it appears everywhere. Any name works.
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db openbrain actor create claude --name "Claude"

# 2. Issue a token for one machine. The secret is printed once, on stdout.
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db openbrain token create --actor claude --label laptop
```

Repeat for every agent and machine, for example `codex`, `opencode`, `hermes`
or `ci-bot --kind automation`. Re-running `actor create` for an existing slug
changes nothing and says so.

```sh
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db openbrain token list          # metadata only: prefix, label, last use
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db openbrain token revoke 3      # takes effect on the next request
```

On the **Agents** page, administrators can add actors, create a token (shown
once), see when each token was last used, and revoke it. The page also shows
the exact MCP URL for this server, base path included.

On the agent's machine, keep the token in an environment variable. The
examples below use `OPENBRAIN_MCP_TOKEN`:

```sh
export OPENBRAIN_MCP_TOKEN='obt_…'             # bash/zsh, e.g. in ~/.profile
```
```powershell
setx OPENBRAIN_MCP_TOKEN "obt_…"               # Windows, new terminals pick it up
```

Never commit a token. Each example below reads it from the environment.

## Reaching the server

| | |
|---|---|
| Endpoint | `<base URL>/mcp`: `https://brain.example.com/mcp` at the root, or `https://example.com/openbrain/mcp` with `OPENBRAIN_BASE_PATH=/openbrain` |
| Transport | Streamable HTTP (POST). Stateless: no sessions, `GET` returns 405. |
| Authentication | `Authorization: Bearer <actor token>` |
| Protocol versions | `2026-07-28` (stateless) and the earlier `2025-11-25`, `2025-06-18`, `2025-03-26` and `2024-11-05` handshakes |

OpenBrain listens on `127.0.0.1` only. The agent runs on your developer
machine, so it needs a secure route to the server. Pick one:

- **Tailscale** (recommended for several machines): on the server, run
  `sudo tailscale serve --bg 8080`. The URL is
  `https://<machine>.<tailnet>.ts.net/mcp` (with `OPENBRAIN_BASE_PATH=/`),
  with HTTPS and private access handled by Tailscale.
- **HTTPS reverse proxy** (Caddy, nginx), at the root or under a path. See
  [DEPLOYMENT.md](DEPLOYMENT.md#3-choose-the-url-and-configure-the-proxy). No
  MCP-specific proxy settings are needed: responses are plain JSON, not
  long-lived streams.
- **SSH tunnel** (single machine, nothing to install):
  `ssh -N -L 8080:127.0.0.1:8080 you@server`. The URL is then
  `http://127.0.0.1:8080/mcp`, or `http://127.0.0.1:8080/openbrain/mcp` with
  a base path. The tunnel must stay open while the agent runs.

Don't send tokens over plain HTTP across a network you don't control.

## Client configuration

Replace `https://brain.example.com/mcp` with your URL, including any base path
(`https://example.com/openbrain/mcp`). These formats come from
each client's official documentation as of October 2026. Check it if your
version differs.

### Claude Code

```sh
claude mcp add-json --scope user openbrain \
  '{"type":"http","url":"https://brain.example.com/mcp","headers":{"Authorization":"Bearer ${OPENBRAIN_MCP_TOKEN}"}}'
claude mcp list        # expect: openbrain ... ✔ Connected
```

- Claude Code expands `${OPENBRAIN_MCP_TOKEN}` when it connects, so the
  stored config holds the placeholder, not the secret.
- `--scope user` makes the server available in every project. A shared,
  committed `.mcp.json` works too, because it uses the same placeholder:

  ```json
  {
    "mcpServers": {
      "openbrain": {
        "type": "http",
        "url": "https://brain.example.com/mcp",
        "headers": { "Authorization": "Bearer ${OPENBRAIN_MCP_TOKEN}" }
      }
    }
  }
  ```
- Tools show up as `mcp__openbrain__get_project_context` and so on.
- Don't reuse variable names that Claude Code blanks in remote headers, such
  as `ANTHROPIC_AUTH_TOKEN`.
- With `claude mcp add --header`, quote the header in single quotes. Double
  quotes make your shell expand the variable and store the real token.

### Codex

```toml
# ~/.codex/config.toml (or a trusted project's .codex/config.toml)
[mcp_servers.openbrain]
url = "https://brain.example.com/mcp"
bearer_token_env_var = "OPENBRAIN_MCP_TOKEN"
```

The equivalent command is
`codex mcp add openbrain --url https://brain.example.com/mcp --bearer-token-env-var OPENBRAIN_MCP_TOKEN`.
Check it with `codex mcp list`.

If the variable isn't set when Codex starts, it connects without credentials
and every call gets 401.

### Hermes

```yaml
# ~/.hermes/config.yaml (Windows: %LOCALAPPDATA%\hermes\config.yaml)
mcp_servers:
  openbrain:
    url: "https://brain.example.com/mcp"
    headers:
      Authorization: "Bearer ${OPENBRAIN_MCP_TOKEN}"
```

Put `OPENBRAIN_MCP_TOKEN=obt_…` in Hermes' `.env` file in the same directory,
or in the environment. Check the connection with
`hermes mcp test openbrain`, which should print `✓ Connected` and
`Tools discovered: 19`. `hermes mcp add openbrain --url … --auth header`
does the same interactively and stores the token in `.env` for you.

### OpenCode

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "openbrain": {
      "type": "remote",
      "url": "https://brain.example.com/mcp",
      "oauth": false,
      "headers": { "Authorization": "Bearer {env:OPENBRAIN_MCP_TOKEN}" }
    }
  }
}
```

Check it with `opencode mcp list`. `"oauth": false` stops OpenCode from trying
OAuth discovery. A misspelled variable silently becomes an empty token, so a
401 usually means a typo.

### Any other client, and testing by hand

Use Streamable HTTP, the URL above, and the bearer header. To check a token
without an agent:

```sh
# Who does this token belong to? (REST)
curl -s -H "Authorization: Bearer $OPENBRAIN_MCP_TOKEN" https://brain.example.com/api/whoami

# MCP tool list
curl -s https://brain.example.com/mcp \
  -H "Authorization: Bearer $OPENBRAIN_MCP_TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'

# Official MCP Inspector (Node 22.19+), non-interactive
npx @modelcontextprotocol/inspector --cli https://brain.example.com/mcp --transport http \
  --header "Authorization: Bearer $OPENBRAIN_MCP_TOKEN" --method tools/list
```

## Tools

Nineteen tools, each with a description that tells the model *when* to use it.
Every tool returns structured content with a declared output schema, plus the
same JSON as text for clients that only read text. `project` is always a
project ID or slug. Write tools never take an actor.

| Tool | Kind | Inputs (* required) | Returns |
|------|------|---------------------|---------|
| `list_projects` | read | `status`, `include_archived` | `projects`: id, slug, name, status, focus, repo, open blockers, last update |
| `find_project` | read | `repo_url` and/or `project` (at least one) | `status`: `found`, `not_found` or `ambiguous`; `project` or `candidates`; `message` with the next step |
| `get_project_context` | read | `project`*, `compact` (default true), `activity_limit` | The context bundle (below) |
| `get_project_profile` | read | `project`*, `section`, `feature`, `include_sections` (default true), `include_features` (default true), `include_removed` | The profile (below): project, `sections`, `features`, `completeness`, `updated_at`. Complete and unclipped, or one section or one feature |
| `get_project_updates` | read | `project`*, `category`, `by_actor`, `before`, `limit` (default 20, max 100) | `updates`, newest first, and `next_before` for the next page |
| `get_project_history` | read | `project`*, `kind`* (`handoffs`, `decisions`, `notes`, `activity`), `by_actor` (handoffs and notes), `before`, `limit` | That kind of record, newest first, and `next_before` |
| `get_overview` | read | `include_archived` | Every project with its latest update and each actor's state, task, last update title, and clipped last handoff, next steps and note; plus `recent_updates`, the 10 newest updates across projects |
| `register_project` | write | `name`*, `slug`, `description`, `repo_url`, `default_branch`, `current_focus`, `idempotency_key` | `created`, `project`, `message`. Returns the existing project if the repository is already tracked. |
| `update_project` | write | `project`*, any of `status`, `current_focus`, `repo_url`, `default_branch`, `description` | The project |
| `sync_project_profile` | write | `project`*, `sections` (`key`*, `title`, `content`, `status`), `features` (`slug`, `name`, `summary`, `description`, `status`, `target_users`, `use_cases`, `limitations`), `source_branch`, `source_commit` | What was `created`, `updated` and left `unchanged`, by key and slug, plus `completeness` |
| `update_project_knowledge` | write | `project`*, `section_key`*, `title`, `content`, `status`, `source_branch`, `source_commit` | The section |
| `upsert_project_feature` | write | `project`*, `slug` or `name`, `summary`, `description`, `status`, `target_users`, `use_cases`, `limitations`, `source_branch`, `source_commit` | `created` and the feature |
| `add_note` | write | `project`*, `content`*, `category`, `pinned`, `idempotency_key` | The note |
| `record_handoff` | write | `project`*, `summary`*, `current_state`, `current_task`, `blockers`, `next_steps`, `branch`, `commit_sha`, `metadata` (object), `set_state`, `user_update` (object: `title`*, `summary`*, `category`*, `details`, `task_reference`), `idempotency_key` | The handoff, with `user_update` if one was published |
| `publish_project_update` | write | `project`*, `title`*, `summary`*, `category`* (`new`, `improved`, `fixed`, `changed`, `important`), `details`, `task_reference`, `idempotency_key` | The update |
| `set_agent_state` | write | `project`*, `state`* (`idle`, `working`, `blocked`, `waiting`, `finished`), `current_task` | The state, with `stale` |
| `record_decision` | write | `project`*, `decision`*, `reasoning`, `idempotency_key` | The decision |
| `raise_blocker` | write | `project`*, `description`*, `context` (appended to the description), `idempotency_key` | The blocker |
| `resolve_blocker` | write | `blocker_id`*, `resolution` | The blocker, with resolver |

Annotations:
- The seven read tools are marked `readOnlyHint`.
- `register_project`, `add_note`, `record_handoff`, `publish_project_update`,
  `record_decision` and `raise_blocker` only add records
  (`destructiveHint: false`).
- `update_project`, `set_agent_state`, `resolve_blocker`, `sync_project_profile`,
  `update_project_knowledge` and `upsert_project_feature` change existing values
  and are idempotent.
- All tools are closed-world.

There is deliberately no SQL, filesystem, shell or HTTP-proxy tool.

### Repository lookup

The server cannot see your machine, so the agent sends what it observes. It
runs `git remote get-url origin`, `git rev-parse --abbrev-ref HEAD` and
`git rev-parse HEAD` locally, then calls `find_project` with the remote URL.

Remotes are compared in a normalised `host/path` form:

- The same repository matches regardless of spelling: `git@github.com:Acme/Widget.git`,
  `https://github.com/acme/widget`, `ssh://git@github.com:22/acme/widget` and
  `https://user:pw@github.com/acme/widget` all match.
- Normalisation drops credentials, ports, a trailing `.git` and letter case.
- Repository URLs containing credentials are rejected when you store them.

The repository URL is a lookup key, not identity. A project keeps its numeric
ID even if its remote changes, and two projects may share a repository (for
example in a monorepo). That is when `find_project` answers `ambiguous`.

### Context bundle

`get_project_context` returns the following, from a single consistent read:

- the project (identity, status, current focus, repository)
- `profile`: only how many sections and features the profile has and when it
  last changed; read it with `get_project_profile`
- `latest_handoffs`: the newest from every actor
- `recent_updates`: the newest project updates, newest first
- `actor_states`: each with a `stale` flag
- `open_blockers`
- `pinned_notes`
- `human_notes`: the latest written by people
- `recent_notes`
- `recent_decisions`
- `recent_activity`
- `compact` and `truncated` flags

| | compact (default) | full (`compact: false`) |
|---|---|---|
| Pinned notes | 10 | 50 |
| Human notes | 5 | 10 |
| Project updates | 5 (details clipped at 300 characters) | 10 |
| Recent notes from anyone | none | 10 |
| Decisions | 5 | 10 |
| Open blockers | 20 | 100 |
| Activity | 10 (`activity_limit` up to 200) | 25 |
| Long text fields | clipped at 1,500 characters, marked `[truncated…]` | complete |

Latest handoffs and states are never dropped. For a project with 9 handoffs
from 2 actors, 21 notes, 5 decisions and 10 project updates, the measured
payload was about 11 KB compact and 22 KB full (8.6 KB and 17.6 KB before the
updates were added).

The context bundle deliberately leaves out the project profile. That has its
own tool, below.

### Project profile

The profile is the durable, non-code description of a project: what it is,
who it is for, what it can do and what it cannot. It is written by the agents
that can read the repository, so that an assistant without the repository
(such as Hermes) can explain the product, answer customers and draft
marketing.

- **Knowledge sections** have a key, a title, plain-text content and a status:
  `known`, `unknown` (not established; content may say why) or
  `not_applicable`. Explicit gaps mean no one has to guess.
  - Canonical keys, in display order: `overview`, `target_audience`, `icp`,
    `problem`, `features_overview`, `use_cases`, `user_workflows`,
    `positioning`, `differentiators`, `terminology`, `competitors`,
    `integrations`, `limitations`, `known_issues`, `faq`,
    `customer_support_notes`, `business_model`, `pricing`, `marketing_notes`,
    `seo_notes`, `architecture`, `deployment`, `security`, `future_direction`.
  - Any other lowercase key is a custom section and needs no migration.
- **Features** have a slug, name, one-sentence summary, description, target
  users, use cases, limitations and a status: `planned`, `experimental`,
  `available`, `deprecated` or `removed`. Features are never deleted; removed
  ones are hidden unless asked for.
- **Provenance:** every section and feature records who first wrote it, who
  last changed it and when, and optionally the `source_branch` and
  `source_commit` it was derived from. A write that changes nothing leaves all
  of that alone, so `updated_at` is when the content last changed. An edit made
  without a source (for example by hand on the dashboard) clears the old one.
- **Completeness** is derived, not stored: which canonical sections are known,
  unknown, not applicable or missing, which custom ones exist, and how many
  features there are. It says nothing about quality.

`get_project_profile` modes:

| Call | Returns |
|------|---------|
| `{project}` | Everything: all sections in canonical order (then custom ones), all features except removed ones, completeness. Nothing is clipped. |
| `{project, section: "integrations"}` | Just that section. A missing section is an error that says OpenBrain has no verified information about it. |
| `{project, feature: "team-workspaces"}` | Just that feature, even if removed |
| `{project, include_sections: false}` | Features only |

`sync_project_profile` merges: sections are matched by key and features by
slug (derived from the name if omitted). Only the fields you send change,
anything you leave out stays as it is, and nothing is ever deleted (removing a
section is a separate, explicit operation on the dashboard or REST API). The
whole sync is one transaction, so an invalid item rejects all of it. Repeating
a sync is a no-op. The result lists what was created, updated and unchanged.

Sizes measured: a realistic profile of 16 sections and 8 features is about
15 KB of JSON (MCP sends it twice, as structured content and as text, so about
32 KB). One section is 3 KB. A deliberately large profile of 24 sections of
3 KB each and 60 features is 290 KB over MCP and takes about 7 ms.

### History and paging

The context bundle is bounded, but nothing is lost: `get_project_updates` and
`get_project_history` page through everything, newest first. Each response
carries `next_before`; pass it as `before` for the next page, and stop when it
is absent.

## Recommended workflow

The full guide, with examples of good and bad handoffs, is
[AGENT_WORKFLOW.md](AGENT_WORKFLOW.md). Short instruction snippets for Claude
Code, Codex, OpenCode, Hermes and any other client are in
[clients/](clients/). In brief:

1. **Start of session.** Read your repository's remote, branch and commit.
   Call `find_project` with the remote, then `get_project_context`, even if
   you worked on the project before. Set your state to `working` with a task.
   If the project isn't found and the user wants it tracked, call
   `register_project`.
2. **During work.** Record only what's durable:
   - a decision that future work must respect: `record_decision`
   - something in the way: `raise_blocker`, later `resolve_blocker`
   - a fact worth keeping: `add_note`
   - a change in what you're doing: `set_agent_state`
   - a durable product fact that changed (a feature, limitation, integration,
     platform, onboarding or pricing): `update_project_knowledge` or
     `upsert_project_feature`
3. **Pause or end.** Call `record_handoff` with what changed, where things
   stand, next steps, blockers, branch and commit, and pass `set_state`
   (`finished`, `idle`, `blocked` or `waiting`) so you aren't left shown as
   working. If something meaningful changed for the project owner, include
   `user_update`. For a milestone in the middle of a long session, call
   `publish_project_update` instead.

**Onboarding a project.** When the user asks an agent to fully add a project
to OpenBrain, it inspects the repository and docs deeply and writes the
profile with one `sync_project_profile` call: knowledge sections and every
user-facing feature, in plain English, with unknown facts marked `unknown`
rather than guessed. [AGENT_WORKFLOW.md](AGENT_WORKFLOW.md#building-a-projects-profile-initial-onboarding)
lists what to inspect and what to leave out.

**Assistants such as Hermes** read a project with `get_project_profile`, then
`get_project_context`, then `get_project_updates`. They answer focused
questions from one profile section, and "what changed?" from updates rather
than raw activity.

**Handoffs and updates have different readers.** A handoff is for the next
agent ("Implemented session caching; all auth tests pass; next, update the
login UI; main @ a83bd21"). A project update is for the person who owns the
project ("Login is now faster: the delay noticed during task 3 is fixed, so
signing in should feel immediate"). Same work, different audience. Publish an
update only when there is something worth telling that person; most sessions
don't need one.

**Store results, not reasoning.** OpenBrain holds operational artifacts:
summaries, decisions, blockers, next steps and checkable repository facts. It
is not a place for chain-of-thought, scratch reasoning, transcripts or every
command run. The server's instructions and tool descriptions tell models this,
and the same applies to anything you build on top.

There is no background heartbeat. An agent that says it's `working` and then
goes quiet for longer than `OPENBRAIN_STALE_AFTER` (default 1 hour) is shown as
stale. Re-sending the same state refreshes it.

## Retries and idempotency

| Tool | Retry-safe? |
|------|-------------|
| `update_project`, `set_agent_state`, `resolve_blocker`, `sync_project_profile`, `update_project_knowledge`, `upsert_project_feature` | Yes. Repeating the same call changes nothing more. Resolving an already-resolved blocker keeps the original resolver and resolution. |
| `register_project` | Yes for a repository already tracked, which returns the existing project. Otherwise use `idempotency_key`. |
| `add_note`, `record_handoff`, `publish_project_update`, `record_decision`, `raise_blocker` | With `idempotency_key`. A retry with the same key returns the original record (for a handoff, with the update it published). |
| Read tools | Yes |

Idempotency keys are any string you generate (a UUID is ideal), scoped to the
calling actor and kept for 7 days. Reusing a key for a different call is
rejected.

## Security recommendations

- Give every agent and every machine its own token, so one can be revoked
  without touching the others. Labels and "last used" times on the Agents page
  show which token is which.
- Revoke tokens you no longer use, and any token that may have leaked.
- Keep `OPENBRAIN_TOKEN` (the optional admin API token) for scripts and administration. Don't hand
  it to agents.
- Reach the server over Tailscale, WireGuard, an SSH tunnel or HTTPS. Keep the
  default `127.0.0.1` binding.
- Treat what agents write as claims, not facts. The activity log shows which
  actor and channel each write came from, and handoffs keep checkable facts
  (branch, commit) apart from narrative.

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| `401` / "Needs authentication" / "invalid token" | Token missing, mistyped or revoked; the variable isn't set in the client's environment; or you used `OPENBRAIN_TOKEN`, which `/mcp` refuses. Check with `curl …/api/whoami`. |
| `403 Forbidden` from `/mcp` | A browser-originated cross-site request, which is rejected by design. CLI agents never send these headers. |
| `405` on GET | Expected: the endpoint is stateless and has no GET stream. Clients fall back automatically. |
| `413` | A request body larger than 1 MiB. |
| Tool error "not found" | The project ID or slug is wrong. Use `find_project` or `list_projects`. |
| Tool error "unexpected additional properties" | An argument the tool doesn't define. This includes any attempt to pass `actor`. |
| `find_project` says `ambiguous` | Several projects share the repository. Pass the slug you want as `project`. |
| Agent shows as "stale" | It reported a busy state and never updated it. Expected after a crash. Its next `set_agent_state` or `record_handoff` with `set_state` clears it. |
| Connection refused | Is the service running (`systemctl status openbrain`), and is your tunnel, Tailscale or proxy up? |
