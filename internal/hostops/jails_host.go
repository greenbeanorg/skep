package hostops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// jailCommandSlack bounds jail -c, which runs nothing in the jail
// (amendment i), and is added to a jail's own timeouts to bound jail -r:
// mounting devfs and the kernel's removal take a moment.
const jailCommandSlack = 30 * time.Second

// jailRCArgs runs a jail's /etc/rc after the /dev guard (amendment i):
// as the jail's root, in the clean login environment exec.clean gave
// before. jexec is never reachable through the API (decision 12).
func jailRCArgs(name string) []string {
	return []string{"-l", "-U", "root", name, "/bin/sh", "/etc/rc"}
}

// jailEnv is the whole environment of jail(8). exec.clean gives the
// commands inside the jail a clean one of their own.
var jailEnv = []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "LC_ALL=C"}

// EnsureJailsParent implements Jails.
func (h *host) EnsureJailsParent(ctx context.Context, pool string) error {
	name, err := h.conf.JailsDataset(pool)
	if err != nil {
		return err
	}
	return h.ensurePrivateParent(ctx, pool, name)
}

// CheckJailsParent implements Jails.
func (h *host) CheckJailsParent(ctx context.Context, pool string) error {
	name, err := h.conf.JailsDataset(pool)
	if err != nil {
		return err
	}
	return h.checkPrivateParent(ctx, name)
}

// jailsMount is the mountpoint of <root>/jail.
func (h *host) jailsMount(ctx context.Context, pool string) (string, error) {
	name, err := h.conf.JailsDataset(pool)
	if err != nil {
		return "", err
	}
	return h.privateMount(ctx, name)
}

// ownedJail returns a jail's dataset, nil if it does not exist, and
// refuses one that does not carry the container's ID or has children.
func (h *host) ownedJail(ctx context.Context, ds, containerID string) (*Dataset, error) {
	list, err := h.listDatasets(ctx, ds, true)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	for _, d := range list {
		if d.Name != ds {
			return nil, fmt.Errorf("hostops: refusing %s: a jail root has no child datasets", d.Name)
		}
		if d.ContainerID != containerID {
			return nil, fmt.Errorf("hostops: refusing %s: its %s property does not name this container", d.Name, propContainerID)
		}
	}
	return &list[0], nil
}

// CreateJailRoot implements Jails.
func (h *host) CreateJailRoot(ctx context.Context, pool, templateID, containerID string) (Dataset, error) {
	ds, err := h.conf.JailDataset(pool, containerID)
	if err != nil {
		return Dataset{}, err
	}
	tds, err := h.conf.TemplateDataset(pool, templateID)
	if err != nil {
		return Dataset{}, err
	}
	if err := h.zfsLoaded(); err != nil {
		return Dataset{}, err
	}
	if err := h.CheckJailsParent(ctx, pool); err != nil {
		return Dataset{}, err
	}
	mp, err := h.jailsMount(ctx, pool)
	if err != nil {
		return Dataset{}, err
	}
	t, err := h.ownedTemplate(ctx, tds, templateID)
	if err != nil {
		return Dataset{}, err
	}
	if t == nil {
		return Dataset{}, notFound("template dataset " + tds)
	}
	if err := checkUnlocked([]Dataset{*t}); err != nil {
		return Dataset{}, err
	}
	origin := tds + "@" + baseSnapshot
	snaps, err := h.listSnapshots(ctx, tds, false)
	if err != nil {
		return Dataset{}, err
	}
	found := false
	for _, sn := range snaps {
		if sn.Name == origin && sn.TemplateID == templateID {
			found = true
		}
	}
	if !found {
		return Dataset{}, &models.Error{Code: models.CodeTemplateNotProvisioned,
			Message: "the template has no " + baseSnapshot + " snapshot carrying its ID; it was never finished",
			Details: map[string]any{"template_id": templateID}}
	}
	if existing, err := h.listDatasets(ctx, ds, false); err != nil {
		return Dataset{}, err
	} else if len(existing) > 0 {
		return Dataset{}, fmt.Errorf("hostops: the jail dataset %s already exists", ds)
	}
	// readonly=off is set, not inherited: the template is readonly=on,
	// and a clone must be writable whatever <root> says.
	if _, err := h.run.Run(ctx, zfsBinary, "clone", "-o", "canmount=on", "-o", "readonly=off",
		"-o", propContainerID+"="+containerID, origin, ds); err != nil {
		return Dataset{}, err
	}
	mounted, got, err := h.mountInfo(ctx, ds)
	if err != nil {
		return Dataset{}, err
	}
	if want := filepath.Join(mp, containerID); !mounted || got != want {
		return Dataset{}, storageUnavailable("the jail dataset " + ds + " is not mounted at " + want)
	}
	return h.oneDataset(ctx, ds)
}

