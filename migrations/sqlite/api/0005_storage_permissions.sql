-- api/0005_storage_permissions: the permissions of storage management and
-- jails (ADR 0010 decision 14).
--
--   vm:snapshot         create, roll back, delete VM snapshots (a clone
--                       needs vm:write as well)
--   container:read      list and read jails, their snapshots, the console
--                       log tail
--   container:write     create (from a template), update, clone
--   container:delete    delete a jail and its dataset
--   container:power     start, stop, restart
--   container:snapshot  create, roll back, delete jail snapshots
--   storage:write       create and delete templates, remove unowned
--                       datasets: admin only, which "*" already covers
--
-- operator gains every one but storage:write; viewer gains
-- container:read. Removing a VM disk needs vm:delete, which operator has.
--
-- The grants are made here, once, as the ADR orders. internal/auth declares
-- each permission when the route that enforces it exists; until then the
-- string is in the role and Role.Allows denies it, as it denies every
-- undeclared permission.
--
-- Built-in roles cannot be changed through the application, so this
-- migration drops and recreates the trigger, as 0004 did.

DROP TRIGGER roles_builtin_no_update;

UPDATE roles SET
    permissions = '["system:read","node:read","task:read","task:cancel","user:read","role:read","audit:read","vm:read","vm:write","vm:delete","vm:power","vm:console","vm:snapshot","storage:read","container:read","container:write","container:delete","container:power","container:snapshot"]',
    description = 'Read everything and perform operational actions: the full VM and jail lifecycle, snapshots, clones, and the VM console. Cannot manage users or build templates.'
WHERE id = 'operator' AND built_in = 1;

UPDATE roles SET
    permissions = '["system:read","node:read","task:read","role:read","vm:read","storage:read","container:read"]',
    description = 'Read-only access to the node, its tasks, its VMs and jails, and its storage.'
WHERE id = 'viewer' AND built_in = 1;

CREATE TRIGGER roles_builtin_no_update BEFORE UPDATE ON roles
WHEN OLD.built_in = 1
BEGIN
    SELECT RAISE(ABORT, 'built-in roles cannot be changed');
END;
