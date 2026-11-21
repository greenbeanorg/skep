package models

import "time"

// Jail templates (ADR 0010 decision 7 and the step 6 decision on package
// scripts). A template is a FreeBSD userland built once by a task into
// <pool>/<root>/templates/<template-id> and snapshotted as @skep-base.

// Template tasks.
const (
	TaskTemplateCreate = "container.template.create"
	TaskTemplateDelete = "container.template.delete"
)

// TemplateResource is the resource type of the template tasks.
const TemplateResource = "container_template"

// Template sources.
const (
	// TemplateSourcePkgbase installs base packages from the host's own
	// pkgbase repository configuration.
	TemplateSourcePkgbase = "pkgbase"
	// TemplateSourceDist extracts an operator-placed base.txz checked
	// against its MANIFEST.
	TemplateSourceDist = "dist"
)

// Error codes of the template operations.
const (
	// CodeReleaseUnsupported: the template's userland is not the host's
	// release (major.minor), or no template can be built for the host's
	// release.
	CodeReleaseUnsupported = "release_unsupported"
	// CodeJailParentInsecure: the directory that holds the templates (or
	// the jail roots) is not root:wheel 0700.
	CodeJailParentInsecure = "jail_parent_insecure"
	// CodeTemplateScriptUnknown: a package script or a trigger file in
	// the new template is not on the compiled allowlist.
	CodeTemplateScriptUnknown = "template_script_unknown"
	// CodeTemplateTriggerLeft: pkg left a deferred trigger in the
	// template.
	CodeTemplateTriggerLeft = "template_trigger_left"
	// CodeTemplateHostChanged: a file under the host's /etc, /var/db/pkg,
	// or /usr/share/certs changed during the build.
	CodeTemplateHostChanged = "template_host_changed"
	// CodeTemplateNameTaken: another template has the name.
	CodeTemplateNameTaken = "template_name_taken"
	// CodeTemplateNotProvisioned: the template's build never finished;
	// it can only be deleted.
	CodeTemplateNotProvisioned = "template_not_provisioned"
	// CodeTemplateBuildFailed: a command of the build (pkg, tar, or a
	// build-jail command) ran and failed; the message has the first line
	// of its stderr.
	CodeTemplateBuildFailed = "template_build_failed"
	// CodeTemplateArchiveMismatch: base.txz does not match its MANIFEST.
	CodeTemplateArchiveMismatch = "template_archive_mismatch"
)

// Template is one jail template.
type Template struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Pool   string `json:"pool"`
	Source string `json:"source"`
	// Packages are the base packages installed (pkgbase).
	Packages []string `json:"packages"`
	// ArchiveSHA256 is the checked SHA-256 of base.txz (dist).
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	// Provisioned is false while the build runs, and after it failed or
	// was interrupted.
	Provisioned bool `json:"provisioned"`
	// Release is freebsd-version -u of the built userland, patch level
	// included.
	Release string `json:"release,omitempty"`
	// HostRelease is the host kernel's release when the template was
	// read. PatchDiffers is set when the two have the same major.minor
	// but a different patch level, in either direction.
	HostRelease  string `json:"host_release,omitempty"`
	PatchDiffers bool   `json:"patch_differs"`
	// HasSSHD reports whether the built template has sshd, by the same
	// check that refuses an SSH key with container_ssh_unavailable
	// (ADR 0010 amendment m). False while the template is not built.
	HasSSHD   bool      `json:"has_sshd"`
	UsedBytes int64     `json:"used_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// TemplateList is a page of templates, by name.
type TemplateList struct {
	Items      []Template `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// TemplateListOptions pages the template list.
type TemplateListOptions struct {
	Limit  int
	Cursor string
}

// TemplateCreate is the body of POST /storage/templates. Nothing in it is
// a package name, a URL, or a path: those come from the agent's
// configuration.
type TemplateCreate struct {
	Name   string `json:"name"`
	Pool   string `json:"pool"`
	Source string `json:"source"`
}

// TemplateCreated is the answer to a template request: the template, not
// yet provisioned, and the task that builds it.
type TemplateCreated struct {
	Template Template `json:"template"`
	Task     Task     `json:"task"`
}

// TemplateSourceState says whether a source can be used on this host.
type TemplateSourceState struct {
	Available bool `json:"available"`
	// Reason says why not.
	Reason string `json:"reason,omitempty"`
}

// TemplateSources is GET /storage/template-sources: what this host can
// build templates from.
type TemplateSources struct {
	// HostRelease is freebsd-version -k of the host.
	HostRelease string              `json:"host_release"`
	Pkgbase     TemplateSourceState `json:"pkgbase"`
	// Packages are storage.template_packages.
	Packages []string            `json:"packages"`
	Dist     TemplateSourceState `json:"dist"`
}