// CloneJail implements Jails. When it fails after zfs clone, what it made
// carries the target's ID, so DestroyJailRoot of the target removes it
// without touching the source.
func (h *host) CloneJail(ctx context.Context, pool, sourceID, snapshotID, targetID string) (Dataset, error) {
	short, err := SnapshotName(snapshotID)
	if err != nil {
		return Dataset{}, err
	}
	if sourceID == targetID {
		return Dataset{}, invalid("a clone needs a new container ID")
	}
	target, err := h.conf.JailDataset(pool, targetID)
	if err != nil {
		return Dataset{}, err
	}
	src, err := h.conf.JailDataset(pool, sourceID)
	if err != nil {
		return Dataset{}, err
	}
	if err := h.zfsLoaded(); err != nil {
		return Dataset{}, err
	}
	if err := h.CheckJailsParent(ctx, pool); err != nil {
		return Dataset{}, err
	}
	mp, err := h.jailsMount(ctx, pool)
	if err != nil {
		return Dataset{}, err
	}
	// The source root, owned and without children, and the snapshot on it.
	d, err := h.ownedJail(ctx, src, sourceID)
	if err != nil {
		return Dataset{}, err
	}
	if d == nil {
		return Dataset{}, notFound("jail dataset " + src)
	}
	if err := checkUnlocked([]Dataset{*d}); err != nil {
		return Dataset{}, err
	}
	snaps, err := h.listSnapshots(ctx, src, false)
	if err != nil {
		return Dataset{}, err
	}
	if _, err := complete([]Dataset{*d}, indexSnapshots(snaps), snapshotID); err != nil {
		return Dataset{}, err
	}
	if existing, err := h.listDatasets(ctx, target, false); err != nil {
		return Dataset{}, err
	} else if len(existing) > 0 {
		return Dataset{}, fmt.Errorf("hostops: the clone's dataset %s already exists", target)
	}
	// readonly=off and canmount=on are set, not inherited, as for a jail
	// made from a template.
	if _, err := h.run.Run(ctx, zfsBinary, "clone", "-o", "canmount=on", "-o", "readonly=off",
		"-o", propContainerID+"="+targetID, src+"@"+short, target); err != nil {
		return Dataset{}, err
	}
	mounted, got, err := h.mountInfo(ctx, target)
	if err != nil {
		return Dataset{}, err
	}
	if want := filepath.Join(mp, targetID); !mounted || got != want {
		return Dataset{}, storageUnavailable("the clone's dataset " + target + " is not mounted at " + want)
	}
	return h.oneDataset(ctx, target)
}

// JailRoot implements Jails.
func (h *host) JailRoot(ctx context.Context, pool, containerID string) (string, error) {
	ds, err := h.conf.JailDataset(pool, containerID)
	if err != nil {
		return "", err
	}
	if err := h.zfsLoaded(); err != nil {
		return "", err
	}
	mp, err := h.jailsMount(ctx, pool)
	if err != nil {
		return "", err
	}
	d, err := h.ownedJail(ctx, ds, containerID)
	if err != nil {
		return "", err
	}
	if d == nil {
		return "", notFound("jail dataset " + ds)
	}
	if d.Locked() {
		return "", checkUnlocked([]Dataset{*d})
	}
	want := filepath.Join(mp, containerID)
	mounted, got, err := h.mountInfo(ctx, ds)
	if err != nil {
		return "", err
	}
	if !mounted || got != want {
		return "", storageUnavailable("the jail dataset " + ds + " is not mounted at " + want)
	}
	return want, nil
}

func containerNotStopped(containerID string) error {
	return &models.Error{Code: models.CodeContainerNotStopped, Message: "the jail is running",
		Details: map[string]any{"container_id": containerID}}
}

// DestroyJailRoot implements Jails. It never uses -R.
func (h *host) DestroyJailRoot(ctx context.Context, pool, containerID string) ([]Dataset, error) {
	ds, err := h.conf.JailDataset(pool, containerID)
	if err != nil {
		return nil, err
	}
	name, err := JailName(containerID)
	if err != nil {
		return nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return nil, err
	}
	if running, err := h.jailExists(ctx, name); err != nil {
		return nil, err
	} else if running {
		return nil, containerNotStopped(containerID)
	}
	// Rules a jail removed outside Skep left on its name go with it.
	if err := h.clearJailRules(ctx, containerID); err != nil {
		return nil, err
	}
	d, err := h.ownedJail(ctx, ds, containerID)
	if err != nil || d == nil {
		return nil, err
	}
	snaps, err := h.listSnapshots(ctx, ds, false)
	if err != nil {
		return nil, err
	}
	if clones := clonesOf(snaps); len(clones) > 0 {
		return nil, hasClones(clones)
	}
	// A devfs a removal outside Skep left on <root>/dev keeps the dataset
	// busy (amendment l).
	mp, err := h.jailsMount(ctx, pool)
	if err != nil {
		return nil, err
	}
	if _, err := h.removeLeftoverDevfs(containerID, filepath.Join(mp, containerID)); err != nil {
		return nil, err
	}
	if _, err := h.run.Run(ctx, zfsBinary, "destroy", "-r", ds); err != nil {
		return nil, err
	}
	return append([]Dataset{*d}, snaps...), nil
}

