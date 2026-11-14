-- agent/0005_storage_and_jails: snapshots, linked clones, and the amended
-- container sketch (ADR 0010 decisions 2, 3, 7, 8, 10, 13).
--
-- The conventions of 0001 and 0004 hold: desired_* and configuration
-- columns are written by tasks that act for a user, observed_* columns by
-- observation only, and the two never share a column. This migration adds
-- tables and columns and rebuilds nothing.

-- Snapshots -------------------------------------------------------------------

-- One row per snapshot Skep took of a VM's or a jail's datasets. The ZFS
-- name is derived from the ID (@skep-<id>), never from the label. A
-- snapshot is one recursive zfs snapshot across every dataset of its
-- resource, so one row stands for all of them (decision 2a).
CREATE TABLE snapshots (
    id                  TEXT PRIMARY KEY,
    resource_kind       TEXT NOT NULL CHECK (resource_kind IN ('vm', 'container')),
    resource_id         TEXT NOT NULL,
    -- The user's label: the character set of VM names.
    name                TEXT NOT NULL
                        CHECK (length(name) BETWEEN 1 AND 63
                               AND name NOT GLOB '*[^A-Za-z0-9._-]*'
                               AND substr(name, 1, 1) NOT IN ('.', '_', '-')),
    description         TEXT NOT NULL DEFAULT '' CHECK (length(description) <= 1024),
    -- consistent: taken while the resource was stopped; crash_consistent:
    -- taken while it ran, what a power cut would leave on disk.
    consistency         TEXT NOT NULL CHECK (consistency IN ('consistent', 'crash_consistent')),
    -- 1 once the snapshot task has finished. A row that is not
    -- provisioned is what an interrupted snapshot task left behind.
    provisioned         INTEGER NOT NULL DEFAULT 0 CHECK (provisioned IN (0, 1)),
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- observed
    observed_exists     INTEGER CHECK (observed_exists IS NULL OR observed_exists IN (0, 1)),
    observed_used_bytes INTEGER CHECK (observed_used_bytes IS NULL OR observed_used_bytes >= 0),
    observed_at         TEXT,
    UNIQUE (resource_kind, resource_id, name)
) STRICT;

CREATE INDEX snapshots_resource ON snapshots (resource_kind, resource_id, created_at);

-- A snapshot names a resource that exists. resource_id cannot carry a
-- foreign key to two tables, so triggers keep it honest.
CREATE TRIGGER snapshots_resource_exists BEFORE INSERT ON snapshots
WHEN NOT (
       (NEW.resource_kind = 'vm' AND EXISTS (SELECT 1 FROM virtual_machines WHERE id = NEW.resource_id))
    OR (NEW.resource_kind = 'container' AND EXISTS (SELECT 1 FROM containers WHERE id = NEW.resource_id))
)
BEGIN
    SELECT RAISE(ABORT, 'snapshots: the resource does not exist');
END;

CREATE TRIGGER snapshots_resource_fixed BEFORE UPDATE OF resource_kind, resource_id ON snapshots
BEGIN
    SELECT RAISE(ABORT, 'snapshots: a snapshot cannot move to another resource');
END;

-- Deleting a resource deletes its snapshot rows. A snapshot that a clone
-- still depends on is protected by the clone's foreign key below, so the
-- resource cannot be deleted while it has clones.
CREATE TRIGGER virtual_machines_delete_snapshots AFTER DELETE ON virtual_machines
BEGIN
    DELETE FROM snapshots WHERE resource_kind = 'vm' AND resource_id = OLD.id;
END;

CREATE TRIGGER containers_delete_snapshots AFTER DELETE ON containers
BEGIN
    DELETE FROM snapshots WHERE resource_kind = 'container' AND resource_id = OLD.id;
END;

-- Clones ----------------------------------------------------------------------

-- A linked clone depends on the snapshot it was made from (decision 3).
-- RESTRICT is the database's half of snapshot_has_clones: the origin
-- snapshot, and so its resource, cannot be deleted while the clone exists.
ALTER TABLE virtual_machines ADD COLUMN cloned_from_snapshot_id TEXT
    REFERENCES snapshots (id) ON DELETE RESTRICT;

CREATE INDEX virtual_machines_cloned_from ON virtual_machines (cloned_from_snapshot_id);

-- Jail templates --------------------------------------------------------------

