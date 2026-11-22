// Package setup is the host configuration behind skep-setup (ADR 0012,
// Milestone 10): read-only check and plan, apply with backups and a
// journal, rollback, upgrade, and remove.
//
// Setup only adds. For files it does not own (/boot/loader.conf,
// /etc/rc.conf) it adds a missing line or appends to a list, through
// sysrc(8) (loader tunables with a dot in the name, which sysrc refuses,
// by setup itself); an existing line with another value is a conflict, reported
// and left alone. It refuses to edit a file it cannot parse, never loads
// a module, never reboots, never creates a pool, and never changes an
// interface or bridge it did not create (ADR 0012 Decisions 2 and 3).
//
// Every path is relative to a root, "/" on a real host and a fixture
// directory in the tests.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/greenbeanorg/skep/internal/config"
)

// Kind is what the plan says about one item.
type Kind string

// The kinds of plan items.
const (
	OK       Kind = "ok"       // already as required; nothing to do
	Add      Kind = "add"      // apply does this
	Warn     Kind = "warn"     // apply goes ahead; the operator should know
	Conflict Kind = "conflict" // an existing value differs; never overwritten
	Manual   Kind = "manual"   // left to the operator, with the steps
	Refuse   Kind = "refuse"   // apply does nothing at all while this stands
)

// Action types.
const (
	ActSysrc        = "sysrc"         // add a variable or append to a list
	ActWrite        = "write"         // create a file that does not exist
	ActMkdir        = "mkdir"         // create a directory that does not exist
	ActServiceStart = "service-start" // start a Skep service (--start)
	ActAdminCreate  = "admin-create"  // skep-api admin create, interactive
	ActServiceStop  = "service-stop"  // stop a Skep service (remove)
	ActRemovePath   = "remove-path"   // delete Skep's data (remove --purge-data)
	ActPwDelete     = "pw-delete"     // delete the skep account (remove --purge-data)
)

// Action is one change apply makes. Paths are host paths (under the root).
type Action struct {
	Type string `json:"type"`
	// File is the file a sysrc action edits or a write action creates.
	File string `json:"file,omitempty"`
	// Name, Op ("=" or "+="), and Value make a sysrc argument.
	Name  string `json:"name,omitempty"`
	Op    string `json:"op,omitempty"`
	Value string `json:"value,omitempty"`
	// Expect is the value the plan saw for a sysrc "=" or "-x" (nil:
	// not set). Just before the change the variable is read again, and
	// the action is skipped if it differs.
	Expect *string `json:"expect,omitempty"`
	// Content, Mode, Owner and Group describe a written file or a
	// directory. Content is not kept in the journal.
	Content []byte      `json:"-"`
	Mode    fs.FileMode `json:"mode,omitempty"`
	Owner   string      `json:"owner,omitempty"`
	Group   string      `json:"group,omitempty"`
	// Service is an rc.d service; Username the first administrator.
	Service  string `json:"service,omitempty"`
	Username string `json:"username,omitempty"`
}

// String is the action as the plan prints it.
func (a Action) String() string {
	switch a.Type {
	case ActSysrc:
		flag := ""
		if a.File != rcConfPath {
			flag = "-f " + a.File + " "
		}
		if !SysrcCanEdit(a.Name) {
			// sysrc refuses the name; setup edits the line itself.
			if a.Op == "-x" {
				return fmt.Sprintf("delete the %s line from %s", a.Name, a.File)
			}
			return fmt.Sprintf(`append %s%s"%s" to %s`, a.Name, a.Op, a.Value, a.File)
		}
		if a.Op == "-x" {
			return fmt.Sprintf("sysrc %s-x %s", flag, a.Name)
		}
		return fmt.Sprintf(`sysrc %s%s%s"%s"`, flag, a.Name, a.Op, a.Value)
	case ActWrite:
		return fmt.Sprintf("write %s (%04o %s:%s)", a.File, a.Mode, a.Owner, a.Group)
	case ActMkdir:
		return fmt.Sprintf("mkdir %s (%04o %s:%s)", a.File, a.Mode, a.Owner, a.Group)
	case ActServiceStart:
		return "service " + a.Service + " start"
	case ActAdminCreate:
		return "skep-api admin create --username " + a.Username + " (as the skep user, once skep_api has created api.db; asks for the password, twice, without echo)"
	case ActServiceStop:
		return "service " + a.Service + " stop"
	case ActRemovePath:
		return "rm -rf " + a.File
	case ActPwDelete:
		return "pw userdel " + a.Name + "; pw groupdel " + a.Name
	}
	return a.Type
}

// Item is one line of the plan.
type Item struct {
	Kind   Kind    `json:"kind"`
	Area   string  `json:"area"`
	What   string  `json:"what"`
	Detail string  `json:"detail,omitempty"`
	Action *Action `json:"action,omitempty"`
	// Steps are commands left to the operator, printed in order.
	Steps []string `json:"steps,omitempty"`
}

// Plan is everything apply would do and everything it leaves alone.
type Plan struct {
	Items []Item `json:"items"`
	// Listen is the API listen address written to api.json.
	Listen string `json:"listen,omitempty"`
}

// Refused reports whether any item refuses.
func (p Plan) Refused() bool {
	return slices.ContainsFunc(p.Items, func(it Item) bool { return it.Kind == Refuse })
}

// Actions returns the actions of the Add items, in order.
func (p Plan) Actions() []Action {
	var out []Action
	for _, it := range p.Items {
		if it.Kind == Add && it.Action != nil {
			out = append(out, *it.Action)
		}
	}
	return out
}

// DirPool is a directory pool for VM disks (ADR 0010 amendment h).
type DirPool struct {
	Name string
	Path string
}

