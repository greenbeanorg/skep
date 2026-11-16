package templates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// cleanupTimeout bounds the clean-up after a failed, cancelled, or
// timed-out build. It runs on its own context, so a cancelled task still
// removes its build jail and its dataset.
const cleanupTimeout = 3 * time.Minute

// BuildSpec is one template build. Every value comes from the agent's
// configuration or a validated request; the paths are derived by hostops.
type BuildSpec struct {
	Pool       string
	TemplateID string
	Source     string
	// Packages are storage.template_packages (pkgbase).
	Packages []string
}

// BuildResult is what a finished build observed.
type BuildResult struct {
	Release     string `json:"release"`
	HostRelease string `json:"host_release"`
	// PatchDiffers is set when the template's patch level differs from
	// the host kernel's.
	PatchDiffers   bool     `json:"patch_differs"`
	ArchiveSHA256  string   `json:"archive_sha256,omitempty"`
	UsedBytes      int64    `json:"used_bytes"`
	Snapshot       string   `json:"snapshot"`
	Packages       int      `json:"packages_installed,omitempty"`
	ScriptsAllowed int      `json:"scripts_allowed,omitempty"`
	TriggerFiles   []string `json:"trigger_files,omitempty"`
	Commands       []string `json:"commands_run,omitempty"`
	// HostFiles is how many host files were compared before and after.
	HostFiles int `json:"host_files_compared"`
}

// Builder runs the build sequence of the step 6 decision.
type Builder struct {
	Host hostops.Templates
	Log  *slog.Logger
	// afterInstall, when set, runs after pkg install and before the
	// allowlist checks. Only this package's tests set it, to show that
	// a changed script or trigger file is refused.
	afterInstall func(ctx context.Context, p hostops.TemplatePaths) error
}

// Progress receives a message for each step.
type Progress func(message string)

func (b *Builder) log() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}
	return b.Log
}