// ConfigureJail implements Jails.
func (h *host) ConfigureJail(ctx context.Context, pool, containerID string, files JailFiles) error {
	name, err := JailName(containerID)
	if err != nil {
		return err
	}
	content, err := files.render()
	if err != nil {
		return err
	}
	if running, err := h.jailExists(ctx, name); err != nil {
		return err
	} else if running {
		return containerNotStopped(containerID)
	}
	root, err := h.JailRoot(ctx, pool, containerID)
	if err != nil {
		return err
	}
	return writeJailFiles(root, content)
}

// TemplateHasSSHD implements Jails.
func (h *host) TemplateHasSSHD(ctx context.Context, pool, templateID string) (bool, error) {
	r, _, err := h.openTemplateRoot(ctx, pool, templateID)
	if err != nil {
		return false, err
	}
	defer r.Close()
	exists, err := lstatDirChain(r, "usr/sbin")
	if err != nil || !exists {
		return false, err
	}
	fi, err := r.Lstat("usr/sbin/sshd")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.Mode().IsRegular(), nil
}

// prepareJailRun creates the runtime directory of a jail, root-only.
func (h *host) prepareJailRun(containerID string) error {
	conf, err := h.conf.JailRunPath(containerID, JailConfFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.conf.JailRunDir, 0o700); err != nil {
		return fmt.Errorf("hostops: create the jail runtime directory: %w", err)
	}
	if err := checkRootOnlyDir(h.conf.JailRunDir); err != nil {
		return err
	}
	if err := mkdirDurable(filepath.Dir(conf)); err != nil {
		return fmt.Errorf("hostops: create the jail runtime directory: %w", err)
	}
	return checkRootOnlyDir(filepath.Dir(conf))
}

// checkRootOnlyDir refuses a runtime directory that is a link or that
// someone besides root could write: jail(8) reads the jail.conf in it as
// root.
func checkRootOnlyDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("hostops: %s is not a directory", dir)
	}
	if uid, ok := ownerUID(fi); !ok || uid != uint32(os.Geteuid()) {
		return fmt.Errorf("hostops: %s is not owned by the agent's user", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("hostops: %s can be written by others (mode %04o)", dir, fi.Mode().Perm())
	}
	return nil
}

// writeJailConf renders and writes a jail's jail.conf, and returns its
// path and the jail root.
func (h *host) writeJailConf(ctx context.Context, spec JailSpec, withStop bool) (conf, root string, err error) {
	if root, err = h.JailRoot(ctx, spec.Pool, spec.ContainerID); err != nil {
		return "", "", err
	}
	conf, err = h.writeJailConfAt(spec, root, withStop)
	return conf, root, err
}

// writeJailConfAt writes the jail.conf of a jail whose root is known;
// withStop false leaves out exec.stop (amendment i).
func (h *host) writeJailConfAt(spec JailSpec, root string, withStop bool) (string, error) {
	if err := h.prepareJailRun(spec.ContainerID); err != nil {
		return "", err
	}
	conf, err := h.conf.JailRunPath(spec.ContainerID, JailConfFile)
	if err != nil {
		return "", err
	}
	data, err := renderJailConfMode(spec, root, withStop)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(conf, data); err != nil {
		return "", err
	}
	return conf, nil
}

// StartJail implements Jails.
func (h *host) StartJail(ctx context.Context, spec JailSpec) (JailState, error) {
	name, err := JailName(spec.ContainerID)
	if err != nil {
		return JailState{}, err
	}
	if err := h.CheckJailsParent(ctx, spec.Pool); err != nil {
		return JailState{}, err
	}
	if running, err := h.jailExists(ctx, name); err != nil {
		return JailState{}, err
	} else if running {
		return JailState{}, containerNotStopped(spec.ContainerID)
	}
	if err := h.checkJailNet(ctx, spec); err != nil {
		return JailState{}, err
	}
	// A jail never runs without its limits; RCTL rules an earlier run
	// left on the name go first (decision 11).
	if err := h.checkRctlForStart(ctx, spec); err != nil {
		return JailState{}, err
	}
	// What a crash left: the epairs of an earlier run (decision 10).
	if gone, err := h.DestroyJailInterfaces(ctx, spec.ContainerID); err != nil {
		return JailState{}, err
	} else if len(gone) > 0 {
		h.log.Warn("hostops: destroyed epairs an earlier run of the jail left", "event", "orphaned_interface_removed",
			"container_id", spec.ContainerID, "interfaces", gone)
	}
	if err := h.checkMACsFree(ctx, spec); err != nil {
		return JailState{}, err
	}
	conf, root, err := h.writeJailConf(ctx, spec, true)
	if err != nil {
		return JailState{}, err
	}
	// And a devfs a removal outside Skep left on <root>/dev, which jail
	// -c would mount over (amendment l).
	if _, err := h.removeLeftoverDevfs(spec.ContainerID, root); err != nil {
		return JailState{}, err
	}
	epairs := map[int]string{}
	for _, nic := range spec.NICs {
		host, err := h.createJailEpair(ctx, spec.ContainerID, nic)
		if host != "" {
			epairs[nic.Slot] = host
		}
		if err != nil {
			h.cleanJailNet(spec)
			var me *models.Error
			if errors.As(err, &me) && me.Code == models.CodePortFilterFailed {
				if me.Details == nil {
					me.Details = map[string]any{}
				}
				me.Details["container_id"] = spec.ContainerID
				me.Details["slot"] = nic.Slot
				return JailState{}, me
			}
			return JailState{}, &models.Error{Code: models.CodeContainerStartFailed,
				Message: "the jail's network could not be set up: " + err.Error(), Details: map[string]any{"container_id": spec.ContainerID}}
		}
	}
	er, err := h.envRunner()
	if err != nil {
		h.cleanJailNet(spec)
		return JailState{}, err
	}
	// The jail.conf has no exec.start: jail -c creates the jail, mounts
	// its devfs, and moves its interfaces in, and nothing runs in it.
	if err := createJail(ctx, er, conf, name); err != nil {
		if rerr := h.discardAfterFailedStart(spec, root); rerr != nil {
			return JailState{}, quarantined(spec, "jail(8) could not create the jail: "+err.Error(), rerr)
		}
		return JailState{}, startFailed(spec, "jail(8) could not create the jail: ", err)
	}
	jid, err := h.finishStart(ctx, spec, name, conf, root)
	if err != nil {
		return JailState{}, err
	}
	return JailState{JID: jid, Name: name, Path: root, Epairs: epairs}, nil
}