// Options are the operator's answers.
type Options struct {
	// Root stands in for "/".
	Root string
	// Mgmt are the management interfaces; the first one's address is the
	// default listen address. Setup never changes them.
	Mgmt []string
	// Bridges are existing bridges to allow-list. Setup changes nothing
	// on them.
	Bridges []string
	// CreateBridge and Uplink ask for a new bridge on a spare,
	// unaddressed interface.
	CreateBridge string
	Uplink       string
	// Pools are ZFS pools and DirPools directory pools to allow-list.
	Pools    []string
	DirPools []DirPool
	// Listen is the API listen address; ListenAll allows 0.0.0.0 or ::.
	Listen    string
	ListenAll bool
	// NoRACCT leaves kern.racct.enable out.
	NoRACCT bool
	// Start starts both services at the end of apply.
	Start bool
	// Admin creates the first administrator with this username.
	Admin string

	// fileOwner returns a file's owner UID; the tests, which do not run
	// as root, replace it. Nil means the owner from stat(2).
	fileOwner func(fs.FileInfo) (uint32, bool)
}

// owner returns the UID that owns a file.
func (o Options) owner(st fs.FileInfo) (uint32, bool) {
	if o.fileOwner != nil {
		return o.fileOwner(st)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return sys.Uid, true
}

// Fixed values (host-requirements.md, ADR 0012).
const (
	rcConfPath     = "/etc/rc.conf"
	loaderConfPath = "/boot/loader.conf"
	agentJSONPath  = "/usr/local/etc/skep/agent.json"
	apiJSONPath    = "/usr/local/etc/skep/api.json"
	mediaDir       = "/var/db/skep/media"
	firmwareDir    = "/usr/local/share/edk2-bhyve"
	dmesgBoot      = "/var/run/dmesg.boot"
	// SkepID is the fixed UID and GID of the skep account (Decision 1).
	SkepID       = "409"
	skepAccount  = "skep"
	defaultPort  = "8443"
	qualifiedRel = "15.1"
)

// KLDModules are the modules guests need after boot (ADR 0010, ADR 0011).
var KLDModules = []string{"if_epair", "netgraph", "ng_socket", "ng_ether", "ng_bpf"}

var (
	ifNameRE   = regexp.MustCompile(`^[a-z][a-z0-9]{0,14}[0-9]$`)
	bridgeRE   = regexp.MustCompile(`^bridge[0-9]{1,4}$`)
	poolNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,254}$`)
	userRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// hostPath joins a host path onto the root.
func hostPath(root, p string) string { return filepath.Join(root, strings.TrimPrefix(p, "/")) }

// ValidateOptions checks the answers' syntax. Everything they name is
// checked against the host by Compute.
func ValidateOptions(o Options) error {
	if o.Root == "" {
		return errors.New("root must not be empty")
	}
	if len(o.Mgmt) == 0 {
		return errors.New("name the management interface (--mgmt): setup will not guess which interface carries management")
	}
	for _, m := range o.Mgmt {
		if !ifNameRE.MatchString(m) {
			return fmt.Errorf("management interface %q: not an interface name", m)
		}
	}
	for _, b := range o.Bridges {
		if !bridgeRE.MatchString(b) {
			return fmt.Errorf("bridge %q: not a bridge name (bridgeN)", b)
		}
	}
	if (o.CreateBridge == "") != (o.Uplink == "") {
		return errors.New("--create-bridge and --uplink go together")
	}
	if o.CreateBridge != "" {
		if !bridgeRE.MatchString(o.CreateBridge) {
			return fmt.Errorf("bridge %q: not a bridge name (bridgeN)", o.CreateBridge)
		}
		if !ifNameRE.MatchString(o.Uplink) {
			return fmt.Errorf("uplink %q: not an interface name", o.Uplink)
		}
		if slices.Contains(o.Bridges, o.CreateBridge) {
			return fmt.Errorf("%s is named both as an existing bridge and as one to create", o.CreateBridge)
		}
	}
	for _, p := range o.Pools {
		if !poolNameRE.MatchString(p) {
			return fmt.Errorf("pool %q: not a pool name", p)
		}
	}
	for _, d := range o.DirPools {
		if !poolNameRE.MatchString(d.Name) || !filepath.IsAbs(d.Path) || filepath.Clean(d.Path) != d.Path || d.Path == "/" {
			return fmt.Errorf("directory pool %q=%q: give name=/absolute/path", d.Name, d.Path)
		}
	}
	if o.Admin != "" && !userRE.MatchString(o.Admin) {
		return fmt.Errorf("administrator %q: use lower-case letters, digits, _ and -", o.Admin)
	}
	if o.Listen != "" {
		host, port, err := net.SplitHostPort(o.Listen)
		if err != nil {
			return fmt.Errorf("listen address %q: %w", o.Listen, err)
		}
		if _, err := netip.ParseAddr(host); err != nil {
			return fmt.Errorf("listen address %q: the host must be an IP address", o.Listen)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("listen address %q: bad port", o.Listen)
		}
	}
	return nil
}

// Compute returns the plan for the host under o.Root with facts f. It
// reads files and nothing else.
func Compute(ctx context.Context, o Options, f Facts) (Plan, error) {
	if err := ValidateOptions(o); err != nil {
		return Plan{}, err
	}
	journals, err := LoadJournals(o.Root)
	if err != nil {
		return Plan{}, err
	}
	b := &builder{o: o, f: f, journals: journals}
	if j := incompleteJournal(journals); j != nil {
		b.refuse("journal", "apply "+j.ID, "an earlier run did not finish; run skep-setup rollback "+j.ID+" first")
	}
	b.platform()
	rc, err := ReadRCConf(o.Root)
	if err != nil {
		return Plan{}, err
	}
	if rc.Main.Missing {
		b.refuse(rcConfPath, "the whole plan", "no rc.conf: not a configured FreeBSD host")
		return b.plan, nil
	}
	loader, err := readLoader(o.Root)
	if err != nil {
		return Plan{}, err
	}
	b.rc, b.loader = rc, loader
	b.editability()
	b.loaderLines()
	b.kldList()
	b.storage()
	b.management()
	b.bridges()
	b.account()
	b.services()
	if err := b.configFiles(); err != nil {
		return Plan{}, err
	}
	b.media()
	b.readOnlyChecks()
	b.guests()
	if o.Start {
		for _, svc := range []string{"skep_agent", "skep_api"} {
			if f.Running[svc] {
				b.ok("services", svc, "running")
			} else {
				b.add("services", "start "+svc, "", Action{Type: ActServiceStart, Service: svc})
			}
		}
	}
	if o.Admin != "" {
		b.admin()
	}
	return b.plan, nil
}

// admin plans the first administrator. skep-api admin create opens
// api.db; run before skep_api has ever started, it would create the API's
// data directory as root and lock the unprivileged API out of it, so it
// runs after --start, or once the directory exists.
func (b *builder) admin() {
	// skep-api admin create refuses a username that exists, so a second
	// apply with the same answers must not plan it again. Rollback never
	// deletes the user, so a rolled-back run counts too.
	for _, j := range b.journals {
		for _, e := range j.Entries {
			if e.Done && e.Action.Type == ActAdminCreate && e.Action.Username == b.o.Admin {
				b.ok("api", "first administrator "+b.o.Admin, "created by skep-setup run "+j.ID)
				return
			}
		}
	}
	if _, err := os.Stat(hostPath(b.o.Root, "/var/db/skep/api")); err != nil && !b.o.Start {
		b.refuse("api", "first administrator "+b.o.Admin,
			"skep_api has not created its data directory yet; add --start, or start skep_api first")
		return
	}
	b.add("api", "first administrator "+b.o.Admin, "never a password in a flag, a file, or the journal",
		Action{Type: ActAdminCreate, Username: b.o.Admin})
}

// builder accumulates the plan.
type builder struct {
	o        Options
	f        Facts
	journals []Journal
	rc       RCConf
	loader   RCConf
	// rcOK and loaderOK say whether setup may edit the file at all.
	rcOK, loaderOK bool
	plan           Plan
}

func (b *builder) item(it Item) { b.plan.Items = append(b.plan.Items, it) }
func (b *builder) ok(area, what, detail string) {
	b.item(Item{Kind: OK, Area: area, What: what, Detail: detail})
}
func (b *builder) warn(area, what, detail string) {
	b.item(Item{Kind: Warn, Area: area, What: what, Detail: detail})
}
func (b *builder) refuse(area, what, detail string) {
	b.item(Item{Kind: Refuse, Area: area, What: what, Detail: detail})
}
func (b *builder) add(area, what, detail string, a Action) {
	b.item(Item{Kind: Add, Area: area, What: what, Detail: detail, Action: &a})
}

func readLoader(root string) (RCConf, error) {
	var l RCConf
	var err error
	if l.Main, err = ReadShVars(hostPath(root, loaderConfPath)); err != nil {
		return l, err
	}
	local, err := ReadShVars(hostPath(root, "/boot/loader.conf.local"))
	if err != nil {
		return l, err
	}
	if !local.Missing {
		l.Overrides = append(l.Overrides, local)
	}
	entries, err := os.ReadDir(hostPath(root, "/boot/loader.conf.d"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return l, err
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".conf") {
			v, err := ReadShVars(hostPath(root, "/boot/loader.conf.d/"+e.Name()))
			if err != nil {
				return l, err
			}
			l.Overrides = append(l.Overrides, v)
		}
	}
	return l, nil
}

// platform: FreeBSD 15.x amd64 only, kernel and userland at the same
// major.minor; a warning off 15.1 (Decision 7, answer 8).
func (b *builder) platform() { b.item(platformItem(b.f)) }

// platformItem checks the release and architecture.
func platformItem(f Facts) Item {
	km, um := majorMinor(f.KernelRelease), majorMinor(f.UserlandRelease)
	switch {
	case f.Arch != "amd64":
		return Item{Kind: Refuse, Area: "platform", What: "architecture " + f.Arch, Detail: "Skep 0.1 supports amd64 only"}
	case !strings.HasPrefix(km, "15."):
		return Item{Kind: Refuse, Area: "platform", What: "FreeBSD " + f.KernelRelease, Detail: "Skep 0.1 supports FreeBSD 15.x only"}
	case km != um:
		return Item{Kind: Refuse, Area: "platform", What: "kernel " + f.KernelRelease + ", userland " + f.UserlandRelease,
			Detail: "the kernel and userland are at different releases; finish the upgrade and reboot first"}
	case km != qualifiedRel:
		return Item{Kind: Warn, Area: "platform", What: "FreeBSD " + f.KernelRelease,
			Detail: "Skep 0.1 is qualified on " + qualifiedRel + "; other 15.x releases are untested"}
	}
	return Item{Kind: OK, Area: "platform", What: "FreeBSD " + f.KernelRelease + " " + f.Arch}
}

// majorMinor returns "15.1" from "15.1-RELEASE-p7".
func majorMinor(rel string) string {
	v, _, _ := strings.Cut(rel, "-")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

func (b *builder) editability() {
	b.rcOK, b.loaderOK = true, true
	check := func(v ShVars, label string) bool {
		if len(v.Complex) == 0 {
			return true
		}
		b.item(Item{Kind: Manual, Area: label, What: "any edit",
			Detail: fmt.Sprintf("%d line(s) are not plain assignments (first: %q); setup edits nothing in this file and prints the steps instead", len(v.Complex), v.Complex[0])})
		return false
	}
	b.rcOK = check(b.rc.Main, rcConfPath)
	if p := b.rc.ComplexOverride(); p != "" {
		b.item(Item{Kind: Manual, Area: rcConfPath, What: "any edit",
			Detail: strings.TrimPrefix(p, b.o.Root) + " is not plain assignments, so setup cannot tell what it overrides; it edits nothing in rc.conf"})
		b.rcOK = false
	}
	b.loaderOK = check(b.loader.Main, loaderConfPath)
	if p := b.loader.ComplexOverride(); p != "" {
		b.item(Item{Kind: Manual, Area: loaderConfPath, What: "any edit",
			Detail: strings.TrimPrefix(p, b.o.Root) + " is not plain assignments; setup edits nothing in loader.conf"})
		b.loaderOK = false
	}
}

// setVar plans name=want in a file: OK when set so, a conflict when set
// otherwise (never overwritten), an addition when missing.
func (b *builder) setVar(file string, conf RCConf, editable bool, name, want, detail string) {
	if over := conf.OverriddenIn(name); over != "" {
		v, _ := conf.Get(name)
		if strings.EqualFold(v, want) {
			b.ok(file, name, "set in "+strings.TrimPrefix(over, b.o.Root))
		} else {
			b.item(Item{Kind: Conflict, Area: file, What: name,
				Detail: fmt.Sprintf("set to %q in %s; left alone", v, strings.TrimPrefix(over, b.o.Root))})
		}
		return
	}
	v, ok := conf.Main.Vals[name]
	switch {
	case ok && strings.EqualFold(v, want):
		b.ok(file, name, "already "+want)
	case ok:
		b.item(Item{Kind: Conflict, Area: file, What: name,
			Detail: fmt.Sprintf("set to %q by the operator; left alone. %s", v, detail)})
	default:
		a := Action{Type: ActSysrc, File: file, Name: name, Op: "=", Value: want}
		if editable {
			b.add(file, name+`="`+want+`"`, detail, a)
		} else {
			b.item(Item{Kind: Manual, Area: file, What: name + `="` + want + `"`, Detail: detail, Steps: []string{a.String()}})
		}
	}
}

func (b *builder) loaderLines() {
	b.setVar(loaderConfPath, b.loader, b.loaderOK, "vmm_load", "YES",
		"bhyve; takes effect at the next boot (setup never reboots or runs kldload; now: kldload vmm)")
	b.setVar(loaderConfPath, b.loader, b.loaderOK, "nmdm_load", "YES",
		"VM serial consoles; takes effect at the next boot (now: kldload nmdm)")
	if b.o.NoRACCT {
		if v, _ := b.loader.Get("kern.racct.enable"); v != "1" {
			b.warn(loaderConfPath, "kern.racct.enable", "left off (--no-racct): jail limits other than disk_quota are refused without RACCT")
		}
		return
	}
	b.setVar(loaderConfPath, b.loader, b.loaderOK, "kern.racct.enable", "1",
		"RACCT, which Skep's jail limits (RCTL) need; takes effect only after a reboot")
}

func (b *builder) kldList() {
	if over := b.rc.OverriddenIn("kld_list"); over != "" {
		b.item(Item{Kind: Manual, Area: rcConfPath, What: "kld_list",
			Detail: "set in " + strings.TrimPrefix(over, b.o.Root) + "; add the missing modules there yourself"})
		return
	}
	have := strings.Fields(b.rc.Main.Vals["kld_list"])
	var missing []string
	for _, m := range KLDModules {
		if !slices.Contains(have, m) && !slices.Contains(have, m+".ko") {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		b.ok(rcConfPath, "kld_list", "has every module guests need")
		return
	}
	val := strings.Join(missing, " ")
	a := Action{Type: ActSysrc, File: rcConfPath, Name: "kld_list", Op: "+=", Value: val}
	detail := "appended; nothing removed or reordered; loaded at the next boot (now: kldload " + val + ")"
	if b.rcOK {
		b.add(rcConfPath, `kld_list+="`+val+`"`, detail, a)
	} else {
		b.item(Item{Kind: Manual, Area: rcConfPath, What: `kld_list+="` + val + `"`, Detail: detail, Steps: []string{a.String()}})
	}
}

func (b *builder) storage() {
	if len(b.o.Pools) == 0 && len(b.o.DirPools) == 0 {
		b.item(Item{Kind: Manual, Area: "storage", What: "no pool named",
			Detail: "VMs need a ZFS pool (--pool) or a directory pool (--dir-pool); jails need ZFS"})
	}
	for _, p := range b.o.Pools {
		switch h, ok := b.f.Pools[p]; {
		case !ok:
			b.refuse("storage", "pool "+p, "not imported on this host; setup never creates or imports a pool")
		case h != "ONLINE":
			b.refuse("storage", "pool "+p, "health "+h+"; fix the pool first")
		default:
			b.ok("storage", "pool "+p, "ONLINE; Skep works only under "+p+"/skep, which the agent creates on use")
		}
	}
	if len(b.o.Pools) > 0 {
		b.setVar(rcConfPath, b.rc, b.rcOK, "zfs_enable", "YES", "imports the pools at boot")
	}
	for _, d := range b.o.DirPools {
		// Lstat: the root agent writes VM disks there, so the path must
		// be a real directory that only root can change, not a link to
		// one.
		st, err := os.Lstat(hostPath(b.o.Root, d.Path))
		uid, haveUID := uint32(0), false
		if err == nil {
			uid, haveUID = b.o.owner(st)
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			b.item(Item{Kind: Manual, Area: "storage", What: "directory pool " + d.Name,
				Detail: d.Path + " does not exist; create it (root:wheel 0700)",
				Steps:  []string{"install -d -o root -g wheel -m 0700 " + d.Path}})
		case err != nil:
			b.refuse("storage", "directory pool "+d.Name, err.Error())
		case st.Mode()&fs.ModeSymlink != 0:
			b.refuse("storage", "directory pool "+d.Name, d.Path+" is a symbolic link; name the directory itself")
		case !st.IsDir():
			b.refuse("storage", "directory pool "+d.Name, d.Path+" is not a directory")
		case !haveUID || uid != 0:
			b.refuse("storage", "directory pool "+d.Name, fmt.Sprintf("%s is owned by UID %d, not root; the root agent writes there", d.Path, uid))
		case st.Mode().Perm()&0o022 != 0:
			b.refuse("storage", "directory pool "+d.Name, fmt.Sprintf("%s is writable by its group or others (%04o); the root agent writes there", d.Path, st.Mode().Perm()))
		default:
			b.ok("storage", "directory pool "+d.Name, d.Path+" (root, not group or other writable)")
		}
	}
}

// configured reports whether an interface line sets an address: inet, or
// DHCP / SYNCDHCP (then dhcp is true).
func configured(line string) (addr, dhcp bool) {
	for _, w := range strings.Fields(strings.ToUpper(line)) {
		switch w {
		case "INET":
			addr = true
		case "DHCP", "SYNCDHCP":
			dhcp = true
		}
	}
	return addr, dhcp
}

func (b *builder) management() {
	for _, m := range b.o.Mgmt {
		if _, exists := b.f.Interfaces[m]; !exists {
			b.refuse(rcConfPath, "management "+m, "no such interface on this host")
			continue
		}
		line, ok := b.rc.Get("ifconfig_" + m)
		addr, dhcp := configured(line)
		switch {
		case !ok:
			b.refuse(rcConfPath, "management "+m, "has no ifconfig_"+m+" line; setup will not guess which interface carries management")
		case dhcp:
			b.warn(rcConfPath, "management "+m, "configured by DHCP: the listen address setup writes is the current lease's and stops working if the lease changes; use a static address or a DHCP reservation")
		case addr:
			b.ok(rcConfPath, "management "+m, "has an address; setup never changes it")
		default:
			b.refuse(rcConfPath, "management "+m, fmt.Sprintf("ifconfig_%s=%q has neither an address nor DHCP", m, line))
		}
	}
}

// members returns the interfaces an ifconfig_bridgeN line adds.
func members(line string) []string {
	f := strings.Fields(line)
	var out []string
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "addm" {
			out = append(out, f[i+1])
		}
	}
	return out
}

func (b *builder) bridgeDefined(name string) bool {
	_, line := b.rc.Get("ifconfig_" + name)
	cloned, _ := b.rc.Get("cloned_interfaces")
	_, live := b.f.Interfaces[name]
	return line || live || slices.Contains(strings.Fields(cloned), name)
}

func (b *builder) bridges() {
	if len(b.o.Bridges) == 0 && b.o.CreateBridge == "" {
		b.item(Item{Kind: Manual, Area: "network", What: "no guest bridge",
			Detail: "guests can have no NIC until a bridge is allow-listed (--bridge, or --create-bridge with --uplink)"})
	}
	for _, br := range b.o.Bridges {
		if !b.bridgeDefined(br) {
			b.refuse("network", br, "does not exist; to have setup create it, use --create-bridge "+br+" --uplink IF")
			continue
		}
		line, _ := b.rc.Get("ifconfig_" + br)
		m := members(line)
		if bad := slices.IndexFunc(m, func(x string) bool { return slices.Contains(b.o.Mgmt, x) }); bad >= 0 {
			b.refuse("network", br, fmt.Sprintf("has the management interface %s as a member; Skep never puts guests next to management", m[bad]))
			continue
		}
		b.ok("network", br, fmt.Sprintf("exists (members %v); setup allow-lists it and changes nothing on it", m))
		if slices.Contains(strings.Fields(line), "vlanfilter") {
			b.ok("network", br+" vlanfilter", "on")
			continue
		}
		steps := []string{}
		for _, x := range m {
			steps = append(steps, fmt.Sprintf("ifconfig %s ifuntagged %s 1", br, x))
		}
		steps = append(steps, "ifconfig "+br+" defuntagged 1", "ifconfig "+br+" vlanfilter")
		var persist []string
		for _, x := range m {
			persist = append(persist, "ifuntagged "+x+" 1")
		}
		steps = append(steps, fmt.Sprintf(`sysrc ifconfig_%s="%s"`, br,
			strings.TrimSpace(strings.Replace(line, " up", "", 1)+" "+strings.Join(persist, " ")+" defuntagged 1 vlanfilter up")))
		b.item(Item{Kind: Manual, Area: "network", What: br + " vlanfilter",
			Detail: "not set; changing an existing bridge can cut off its members, so setup prints the steps (each member's PVID first, then defuntagged, then vlanfilter); run them from the console. Without it the agent reports bridge_vlan_unfiltered",
			Steps:  steps})
	}
	if b.o.CreateBridge != "" {
		b.newBridge()
	}
}

func (b *builder) newBridge() {
	br, up := b.o.CreateBridge, b.o.Uplink
	bridgeLine := fmt.Sprintf("addm %s ifuntagged %s 1 defuntagged 1 vlanfilter up", up, up)
	if b.bridgeDefined(br) {
		line, _ := b.rc.Get("ifconfig_" + br)
		cloned, _ := b.rc.Get("cloned_interfaces")
		upLine, _ := b.rc.Get("ifconfig_" + up)
		if line == bridgeLine && slices.Contains(strings.Fields(cloned), br) && strings.EqualFold(upLine, "up") {
			b.ok("network", "new bridge "+br, "in rc.conf as setup writes it (uplink "+up+", vlanfilter on)")
			return
		}
		b.refuse("network", br, "exists already; setup never changes an existing bridge (use --bridge "+br+" to allow-list it)")
		return
	}
	if slices.Contains(b.o.Mgmt, up) {
		b.refuse("network", br, "the uplink "+up+" is a management interface")
		return
	}
	addrs, exists := b.f.Interfaces[up]
	if !exists {
		b.refuse("network", br, "the uplink "+up+" does not exist on this host")
		return
	}
	if len(addrs) > 0 {
		b.refuse("network", br, fmt.Sprintf("the uplink %s has an address now (%s); it is in use", up, addrs[0]))
		return
	}
	if line, ok := b.rc.Get("ifconfig_" + up); ok {
		if a, d := configured(line); a || d {
			b.refuse("network", br, fmt.Sprintf("the uplink %s has an address in rc.conf (%q); it is in use", up, line))
			return
		}
		if !strings.EqualFold(strings.TrimSpace(line), "up") {
			b.refuse("network", br, fmt.Sprintf("the uplink %s already has settings (%q); setup will not merge them", up, line))
			return
		}
	}
	for _, v := range []ShVars{b.rc.Main} {
		for k, val := range v.Vals {
			if strings.HasPrefix(k, "ifconfig_"+up+"_") {
				b.refuse("network", br, fmt.Sprintf("the uplink %s has other settings (%s); it is in use", up, k))
				return
			}
			if strings.HasPrefix(k, "ifconfig_bridge") && slices.Contains(members(val), up) {
				b.refuse("network", br, fmt.Sprintf("the uplink %s is already a member of %s", up, strings.TrimPrefix(k, "ifconfig_")))
				return
			}
		}
	}
	for _, o := range b.rc.Overrides {
		for k := range o.Vals {
			if k == "cloned_interfaces" || k == "ifconfig_"+br || strings.HasPrefix(k, "ifconfig_"+up) {
				b.item(Item{Kind: Manual, Area: rcConfPath, What: "new bridge " + br,
					Detail: k + " is set in " + strings.TrimPrefix(o.Path, b.o.Root) + "; make the bridge there yourself"})
				return
			}
		}
	}
	acts := []Action{
		{Type: ActSysrc, File: rcConfPath, Name: "cloned_interfaces", Op: "+=", Value: br},
		{Type: ActSysrc, File: rcConfPath, Name: "ifconfig_" + up, Op: "=", Value: "up"},
		{Type: ActSysrc, File: rcConfPath, Name: "ifconfig_" + br, Op: "=", Value: bridgeLine},
	}
	if _, ok := b.rc.Main.Vals["ifconfig_"+up]; ok {
		acts = append(acts[:1], acts[2]) // already "up"
	}
	now := []string{"ifconfig " + br + " create", "ifconfig " + up + " up",
		fmt.Sprintf("ifconfig %s addm %s ifuntagged %s 1 defuntagged 1", br, up, up), "ifconfig " + br + " vlanfilter up"}
	for _, a := range acts {
		if b.rcOK {
			b.add(rcConfPath, a.Name+a.Op+`"`+a.Value+`"`, "new bridge "+br+" on the spare uplink "+up+", vlanfilter on (ADR 0011); takes effect at the next boot", a)
		} else {
			b.item(Item{Kind: Manual, Area: rcConfPath, What: a.Name + a.Op + `"` + a.Value + `"`, Steps: []string{a.String()}})
		}
	}
	b.item(Item{Kind: Manual, Area: "network", What: br + " now",
		Detail: "setup never runs ifconfig; to have the bridge before the next boot, run these from the console, in order",
		Steps:  now})
}

func (b *builder) account() {
	users, err := ReadAccounts(hostPath(b.o.Root, "/etc/passwd"))
	if err != nil {
		b.refuse("account", skepAccount, "cannot read /etc/passwd: "+err.Error())
		return
	}
	groups, err := ReadAccounts(hostPath(b.o.Root, "/etc/group"))
	if err != nil {
		b.refuse("account", skepAccount, "cannot read /etc/group: "+err.Error())
		return
	}
	u, hasU := findAccount(users, skepAccount)
	g, hasG := findAccount(groups, skepAccount)
	switch {
	case hasU != hasG:
		b.refuse("account", skepAccount, "only one of the skep user and group exists; something else made it, resolve it by hand")
	case !hasU:
		detail := "the skep account is missing; the package's pre-install creates it (UID/GID " + SkepID + "): reinstall the package"
		if o, taken := findAccountID(users, SkepID); taken {
			detail = "the skep account is missing and UID " + SkepID + " belongs to " + o.Name
		} else if o, taken := findAccountID(groups, SkepID); taken {
			detail = "the skep account is missing and GID " + SkepID + " belongs to " + o.Name
		}
		b.refuse("account", skepAccount, detail)
	case (u.Shell != "/usr/sbin/nologin" && u.Shell != "/sbin/nologin") || u.Home != "/nonexistent":
		b.refuse("account", skepAccount, fmt.Sprintf("the skep user has shell %q and home %q; the service account has no login shell and no home, so something else made it", u.Shell, u.Home))
	case u.ID != SkepID || g.ID != SkepID:
		b.warn("account", skepAccount, fmt.Sprintf("exists with UID %s and GID %s, not %s; it works on a single host, a cluster will need %s on every node. Setup changes nothing", u.ID, g.ID, SkepID, SkepID))
	default:
		b.ok("account", skepAccount, "UID and GID "+SkepID+", no login shell, no home")
	}
}

func (b *builder) services() {
	b.setVar(rcConfPath, b.rc, b.rcOK, "skep_agent_enable", "YES", "")
	b.setVar(rcConfPath, b.rc, b.rcOK, "skep_api_enable", "YES", "")
}

// listenAddress returns the API's listen address from the answers or the
// first management interface's current address.
func (b *builder) listenAddress() (string, bool) {
	if b.o.Listen != "" {
		host, _, _ := net.SplitHostPort(b.o.Listen)
		if a, _ := netip.ParseAddr(host); a.IsUnspecified() && !b.o.ListenAll {
			b.refuse("api", "listen "+b.o.Listen, "listens on every interface, including guest and storage networks; give the management address, or --listen-all")
			return "", false
		}
		return b.o.Listen, true
	}
	m := b.o.Mgmt[0]
	addrs := b.f.Interfaces[m]
	if len(addrs) == 0 {
		b.refuse("api", "listen address", m+" has no IPv4 address now; give --listen ADDRESS:PORT")
		return "", false
	}
	return net.JoinHostPort(addrs[0].Addr().String(), defaultPort), true
}

// AgentConfig is the part of agent.json setup writes. Every other key
// keeps its default (packaging/etc/agent.json.sample).
type AgentConfig struct {
	Storage struct {
		Pools       []string        `json:"pools"`
		Directories []agentDirEntry `json:"directories"`
		DatasetRoot string          `json:"dataset_root"`
		MediaDir    string          `json:"media_dir"`
	} `json:"storage"`
	Network struct {
		Bridges              []string `json:"bridges"`
		ManagementInterfaces []string `json:"management_interfaces"`
	} `json:"network"`
}

type agentDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (b *builder) agentJSON() []byte {
	var c AgentConfig
	c.Storage.Pools = append([]string{}, b.o.Pools...)
	c.Storage.Directories = []agentDirEntry{}
	for _, d := range b.o.DirPools {
		c.Storage.Directories = append(c.Storage.Directories, agentDirEntry(d))
	}
	c.Storage.DatasetRoot = "skep"
	c.Storage.MediaDir = mediaDir
	c.Network.Bridges = append([]string{}, b.o.Bridges...)
	if b.o.CreateBridge != "" {
		c.Network.Bridges = append(c.Network.Bridges, b.o.CreateBridge)
	}
	c.Network.ManagementInterfaces = append([]string{}, b.o.Mgmt...)
	out, _ := json.MarshalIndent(c, "", "  ")
	return append(out, '\n')
}

func (b *builder) configFiles() error {
	if err := b.configFile(agentJSONPath, b.agentJSON(), 0o600, "root", "wheel", "the agent's allow-lists", b.agentJSONMatches); err != nil {
		return err
	}
	listen, ok := b.listenAddress()
	if !ok {
		return nil
	}
	b.plan.Listen = listen
	if args, _ := b.rc.Get("skep_api_args"); strings.Contains(args, "-listen") {
		b.warn(rcConfPath, "skep_api_args", fmt.Sprintf("%q sets the listen address on the command line, which wins over api.json", args))
	}
	content, _ := json.MarshalIndent(map[string]string{"listen_address": listen}, "", "  ")
	return b.configFile(apiJSONPath, append(content, '\n'), 0o640, "root", skepAccount, "listen address "+listen,
		func(raw []byte) (string, error) { return apiJSONMatches(raw, listen) })
}

// sameSet reports whether two lists hold the same strings, in any order.
func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// agentJSONMatches compares the allow-lists of an agent.json that setup
// did not write with the answers. It returns "" when they are the same,
// otherwise what differs; an error when the file is not JSON.
func (b *builder) agentJSONMatches(raw []byte) (string, error) {
	var have, want AgentConfig
	if err := json.Unmarshal(raw, &have); err != nil {
		return "", err
	}
	if err := json.Unmarshal(b.agentJSON(), &want); err != nil {
		return "", err
	}
	dirs := func(c AgentConfig) []string {
		var out []string
		for _, d := range c.Storage.Directories {
			out = append(out, d.Name+"="+d.Path)
		}
		return out
	}
	var diff []string
	for _, c := range []struct {
		key        string
		have, want []string
	}{
		{"storage.pools", have.Storage.Pools, want.Storage.Pools},
		{"storage.directories", dirs(have), dirs(want)},
		{"network.bridges", have.Network.Bridges, want.Network.Bridges},
		{"network.management_interfaces", have.Network.ManagementInterfaces, want.Network.ManagementInterfaces},
	} {
		if !sameSet(c.have, c.want) {
			diff = append(diff, fmt.Sprintf("%s is %q, the answers give %q", c.key, c.have, c.want))
		}
	}
	return strings.Join(diff, "; "), nil
}

// apiJSONMatches compares the listen address of an api.json that setup
// did not write with the answers (no listen_address means skep-api's
// default).
func apiJSONMatches(raw []byte, listen string) (string, error) {
	var c struct {
		ListenAddress string `json:"listen_address"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	have := c.ListenAddress
	if have == "" {
		have = config.DefaultAPIListenAddress
	}
	if have == listen {
		return "", nil
	}
	return fmt.Sprintf("listen_address is %q, the answers give %q", have, listen), nil
}

