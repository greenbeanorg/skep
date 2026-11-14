-- api/0004_vm_permissions: the permissions of the first working VM (ADR
-- 0009 decision 10).
--
--   vm:read       list and read VMs
--   vm:write      create and update VMs
--   vm:delete     delete a VM and its disks
--   vm:power      start, stop, force-stop, restart
--   vm:console    attach to a VM's console, which is a login on the guest
--   storage:read  list the allow-listed pools and the install media
--
-- operator gains all six; viewer gains the two reads; admin is "*" already.
--
-- Built-in roles cannot be changed through the application. As 0002 said,
-- a migration that has to change one drops and recreates the triggers.

DROP TRIGGER roles_builtin_no_update;

UPDATE roles SET
    permissions = '["system:read","node:read","task:read","task:cancel","user:read","role:read","audit:read","vm:read","vm:write","vm:delete","vm:power","vm:console","storage:read"]',
    description = 'Read everything and perform operational actions, including the full VM lifecycle and the VM console. Cannot manage users.'
WHERE id = 'operator' AND built_in = 1;

UPDATE roles SET
    permissions = '["system:read","node:read","task:read","role:read","vm:read","storage:read"]',
    description = 'Read-only access to the node, its tasks, its VMs, and its storage.'
WHERE id = 'viewer' AND built_in = 1;

CREATE TRIGGER roles_builtin_no_update BEFORE UPDATE ON roles
WHEN OLD.built_in = 1
BEGIN
    SELECT RAISE(ABORT, 'built-in roles cannot be changed');
END;
