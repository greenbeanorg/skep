package hostops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The template build jail (the step 6 decision on package scripts, item 2
// and the follow-up rulings). One persistent jail per build, named
// skep-build-<template-id>, on the template root: no network, no devfs,
// every allow.* off (allow.suser included, unlike an ordinary jail:
// amendment e), securelevel 2, no exec.* command. The fixed commands run
// one by one with jexec -l, without -U (the step 6 probe: -U root needs a
// pwd.db the template does not have yet, and initgroups, which allow.suser
// off forbids). The parameters are checked as the kernel reports them
// before any command runs.

const (
	jailBinary  = "/usr/sbin/jail"
	jexecBinary = "/usr/sbin/jexec"
	jlsBinary   = "/usr/sbin/jls"
)

// buildCommandTimeout bounds one build command; makewhatis of the base
// manual pages takes seconds on hearst.
const buildCommandTimeout = 10 * time.Minute

// BuildCommand is one of the fixed commands run in the build jail.
type BuildCommand int

// The build commands, in the order they run (the follow-up ruling).
const (
	BuildCertctl BuildCommand = iota + 1
	BuildPwdMkdb
	BuildServicesMkdb
	BuildCapMkdb
	BuildMakewhatisMan
	BuildMakewhatisOpenSSL
)

// BuildCommands is every build command, in order.
var BuildCommands = []BuildCommand{
	BuildCertctl, BuildPwdMkdb, BuildServicesMkdb, BuildCapMkdb, BuildMakewhatisMan, BuildMakewhatisOpenSSL,
}

// buildCommandSpec is a command's argument list inside the jail (paths
// relative to the jail root, which is / inside it) and, for makewhatis,
// the directory that must exist for it to apply.
type buildCommandSpec struct {
	argv     []string
	ifExists string
}

// buildCommandSpecs are the package scripts' commands with ${PKG_ROOTDIR}
// empty and PKG_METALOG unset (item 3). services_mkdb reads /etc/services,
// its documented default, not the package script's "${PKG_ROOTDIR}/",
// which is a directory and gives an empty database (the step 6 probe).
var buildCommandSpecs = map[BuildCommand]buildCommandSpec{
	BuildCertctl:           {argv: []string{"/usr/sbin/certctl", "rehash"}},
	BuildPwdMkdb:           {argv: []string{"/usr/sbin/pwd_mkdb", "-i", "-p", "-d", "/etc", "/etc/master.passwd"}},
	BuildServicesMkdb:      {argv: []string{"/usr/sbin/services_mkdb", "-l", "-q", "-o", "/var/db/services.db", "/etc/services"}},
	BuildCapMkdb:           {argv: []string{"/usr/bin/cap_mkdb", "-l", "/etc/login.conf"}},
	BuildMakewhatisMan:     {argv: []string{"/usr/bin/makewhatis", "/usr/share/man"}},
	BuildMakewhatisOpenSSL: {argv: []string{"/usr/bin/makewhatis", "/usr/share/openssl/man"}, ifExists: "usr/share/openssl/man"},
}

// String is the command line, for progress messages and errors.
func (c BuildCommand) String() string {
	if s, ok := buildCommandSpecs[c]; ok {
		return strings.Join(s.argv, " ")
	}
	return fmt.Sprintf("build command %d", int(c))
}

// buildJailAllow is every allow.* parameter of the 15.1 GENERIC kernel
// that is not a sub-parameter of allow.mount, each set to 0. allow.mount=0
// covers the allow.mount.* ones. Two of them, allow.reserved_ports and
// allow.unprivileged_proc_debug, default to on, which is why every one is
// listed. Parameters a module adds (allow.vmm, allow.vmm_ppt,
// allow.mount.zfs) default to off; checkBuildJail confirms every allow.*
// the kernel reports is off, whatever this list misses.
var buildJailAllow = []string{
	"adjtime", "chflags", "extattr", "mlock", "mount", "nfsd", "quotas", "raw_sockets", "read_msgbuf",
	"reserved_ports", "routing", "set_hostname", "setaudit", "settime", "socket_af", "suser", "sysvipc",
	"unprivileged_parent_tampering", "unprivileged_proc_debug",
}

// buildJailArgs is the fixed argument list of jail -c.
func buildJailArgs(name, root string) []string {
	args := []string{"-c",
		"name=" + name,
		"path=" + root,
		"host.hostname=skep-build",
		"persist",
		"ip4=disable",
		"ip6=disable",
		"enforce_statfs=2",
		"children.max=0",
		"securelevel=2",
		// No System V IPC, set rather than left to the kernel's default
		// (ADR 0010 amendment m), and checked by checkBuildJail.
		"sysvshm=disable",
		"sysvsem=disable",
		"sysvmsg=disable",
		"exec.clean",
	}
	for _, a := range buildJailAllow {
		args = append(args, "allow."+a+"=0")
	}
	return args
}

// StartBuildJail implements Templates.
func (h *host) StartBuildJail(ctx context.Context, pool, templateID string) error {
	name, err := BuildJailName(templateID)
	if err != nil {
		return err
	}
	if err := h.CheckTemplatesParent(ctx, pool); err != nil {
		return err
	}
	p, err := h.TemplatePaths(ctx, pool, templateID)
	if err != nil {
		return err
	}
	if _, err := h.run.Run(ctx, jailBinary, buildJailArgs(name, p.Root)...); err != nil {
		return err
	}
	params, err := h.jailParams(ctx, name)
	if err != nil {
		return err
	}
	return checkBuildJail(params, name, p.Root)
}

// jailParams reads every parameter of a jail as jls reports it in JSON:
// one flat object whose booleans are JSON booleans.
func (h *host) jailParams(ctx context.Context, name string) (map[string]any, error) {
	out, err := h.run.Run(ctx, jlsBinary, "--libxo=json", "-n", "-j", name)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Info struct {
			Jail []map[string]any `json:"jail"`
		} `json:"jail-information"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("hostops: jls: %w", err)
	}
	if len(doc.Info.Jail) != 1 {
		return nil, fmt.Errorf("hostops: jls -j %s reported %d jails", name, len(doc.Info.Jail))
	}
	return doc.Info.Jail[0], nil
}

// checkBuildJail confirms the build jail's parameters as the kernel
// reports them: the name and path, no network, no child jails, the
// statfs and securelevel limits, no System V IPC, and every allow.* off.
func checkBuildJail(params map[string]any, name, root string) error {
	want := map[string]string{
		"name": name, "path": root, "ip4": "disable", "ip6": "disable",
		"enforce_statfs": "2", "children.max": "0", "securelevel": "2", "vnet": "inherit",
		"sysvshm": "disable", "sysvsem": "disable", "sysvmsg": "disable",
	}
	var bad []string
	for k, v := range want {
		if got := fmt.Sprint(params[k]); got != v {
			bad = append(bad, fmt.Sprintf("%s=%s (want %s)", k, got, v))
		}
	}
	allows := 0
	for k, v := range params {
		if !strings.HasPrefix(k, "allow.") {
			continue
		}
		allows++
		if b, ok := v.(bool); !ok || b {
			bad = append(bad, fmt.Sprintf("%s=%v (want false)", k, v))
		}
	}
	if allows == 0 {
		bad = append(bad, "no allow.* parameters reported")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("hostops: the build jail %s does not have the required parameters: %s", name, strings.Join(bad, ", "))
	}
	return nil
}

// BuildJailExec implements Templates.
func (h *host) BuildJailExec(ctx context.Context, pool, templateID string, cmd BuildCommand) (bool, error) {
	spec, ok := buildCommandSpecs[cmd]
	if !ok {
		return false, invalid("invalid build command")
	}
	name, err := BuildJailName(templateID)
	if err != nil {
		return false, err
	}
	if spec.ifExists != "" {
		r, _, err := h.openTemplateRoot(ctx, pool, templateID)
		if err != nil {
			return false, err
		}
		exists, err := lstatDirChain(r, spec.ifExists)
		r.Close()
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}
	er, err := h.envRunner()
	if err != nil {
		return false, err
	}
	// -l: the command gets root's login-class environment inside the
	// jail, not the agent's. No -U: the step 6 probe.
	args := append([]string{"-l", name}, spec.argv...)
	_, err = er.RunEnv(ctx, EnvCommand{Path: jexecBinary, Args: args,
		Env: []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "LC_ALL=C"}, Timeout: buildCommandTimeout})
	return err == nil, err
}

// RemoveBuildJail implements Templates.
func (h *host) RemoveBuildJail(ctx context.Context, templateID string) error {
	name, err := BuildJailName(templateID)
	if err != nil {
		return err
	}
	exists, err := h.jailExists(ctx, name)
	if err != nil || !exists {
		return err
	}
	if _, err := h.run.Run(ctx, jailBinary, "-r", name); err != nil {
		return err
	}
	// jail -r returns once the jail is removed or dying; a jail with
	// nothing running goes at once. Confirm it.
	if exists, err := h.jailExists(ctx, name); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("hostops: the build jail %s is still there after jail -r", name)
	}
	return nil
}

// jailExists reports whether jls knows a jail by name.
func (h *host) jailExists(ctx context.Context, name string) (bool, error) {
	_, err := h.run.Run(ctx, jlsBinary, "-j", name, "jid")
	if err == nil {
		return true, nil
	}
	if stderrContains(err, "not found") {
		return false, nil
	}
	return false, err
}

// BuildJails implements Templates.
func (h *host) BuildJails(ctx context.Context) ([]string, error) {
	out, err := h.run.Run(ctx, jlsBinary, "--libxo=json", "name")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Info struct {
			Jail []struct {
				Name string `json:"name"`
			} `json:"jail"`
		} `json:"jail-information"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("hostops: jls: %w", err)
	}
	var ids []string
	for _, j := range doc.Info.Jail {
		if id, ok := strings.CutPrefix(j.Name, BuildJailPrefix); ok && ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