// createJail runs jail -c on a rendered jail.conf. A failure's stderr
// line is cleaned before it reaches the task error (amendment r).
func createJail(ctx context.Context, er EnvRunner, conf, name string) error {
	_, err := er.RunEnv(ctx, EnvCommand{Path: jailBinary, Args: []string{"-f", conf, "-c", name},
		Env: jailEnv, Timeout: jailCommandSlack})
	return cleanStderr(err)
}

// startFailed is container_start_failed; a refusal that already is one
// keeps its message and details.
func startFailed(spec JailSpec, prefix string, err error) error {
	var me *models.Error
	if errors.As(err, &me) && me.Code == models.CodeContainerStartFailed {
		return me
	}
	return &models.Error{Code: models.CodeContainerStartFailed, Message: prefix + err.Error(),
		Details: map[string]any{"container_id": spec.ContainerID}}
}

// finishStart takes a jail jail -c just created, with nothing running in
// it, through the checks and then /etc/rc (amendment i): its parameters,
// its interfaces moved in and renamed to skep<slot> (amendment k), and its
// /dev against the allow-list. A failed
// check removes the jail without exec.stop, since none of its code has
// run. Then /etc/rc runs through jexec, bounded by the start timeout, its
// output on a pipe the agent drains into console.log (truncated first;
// capped at ConsoleLogHead plus ConsoleLogTail) and closes when rc ends
// (amendment o, SR-01); if it fails or times out, the jail is removed with
// jail -r, which runs exec.stop. It returns the JID.
func (h *host) finishStart(ctx context.Context, spec JailSpec, name, conf, root string) (int, error) {
	err := h.applyJailRules(ctx, spec)
	var params map[string]any
	if err == nil {
		params, err = h.jailParams(ctx, name)
	}
	if err == nil {
		err = checkJailParams(params, spec, root)
	}
	if err == nil {
		err = h.checkMovedIn(ctx, spec)
	}
	if err == nil {
		err = h.renameInJail(ctx, spec, name)
	}
	if err == nil {
		err = h.checkJailDevfs(spec, root)
	}
	if err != nil {
		if rerr := h.discardAfterFailedStart(spec, root); rerr != nil {
			return 0, quarantined(spec, err.Error(), rerr)
		}
		return 0, startFailed(spec, "the jail was removed before any of its code ran: ", err)
	}
	logPath, err := h.conf.JailRunPath(spec.ContainerID, JailConsoleLog)
	if err == nil {
		var er EnvRunner
		if er, err = h.envRunner(); err == nil {
			_, err = er.RunEnv(ctx, EnvCommand{Path: jexecBinary, Args: jailRCArgs(name), Env: jailEnv,
				Timeout: spec.StartTimeout, ConsoleLog: logPath})
		}
	}
	if err != nil {
		h.log.Warn("hostops: /etc/rc failed in a jail; removing it", "event", "jail_rc_failed",
			"container_id", spec.ContainerID, "error", err)
		h.removeAfterFailedStart(name, conf, root, spec)
		return 0, startFailed(spec, "the jail's /etc/rc failed or outlasted the start timeout, so the jail was stopped and removed (its output is in the console log): ", err)
	}
	jid, _ := strconv.Atoi(fmt.Sprint(params["jid"]))
	return jid, nil
}

