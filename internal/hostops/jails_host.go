package hostops

import (
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
	logPath, err := h.conf.JailRunPath(spec.ContainerID, JailConsoleLog)
	if err != nil {
		return "", err
	}
	data, err := renderJailConfMode(spec, root, logPath, withStop)
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
	if _, err := er.RunEnv(ctx, EnvCommand{Path: jailBinary, Args: []string{"-f", conf, "-c", name},
		Env: jailEnv, Timeout: jailCommandSlack}); err != nil {
		h.discardAfterFailedStart(spec, root)
		return JailState{}, startFailed(spec, "jail(8) could not create the jail: ", err)
	}
	jid, err := h.finishStart(ctx, spec, name, conf, root)
	if err != nil {
		return JailState{}, err
	}
	return JailState{JID: jid, Name: name, Path: root, Epairs: epairs}, nil
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
// run. Then /etc/rc runs through jexec, its output appended to
// console.log, bounded by the start timeout; if it fails or times out, the
// jail is removed with jail -r, which runs exec.stop. It returns the JID.
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
		h.discardAfterFailedStart(spec, root)
		return 0, startFailed(spec, "the jail was removed before any of its code ran: ", err)
	}
	logPath, err := h.conf.JailRunPath(spec.ContainerID, JailConsoleLog)
	if err == nil {
		var er EnvRunner
		if er, err = h.envRunner(); err == nil {
			_, err = er.RunEnv(ctx, EnvCommand{Path: jexecBinary, Args: jailRCArgs(name), Env: jailEnv,
				Timeout: spec.StartTimeout, Output: logPath})
		}
	}
	if err != nil {
		h.log.Warn("hostops: /etc/rc failed in a jail; removing it", "event", "jail_rc_failed",
			"container_id", spec.ContainerID, "error", err)
		h.removeAfterFailedStart(name, conf, spec)
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
// created: if_epair, a usable allow-listed bridge for each, and, for DHCP,
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
// cancelled request still cleans up. A failure is logged.
func (h *host) discardAfterFailedStart(spec JailSpec, root string) {
	ctx, cancel := context.WithTimeout(context.Background(), spec.StopTimeout+spec.StartTimeout+jailCommandSlack)
	defer cancel()
	if err := h.discardJail(ctx, spec, root); err != nil {
		h.log.Error("hostops: a jail left by a failed start could not be removed", "container_id", spec.ContainerID, "error", err)
		return
	}
	h.cleanJailNet(spec)
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
	return h.removeJail(ctx, name, conf, spec)
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
func (h *host) removeAfterFailedStart(name, conf string, spec JailSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), spec.StopTimeout+spec.StartTimeout+jailCommandSlack)
	defer cancel()
	exists, err := h.jailExists(ctx, name)
	if err != nil {
		return
	}
	if exists {
		if err := h.removeJail(ctx, name, conf, spec); err != nil {
			h.log.Error("hostops: a jail left by a failed start could not be removed", "jail", name, "error", err)
			return
		}
	}
	h.cleanJailNet(spec)
}

// removeJail destroys the jail's epairs, then runs jail -r with the
// jail's own jail.conf (whose exec.stop, if any, runs) and confirms the
// jail is gone.
//
// The epairs go first (amendment k, "or its epair destroyed"): destroying
// a host end destroys its jail end inside the running jail, so no jail end
// returns to the host on a removal Skep makes. Otherwise every jail's end
// would return named skep<slot> (or whatever the jail's root renamed it
// to), and two of them can share a name on the host (the probe of
// amendment k). The jail's shutdown therefore runs without its NICs.
func (h *host) removeJail(ctx context.Context, name, conf string, spec JailSpec) error {
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
	if _, err := er.RunEnv(ctx, EnvCommand{Path: jailBinary, Args: []string{"-f", conf, "-r", name},
		Env: jailEnv, Timeout: spec.StartTimeout + spec.StopTimeout + jailCommandSlack}); err != nil {
		return err
	}
	if exists, err := h.jailExists(ctx, name); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("hostops: the jail %s is still there after jail -r", name)
	}
	// RCTL keeps rules on the name after the jail is gone. Rules left
	// here are removed by the next start; the removal is not failed for
	// them.
	if err := h.clearJailRules(ctx, spec.ContainerID); err != nil {
		h.log.Error("hostops: the RCTL rules of a removed jail could not be removed", "event", "jail_rctl_left",
			"container_id", spec.ContainerID, "error", err)
	}
	return nil
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
		conf, _, err := h.writeJailConf(ctx, spec, true)
		if err != nil {
			return err
		}
		if err := h.removeJail(ctx, name, conf, spec); err != nil {
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
