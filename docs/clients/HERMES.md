# OpenBrain instructions for Hermes

Paste everything below the line into `SOUL.md` in your Hermes home (loaded in
every session), or into a project's `AGENTS.md` or `.hermes.md`. Assumes the
`openbrain` MCP server is configured with Hermes' own actor token
([MCP.md](../MCP.md)).

---

## OpenBrain

OpenBrain (MCP server `openbrain`) is the source of truth about the user's
projects, without their code: each project's profile (what the product is and
does, who it is for, features, positioning, limitations, FAQ), its current
state, and its updates (what changed). Re-read it when you need it rather than
relying on memory.

- **To understand a project for the first time:** `find_project`, then
  `get_project_profile`, then `get_project_context`, then
  `get_project_updates`.
- **For a focused question** ("Does it support Stripe?", "How should I describe
  it to agencies?"): `get_project_profile` with the relevant `section`
  (integrations, limitations, positioning, faq, ...) or `feature`.
- **For "what changed?"**: `get_project_updates` for one project, or
  `get_overview` (its `recent_updates`) across projects. Prefer updates to raw
  activity.

Answer only from OpenBrain. If a section is missing or marked unknown, or a
feature is not listed (or is only planned), say that OpenBrain has no verified
information about it; never invent features, pricing or claims. Say who did
what and when.

When a recent update matters to the profile's audience, you may propose
content (a post, an announcement, a feature page) and ask the user before
doing it.

You may add notes, record decisions you took part in, raise blockers you
discover, update your own state and publish updates about your own work. Never
record a handoff or publish an update for work another agent did, and never
store private reasoning or transcripts.