// checkJailDevfs is the /dev guard (amendment i): the jail's devfs,
// listed from the host before any process has run in the jail, may hold
// only the allow-listed entries.
func (h *host) checkJailDevfs(spec JailSpec, root string) error {
	entries, err := h.listDev(root)
	if err != nil {
		return &models.Error{Code: models.CodeContainerStartFailed,
			Message: "the jail's /dev could not be checked, so it was removed before any of its code ran: " + err.Error(),
			Details: map[string]any{"container_id": spec.ContainerID}}
	}
	dhcp := spec.devfsRuleset() == JailDHCPDevfsRuleset
	if err := checkJailDev(spec.ContainerID, entries, dhcp); err != nil {
		h.log.Error("hostops: a jail's /dev holds more than the allow-list; removing it before any of its code runs",
			"event", "jail_devfs_refused", "container_id", spec.ContainerID, "ruleset", spec.devfsRuleset(), "error", err)
		return err
	}
	sort.Strings(entries)
	h.log.Info("hostops: the jail's /dev holds only the allow-list", "event", "jail_devfs_checked",
		"container_id", spec.ContainerID, "ruleset", spec.devfsRuleset(), "entries", strings.Join(entries, " "))
	return nil
}

// checkJailNet checks what a jail's NICs need before anything is
// created: if_epair, the netgraph modules of the port filters (ADR 0011),
// a usable allow-listed bridge for each, and, for DHCP,
// Skep's devfs ruleset (added when absent).
func (h *host) checkJailNet(ctx context.Context, spec JailSpec) error {
	if len(spec.NICs) == 0 {
		return nil
	}
	if err := checkNICs(spec.NICs); err != nil {
		return err
	}
	if err := h.EpairAvailable(ctx); err != nil {
		return err
	}
	if err := h.PortFilterAvailable(ctx); err != nil {
		return err
	}
	for _, nic := range spec.NICs {
		b, err := h.Bridge(ctx, nic.Bridge)
		if err != nil {
			return err
		}
		if err := checkBridgeUsable(h.conf, b); err != nil {
			return err
		}
	}
	if spec.devfsRuleset() == JailDHCPDevfsRuleset {
		return h.ensureDHCPRuleset(ctx)
	}
	return nil
}

// checkMovedIn confirms that every jail end left the host's stack.
func (h *host) checkMovedIn(ctx context.Context, spec JailSpec) error {
	for _, nic := range spec.NICs {
		jif, err := JailInterfaceName(spec.ContainerID, nic.Slot)
		if err != nil {
			return err
		}
		if exists, err := h.interfaceExists(ctx, jif); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("hostops: the interface %s is still on the host after jail -c", jif)
		}
	}
	return nil
}

// renameInJail gives each jail end, moved in by jail -c, its guest name
// skep<slot> (amendment k), with the host's ifconfig run in the jail's
// network stack: fixed arguments, and none of the jail's code runs. It
// runs before the /dev guard and /etc/rc, so nothing in the jail has
// had a chance to rename anything.
func (h *host) renameInJail(ctx context.Context, spec JailSpec, name string) error {
	for _, nic := range spec.NICs {
		jif, err := JailInterfaceName(spec.ContainerID, nic.Slot)
		if err != nil {
			return err
		}
		guest, err := GuestInterfaceName(nic.Slot)
		if err != nil {
			return err
		}
		out, err := h.run.Run(ctx, ifconfigBinary, "-j", name, jif, "name", guest)
		if err != nil {
			return fmt.Errorf("hostops: rename %s to %s inside the jail: %w", jif, guest, err)
		}
		// ifconfig prints the interface's new name.
		if got := strings.TrimSpace(string(out)); got != guest {
			return fmt.Errorf("hostops: renaming %s inside the jail gave %q, not %s", jif, printable(got), guest)
		}
	}
	return nil
}

// cleanJailNet destroys a jail's epairs after a failed start, on its own
// short context so a cancelled request still cleans up. A failure is
// logged; the epairs are then orphans, reported and removed by the next
// start, stop, or delete.
func (h *host) cleanJailNet(spec JailSpec) {
	if len(spec.NICs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jailCommandSlack)
	defer cancel()
	if _, err := h.DestroyJailInterfaces(ctx, spec.ContainerID); err != nil {
		h.log.Error("hostops: the epairs of a failed start could not be destroyed", "container_id", spec.ContainerID, "error", err)
	}
}

// discardAfterFailedStart removes a jail none of whose code has run,
// without exec.stop, and then its epairs, on its own short context so a
// cancelled request still cleans up. It returns the error when the jail
// could not be removed: the caller then reports the jail quarantined
// (amendment n, SR-06). Epairs that could not be destroyed are logged;
// they are orphans, removed by the next start, stop, or delete.
func (h *host) discardAfterFailedStart(spec JailSpec, root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), spec.StopTimeout+spec.StartTimeout+jailCommandSlack)
	defer cancel()
	if err := h.discardJail(ctx, spec, root); err != nil {
		h.log.Error("hostops: a jail refused before any of its code ran could not be removed; it is quarantined",
			"event", "jail_quarantined", "container_id", spec.ContainerID, "error", err)
		return err
	}
	h.cleanJailNet(spec)
	return nil
}

// quarantined is container_quarantined: a start refused the jail before
// any of its code ran, and the removal without exec.stop then failed
// (amendment n, SR-06). The jail may still exist with the /dev or the
// parameters the check refused; nothing of it may run, its
// /etc/rc.shutdown included.
func quarantined(spec JailSpec, refusal string, removal error) error {
	return &models.Error{Code: models.CodeContainerQuarantined,
		Message: "the start refused the jail before any of its code ran (" + refusal + "), and removing it then failed (" +
			removal.Error() + "); the jail is quarantined: Skep will not start or stop it or run its /etc/rc.shutdown, " +
			"and a delete or the next agent start removes it without running any of its code",
		Details: map[string]any{"container_id": spec.ContainerID}}
}

