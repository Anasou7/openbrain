-- Initial schema.
--
-- Timestamps are UTC ISO-8601 text with millisecond precision
-- ('2006-01-02T15:04:05.000Z') so they sort correctly as strings.
-- Never edit this file once released; add a new numbered migration instead.

CREATE TABLE actors (
    id           INTEGER PRIMARY KEY,
    slug         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('human', 'agent', 'automation')),
    metadata     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata) AND json_type(metadata) = 'object'),
    created_at   TEXT NOT NULL
) STRICT;

-- The human operator. The dashboard writes as this actor.
INSERT INTO actors (slug, display_name, kind, created_at)
VALUES ('user', 'User', 'human', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE projects (
    id             INTEGER PRIMARY KEY,
    slug           TEXT NOT NULL UNIQUE,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'done', 'archived')),
    current_focus  TEXT NOT NULL DEFAULT '',
    repo_url       TEXT NOT NULL DEFAULT '',
    default_branch TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    archived_at    TEXT
) STRICT;

CREATE TABLE notes (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    actor_id   INTEGER NOT NULL REFERENCES actors (id),
    kind       TEXT NOT NULL DEFAULT 'note',
    content    TEXT NOT NULL CHECK (content <> ''),
    pinned     INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX notes_by_project ON notes (project_id, id);
-- Serves "latest note per actor" on the overview.
CREATE INDEX notes_by_project_actor ON notes (project_id, actor_id, id);

CREATE TABLE handoffs (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    actor_id      INTEGER NOT NULL REFERENCES actors (id),
    -- The actor's own account of the session (interpretation).
    summary       TEXT NOT NULL CHECK (summary <> ''),
    current_state TEXT NOT NULL DEFAULT '',
    current_task  TEXT NOT NULL DEFAULT '',
    blockers      TEXT NOT NULL DEFAULT '',
    next_steps    TEXT NOT NULL DEFAULT '',
    -- Checkable facts about where the work stands.
    branch        TEXT NOT NULL DEFAULT '',
    commit_sha    TEXT NOT NULL DEFAULT '',
    metadata      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata) AND json_type(metadata) = 'object'),
    created_at    TEXT NOT NULL
) STRICT;
-- Serves "latest handoff per actor" (max(id) per group) and per-actor history.
CREATE INDEX handoffs_by_project_actor ON handoffs (project_id, actor_id, id);

CREATE TABLE decisions (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    actor_id   INTEGER NOT NULL REFERENCES actors (id),
    decision   TEXT NOT NULL CHECK (decision <> ''),
    reasoning  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX decisions_by_project ON decisions (project_id, id);

CREATE TABLE blockers (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    actor_id    INTEGER NOT NULL REFERENCES actors (id),
    description TEXT NOT NULL CHECK (description <> ''),
    resolution  TEXT NOT NULL DEFAULT '',
    resolved_by INTEGER REFERENCES actors (id),
    resolved_at TEXT,
    created_at  TEXT NOT NULL,
    CHECK ((resolved_at IS NULL) = (resolved_by IS NULL))
) STRICT;
CREATE INDEX blockers_by_project ON blockers (project_id, id);

CREATE TABLE actor_states (
    project_id   INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    actor_id     INTEGER NOT NULL REFERENCES actors (id),
    state        TEXT NOT NULL CHECK (state IN ('idle', 'working', 'blocked', 'waiting', 'finished')),
    current_task TEXT NOT NULL DEFAULT '',
    started_at   TEXT,          -- last transition into 'working'
    finished_at  TEXT,          -- last transition into 'finished'
    updated_at   TEXT NOT NULL, -- last report from the actor, even if nothing changed
    PRIMARY KEY (project_id, actor_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE activity (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    actor_id   INTEGER NOT NULL REFERENCES actors (id),
    kind       TEXT NOT NULL,  -- e.g. note.added, handoff.created, blocker.resolved
    subject_id INTEGER,        -- id of the note/handoff/blocker/... the event is about
    summary    TEXT NOT NULL,  -- one human-readable line
    via        TEXT NOT NULL,  -- channel the write arrived through: dashboard, api, ...
    created_at TEXT NOT NULL
) STRICT;
CREATE INDEX activity_by_project ON activity (project_id, id);
