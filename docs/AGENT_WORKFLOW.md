# Agent workflow

How AI agents (Claude Code, Codex, OpenCode, Hermes or anything else that
speaks MCP) should use OpenBrain.

OpenBrain is the **complete non-code representation of each project**. An
assistant that never sees the repository, such as Hermes, should be able to
explain the product, answer customer questions, draft marketing and say what
changed recently, from OpenBrain alone. It holds three levels of knowledge:

1. **Project knowledge** (mostly durable): what the product is and does, who it
   is for, its features, use cases, positioning, integrations, limitations,
   terminology and FAQ. This is the **profile**: `get_project_profile`.
2. **Current state**: what is being worked on, where each agent stopped, what
   is blocked. Handoffs, states, notes, blockers, decisions:
   `get_project_context`.
3. **Change history**: what meaningfully changed, when, by whom and why it
   matters. Project updates: `get_project_updates`.

The coding agents that can read the repository translate it into this
knowledge. OpenBrain contains no LLM; it stores what agents and people write.
It is not a place for source code, chain-of-thought, scratch reasoning or
transcripts.

The rule in one line: **read once near the start, write durable context when
useful, always hand off after meaningful work, and publish a human-facing
update only when there is something worth telling the project owner.**

Ready-to-paste instructions for each client are in [clients/](clients/).
Connection setup is in [MCP.md](MCP.md).

## Credentials

Each agent connects over MCP with **its own actor token** (`obt_…`). The token
decides who a write is attributed to, so the activity log can tell Claude's
work from Codex's.

