-- Human login accounts and their browser sessions.
--
-- A user is a person who can sign in to the web interface. Every user maps to
-- exactly one human actor, which is what their writes are attributed to; the
-- display name lives on the actor. Agents never have user accounts; they use
-- actor tokens.

CREATE TABLE users (
    id                  INTEGER PRIMARY KEY,
    actor_id            INTEGER NOT NULL UNIQUE REFERENCES actors (id),
    username            TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash       TEXT NOT NULL, -- Argon2id, PHC string format
    role                TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    password_changed_at TEXT NOT NULL,
    last_login_at       TEXT,
    disabled_at         TEXT
) STRICT;

-- Only the SHA-256 hash of a session token is stored; the token itself lives
-- in the browser's cookie.
CREATE TABLE user_sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    revoked_at   TEXT
) STRICT;
CREATE INDEX user_sessions_by_user ON user_sessions (user_id);
