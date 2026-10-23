# OpenBrain instructions for any MCP agent

Connect the agent to `<your OpenBrain URL>/mcp` (Streamable HTTP) with the
header `Authorization: Bearer <its own actor token>`. Then put everything below
the line wherever the agent keeps standing instructions.

---

## OpenBrain

OpenBrain (MCP server `openbrain`) holds each project's profile (what the product is
and does), its current state (where the user and every agent left off) and
what changed.

When starting meaningful work in a git repository:
- Note the remote, branch and HEAD commit.
- Call `find_project` with the remote URL. If found, read `get_project_context`
  before continuing, even if you worked here before; others may have changed it.
- Call `set_agent_state` with `working` and a one-line task.

While working, record only what will matter later: `record_decision` for choices
to respect, `raise_blocker` for real blockers (not to-dos), `add_note` for durable
facts.

Before stopping after meaningful work, call `record_handoff` with what you did,
where it stands, blockers, next steps, branch and commit, and set your final
state (finished, idle, blocked or waiting).
If something meaningful changed for the project owner, include `user_update`:
a plain-English title and summary of the outcome ("Login is now faster"), not
the task status. For a milestone mid-session, use `publish_project_update`.
Most sessions need no update.

If your work changes a durable product fact (a feature, limitation, integration,
platform, onboarding or pricing), also call `update_project_knowledge` or
`upsert_project_feature`, so the profile stays true. Not for ordinary bug fixes.

When asked to fully add this project to OpenBrain or to build its profile:
register it with `register_project` if `find_project` does not find it;
inspect the repository and docs deeply (README, UI routes, CLI, API, config,
schema, integrations, deployment), then call `sync_project_profile` with the
knowledge sections and every user-facing feature, described for people who
never see the code. Record only what the repository supports: mark facts you
cannot establish (pricing, ICP, competitors) as `unknown`; never guess.

For questions across projects, call `get_overview`; its `recent_updates` say
what changed. Never store private reasoning, transcripts or routine shell
history. Don't call `register_project` unless the user wants the repository
tracked.
