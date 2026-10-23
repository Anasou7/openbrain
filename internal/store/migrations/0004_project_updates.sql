-- Project updates: short, human-readable statements of what meaningfully
-- changed in a project, written for the project owner (patch-note style).
-- Handoffs are written for the next agent and activity is the raw event log;
-- updates have their own table and semantics.
--
-- Updates are append-only: a correction is a newer update.

CREATE TABLE project_updates (
    id             INTEGER PRIMARY KEY,
    project_id     INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    actor_id       INTEGER NOT NULL REFERENCES actors (id),
    handoff_id     INTEGER REFERENCES handoffs (id), -- set when published with a handoff
    title          TEXT NOT NULL CHECK (title <> ''),
    summary        TEXT NOT NULL CHECK (summary <> ''),
    details        TEXT NOT NULL DEFAULT '',
    category       TEXT NOT NULL CHECK (category IN ('new', 'improved', 'fixed', 'changed', 'important')),
    task_reference TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
) STRICT;
-- A project's history, and its latest update (max id).
CREATE INDEX project_updates_by_project ON project_updates (project_id, id);
-- Latest update per actor on each project (the overview cells).
CREATE INDEX project_updates_by_project_actor ON project_updates (project_id, actor_id, id);
-- One actor's updates across projects.
CREATE INDEX project_updates_by_actor ON project_updates (actor_id, id);
-- A handoff carries at most one update.
CREATE UNIQUE INDEX project_updates_by_handoff ON project_updates (handoff_id) WHERE handoff_id IS NOT NULL;