// discardJail removes a jail with a jail.conf without exec.stop, so none
// of the jail's code runs: SIGTERM, then SIGKILL, then devfs unmounted.
func (h *host) discardJail(ctx context.Context, spec JailSpec, root string) error {
	name, err := JailName(spec.ContainerID)
	if err != nil {
		return err
	}
	exists, err := h.jailExists(ctx, name)
	if err != nil || !exists {
		return err
	}
	conf, err := h.writeJailConfAt(spec, root, false)
	if err != nil {
		return err
	}
	return h.removeJail(ctx, name, conf, root, spec, false)
}

// DiscardJail implements Jails.
func (h *host) DiscardJail(ctx context.Context, spec JailSpec) error {
	name, err := JailName(spec.ContainerID)
	if err != nil {
		return err
	}
	exists, err := h.jailExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		root, err := h.JailRoot(ctx, spec.Pool, spec.ContainerID)
		if err != nil {
			return err
		}
		if err := h.discardJail(ctx, spec, root); err != nil {
			return err
		}
	}
	_, err = h.DestroyJailInterfaces(ctx, spec.ContainerID)
	return err
}

// removeAfterFailedStart removes a jail whose /etc/rc failed, with
// exec.stop, and then its epairs, on its own short context so a cancelled
// request still cleans up. A failure is logged; the jail then shows as
// running and can be stopped.
func (h *host) removeAfterFailedStart(name, conf, root string, spec JailSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), spec.StopTimeout+spec.StartTimeout+jailCommandSlack)
	defer cancel()
	exists, err := h.jailExists(ctx, name)
	if err != nil {
		return
	}
	if exists {
		if err := h.removeJail(ctx, name, conf, root, spec, true); err != nil {
			h.log.Error("hostops: a jail left by a failed start could not be removed", "jail", name, "error", err)
			return
		}
	}
	h.cleanJailNet(spec)
}

// removeJail destroys the jail's epairs, then runs jail -r with the
// jail's own jail.conf (whose exec.stop, if any, runs) and confirms the
// jail is gone. withStop says the jail.conf has exec.stop: then the
// output jail -r returned (/etc/rc.shutdown's) is appended to the console
// log (amendment p). root is the jail's root, for the leftover devfs a
// forced removal leaves.
//
// The epairs go first (amendment k, "or its epair destroyed"): destroying
// a host end destroys its jail end inside the running jail, so no jail end
// returns to the host on a removal Skep makes. Otherwise every jail's end
// would return named skep<slot> (or whatever the jail's root renamed it
// to), and two of them can share a name on the host (the probe of
// amendment k). The jail's shutdown therefore runs without its NICs.
//
// Skep must always be able to remove its jails (amendment u): when jail
// -r fails, times out, or leaves the jail, the shutdown sections are
// appended as for a successful stop, followed by a "--- jail -r failed:
// ... ---" line, and the jail is removed with jail -R (forceRemove). A
// caller's cancellation is returned as it is.
func (h *host) removeJail(ctx context.Context, name, conf, root string, spec JailSpec, withStop bool) error {
	er, err := h.envRunner()
	if err != nil {
		return err
	}
	if gone, err := h.DestroyJailInterfaces(ctx, spec.ContainerID); err != nil {
		return err
	} else if len(gone) > 0 {
		h.log.Info("hostops: destroyed the jail's epairs before removing it", "event", "jail_interfaces_destroyed",
			"container_id", spec.ContainerID, "interfaces", gone)
	}
	// The jail.conf has no exec.consolelog, so /etc/rc.shutdown writes to
	// jail(8)'s own output: the head and tail of its standard output, and
	// the tail of its standard error, kept in memory with markers where
	// bytes were dropped, and never a failure for their size (amendments
	// o, p, and q, SR-01); kept on a failure too (amendment u).
	var stderr bytes.Buffer
	out, runErr := er.RunEnv(ctx, EnvCommand{Path: jailBinary, Args: []string{"-f", conf, "-r", name},
		Env: jailEnv, Timeout: spec.StartTimeout + spec.StopTimeout + jailCommandSlack,
		MaxOutput: ShutdownLogPart, MarkOverflow: true, StderrLastLine: true, StderrTail: &stderr, OutputOnFailure: true})
	var failed string
	if runErr == nil {
		h.log.Debug("hostops: jail -r done", "container_id", spec.ContainerID, "output_bytes", len(out), "stderr_bytes", stderr.Len())
		exists, err := h.jailExists(ctx, name)
		if err != nil {
			return err
		}
		if exists {
			failed = "the jail was still there after jail -r exited 0"
			runErr = fmt.Errorf("hostops: the jail %s is still there after jail -r", name)
			h.log.Warn("hostops: jail -r left the jail", "event", "jail_remove_failed", "container_id", spec.ContainerID)
		}
	} else {
		failed = h.jailRemoveFailed(spec, runErr)
		if failed == "" {
			return runErr
		}
	}
	if withStop {
		h.appendShutdownLog(spec.ContainerID, out, stderr.Bytes(), failed)
	}
	if runErr != nil {
		return h.forceRemove(ctx, name, root, spec, withStop, failed)
	}
	// RCTL keeps rules on the name after the jail is gone. Rules left
	// here are removed by the next start; the removal is not failed for
	// them.
	h.clearRulesAfterRemoval(ctx, spec)
	return nil
}