// configFile plans a configuration file setup writes once: created when
// missing, never rewritten. A file setup did not write is the
// administrator's: it is fine when match finds its settings equal to the
// answers, a conflict when they differ, and refused when it cannot be
// parsed.
func (b *builder) configFile(path string, want []byte, mode fs.FileMode, owner, group, detail string, match func([]byte) (string, error)) error {
	full := hostPath(b.o.Root, path)
	raw, err := os.ReadFile(full)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	sum := ""
	if err == nil {
		sum = sha256Hex(raw)
	}
	switch {
	case sum == "":
		b.add("config", path, detail, Action{Type: ActWrite, File: path, Content: want, Mode: mode, Owner: owner, Group: group})
	case sum == sha256Hex(want):
		b.ok("config", path, "as setup would write it")
	case writtenBySetup(b.journals, path, sum):
		b.item(Item{Kind: Conflict, Area: "config", What: path,
			Detail: "written by an earlier apply with other answers; setup writes it once and never rewrites it. Proposed content:\n" + string(want)})
	default:
		diff, err := match(raw)
		switch {
		case err != nil:
			b.refuse("config", path, fmt.Sprintf("the administrator's file, and it cannot be parsed (%v); setup never rewrites it. Fix it, or move it away for setup to write:\n%s", err, want))
		case diff == "":
			b.ok("config", path, "the administrator's file; its settings match the answers, so it is left as it is")
		default:
			b.item(Item{Kind: Conflict, Area: "config", What: path,
				Detail: "the administrator's file: " + diff + "; setup never rewrites it. Proposed content:\n" + string(want)})
		}
	}
	return nil
}