-- A FreeBSD userland built once by container.template.create and then
-- read-only (decision 7b). Templates are per pool: clones cannot cross
-- pools.
CREATE TABLE container_templates (
    id              TEXT PRIMARY KEY,
    node_id         TEXT NOT NULL REFERENCES nodes (id) ON DELETE RESTRICT,
    storage_pool_id TEXT NOT NULL REFERENCES storage_pools (id) ON DELETE RESTRICT,
    name            TEXT NOT NULL UNIQUE
                    CHECK (length(name) BETWEEN 1 AND 63
                           AND name NOT GLOB '*[^A-Za-z0-9._-]*'
                           AND substr(name, 1, 1) NOT IN ('.', '_', '-')),
    source          TEXT NOT NULL CHECK (source IN ('pkgbase', 'dist')),
    -- pkgbase: the package names, from storage.template_packages.
    packages        TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(packages)),
    -- dist: the SHA-256 of base.txz as checked against its MANIFEST.
    archive_sha256  TEXT CHECK (archive_sha256 IS NULL OR (length(archive_sha256) = 64
                                AND archive_sha256 NOT GLOB '*[^0-9a-f]*')),
    provisioned     INTEGER NOT NULL DEFAULT 0 CHECK (provisioned IN (0, 1)),
    -- observed: freebsd-version -u of the built userland, and its size.
    observed_release    TEXT,
    observed_used_bytes INTEGER CHECK (observed_used_bytes IS NULL OR observed_used_bytes >= 0),
    observed_at     TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK ((source = 'dist') = (archive_sha256 IS NOT NULL))
) STRICT;

-- Containers ------------------------------------------------------------------

-- 0001 sketched containers before anything used them; they have never held
-- a row. These are the amendments of decision 8.

ALTER TABLE containers ADD COLUMN template_id TEXT
    REFERENCES container_templates (id) ON DELETE RESTRICT;
ALTER TABLE containers ADD COLUMN storage_pool_id TEXT
    REFERENCES storage_pools (id) ON DELETE RESTRICT;
-- 1 once container.create has finished, as for VMs.
ALTER TABLE containers ADD COLUMN provisioned INTEGER NOT NULL DEFAULT 0
    CHECK (provisioned IN (0, 1));
-- 1 from the first start on, and never 0 again: not after a rollback, and
-- not on a clone of a jail that ever ran (decision 13).
ALTER TABLE containers ADD COLUMN started_once INTEGER NOT NULL DEFAULT 0
    CHECK (started_once IN (0, 1));
ALTER TABLE containers ADD COLUMN cloned_from_snapshot_id TEXT
    REFERENCES snapshots (id) ON DELETE RESTRICT;

CREATE TRIGGER containers_started_once_sticky BEFORE UPDATE OF started_once ON containers
WHEN OLD.started_once = 1 AND NEW.started_once <> 1
BEGIN
    SELECT RAISE(ABORT, 'containers: started_once never goes back to 0');
END;

CREATE INDEX containers_template ON containers (template_id);
CREATE INDEX containers_cloned_from ON containers (cloned_from_snapshot_id);

-- One VNET interface of a jail: an epair whose host end joins an
-- allow-listed bridge (decision 10). It mirrors vm_networks.
CREATE TABLE container_networks (
    id             TEXT PRIMARY KEY,
    container_id   TEXT NOT NULL REFERENCES containers (id) ON DELETE CASCADE,
    slot           INTEGER NOT NULL CHECK (slot >= 0),
    mac_address    TEXT UNIQUE,
    bridge         TEXT NOT NULL,
    -- IPv4: dhcp, or static with an address in CIDR form and a gateway.
    ipv4_mode      TEXT NOT NULL DEFAULT 'dhcp' CHECK (ipv4_mode IN ('dhcp', 'static')),
    ipv4_address   TEXT,
    ipv4_gateway   TEXT,
    -- IPv6: none, slaac, or static with an address in CIDR form.
    ipv6_mode      TEXT NOT NULL DEFAULT 'none' CHECK (ipv6_mode IN ('none', 'slaac', 'static')),
    ipv6_address   TEXT,
    ipv6_gateway   TEXT,
    -- observed: the host end of the epair, while the jail runs.
    observed_epair TEXT,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (container_id, slot),
    CHECK ((ipv4_mode = 'static') = (ipv4_address IS NOT NULL)),
    CHECK (ipv4_mode = 'static' OR ipv4_gateway IS NULL),
    CHECK ((ipv6_mode = 'static') = (ipv6_address IS NOT NULL)),
    CHECK (ipv6_mode = 'static' OR ipv6_gateway IS NULL)
) STRICT;