// clearRulesAfterRemoval removes a removed jail's RCTL rules; a failure
// is logged (the next start removes them) and never fails the removal.
func (h *host) clearRulesAfterRemoval(ctx context.Context, spec JailSpec) {
	if err := h.clearJailRules(ctx, spec.ContainerID); err != nil {
		h.log.Error("hostops: the RCTL rules of a removed jail could not be removed", "event", "jail_rctl_left",
			"container_id", spec.ContainerID, "error", err)
	}
}

// jailRemoveFailed logs a jail -r that failed or timed out and returns
// the text of its "--- jail -r failed: ... ---" line: the last line of
// its stderr (cleaned: control and format characters removed, at most 200
// bytes), which may be the jail's own output, since jail -r's stderr
// starts with what /etc/rc.shutdown wrote (amendments p and u). It
// returns "" for any other error (a caller's cancellation, a runner that
// could not start jail(8)), which is not forced.
func (h *host) jailRemoveFailed(spec JailSpec, err error) string {
	var ce *CommandError
	var te *TimeoutError
	switch {
	case errors.As(err, &ce):
		h.log.Warn("hostops: jail -r failed", "event", "jail_remove_failed", "container_id", spec.ContainerID,
			"exit_code", ce.ExitCode, "jail_output", ce.Stderr)
		if ce.Stderr == "" {
			return "no jail output on stderr"
		}
		return ce.Stderr
	case errors.As(err, &te):
		h.log.Warn("hostops: jail -r timed out", "event", "jail_remove_failed", "container_id", spec.ContainerID,
			"timeout", te.After.String(), "jail_output", te.Stderr)
		if te.Stderr == "" {
			return "timed out after " + te.After.String() + ", no jail output on stderr"
		}
		return "timed out after " + te.After.String() + ", last line of stderr: " + te.Stderr
	}
	return ""
}

// forceRemove removes a jail whose jail -r failed with jail -R, which
// runs none of the jail's code and no exec.* command, then does the
// cleanup jail -r would have done or that follows it: the devfs left on
// <root>/dev (by fsid, amendment l), the epairs, and the RCTL rules. The
// console log and the run directory stay. It runs on its own short
// context, so a task that spent its time in jail -r still removes the
// jail. If jail -R fails too, or leaves the jail, it returns
// container_remove_failed with both commands' cleaned stderr lines
// (amendment u).
func (h *host) forceRemove(ctx context.Context, name, root string, spec JailSpec, withStop bool, failed string) error {
	er, err := h.envRunner()
	if err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jailCommandSlack)
	defer cancel()
	_, err = er.RunEnv(fctx, EnvCommand{Path: jailBinary, Args: []string{"-R", name}, Env: jailEnv,
		Timeout: jailCommandSlack, StderrLastLine: true})
	if err == nil {
		var exists bool
		if exists, err = h.jailExists(fctx, name); err == nil && exists {
			err = fmt.Errorf("hostops: the jail %s is still there after jail -R", name)
		}
	}
	if err != nil {
		var ce *CommandError
		detail := err.Error()
		output := ""
		if errors.As(err, &ce) {
			output = ce.Stderr
			if ce.Stderr == "" {
				detail = fmt.Sprintf("exit status %d, no output on stderr", ce.ExitCode)
			} else {
				detail = fmt.Sprintf("exit status %d, last line of stderr: %s", ce.ExitCode, strconv.Quote(ce.Stderr))
			}
		}
		h.log.Error("hostops: jail -R failed after jail -r failed; the jail could not be removed", "event", "container_remove_failed",
			"container_id", spec.ContainerID, "jail_output", failed, "jail_R_output", output, "error", err)
		return &models.Error{Code: models.CodeContainerRemoveFailed,
			Message: "the jail could not be removed: jail -r failed (jail output: " + strconv.Quote(failed) +
				"), and jail -R then failed (" + detail + "); it may still be running",
			Details: map[string]any{"container_id": spec.ContainerID}}
	}
	if root != "" {
		if _, err := h.removeLeftoverDevfs(spec.ContainerID, root); err != nil {
			h.log.Error("hostops: the devfs of a jail removed with jail -R could not be unmounted; the next start or delete tries again",
				"event", "jail_devfs_left", "container_id", spec.ContainerID, "error", err)
		}
	}
	if gone, err := h.DestroyJailInterfaces(fctx, spec.ContainerID); err != nil {
		h.log.Error("hostops: the epairs of a jail removed with jail -R could not be destroyed", "container_id", spec.ContainerID, "error", err)
	} else if len(gone) > 0 {
		h.log.Info("hostops: destroyed the epairs of a jail removed with jail -R", "event", "jail_interfaces_destroyed",
			"container_id", spec.ContainerID, "interfaces", gone)
	}
	h.clearRulesAfterRemoval(fctx, spec)
	h.log.Warn("hostops: jail -r failed, so the jail was removed with jail -R", "event", "container_stop_forced",
		"container_id", spec.ContainerID, "exec_stop", withStop, "jail_output", failed)
	return nil
}