Agents should never use `OPENBRAIN_TOKEN`. That is an optional administrative
credential for REST scripts. It isn't accepted on `/mcp`, and its writes don't
belong to any one agent. See [Credentials](DEPLOYMENT.md#credentials).

## What goes where

| | What it is | Written with |
|---|---|---|
| **Profile** | What the project or product **is**: knowledge sections such as overview, audience, positioning, limitations, FAQ | `sync_project_profile`, `update_project_knowledge` |
| **Features** | What it **can do**, from the user's point of view | `sync_project_profile`, `upsert_project_feature` |
| **Updates** | What **changed**, in plain English, for the project owner | `publish_project_update`, or `user_update` on a handoff |
| **Handoffs** | Where an agent **left off**: state, next steps, branch, commit | `record_handoff` |
| **Notes** | Durable freeform context: a quirk, a caveat, an instruction from the user | `add_note` |
| **Decisions** | Durable choices to respect | `record_decision` |
| **Blockers** | Things preventing progress | `raise_blocker`, `resolve_blocker` |
| **Activity** | Raw operational history | written automatically |

## Building a project's profile (initial onboarding)

When the user asks you to **fully add a project to OpenBrain**, or to **build
or update its OpenBrain profile**, do a thorough, one-time analysis. Recording
the name, language and repository is not enough.

1. **Inspect the repository and its documentation deeply**, as relevant:
   README and docs, the user-facing app (frontend routes and pages, CLI
   commands, API endpoints), configuration, the database schema, integrations,
   tests where they reveal behaviour, examples, deployment files, pricing or
   plan configuration, product copy and existing marketing, and known
   limitations in code or docs. Read TODOs only where they reveal what the
   product can or cannot do.
2. **Understand the product from a user's and a business's point of view.**
3. **Find or register the project** (`find_project`, then `register_project`
   if the user wants it tracked).
4. **Write the profile in one call to `sync_project_profile`:**
   - knowledge sections, using canonical keys where they fit: `overview`,
     `problem`, `target_audience`, `icp`, `features_overview`, `use_cases`,
     `positioning`, `differentiators`, `integrations`, `user_workflows`,
     `limitations`, `faq`, `terminology`, `business_model`, `pricing`,
     `competitors`, `marketing_notes`, `seo_notes`, `customer_support_notes`,
     `architecture`, `deployment`, `security`, `known_issues`,
     `future_direction`. Add custom keys for anything else;
   - every meaningful user-facing feature: name, one-sentence summary,
     description, who it is for, use cases, limitations and status
     (`planned`, `experimental`, `available`, `deprecated`, `removed`);
   - `source_branch` and `source_commit`, so readers know what it was derived
     from.
5. **Record a handoff** with the current state. Publish a project update only
   if there is a meaningful event to report.

**Accuracy beats completeness.** Write only what the repository and docs
establish. If you cannot establish a fact (pricing, the ideal customer,
competitors, a supported platform), add the section with status `unknown` and
say why, or leave it out. Write `pricing: unknown`, never `$19/month`. Use
`not_applicable` when a section does not apply. You may describe product facts
that marketing can use; do not invent a marketing strategy the repository gives
no evidence for.

**Describe the product, not the code.** Write "Authentication supports local
username and password accounts", not "auth.go calls verifyPassword(), which
uses argon2.IDKey". Do not store source files, function bodies, large
snippets, package-by-package internals, lockfiles or dependency lists, unless
a fact like that matters to customers (for example "self-hosted", "runs on
Linux", "data stays on your server").

Syncing merges: sections are matched by key and features by slug, anything you
leave out stays as it is, and nothing is deleted. A repeated sync with the same
content changes nothing.

## The loop

```
start session
  ↓ inspect the local git repository (remote, branch, HEAD)
  ↓ find_project
  ↓ get_project_context
  ↓ set_agent_state: working
do the work
  ↓ record decisions, blockers and notes that will matter later
  ↓ if a durable product fact changed: update_project_knowledge / upsert_project_feature
  ↓ (publish_project_update for a milestone in a long session)
record_handoff
  ↓ include user_update if something meaningful changed for the owner
  ↓ set the final state (finished, idle, blocked or waiting)
```

## Starting work

When you begin meaningful work in a repository:

1. Inspect the repository:
   ```sh
   git remote get-url origin
   git rev-parse --abbrev-ref HEAD
   git rev-parse HEAD
   ```
2. Call `find_project` with the remote URL. SSH and HTTPS spellings of the same
   remote match. Without a remote, pass the project's slug if you know it.
3. If the project is found, call `get_project_context`. Read the latest
   handoff from **every** participant, recent updates, open blockers, recent
   decisions and pinned human notes.
4. Call `set_agent_state` with `working` and a one-line task.

**Never assume your own previous session is the latest state.** Claude works
on Monday, Codex on Tuesday, OpenCode on Wednesday. When Claude returns on
Thursday, it reads OpenBrain before continuing, because two other agents have
changed things since.

If `find_project` returns `not_found`, ask the user before calling
`register_project`. Not every repository should be tracked.

## During work

Write selectively. Each entry should still be useful to someone in a week.

| Tool | Use it for | Examples | Not for |
|------|------------|----------|---------|
| `record_decision` | A durable choice others should respect | "Use SQLite, not PostgreSQL." "Production deploys need manual approval." | Passing thoughts, options you're still weighing |
| `raise_blocker` | Something that genuinely stops or constrains progress | Missing credentials; user approval needed; an external dependency is down; an unresolved architectural question | Ordinary to-dos |
| `resolve_blocker` | Closing a blocker, with how it was resolved | | |
| `add_note` | Durable facts that fit nowhere else | An environment quirk; a test that only fails on CI; a temporary operational caveat; an instruction from the user | Logging commands or edits |
| `set_agent_state` | Real changes in your working state | idle → working, working → blocked, blocked → working, working → waiting or finished | Every small step |
| `publish_project_update` | A meaningful outcome in the middle of a long session | "Remote access is now available securely over HTTPS." | Progress reports, every finished task |
| `update_project_knowledge` | A durable product fact changed | Pricing changed; a new supported platform; onboarding changed; a limitation removed | Bug fixes, refactors |
| `upsert_project_feature` | A feature was added, meaningfully changed, gained or lost a limitation, or was deprecated or removed | "Team workspaces" now available | Implementation-only changes |

### Keep the profile true

If your work changes a durable product fact, update the profile in the same
session. Otherwise the profile will contradict the product, and an assistant
like Hermes will repeat the old claim to a customer.

Suppose the profile says "Limitation: no mobile support", and you implement
mobile support. Then:

1. `record_handoff`, for the next agent;
2. `user_update` (or `publish_project_update`): "The app now works on phones";
3. `update_project_knowledge` to rewrite `limitations`, and
   `upsert_project_feature` to add or update the mobile feature.

Profile-worthy changes include a new or removed feature, a new integration, a
pricing change, a removed limitation, a material change of audience, a new
onboarding flow, or new platform support. Ordinary bug fixes are not.
`update_project_knowledge` replaces the whole section, so write its complete
new text. Features are never deleted: set their status to `deprecated` or
`removed`.

There is no heartbeat. Don't build background daemons to refresh your state.
A `working`, `waiting` or `blocked` state that isn't refreshed is shown as
**stale** after a while (an hour by default), which is the honest signal that
nobody knows whether you're still at it.

## Ending work: the handoff

Before you stop after meaningful work, call `record_handoff`. It is the most
important call in the loop: it is how the next participant knows what
happened.

A useful handoff has:

- **Summary:** what you completed.
- **Current state:** where things stand now.
- **Current task:** what you were working on last.
- **Blockers:** anything in the way, or "none".
- **Next steps:** what should happen next, in order.
- **Branch and commit:** the facts others can check.

Good:

```
Summary:       Implemented actor-token authentication and repository lookup.
Current state: MCP works; Inspector tests pass.
Current task:  Secure remote access.
Blockers:      None in the code.
Next steps:    1. Configure HTTPS  2. Connect Claude  3. Connect Codex
Branch:        main
Commit:        a81c9e3
```

Not useful:

```
Did some work. Continue later.
```

Then leave your state accurate: `finished`, `idle`, `blocked` or `waiting`.
`record_handoff` takes `set_state`, so one call does both. Never leave
yourself marked `working` after you stop.

## Project updates: telling the owner what changed

A handoff is written for the next agent. A **project update** is written for
the person who owns the project. They show up on the dashboard overview, at
the top of the project page, and in the project's Updates page, which reads
like patch notes.

Same work, different audience:

```
Handoff:  Implemented session caching. All auth tests pass.
          Next step: update the login UI. Branch main, commit a83bd21.

Update:   Login is now faster
          The delay noticed during task 3 has been fixed.
          Signing in should now feel immediate.
```

**Describe the outcome, not the task status.** Use plain English and minimal
jargon, and say why the change matters.

| | Example |
|---|---|
| Good | "The slow API request was fixed. Pages that list projects now load straight away." |
| Good | "Remote access is now available securely over HTTPS." |
| Good | "Mobile navigation now works correctly." |
| Bad | "Task 3 completed." (task status, not an outcome) |
| Bad | "Refactored the request pipeline and changed the Redis fallback logic." (jargon; why should the owner care?) |
| Not worth an update | Renamed a variable. Ran tests. Changed formatting. |

How to write one:

- **Title:** a short headline stating the outcome. "Login is now faster", not
  "Auth changes" or "Task 4 complete".
- **Summary:** one to three short paragraphs: what changed, why it matters,
  what the owner can expect now.
- **Details** (optional): slightly more technical context, still readable. No
  implementation dumps.
- **Category:** `new`, `improved`, `fixed`, `changed` or `important`.
- **Task reference** (optional): "Task 3" or "Network setup". Supporting
  metadata, never the main content.

How to publish:

- **At the end of a session:** pass `user_update` to `record_handoff`. The
  update is linked to the handoff and published in the same transaction, so
  its branch and commit appear as technical details.
- **For a milestone in a long session:** call `publish_project_update`.

If there is nothing meaningful to tell the owner, don't publish an update.
Most sessions end with a handoff alone. Updates are permanent: to correct one,
publish a newer update.

## Never store

- private chain-of-thought or scratch reasoning
- full terminal transcripts or routine shell history
- every file edit or tool call
- source code, function bodies, lockfiles or dependency lists
- guesses presented as facts (mark them `unknown` instead)
- secrets: tokens, passwords, keys (OpenBrain rejects repository URLs that
  contain credentials, but it can't recognise every secret in free text)

## Assistants that don't write code

An operational assistant such as Hermes works around the project without the
repository: it explains the product, answers customer questions, drafts
campaigns, SEO content, outreach and announcements, and reports progress.

**First time it needs to understand a project:**

```
find_project
  ↓ get_project_profile            the durable product knowledge, complete
  ↓ get_project_context            the current state
  ↓ get_project_updates            recent changes
```

It should not rely on remembering this. OpenBrain is the source of truth, so it
re-reads when it needs to.

**A focused question** ("Does it support Stripe?", "What are its
limitations?", "How should I describe it to agencies?"): call
`get_project_profile` with the relevant `section` (for example
`integrations`, `limitations`, `positioning`, `faq`), or with `feature` for one
feature. `include_sections=false` lists all features.

**"What changed?"** ("What changed this week?", "What did Claude actually
accomplish?", "What happened across my projects today?"): call
`get_project_updates` for one project, or `get_overview`, whose
`recent_updates` span every project. Prefer updates to raw activity. Page
further back with `before`.

**Never invent missing facts.** Answer only from the profile, features and
updates. A section marked `unknown`, a missing section, or a feature that isn't
listed means OpenBrain has no verified information, so say so: "OpenBrain does
not currently contain verified information about that." Check a feature's
`status` before telling anyone it exists; `planned` and `deprecated` features
are not the same as `available` ones. Each section and feature shows who last
updated it and when, so mention it if the information looks old.

**Suggesting marketing from changes.** Hermes may connect a recent update with
the profile. For example, if the ICP is small SaaS founders and a new update
says automatic Stripe reconciliation was added, it may ask: "Automatic Stripe
reconciliation was recently added. Should I write a blog post targeting
self-hosted Stripe revenue analytics, and update the feature page?" OpenBrain
never triggers marketing actions itself; Hermes proposes, and the user
decides.

Hermes may add notes, record decisions it took part in, raise blockers it
discovers, update its own state, and publish an update about its own work. It
must not record handoffs or publish updates on another agent's behalf, and it
should say who did what.

## Tools

| Tool | Purpose |
|------|---------|
| `find_project` | Identify the project from a git remote, ID or slug |
| `get_project_profile` | What the project is and does: knowledge sections and features (all, one section, or one feature) |
| `get_project_context` | The current state: everything needed to resume work |
| `get_project_updates` | What changed, newest first, paged |
| `get_project_history` | Older handoffs, decisions, notes or activity, paged |
| `get_overview` | All projects, where each participant left them, and recent updates |
| `list_projects` | Browse projects |
| `register_project` | Track a new project (only when the user wants it) |
| `update_project` | Change status, focus, repository or description |
| `sync_project_profile` | Write a comprehensive profile in one call (onboarding, refreshes) |
| `update_project_knowledge` | Replace one knowledge section when a durable fact changes |
| `upsert_project_feature` | Add or update one feature |
| `set_agent_state` | Your current state and task |
| `record_handoff` | Where you left the project (optionally with a project update) |
| `publish_project_update` | Tell the project owner what meaningfully changed |
| `record_decision` | A durable decision |
| `add_note` | A durable note |
| `raise_blocker` / `resolve_blocker` | Track blockers |

The tool descriptions served over MCP carry the same guidance, so a client
that reads them behaves well even without these instructions.

## Why no project marker file?

Agents identify projects by their git remote, which OpenBrain normalises (SSH
and HTTPS spellings, case, a trailing `.git`). A marker file such as
`.openbrain` inside each repository would add a file to every repository you
track and could go stale. If a remote changes, update the project's repository
URL instead (`update_project`, or the project page). For repositories without
a remote, pass the project slug to `find_project`.
