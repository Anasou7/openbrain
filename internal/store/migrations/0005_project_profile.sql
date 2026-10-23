-- Project profile: durable, human-readable knowledge of what a project is and
-- does, for people and assistants that never see the code (what it is, who it
-- is for, positioning, limitations, FAQ, ...), plus its user-facing features.
-- It is not a copy of the repository.
--
-- Section keys are free-form: a set of canonical keys is suggested by the
-- application, and custom keys need no migration. A section can say that a
-- fact is known, unknown or not applicable, so gaps are explicit rather than
-- guessed. Provenance: who created and last changed each row, when, and
-- optionally the repository state it was derived from.

CREATE TABLE profile_sections (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    title         TEXT NOT NULL CHECK (title <> ''),
    content       TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'known' CHECK (status IN ('known', 'unknown', 'not_applicable')),
    source_branch TEXT NOT NULL DEFAULT '',
    source_commit TEXT NOT NULL DEFAULT '',
    created_by    INTEGER NOT NULL REFERENCES actors (id),
    updated_by    INTEGER NOT NULL REFERENCES actors (id),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (project_id, key), -- also serves lookups of one section
    CHECK (status <> 'known' OR content <> '')
) STRICT;

-- Features are never deleted when they go away: status records it.
CREATE TABLE features (
    id            INTEGER PRIMARY KEY,
    project_id    INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    slug          TEXT NOT NULL,
    name          TEXT NOT NULL CHECK (name <> ''),
    summary       TEXT NOT NULL CHECK (summary <> ''),
    description   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'available'
                  CHECK (status IN ('planned', 'experimental', 'available', 'deprecated', 'removed')),
    target_users  TEXT NOT NULL DEFAULT '',
    use_cases     TEXT NOT NULL DEFAULT '', -- one per line
    limitations   TEXT NOT NULL DEFAULT '',
    source_branch TEXT NOT NULL DEFAULT '',
    source_commit TEXT NOT NULL DEFAULT '',
    created_by    INTEGER NOT NULL REFERENCES actors (id),
    updated_by    INTEGER NOT NULL REFERENCES actors (id),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (project_id, slug)
) STRICT;
CREATE INDEX features_by_project_status ON features (project_id, status);
