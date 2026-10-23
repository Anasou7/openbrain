-- Per-actor credentials and idempotency keys.
--
-- An actor token authenticates one actor: writes made with it are always
-- attributed to that actor. Only the SHA-256 hash of a token is stored; the
-- secret is shown once when it is created.

CREATE TABLE actor_tokens (
    id           INTEGER PRIMARY KEY,
    actor_id     INTEGER NOT NULL REFERENCES actors (id),
    token_hash   TEXT NOT NULL UNIQUE, -- hex SHA-256 of the full token
    prefix       TEXT NOT NULL,        -- first characters, to recognise a token in lists
    label        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
) STRICT;
CREATE INDEX actor_tokens_by_actor ON actor_tokens (actor_id);

-- A mutating call that carries a client-chosen idempotency key records the
-- record it created, so a retry returns that record instead of a duplicate.
CREATE TABLE idempotency_keys (
    actor_id   INTEGER NOT NULL REFERENCES actors (id),
    key        TEXT NOT NULL,
    operation  TEXT NOT NULL,    -- e.g. add_note:12 (operation and project)
    subject_id INTEGER NOT NULL, -- id of the record the first call created
    created_at TEXT NOT NULL,
    PRIMARY KEY (actor_id, key)
) STRICT, WITHOUT ROWID;
