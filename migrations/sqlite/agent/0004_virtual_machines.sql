-- agent/0004_virtual_machines: what the first working VM needs (ADR 0009
-- decision 2b).
--
-- 0001 sketched virtual_machines, vm_disks, vm_networks, storage_pools, and
-- storage_volumes before anything used them. They have never held a row.
-- This migration amends the sketch; it adds columns and rules and rebuilds
-- nothing.
--
-- The conventions of 0001 hold: desired_* and configuration columns are
-- written by tasks that act for a user, observed_* columns by observation
-- only, and the two never share a column.

-- Virtual machines ------------------------------------------------------------

-- 1 once vm.create has finished. A VM that is not provisioned can only be
-- deleted: an interrupted create leaves such a row behind.
ALTER TABLE virtual_machines ADD COLUMN provisioned INTEGER NOT NULL DEFAULT 0
    CHECK (provisioned IN (0, 1));

-- The bhyve process, while the VM runs.
ALTER TABLE virtual_machines ADD COLUMN observed_pid INTEGER
    CHECK (observed_pid IS NULL OR observed_pid > 0);

CREATE INDEX virtual_machines_name ON virtual_machines (name);

-- Disks -----------------------------------------------------------------------

-- Install media: the name of an ISO file in the media directory. A name,
-- never a path.
ALTER TABLE vm_disks ADD COLUMN media_name TEXT
    CHECK (media_name IS NULL OR (media_name <> '' AND instr(media_name, '/') = 0));

-- A disk has exactly one backing: install media on the 'ahci-cd' bus, with
-- a media name and no volume, or a volume on any other bus, with no media
-- name. SQLite cannot add a table constraint to an existing table, so two
-- triggers enforce it.
CREATE TRIGGER vm_disks_one_backing_insert BEFORE INSERT ON vm_disks
WHEN NOT (
       (NEW.bus = 'ahci-cd' AND NEW.media_name IS NOT NULL AND NEW.storage_volume_id IS NULL
            AND NEW.read_only = 1)
    OR (NEW.bus <> 'ahci-cd' AND NEW.media_name IS NULL AND NEW.storage_volume_id IS NOT NULL)
)
BEGIN
    SELECT RAISE(ABORT, 'vm_disks: a disk is either install media on ahci-cd (read-only, no volume) or a volume on another bus');
END;

CREATE TRIGGER vm_disks_one_backing_update BEFORE UPDATE ON vm_disks
WHEN NOT (
       (NEW.bus = 'ahci-cd' AND NEW.media_name IS NOT NULL AND NEW.storage_volume_id IS NULL
            AND NEW.read_only = 1)
    OR (NEW.bus <> 'ahci-cd' AND NEW.media_name IS NULL AND NEW.storage_volume_id IS NOT NULL)
)
BEGIN
    SELECT RAISE(ABORT, 'vm_disks: a disk is either install media on ahci-cd (read-only, no volume) or a volume on another bus');
END;

-- Volumes ---------------------------------------------------------------------

-- A thin volume has no reservation (zfs create -s). The default is a full
-- reservation, which cannot overcommit the pool.
ALTER TABLE storage_volumes ADD COLUMN desired_thin INTEGER NOT NULL DEFAULT 0
    CHECK (desired_thin IN (0, 1));
-- The volblocksize the volume is created with. It cannot change afterwards.
ALTER TABLE storage_volumes ADD COLUMN desired_volblocksize INTEGER
    CHECK (desired_volblocksize IS NULL OR desired_volblocksize > 0);
ALTER TABLE storage_volumes ADD COLUMN observed_volsize_bytes INTEGER
    CHECK (observed_volsize_bytes IS NULL OR observed_volsize_bytes >= 0);
ALTER TABLE storage_volumes ADD COLUMN observed_referenced_bytes INTEGER
    CHECK (observed_referenced_bytes IS NULL OR observed_referenced_bytes >= 0);

-- Pools -----------------------------------------------------------------------

-- storage_pools is a cache of the allow-listed pools as observed: one row
-- per pool in storage.pools of the agent's configuration. The space a new
-- dataset could use, which is not the pool's free space.
ALTER TABLE storage_pools ADD COLUMN observed_available_bytes INTEGER
    CHECK (observed_available_bytes IS NULL OR observed_available_bytes >= 0);
-- Why the pool could not be read, when it could not.
ALTER TABLE storage_pools ADD COLUMN observed_error TEXT;

-- Networks --------------------------------------------------------------------

-- The tap interface on the host, while the VM runs.
ALTER TABLE vm_networks ADD COLUMN observed_tap TEXT;

-- A VLAN on a bridge port belongs to the networking milestone. Until then
-- the column must stay empty.
CREATE TRIGGER vm_networks_no_vlan_insert BEFORE INSERT ON vm_networks
WHEN NEW.vlan_tag IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'vm_networks: vlan_tag is not supported yet');
END;

CREATE TRIGGER vm_networks_no_vlan_update BEFORE UPDATE OF vlan_tag ON vm_networks
WHEN NEW.vlan_tag IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'vm_networks: vlan_tag is not supported yet');
END;