func (b *builder) media() {
	st, err := os.Stat(hostPath(b.o.Root, mediaDir))
	switch {
	case err == nil && st.IsDir():
		b.ok("storage", mediaDir, "exists")
	case err == nil:
		b.refuse("storage", mediaDir, "exists and is not a directory")
	default:
		b.add("storage", mediaDir, "install media; Skep only reads it",
			Action{Type: ActMkdir, File: mediaDir, Mode: 0o700, Owner: "root", Group: "wheel"})
	}
}

func (b *builder) readOnlyChecks() {
	if st, err := os.Stat(hostPath(b.o.Root, firmwareDir)); err != nil || !st.IsDir() {
		b.item(Item{Kind: Manual, Area: "packages", What: "edk2-bhyve",
			Detail: "UEFI firmware for VMs is not installed; jails do not need it", Steps: []string{"pkg install edk2-bhyve"}})
	} else {
		b.ok("packages", "edk2-bhyve", firmwareDir)
	}
	if !baseRepoEnabled(b.o.Root) {
		b.item(Item{Kind: Manual, Area: "packages", What: "FreeBSD-base repository",
			Detail: "disabled; the pkgbase jail template source needs it (or use the dist source)",
			Steps:  []string{`printf 'FreeBSD-base: { enabled: yes }\n' >> /usr/local/etc/pkg/repos/FreeBSD.conf`}})
	} else {
		b.ok("packages", "FreeBSD-base repository", "enabled")
	}
	dm, err := os.ReadFile(hostPath(b.o.Root, dmesgBoot))
	switch {
	case err != nil:
		b.warn("hardware", "virtualization", "cannot read "+dmesgBoot+"; whether bhyve can run is unknown (jails work regardless)")
	case vmxReady(string(dm)):
		b.ok("hardware", "virtualization", "VT-x with EPT and unrestricted guest, or AMD-V with nested paging")
	default:
		b.warn("hardware", "virtualization", "no VT-x with EPT and UG (or AMD-V with RVI) in "+dmesgBoot+"; VMs will be unavailable, jails still work")
	}
}

