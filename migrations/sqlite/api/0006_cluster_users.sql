-- api/0006_cluster_users: the users table becomes skep-api's read copy of
-- the cluster's replicated users (ADR 0013 amendment 4, step 8c-1).
--
-- From this migration on, the only code that writes users.username,
-- display_name, password_hash, role_id, disabled and credential_epoch is
-- the copy's apply function (internal/apidb ApplyAnswer). last_login_at
-- stays a local column that the copy never touches.

-- The user's credential epoch (D6): raised by a password change, a role
-- change and a change of the disabled flag.
ALTER TABLE users ADD COLUMN credential_epoch INTEGER NOT NULL DEFAULT 1 CHECK (credential_epoch >= 1);

-- A session is valid only while its epoch equals its user's (D15). The
-- default keeps every existing session valid.
ALTER TABLE sessions ADD COLUMN credential_epoch INTEGER NOT NULL DEFAULT 1;

-- A session created without quorum (D18): at most one hour long.
ALTER TABLE sessions ADD COLUMN degraded INTEGER NOT NULL DEFAULT 0 CHECK (degraded IN (0, 1));

-- One row: the cluster the copy belongs to ('' until the first answer is
-- adopted), the index of the last entry that changed the users as the copy
-- reflects it, and the import of this host's 0.1 users (D14):
-- not_needed (no user when this migration ran), pending (users that are
-- not yet the cluster's), done, superseded. import_op_id is the import's
-- operation ID, recorded before it is sent.
CREATE TABLE cluster_copy (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    cluster_id   TEXT NOT NULL DEFAULT '',
    users_index  INTEGER NOT NULL DEFAULT 0 CHECK (users_index >= 0),
    import_state TEXT NOT NULL CHECK (import_state IN ('not_needed', 'pending', 'done', 'superseded')),
    import_op_id TEXT NOT NULL DEFAULT ''
) STRICT;

INSERT INTO cluster_copy (id, import_state)
SELECT 1, CASE WHEN EXISTS (SELECT 1 FROM users) THEN 'pending' ELSE 'not_needed' END;
