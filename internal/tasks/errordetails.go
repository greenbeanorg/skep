package tasks

import "github.com/greenbeanorg/skep/pkg/models"

// ErrorDetailKeys lists, for each error code, the keys of a failure's
// details that the API shows with the task's error. A failed task's
// details come from its "finished" audit entry; only the keys listed here
// for the task's error code leave the agent. Anything else, including a
// key added later and not yet reviewed, stays in the audit log only.
//
// Every key a code can carry was reviewed (fix: feat/task-error-details).
// These are never listed, because they could hold a secret, a key path or
// a password, or free text from a command or a package that nobody
// reviewed:
//
//   - error (disk_device_missing): the text of a failed zfs command
//   - stderr (template_build_failed): a build step's standard error
//   - files (template_build_failed): paths of SSH host key files found in
//     a template
//   - cleanup_error (any template build code): the text of failed clean-up
//     steps
//   - paths, build_error (template_host_changed): host files under /etc,
//     /var/db/pkg and /usr/share/certs that changed (key files among them)
//     and the text of the build's failure
//   - text (template_script_unknown): a package script's text
//   - seen_on (container_mac_in_use): built from ifconfig's output, up to
//     the port name in the bridge's address table, so it is command output
//     and not a fixed value
//
// The other keys are identifiers, names, sizes, states and fixed commands
// that the error's message already states or that a caller who can read
// the task can read on the resource itself.
var ErrorDetailKeys = map[string][]string{
	models.CodeBridgeNotAllowed:               {"bridge", "member"},
	models.CodeCloneAddressingRequired:        {"slots"},
	models.CodeConsoleBusy:                    {"max_attachments"},
	models.CodeContainerAddressInUse:          {"slot", "address", "bridge", "held_by_container_id", "held_by_container_name"},
	models.CodeContainerDiskQuotaBelowUsage:   {"container_id", "template_id", "disk_quota", "referenced_bytes"},
	models.CodeContainerLockedMemoryAboveMax:  {"locked_memory", "max_locked_memory_bytes"},
	models.CodeContainerLockedMemoryExhausted: {"container_id", "running_bytes", "required_bytes", "max_locked_memory_bytes"},
	models.CodeContainerMACInUse:              {"container_id", "slot", "mac_address", "bridge", "held_by_kind", "held_by_id", "held_by_name"},
	models.CodeContainerMACInvalid:            {"field", "slot", "mac_address"},
	models.CodeContainerNotProvisioned:        {"container_id"},
	models.CodeContainerNotRunning:            {"container_id"},
	models.CodeContainerNotStopped:            {"container_id"},
	models.CodeContainerQuarantined:           {"container_id", "state_detail"},
	models.CodeContainerRemoveFailed:          {"container_id"},
	models.CodeContainerSSHUnavailable:        {"template_id", "template_name"},
	models.CodeContainerStartFailed:           {"container_id", "extra_devices"},
	models.CodeContainerStarted:               {"container_id"},
	models.CodeDevfsRulesetConflict:           {"ruleset"},
	models.CodeDiskDeviceMissing:              {"dataset", "reason", "volmode", "command", "rounds"},
	models.CodeDiskHasSnapshots:               {"snapshots", "foreign_snapshots"},
	models.CodeInsufficientMemory:             {"running_bytes", "required_bytes", "guest_memory_bytes"},
	models.CodeInsufficientSpace:              {"pool", "available_bytes", "pool_size_bytes", "required_bytes"},
	models.CodeJailParentInsecure:             {"path"},
	models.CodeMediaNotFound:                  {"media"},
	models.CodeNameserversRequireNIC:          {"nameservers_given"},
	models.CodePoolNotAllowed:                 {"pool"},
	models.CodePortFilterFailed:               {"container_id", "slot", "interface", "step"},
	models.CodePortFilterUnavailable:          {"missing_modules"},
	models.CodeRctlUnavailable:                {"container_id", "racct_enabled"},
	models.CodeReleaseUnsupported:             {"template_release", "host_release"},
	models.CodeResourceBusy:                   {"task_id"},
	models.CodeSnapshotHasClones:              {"clones"},
	models.CodeSnapshotIncomplete:             {"snapshot_id", "datasets_without_snapshot"},
	models.CodeSnapshotLimit:                  {"limit"},
	models.CodeSnapshotNotLatest:              {"snapshot_id", "newer_snapshot_ids", "foreign_snapshots"},
	models.CodeStopTimeout:                    {"timeout_seconds"},
	models.CodeStorageFeatureUnsupported:      {"pool", "pool_type", "pools", "source"},
	models.CodeStorageLocked:                  {"dataset", "keystatus"},
	models.CodeStorageUnavailable:             {"pool", "slot"},
	models.CodeTemplateArchiveMismatch:        {"manifest_sha256", "archive_sha256"},
	models.CodeTemplateBuildFailed:            {"step", "exit_code"},
	models.CodeTemplateNotProvisioned:         {"template_id"},
	models.CodeTemplateScriptUnknown:          {"package", "version", "kind", "phase", "sha256", "path"},
	models.CodeTemplateTriggerLeft:            {"files"},
	models.CodeVMMACInUse:                     {"slot", "mac_address", "held_by_kind", "held_by_id", "held_by_name"},
	models.CodeVMNotRunning:                   {"state"},
	models.CodeVMNotStopped:                   {"state"},
	models.CodeVMStartFailed:                  {"state"},
	models.CodeTemplateHostChanged:            {},
}

// PublicErrorDetails returns the keys of details that ErrorDetailKeys
// lists for code, or nil when none is left.
func PublicErrorDetails(code string, details map[string]any) map[string]any {
	var out map[string]any
	for _, k := range ErrorDetailKeys[code] {
		if v, ok := details[k]; ok {
			if out == nil {
				out = map[string]any{}
			}
			out[k] = v
		}
	}
	return out
}