// Build runs a whole build. On success the template has its @skep-base
// snapshot and is read-only, and no build jail or work area is left. On
// any failure, cancellation, or timeout, the build jail is removed (and
// its removal confirmed), and the template's dataset and work area are
// destroyed. A change to the host files fails the build with
// template_host_changed, whatever else happened.
func (b *Builder) Build(ctx context.Context, spec BuildSpec, progress Progress) (res BuildResult, err error) {
	if progress == nil {
		progress = func(string) {}
	}
	if !hostops.ValidID(spec.TemplateID) {
		return res, models.Invalid("invalid template ID")
	}
	var allow Allowlist
	switch spec.Source {
	case models.TemplateSourcePkgbase:
		if ok, why := b.Host.PkgbaseAvailable(); !ok {
			return res, sourceUnavailable(spec.Source, why)
		}
	case models.TemplateSourceDist:
		if ok, why := b.Host.DistAvailable(); !ok {
			return res, sourceUnavailable(spec.Source, why)
		}
	default:
		return res, models.Invalid("invalid template source")
	}
	if res.HostRelease, err = b.Host.HostRelease(ctx); err != nil {
		return res, err
	}
	hostRel, err := ParseRelease(res.HostRelease)
	if err != nil {
		return res, err
	}
	if spec.Source == models.TemplateSourcePkgbase {
		var ok bool
		if allow, ok = AllowlistFor(hostRel.MajorMinor()); !ok {
			return res, releaseUnsupported("Skep has no package-script allowlist for FreeBSD "+hostRel.MajorMinor()+
				", so it cannot build pkgbase templates on this host", "", res.HostRelease)
		}
	}

	progress("checking the templates directory")
	if err := b.Host.EnsureTemplatesParent(ctx, spec.Pool); err != nil {
		return res, err
	}
	progress("recording the host files")
	before, err := b.Host.HostFiles(ctx)
	if err != nil {
		return res, err
	}
	res.HostFiles = len(before)

	// From here on the host may hold something of this build: whatever
	// happens, clean it up and compare the host files again.
	jailStarted := false
	defer func() {
		if err == nil {
			return
		}
		err = b.cleanup(spec, jailStarted, before, err)
	}()

	progress("creating the template dataset")
	p, err := b.Host.PrepareTemplate(ctx, spec.Pool, spec.TemplateID)
	if err != nil {
		return res, err
	}

	var cmds []hostops.BuildCommand
	if spec.Source == models.TemplateSourcePkgbase {
		progress("installing the base packages (scripts and triggers off)")
		if err = b.Host.PkgInstall(ctx, spec.Pool, spec.TemplateID, spec.Packages); err != nil {
			return res, stepFailed("pkg install", err)
		}
		if b.afterInstall != nil {
			if err = b.afterInstall(ctx, p); err != nil {
				return res, err
			}
		}
		progress("checking the package scripts against the allowlist")
		pkgs, err := b.Host.PkgScripts(ctx, spec.Pool, spec.TemplateID)
		if err != nil {
			return res, stepFailed("pkg info --raw", err)
		}
		res.Packages = len(pkgs)
		fromScripts, err := allow.CheckScripts(pkgs)
		if err != nil {
			return res, err
		}
		for _, pk := range pkgs {
			res.ScriptsAllowed += len(pk.Scripts)
			for _, l := range pk.LuaScripts {
				res.ScriptsAllowed += len(l)
			}
		}
		progress("checking the trigger files against the allowlist")
		triggers, err := b.Host.TriggerFiles(ctx, spec.Pool, spec.TemplateID)
		if err != nil {
			return res, err
		}
		fromTriggers, err := allow.CheckTriggers(triggers)
		if err != nil {
			return res, err
		}
		for _, t := range triggers {
			res.TriggerFiles = append(res.TriggerFiles, t.Path)
		}
		cmds = orderCommands(fromScripts, fromTriggers)
	} else {
		progress("checking base.txz against its MANIFEST and extracting it")
		if res.ArchiveSHA256, err = b.Host.ExtractDist(ctx, spec.Pool, spec.TemplateID); err != nil {
			return res, stepFailed("tar -x of base.txz", err)
		}
	}

	progress("checking the template's release")
	if res.Release, err = b.Host.TemplateRelease(ctx, spec.Pool, spec.TemplateID); err != nil {
		return res, err
	}
	if err = checkRelease(res.Release, res.HostRelease); err != nil {
		return res, err
	}
	res.PatchDiffers = PatchDiffers(res.Release, res.HostRelease)

	if len(cmds) > 0 {
		progress("starting the build jail")
		jailStarted = true
		if err = b.Host.StartBuildJail(ctx, spec.Pool, spec.TemplateID); err != nil {
			return res, err
		}
		for _, c := range cmds {
			progress("running " + c.String() + " in the build jail")
			ran, err := b.Host.BuildJailExec(ctx, spec.Pool, spec.TemplateID, c)
			if err != nil {
				return res, commandFailed(c, err)
			}
			if ran {
				res.Commands = append(res.Commands, c.String())
			}
		}
		progress("removing the build jail")
		if err = b.Host.RemoveBuildJail(ctx, spec.TemplateID); err != nil {
			return res, err
		}
		jailStarted = false
	}

	progress("checking for deferred triggers")
	left, err := b.Host.DeferredTriggers(ctx, spec.Pool, spec.TemplateID)
	if err != nil {
		return res, err
	}
	if len(left) > 0 {
		return res, &models.Error{Code: models.CodeTemplateTriggerLeft,
			Message: "pkg left deferred triggers in the template", Details: map[string]any{"files": left}}
	}
	progress("removing the repository catalogues and checking the package cache")
	if err = b.Host.RemoveCatalogues(ctx, spec.Pool, spec.TemplateID); err != nil {
		return res, err
	}
	cache, err := b.Host.CacheEntries(ctx, spec.Pool, spec.TemplateID)
	if err != nil {
		return res, err
	}
	if len(cache) > 0 {
		return res, fmt.Errorf("templates: the template's var/cache/pkg is not empty: %v", cache)
	}

	progress("comparing the host files")
	if err = b.compareHost(ctx, before, nil); err != nil {
		return res, err
	}
	progress("removing the work area")
	if err = b.Host.RemoveWorkArea(ctx, spec.Pool, spec.TemplateID); err != nil {
		return res, err
	}
	progress("taking @" + hostops.BaseSnapshot)
	d, err := b.Host.FinishTemplate(ctx, spec.Pool, spec.TemplateID)
	if err != nil {
		return res, err
	}
	res.UsedBytes = d.UsedBytes
	res.Snapshot = d.Name + "@" + hostops.BaseSnapshot
	return res, nil
}

