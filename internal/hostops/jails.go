package hostops

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Jails (ADR 0010 decisions 8, 9, and 13, with amendments e, f, and g).
//
// Layout, in an allow-listed ZFS pool:
//
//	<pool>/<root>/jail          mounted, its directory root:wheel 0700
//	<pool>/<root>/jail/<id>     the jail root: a clone of a template's
//	                            @skep-base, skep:container_id=<id>
//	<JailRunDir>/<id>/jail.conf root-only, generated before every start
//	<JailRunDir>/<id>/console.log  exec.consolelog
//
// A jail is named skep-<id>. Its jail.conf is rendered from typed values
// (JailSpec) and the fixed baseline below; nothing in it comes from a
// request except the validated hostname. There is never an exec.prestart,
// exec.poststart, or any other host-side command.

// Jails creates, starts, stops, and destroys jails.
type Jails interface {
	// EnsureJailsParent creates <pool>/<root>/jail on first use, mounted,
	// with its directory root:wheel 0700, and then checks it as
	// CheckJailsParent does.
	EnsureJailsParent(ctx context.Context, pool string) error
	// CheckJailsParent refuses (jail_parent_insecure) a jail directory
	// that is not a real directory owned by root:wheel with mode 0700, or
	// that does not exist.
	CheckJailsParent(ctx context.Context, pool string) error
	// CreateJailRoot clones <template>@skep-base to <pool>/<root>/jail/<id>,
	// carrying skep:container_id, with readonly=off set explicitly, and
	// returns it. The template must carry its ID and have @skep-base. An
	// existing dataset of that name is refused.
	CreateJailRoot(ctx context.Context, pool, templateID, containerID string) (Dataset, error)
	// JailRoot returns the path of a jail's root after checking that its
	// dataset carries the container's ID and is mounted where it should
	// be.
	JailRoot(ctx context.Context, pool, containerID string) (string, error)
	// DestroyJailRoot destroys a jail's dataset and its snapshots, and
	// returns what existed. It refuses a dataset that does not carry the
	// container's ID, and a jail that is running. A jail without a
	// dataset yields nil.
	DestroyJailRoot(ctx context.Context, pool, containerID string) ([]Dataset, error)
	// ConfigureJail writes Skep's files into the tree of a jail that is
	// not running (decision 13): /etc/rc.conf (Skep's own, replaced
	// whole) and root's authorized_keys. Every open is relative to the
	// jail root's directory descriptor with O_RESOLVE_BENEATH, and the
	// last component is opened with O_NOFOLLOW. The caller makes sure the
	// jail has never been started.
	ConfigureJail(ctx context.Context, pool, containerID string, files JailFiles) error
	// TemplateHasSSHD reports whether a template has a regular file
	// usr/sbin/sshd (amendment g).
	TemplateHasSSHD(ctx context.Context, pool, templateID string) (bool, error)

	// StartJail checks the jail directory, writes the jail's jail.conf,
	// creates the jail with jail(8) (which runs /etc/rc), and checks its
	// parameters as the kernel reports them. A jail whose parameters do
	// not match is removed and the start fails.
	StartJail(ctx context.Context, spec JailSpec) (JailState, error)
	// StopJail removes a jail with jail(8): exec.stop (/etc/rc.shutdown),
	// then SIGTERM, then after the stop timeout SIGKILL. It confirms the
	// jail is gone. A jail that is not running is not an error.
	StopJail(ctx context.Context, spec JailSpec) error
	// RunningJails lists the running Skep jails (skep-<id>) by container
	// ID.
	RunningJails(ctx context.Context) (map[string]JailState, error)
	// ConsoleLogTail returns the end of a jail's console.log, at most
	// maxBytes. A missing file yields "".
	ConsoleLogTail(containerID string, maxBytes int) (string, error)
	// RemoveJailRun removes a jail's runtime directory. A missing one is
	// not an error.
	RemoveJailRun(containerID string) error

	// EpairAvailable refuses (epair_unavailable) when if_epair is not
	// loaded. StartJail checks it before any epair is created.
	EpairAvailable(ctx context.Context) error
	// DestroyJailInterfaces destroys every epair whose host end is
	// described skep:<container-id>:<slot> and returns their host ends.
	// StopJail calls it after jail -r; a start and a delete call it for
	// what a crash left (decision 10).
	DestroyJailInterfaces(ctx context.Context, containerID string) ([]string, error)
	// SkepInterfaces lists the epair host ends and taps whose description
	// is skep:<owner-id>:<slot>.
	SkepInterfaces(ctx context.Context) ([]SkepInterface, error)
}

// JailState is a running jail as jls reports it.
type JailState struct {
	JID  int
	Name string
	Path string
	// Epairs are the host ends of the jail's epairs by slot, as StartJail
	// created them.
	Epairs map[int]string
}