// vmxReady looks for the CPU features bhyve needs in dmesg.boot.
func vmxReady(dmesg string) bool {
	for _, line := range strings.Split(dmesg, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "VT-x:") && strings.Contains(l, "EPT") && strings.Contains(l, "UG") {
			return true
		}
		if strings.HasPrefix(l, "SVM:") && strings.Contains(l, "NP") {
			return true
		}
	}
	return false
}

var baseRepoRE = regexp.MustCompile(`(?s)FreeBSD-base\s*:\s*\{[^}]*enabled\s*:\s*(yes|true)`)

// baseRepoEnabled reads the pkg repository files (/etc/pkg, then
// /usr/local/etc/pkg/repos, which wins).
func baseRepoEnabled(root string) bool {
	enabled := false
	for _, dir := range []string{"/etc/pkg", "/usr/local/etc/pkg/repos"} {
		entries, _ := os.ReadDir(hostPath(root, dir))
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			bs, err := os.ReadFile(hostPath(root, dir+"/"+e.Name()))
			if err != nil {
				continue
			}
			s := string(bs)
			if baseRepoRE.MatchString(s) {
				enabled = true
			} else if regexp.MustCompile(`FreeBSD-base\s*:`).MatchString(s) {
				enabled = false
			}
		}
	}
	return enabled
}

func (b *builder) guests() {
	sj, sv := b.f.SkepGuests()
	oj, ov := b.f.OtherGuests()
	if len(sj)+len(sv) > 0 {
		b.ok("guests", fmt.Sprintf("%d Skep jail(s), %d Skep VM(s) running", len(sj), len(sv)),
			"apply changes boot-time lines and files only; it never runs ifconfig or kldload")
	}
	if len(oj)+len(ov) > 0 {
		b.warn("guests", "guests not managed by Skep", fmt.Sprintf("jails %v, VMs %v share this host; if they use a bridge Skep allow-lists, filter their ports yourself (ADR 0011)", oj, ov))
	}
}