func sourceUnavailable(source, why string) error {
	return &models.Error{Code: models.CodeStorageFeatureUnsupported,
		Message: "the " + source + " template source is not available on this host: " + why,
		Details: map[string]any{"source": source}}
}

// commandFailed turns a failed build command into the task error: the
// command and the first line of its stderr.
func commandFailed(c hostops.BuildCommand, err error) error {
	return stepFailed(c.String()+" in the build jail", err)
}

// stepFailed turns a command of the build that ran and failed into
// template_build_failed, with the first line of its stderr, so the
// operator sees why in the task. Any other error is returned as it is.
func stepFailed(step string, err error) error {
	var ce *hostops.CommandError
	if !errors.As(err, &ce) {
		return err
	}
	msg := fmt.Sprintf("%s failed (exit %d)", step, ce.ExitCode)
	if ce.Stderr != "" {
		msg += ": " + ce.Stderr
	}
	return &models.Error{Code: models.CodeTemplateBuildFailed, Message: msg,
		Details: map[string]any{"step": step, "exit_code": ce.ExitCode, "stderr": ce.Stderr}}
}

// compareHost records the host files again and fails with
// template_host_changed if anything differs. cause, when set, is the
// failure that ended the build, and is kept in the details.
func (b *Builder) compareHost(ctx context.Context, before hostops.HostFileState, cause error) error {
	after, err := b.Host.HostFiles(ctx)
	if err != nil {
		return err
	}
	changed := before.Changed(after)
	if len(changed) == 0 {
		return nil
	}
	b.log().Error("templates: the host files changed during a template build; the change is reported, not repaired",
		"event", "template_host_changed", "paths", changed)
	details := map[string]any{"paths": changed}
	msg := fmt.Sprintf("%d host files under /etc, /var/db/pkg, or /usr/share/certs changed during the build; Skep reports this and does not repair it", len(changed))
	if cause != nil {
		details["build_error"] = cause.Error()
	}
	return &models.Error{Code: models.CodeTemplateHostChanged, Message: msg, Details: details}
}

// cleanup undoes a failed build on its own context: the build jail, the
// dataset, and the work area. It then compares the host files, so a
// failed build that changed the host is still reported as such.
func (b *Builder) cleanup(spec BuildSpec, jailStarted bool, before hostops.HostFileState, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	log := b.log().With("template_id", spec.TemplateID)
	var problems []error
	// The jail is removed even when its start reported an error: jail -c
	// may have created it before a later check failed.
	if jailStarted {
		if err := b.Host.RemoveBuildJail(ctx, spec.TemplateID); err != nil {
			log.Error("templates: the build jail could not be removed", "error", err)
			problems = append(problems, fmt.Errorf("the build jail could not be removed: %w", err))
		}
	}
	if _, err := b.Host.DestroyTemplate(ctx, spec.Pool, spec.TemplateID); err != nil {
		log.Error("templates: the template dataset could not be destroyed", "error", err)
		problems = append(problems, fmt.Errorf("the template could not be destroyed: %w", err))
	}
	if err := b.compareHost(ctx, before, cause); err != nil {
		return err
	}
	if len(problems) == 0 {
		return cause
	}
	// The clean-up failing (a jail that will not go, a dataset that will
	// not be destroyed) is reported in the task, beside the cause.
	joined := errors.Join(problems...).Error()
	var me *models.Error
	if errors.As(cause, &me) {
		out := *me
		out.Details = map[string]any{"cleanup_error": joined}
		for k, v := range me.Details {
			out.Details[k] = v
		}
		out.Message = me.Message + "; the clean-up also failed: " + joined
		return &out
	}
	return &models.Error{Code: models.CodeTemplateBuildFailed,
		Message: "the build failed and its clean-up failed too: " + joined,
		Details: map[string]any{"cleanup_error": joined}}
}