// ShutdownLogPart is how much of the head, and of the tail, of
// /etc/rc.shutdown's standard output, and how much of the tail of its
// standard error, is appended to the console log (amendments p and q).
const ShutdownLogPart = 32 << 10

// The lines written to the console log before /etc/rc.shutdown's
// standard output and before its standard error.
const (
	shutdownMarker       = "--- shutdown ---\n"
	shutdownStderrMarker = "--- shutdown stderr ---\n"
)

// jailRemoveFailedLine is the line written after the shutdown sections
// when jail -r failed (amendment u). failed is already cleaned, so it is
// one line.
func jailRemoveFailedLine(failed string) string {
	return "--- jail -r failed: " + failed + " ---\n"
}

// appendShutdownLog appends the shutdown markers, out, and errOut to the
// jail's console log, and, when failed is not empty, the line
// "--- jail -r failed: <failed> ---" (amendment u). A failure is logged
// and never fails the removal (amendments p and q).
func (h *host) appendShutdownLog(containerID string, out, errOut []byte, failed string) {
	path, err := h.conf.JailRunPath(containerID, JailConsoleLog)
	if err == nil {
		err = appendShutdownOutput(path, out, errOut, failed)
	}
	if err != nil {
		h.log.Warn("hostops: the jail's shutdown output could not be written to its console log", "event", "jail_shutdown_log_failed",
			"container_id", containerID, "output_bytes", len(out), "stderr_bytes", len(errOut), "error", err)
	}
}

// appendShutdownOutput appends to the file at path, created 0600 if
// missing, the shutdown marker line, out, the stderr marker line, and
// errOut, then, when failed is not empty, the failure line, in one
// write. It never follows a symbolic link, never blocks on a FIFO, and
// refuses anything but a regular file. Each marker starts on a line of
// its own; both are written even when their part is empty.
func appendShutdownOutput(path string, out, errOut []byte, failed string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("hostops: %s is not a regular file", path)
	}
	data := make([]byte, 0, 2+len(shutdownMarker)+len(out)+len(shutdownStderrMarker)+len(errOut))
	if fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			data = append(data, '\n')
		}
	}
	data = append(data, shutdownMarker...)
	data = append(data, out...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, shutdownStderrMarker...)
	data = append(data, errOut...)
	if failed != "" {
		if len(errOut) > 0 && errOut[len(errOut)-1] != '\n' {
			data = append(data, '\n')
		}
		data = append(data, jailRemoveFailedLine(failed)...)
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}

// StopJail implements Jails.
func (h *host) StopJail(ctx context.Context, spec JailSpec) error {
	name, err := JailName(spec.ContainerID)
	if err != nil {
		return err
	}
	exists, err := h.jailExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		// The jail.conf is written again with this stop's timeout.
		conf, root, err := h.writeJailConf(ctx, spec, true)
		if err != nil {
			return err
		}
		if err := h.removeJail(ctx, name, conf, root, spec, true); err != nil {
			return err
		}
	}
	// Epairs a jail removed outside Skep returned to the host (their jail
	// ends named skep<slot>): destroying the host ends removes both
	// (decision 10).
	_, err = h.DestroyJailInterfaces(ctx, spec.ContainerID)
	return err
}

// RunningJails implements Jails.
func (h *host) RunningJails(ctx context.Context) (map[string]JailState, error) {
	out, err := h.run.Run(ctx, jlsBinary, "--libxo=json", "jid", "name", "path")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Info struct {
			Jail []struct {
				JID  json.Number `json:"jid"`
				Name string      `json:"name"`
				Path string      `json:"path"`
			} `json:"jail"`
		} `json:"jail-information"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("hostops: jls: %w", err)
	}
	states := map[string]JailState{}
	for _, j := range doc.Info.Jail {
		id, ok := strings.CutPrefix(j.Name, JailPrefix)
		if !ok || !ValidID(id) {
			continue
		}
		jid, _ := strconv.Atoi(j.JID.String())
		states[id] = JailState{JID: jid, Name: j.Name, Path: j.Path}
	}
	return states, nil
}

// ConsoleLogTail implements Jails.
func (h *host) ConsoleLogTail(containerID string, maxBytes int) (string, error) {
	path, err := h.conf.JailRunPath(containerID, JailConsoleLog)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("hostops: %s is not a regular file", path)
	}
	if off := fi.Size() - int64(maxBytes); off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return "", err
		}
	}
	buf, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	return string(buf), err
}

// RemoveJailRun implements Jails.
func (h *host) RemoveJailRun(containerID string) error {
	conf, err := h.conf.JailRunPath(containerID, JailConfFile)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Dir(conf))
}
