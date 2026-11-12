-- api/0002_authentication: sessions, the built-in roles, and
-- users.password_changed_at (ADR 0006).

-- When the password was last set. NULL only for rows that predate this
-- migration; there are none, because nothing could create a user before it.
ALTER TABLE users ADD COLUMN password_changed_at TEXT;

-- Sessions. The token itself is never stored: token_hash is the SHA-256 of
-- the 32 random bytes the client holds, so a copy of this database does not
-- contain a usable session.
--
-- kind says how the token is presented: 'cookie' sessions are accepted only
-- from the session cookie, 'bearer' sessions only from the Authorization
-- header.
CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY CHECK (length(token_hash) = 64),
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('cookie', 'bearer')),
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    -- The absolute end of the session, fixed when it is created.
    expires_at   TEXT NOT NULL,
    source_ip    TEXT,
    user_agent   TEXT
) STRICT, WITHOUT ROWID;

CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);

-- The three built-in roles. Their IDs are their names. "*" is every
-- permission, including ones added by later releases. The other two list
-- their permissions; a release that adds a permission updates them in its
-- own migration.
INSERT INTO roles (id, name, description, permissions, built_in) VALUES
    ('admin', 'admin',
     'Full access, including user management and every permission added later.',
     '["*"]', 1),
    ('operator', 'operator',
     'Read everything and perform operational actions. Cannot manage users.',
     '["system:read","node:read","task:read","task:cancel","user:read","role:read","audit:read"]', 1),
    ('viewer', 'viewer',
     'Read-only access to the node and its tasks.',
     '["system:read","node:read","task:read","role:read"]', 1);

-- Built-in roles cannot be edited or deleted through the application. A
-- migration that has to change one drops and recreates these triggers.
CREATE TRIGGER roles_builtin_no_update BEFORE UPDATE ON roles
WHEN OLD.built_in = 1
BEGIN
    SELECT RAISE(ABORT, 'built-in roles cannot be changed');
END;

CREATE TRIGGER roles_builtin_no_delete BEFORE DELETE ON roles
WHEN OLD.built_in = 1
BEGIN
    SELECT RAISE(ABORT, 'built-in roles cannot be deleted');
END;