// JailSpec is everything a jail.conf is rendered from. Every field is
// typed or validated; the paths are derived from the pool and the ID.
type JailSpec struct {
	Pool        string
	ContainerID string
	// Hostname is host.hostname: a DNS name (ValidHostname).
	Hostname string
	// SysVIPC sets sysvshm, sysvsem, and sysvmsg to new (an opt-in).
	SysVIPC bool
	// Mlock sets allow.mlock (an opt-in, accepted only with a memory
	// limit; the service decides).
	Mlock bool
	// StartTimeout is exec.timeout; StopTimeout is stop.timeout.
	StartTimeout time.Duration
	StopTimeout  time.Duration
	// NICs are the jail's VNET interfaces (decision 10).
	NICs []JailNIC
}

// JailFiles is what Skep writes into a jail before its first start.
type JailFiles struct {
	// RCConf are the lines of /etc/rc.conf after Skep's header. None
	// removes Skep's rc.conf.
	RCConf []string
	// AuthorizedKey is root's SSH public key, one line. Empty removes
	// root's authorized_keys.
	AuthorizedKey string
	// Nameservers are the nameserver lines of /etc/resolv.conf, for a
	// jail with static addressing. None removes Skep's resolv.conf and
	// leaves any other alone.
	Nameservers []string
}

// The jail layout and naming.
const (
	jailsDir = "jail"
	// JailPrefix starts the name of every Skep jail.
	JailPrefix = "skep-"
	// JailConfFile and JailConsoleLog are the fixed files of a jail's
	// runtime directory.
	JailConfFile   = "jail.conf"
	JailConsoleLog = "console.log"
	// JailDevfsRuleset is devfsrules_jail_vnet, which every Skep jail is.
	JailDevfsRuleset = 5
)

// Bounds of the jail timeouts.
const (
	MinJailTimeout = time.Second
	MaxJailTimeout = 10 * time.Minute
)

// JailsDataset is <pool>/<root>/jail.
func (c Confinement) JailsDataset(pool string) (string, error) {
	root, err := c.rootDataset(pool)
	if err != nil {
		return "", err
	}
	return root + "/" + jailsDir, nil
}

// JailDataset is <pool>/<root>/jail/<container-id>.
func (c Confinement) JailDataset(pool, containerID string) (string, error) {
	return c.OwnerDataset(pool, Owner{Kind: OwnerContainer, ID: containerID})
}

// JailName is the jail(8) name of a container: skep-<id>.
func JailName(containerID string) (string, error) {
	if !ValidID(containerID) {
		return "", invalid("invalid container ID")
	}
	return JailPrefix + containerID, nil
}

// JailRunPath is a fixed file of a jail's runtime directory.
func (c Confinement) JailRunPath(containerID, name string) (string, error) {
	if !ValidID(containerID) {
		return "", invalid("invalid container ID")
	}
	if name != JailConfFile && name != JailConsoleLog {
		return "", invalid("invalid jail runtime file")
	}
	if !filepath.IsAbs(c.JailRunDir) {
		return "", fmt.Errorf("hostops: the jail runtime directory is not configured")
	}
	return filepath.Join(c.JailRunDir, containerID, name), nil
}

var hostLabelRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// ValidHostname reports whether s is a DNS name of letters, digits, and
// hyphens in dot-separated labels of 1 to 63 characters, 253 at most.
func ValidHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !hostLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// jailAllow is the allow.* set of an ordinary Skep jail (decision 9,
// amendments e and f): allow.suser and allow.reserved_ports at their
// default, on; allow.mlock as the opt-in says; every other allow.* of the
// 15.1 GENERIC kernel off, the defaults allow.set_hostname and
// allow.unprivileged_proc_debug included. allow.mount=0 covers the
// allow.mount.* ones. allow.raw_sockets is set off although a VNET jail
// has raw sockets whatever it says (amendment f). checkJailParams
// confirms every allow.* the kernel reports, whatever this list misses.
func jailAllow(mlock bool) []string {
	out := make([]string, 0, len(buildJailAllow))
	for _, a := range buildJailAllow {
		v := "0"
		switch {
		case a == "suser" || a == "reserved_ports":
			v = "1"
		case a == "mlock" && mlock:
			v = "1"
		}
		out = append(out, "allow."+a+" = "+v)
	}
	return out
}

func (s JailSpec) sysv() string {
	if s.SysVIPC {
		return "new"
	}
	return "disable"
}

func checkTimeout(name string, d time.Duration) error {
	if d < MinJailTimeout || d > MaxJailTimeout || d%time.Second != 0 {
		return invalid("%s must be a whole number of seconds between %d and %d", name,
			int(MinJailTimeout/time.Second), int(MaxJailTimeout/time.Second))
	}
	return nil
}

// confValue refuses anything that could end a quoted jail.conf value or
// start a variable: every value is derived or validated, so this only
// catches a bug.
func confValue(v string) (string, error) {
	if strings.ContainsAny(v, "\"\\$\n\r;{}") {
		return "", fmt.Errorf("hostops: refusing a jail.conf value with a special character: %q", printable(v))
	}
	return `"` + v + `"`, nil
}

