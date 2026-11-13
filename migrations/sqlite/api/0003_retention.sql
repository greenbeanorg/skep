-- api/0003_retention: what retention of the access audit log needs
-- (ADR 0007, decision 7).
--
-- The same mechanism as agent/0003, in skep-api's own database: an entry
-- leaves the live table only after it is inside a sealed archive segment.
-- audit_segments is the index of those segments, and the delete trigger
-- consults it. skep-api seals and removes; the agent never opens this
-- database (ADR 0004).

-- Audit segments ------------------------------------------------------------

-- One row per sealed archive segment. Inserting the row is the seal. A
-- segment is a contiguous range of audit_log IDs; the ID alone decides
-- which segment an entry belongs to, and month is a label. The segments
-- form a hash chain: previous_hash is the hash of the segment before.
CREATE TABLE audit_segments (
    -- 1, 2, 3, ... in sealing order.
    seq           INTEGER PRIMARY KEY CHECK (seq > 0),
    -- The calendar month (UTC) the segment was sealed for, as YYYY-MM.
    month         TEXT NOT NULL UNIQUE
                  CHECK (month GLOB '[0-9][0-9][0-9][0-9]-[01][0-9]'),
    first_id      INTEGER NOT NULL UNIQUE CHECK (first_id > 0),
    last_id       INTEGER NOT NULL UNIQUE CHECK (last_id >= first_id),
    entry_count   INTEGER NOT NULL CHECK (entry_count > 0),
    node_id       TEXT NOT NULL CHECK (node_id <> ''),
    -- The file in the archive directory. A name, never a path.
    file_name     TEXT NOT NULL UNIQUE
                  CHECK (file_name <> '' AND instr(file_name, '/') = 0),
    -- SHA-256 of the uncompressed segment, in hex.
    hash          TEXT NOT NULL CHECK (length(hash) = 64),
    -- Empty for the first segment.
    previous_hash TEXT NOT NULL CHECK (previous_hash = '' OR length(previous_hash) = 64),
    sealed_at     TEXT NOT NULL
) STRICT;

-- The index of sealed segments is never modified or shortened.
CREATE TRIGGER audit_segments_no_update BEFORE UPDATE ON audit_segments
BEGIN
    SELECT RAISE(ABORT, 'audit_segments is insert-only');
END;

CREATE TRIGGER audit_segments_no_delete BEFORE DELETE ON audit_segments
BEGIN
    SELECT RAISE(ABORT, 'audit_segments is insert-only');
END;

-- The first segment starts at the first entry of the log and has no
-- predecessor.
CREATE TRIGGER audit_segments_first BEFORE INSERT ON audit_segments
WHEN NOT EXISTS (SELECT 1 FROM audit_segments)
 AND (NEW.seq <> 1
   OR NEW.previous_hash <> ''
   OR EXISTS (SELECT 1 FROM audit_log WHERE id < NEW.first_id))
BEGIN
    SELECT RAISE(ABORT, 'audit_segments: the first segment must start at the first entry and have no predecessor');
END;

-- Every later segment starts right after the one before it, so ranges never
-- overlap and never leave a gap; it is for a later month; and it continues
-- the hash chain.
CREATE TRIGGER audit_segments_next BEFORE INSERT ON audit_segments
WHEN EXISTS (SELECT 1 FROM audit_segments)
 AND (NEW.seq <> (SELECT MAX(seq) FROM audit_segments) + 1
   OR NEW.first_id <> (SELECT MAX(last_id) FROM audit_segments) + 1
   OR NEW.month <= (SELECT MAX(month) FROM audit_segments)
   OR NEW.previous_hash <> (SELECT hash FROM audit_segments ORDER BY seq DESC LIMIT 1))
BEGIN
    SELECT RAISE(ABORT, 'audit_segments: a segment must start right after the previous one, be for a later month, and continue the hash chain');
END;

-- A segment describes entries that are in the live table at the moment it
-- is sealed: its last entry exists and its count is the number of entries
-- in its range.
CREATE TRIGGER audit_segments_matches_log BEFORE INSERT ON audit_segments
WHEN NOT EXISTS (SELECT 1 FROM audit_log WHERE id = NEW.last_id)
  OR NEW.entry_count <> (SELECT COUNT(*) FROM audit_log WHERE id BETWEEN NEW.first_id AND NEW.last_id)
BEGIN
    SELECT RAISE(ABORT, 'audit_segments: the segment does not match the entries in audit_log');
END;

-- Audit log -----------------------------------------------------------------

-- The unconditional rule "no DELETE" becomes a conditional one, evaluated
-- by the database: an entry may be removed only when it lies inside a
-- sealed segment. The update trigger of 0001 is unchanged: an entry is
-- never modified, archived or not.
DROP TRIGGER audit_log_no_delete;

CREATE TRIGGER audit_log_delete_only_archived BEFORE DELETE ON audit_log
WHEN NOT EXISTS (
    SELECT 1 FROM audit_segments s
    WHERE OLD.id BETWEEN s.first_id AND s.last_id
)
BEGIN
    SELECT RAISE(ABORT, 'audit_log: an entry may be removed only after it is archived');
END;

-- Nothing is ever added inside a sealed range. Without this, a row
-- inserted with an explicit old ID would count as archived, and could be
-- removed, without being in any segment file.
CREATE TRIGGER audit_log_no_insert_into_segment AFTER INSERT ON audit_log
WHEN NEW.id <= COALESCE((SELECT MAX(last_id) FROM audit_segments), 0)
BEGIN
    SELECT RAISE(ABORT, 'audit_log: an entry cannot be added inside a sealed segment');
END;