// renderJailConf renders the jail.conf of a jail from its spec and the
// paths hostops derived.
func renderJailConf(spec JailSpec, root, consoleLog string) ([]byte, error) {
	name, err := JailName(spec.ContainerID)
	if err != nil {
		return nil, err
	}
	if !ValidHostname(spec.Hostname) {
		return nil, invalid("invalid hostname")
	}
	if err := checkTimeout("the start timeout", spec.StartTimeout); err != nil {
		return nil, err
	}
	if err := checkTimeout("the stop timeout", spec.StopTimeout); err != nil {
		return nil, err
	}
	if err := checkNICs(spec.NICs); err != nil {
		return nil, err
	}
	var ifaces []string
	for _, n := range spec.NICs {
		jif, err := JailInterfaceName(spec.ContainerID, n.Slot)
		if err != nil {
			return nil, err
		}
		q, err := confValue(jif)
		if err != nil {
			return nil, err
		}
		ifaces = append(ifaces, q)
	}
	quoted := map[string]string{}
	for k, v := range map[string]string{"path": root, "host.hostname": spec.Hostname, "exec.consolelog": consoleLog,
		"exec.start": "/bin/sh /etc/rc", "exec.stop": "/bin/sh /etc/rc.shutdown jail"} {
		q, err := confValue(v)
		if err != nil {
			return nil, err
		}
		quoted[k] = q
	}
	lines := []string{
		"path = " + quoted["path"],
		"host.hostname = " + quoted["host.hostname"],
		"vnet",
	}
	if len(ifaces) > 0 {
		// jail(8) moves these into the jail's stack after creating it;
		// hostops created and named them before (decision 10).
		lines = append(lines, "vnet.interface = "+strings.Join(ifaces, ", "))
	}
	lines = append(lines,
		"persist",
		"enforce_statfs = 2",
		"children.max = 0",
		"securelevel = 2",
		"mount.devfs",
		"devfs_ruleset = "+strconv.Itoa(spec.devfsRuleset()),
	)
	lines = append(lines, jailAllow(spec.Mlock)...)
	lines = append(lines,
		"sysvshm = "+spec.sysv(),
		"sysvsem = "+spec.sysv(),
		"sysvmsg = "+spec.sysv(),
		"exec.clean",
		"exec.start = "+quoted["exec.start"],
		"exec.stop = "+quoted["exec.stop"],
		"exec.consolelog = "+quoted["exec.consolelog"],
		"exec.timeout = "+strconv.Itoa(int(spec.StartTimeout/time.Second)),
		"stop.timeout = "+strconv.Itoa(int(spec.StopTimeout/time.Second)),
	)
	var b strings.Builder
	b.WriteString("# Generated by Skep (ADR 0010 decisions 8 and 9). Do not edit: it is\n# rewritten before every start and stop.\n")
	b.WriteString(name + " {\n")
	for _, l := range lines {
		b.WriteString("\t" + l + ";\n")
	}
	b.WriteString("}\n")
	return []byte(b.String()), nil
}

// checkJailParams confirms a jail's parameters as the kernel reports them
// (jls --libxo=json -n): its name, path, and hostname; its own network
// stack; the statfs, child, securelevel, and devfs limits (ruleset 5, or
// Skep's DHCP ruleset for a jail with a DHCP NIC); the System V
// IPC setting; allow.suser and allow.reserved_ports on, allow.mlock as
// asked, and every other allow.* off.
func checkJailParams(params map[string]any, spec JailSpec, root string) error {
	name, err := JailName(spec.ContainerID)
	if err != nil {
		return err
	}
	want := map[string]string{
		"name": name, "path": root, "host.hostname": spec.Hostname, "vnet": "new",
		"enforce_statfs": "2", "children.max": "0", "securelevel": "2",
		"devfs_ruleset": strconv.Itoa(spec.devfsRuleset()), "persist": "true",
		"sysvshm": spec.sysv(), "sysvsem": spec.sysv(), "sysvmsg": spec.sysv(),
	}
	var bad []string
	for k, v := range want {
		if got := fmt.Sprint(params[k]); got != v {
			bad = append(bad, fmt.Sprintf("%s=%s (want %s)", k, got, v))
		}
	}
	on := map[string]bool{"allow.suser": true, "allow.reserved_ports": true, "allow.mlock": spec.Mlock}
	allows := 0
	for k, v := range params {
		if !strings.HasPrefix(k, "allow.") {
			continue
		}
		allows++
		b, ok := v.(bool)
		if !ok || b != on[k] {
			bad = append(bad, fmt.Sprintf("%s=%v (want %v)", k, v, on[k]))
		}
	}
	for k, v := range on {
		if v {
			if _, ok := params[k]; !ok {
				bad = append(bad, k+" not reported (want true)")
			}
		}
	}
	if allows == 0 {
		bad = append(bad, "no allow.* parameters reported")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("hostops: the jail %s does not have the required parameters: %s", name, strings.Join(bad, ", "))
	}
	return nil
}

// RenderJailConf renders a jail.conf the way StartJail does; the
// development fake uses it so a simulated start checks the same things.
func RenderJailConf(spec JailSpec, root, consoleLog string) ([]byte, error) {
	return renderJailConf(spec, root, consoleLog)
}

// Check validates JailFiles the way ConfigureJail does, without writing.
func (f JailFiles) Check() error {
	_, err := f.render()
	return err
}
