package hostops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	vmA   = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"
	diskA = "7e3c1a90-1111-4222-8333-444455556666"
)

// script is a Runner that answers from a table and records every call.
type script struct {
	// answers maps "binary arg arg ..." to an output or an error.
	answers map[string]any
	calls   []string
	// envs holds the environment of each RunEnv call, in order, and
	// envCmds the whole command.
	envs    [][]string
	envCmds []EnvCommand
	// onRun, when set, sees every call before it is answered.
	onRun func(call string)
}

// RunEnv implements EnvRunner: the call is recorded and answered like
// Run, and its environment and output file are kept.
func (s *script) RunEnv(ctx context.Context, c EnvCommand) ([]byte, error) {
	s.envs = append(s.envs, c.Env)
	s.envCmds = append(s.envCmds, c)
	return s.Run(ctx, c.Path, c.Args...)
}

func (s *script) Run(_ context.Context, path string, args ...string) ([]byte, error) {
	call := filepath.Base(path) + " " + strings.Join(args, " ")
	s.calls = append(s.calls, call)
	if s.onRun != nil {
		s.onRun(call)
	}
	v := s.answers[call]
	if seq, ok := v.([]any); ok {
		// A sequence answers the calls in turn; the last answer repeats.
		n := 0
		for _, c := range s.calls[:len(s.calls)-1] {
			if c == call {
				n++
			}
		}
		v = seq[min(n, len(seq)-1)]
	}
	switch v := v.(type) {
	case string:
		return []byte(v), nil
	case error:
		return nil, v
	}
	return nil, nil
}

func noSuchDataset() error {
	return &CommandError{Name: "zfs", ExitCode: 1, Stderr: "cannot open 'x': dataset does not exist", Err: errors.New("exit status 1")}
}

// fakeSystem is the kernel as a test sees it.
type fakeSystem struct {
	oids  map[string]uint64
	paths map[string]bool
	// procs maps a live process ID to its arguments; kills records
	// "pid signal".
	procs map[int][]string
	kills *[]string
	// modes maps a path to what LstatMode returns; lstat, when set,
	// answers instead.
	modes map[string]fs.FileMode
	lstat func(path string) (fs.FileMode, error)
}

func (f fakeSystem) SysctlUint64(name string) (uint64, error) {
	v, ok := f.oids[name]
	if !ok {
		return 0, errNoSuchOID
	}
	return v, nil
}

func (f fakeSystem) SysctlExists(name string) (bool, error) {
	_, ok := f.oids[name]
	return ok, nil
}

func (f fakeSystem) Exists(path string) bool { return f.paths[path] }

// LstatMode answers from modes; a path not in it does not exist.
func (f fakeSystem) LstatMode(path string) (fs.FileMode, error) {
	if f.lstat != nil {
		return f.lstat(path)
	}
	if m, ok := f.modes[path]; ok {
		return m, nil
	}
	return 0, &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrNotExist}
}

func (f fakeSystem) Alive(pid int) bool { _, ok := f.procs[pid]; return ok }

func (f fakeSystem) ProcessArgs(pid int) ([]string, error) {
	args, ok := f.procs[pid]
	if !ok {
		return nil, errors.New("no such process")
	}
	return args, nil
}

func (f fakeSystem) Kill(pid int, sig syscall.Signal) error {
	if f.kills != nil {
		*f.kills = append(*f.kills, fmt.Sprintf("%d %v", pid, sig))
	}
	return nil
}

func testConf(t *testing.T) Confinement {
	t.Helper()
	dir := t.TempDir()
	return Confinement{
		Pools: []string{"skeptest"}, DatasetRoot: "skep",
		MediaDir: filepath.Join(dir, "media"), Bridges: []string{"bridge0"},
		ManagementInterfaces: []string{"vtnet0"}, FirmwareDir: filepath.Join(dir, "firmware"),
		RunDir: filepath.Join(dir, "run"), StateDir: filepath.Join(dir, "state"),
	}
}

func testHost(t *testing.T, run *script, zfsLoaded bool) *host {
	t.Helper()
	sys := fakeSystem{oids: map[string]uint64{}, paths: map[string]bool{}, procs: map[int][]string{}, kills: &[]string{}, modes: map[string]fs.FileMode{}}
	if zfsLoaded {
		sys.oids[sysctlZFSLoaded] = 1
	}
	return newHost(testConf(t), run, sys, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var me *models.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestConfinementDerivesNamesFromIDs(t *testing.T) {
	c := testConf(t)
	ds, err := c.DiskDataset("skeptest", vmA, 0)
	if err != nil || ds != "skeptest/skep/vm/"+vmA+"/disk0" {
		t.Fatalf("disk dataset = %q, %v", ds, err)
	}
	dev, _ := c.DiskDevice("skeptest", vmA, 3)
	if dev != "/dev/zvol/skeptest/skep/vm/"+vmA+"/disk3" {
		t.Errorf("device = %q", dev)
	}
	if name, _ := Instance(vmA); name != "skep-"+vmA {
		t.Errorf("instance = %q", name)
	}

	_, err = c.VMDataset("zroot", vmA)
	wantCode(t, err, models.CodePoolNotAllowed)
	wantCode(t, c.CheckBridge("bridge1"), models.CodeBridgeNotAllowed)
	if c.CheckBridge("bridge0") != nil || c.CheckPool("skeptest") != nil {
		t.Error("an allow-listed name was refused")
	}

	// Nothing that is not an ID becomes part of a name.
	for _, bad := range []string{"", "..", "../../zroot", vmA + "/..", strings.ToUpper(vmA), vmA + " ", "a/b", "skep@snap"} {
		if _, err := c.VMDataset("skeptest", bad); err == nil {
			t.Errorf("VM ID %q was accepted", bad)
		}
		if _, err := c.VMStateDir(bad); err == nil {
			t.Errorf("VM ID %q became a directory", bad)
		}
		if _, err := Instance(bad); err == nil {
			t.Errorf("VM ID %q became an instance name", bad)
		}
	}
	for _, slot := range []int{-1, MaxSlot + 1} {
		if _, err := c.DiskDataset("skeptest", vmA, slot); err == nil {
			t.Errorf("slot %d was accepted", slot)
		}
	}
	for name, want := range map[string]bool{
		"FreeBSD-15.1-RELEASE-amd64-disc1.iso": true, "a.iso": true,
		"": false, ".iso": false, "../x.iso": false, "a/b.iso": false, "x.img": false, "-x.iso": false,
		"x.iso\n": false, strings.Repeat("a", 129) + ".iso": false,
	} {
		if ValidMediaName(name) != want {
			t.Errorf("ValidMediaName(%q) = %v", name, !want)
		}
	}
}

func TestZFSRunsNothingWhenTheModuleIsNotLoaded(t *testing.T) {
	run := &script{}
	h := testHost(t, run, false)
	ctx := context.Background()

	pools, err := h.Pools(ctx)
	if err != nil || len(pools) != 1 || pools[0].Error == "" {
		t.Fatalf("pools = %+v, %v", pools, err)
	}
	_, err = h.Pool(ctx, "skeptest")
	wantCode(t, err, models.CodeStorageUnavailable)
	_, err = h.CreateVMDataset(ctx, "skeptest", vmA)
	wantCode(t, err, models.CodeStorageUnavailable)
	_, err = h.CreateVolume(ctx, VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA, SizeBytes: 1 << 30, BlockSize: 16384})
	wantCode(t, err, models.CodeStorageUnavailable)
	_, err = h.VMDatasets(ctx, "skeptest", vmA)
	wantCode(t, err, models.CodeStorageUnavailable)
	_, err = h.DestroyVM(ctx, "skeptest", vmA)
	wantCode(t, err, models.CodeStorageUnavailable)
	// zfs(8) and zpool(8) load zfs.ko when root runs them.
	if len(run.calls) != 0 {
		t.Errorf("commands ran without the module: %v", run.calls)
	}
}

func TestPoolsAreReadWithFixedArguments(t *testing.T) {
	run := &script{answers: map[string]any{
		"zpool list -H -p -o name,size,allocated,free,health skeptest": "skeptest\t17045651456\t1048576\t17044602880\tONLINE\n",
		"zfs get -H -p -o value available skeptest":                    "16508780544\n",
	}}
	h := testHost(t, run, true)
	p, err := h.Pool(context.Background(), "skeptest")
	if err != nil {
		t.Fatal(err)
	}
	want := Pool{Name: "skeptest", Health: "ONLINE", SizeBytes: 17045651456, AllocatedBytes: 1048576,
		FreeBytes: 17044602880, AvailableBytes: 16508780544}
	if p != want {
		t.Errorf("pool = %+v", p)
	}
	// A pool outside the allow-list is refused before any command.
	before := len(run.calls)
	_, err = h.Pool(context.Background(), "zroot")
	wantCode(t, err, models.CodePoolNotAllowed)
	if len(run.calls) != before {
		t.Error("a command ran for a pool that is not allowed")
	}
	// A pool that is not imported is reported, not fatal.
	run.answers["zpool list -H -p -o name,size,allocated,free,health skeptest"] =
		&CommandError{Name: "zpool", ExitCode: 1, Stderr: "cannot open 'skeptest': no such pool"}
	pools, err := h.Pools(context.Background())
	if err != nil || len(pools) != 1 || pools[0].Error == "" || pools[0].Health != "" {
		t.Errorf("pools = %+v, %v", pools, err)
	}
}

var listCols = datasetColumns

// zline is one line of zfs list -H -p output in datasetColumns order. Any
// column not given prints "-", as zfs prints a property that does not
// apply.
func zline(name, typ string, kv ...string) string {
	cols := strings.Split(datasetColumns, ",")
	vals := map[string]string{"name": name, "type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		vals[kv[i]] = kv[i+1]
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = "-"
		if v, ok := vals[c]; ok {
			out[i] = v
		}
	}
	return strings.Join(out, "\t") + "\n"
}

// vol is a zvol line of a VM's disk.
func vol(name, vmID, diskID string, txg int) string {
	return zline(name, "volume", "volsize", "8589934592", "used", "57344", "referenced", "57344", "refreservation", "0",
		"volmode", "dev", "volblocksize", "16384", "createtxg", fmt.Sprint(txg), "encryption", "off",
		"compression", "on", "compressratio", "1.00", "skep:vm_id", vmID, "skep:disk_id", diskID)
}

// fsys is a filesystem line.
func fsys(name, vmID string, txg int) string {
	return zline(name, "filesystem", "used", "98304", "referenced", "24576", "refreservation", "0",
		"volmode", "default", "createtxg", fmt.Sprint(txg), "encryption", "off", "compression", "on",
		"compressratio", "1.00", "skep:vm_id", vmID)
}

func TestCreateDatasetsAndVolumes(t *testing.T) {
	vmDS := "skeptest/skep/vm/" + vmA
	list := "zfs list -H -p -t filesystem,volume -o " + listCols + " "
	run := &script{answers: map[string]any{
		list + "skeptest/skep":    noSuchDataset(),
		list + "skeptest/skep/vm": noSuchDataset(),
		list + vmDS:               noSuchDataset(),
	}}
	h := testHost(t, run, true)
	ctx := context.Background()

	// The listing after the create is still "does not exist" in this
	// script, which the code reports; the commands are what is checked.
	_, _ = h.CreateVMDataset(ctx, "skeptest", vmA)
	want := []string{
		list + "skeptest/skep", "zfs create -o canmount=off skeptest/skep",
		list + "skeptest/skep/vm", "zfs create -o canmount=off skeptest/skep/vm",
		list + vmDS, "zfs create -o canmount=off -o skep:vm_id=" + vmA + " " + vmDS,
		list + vmDS,
	}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(run.calls, "\n"), strings.Join(want, "\n"))
	}

	run.calls = nil
	run.answers[list+vmDS+"/disk0"] = vol(vmDS+"/disk0", vmA, diskA, 77)
	d, err := h.CreateVolume(ctx, VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA, Slot: 0,
		SizeBytes: 8 << 30, BlockSize: 16384, Thin: true})
	if err != nil {
		t.Fatal(err)
	}
	if firstCreate(run.calls) != "zfs create -s -V 8589934592 -o volblocksize=16384 -o volmode=dev -o skep:vm_id="+vmA+
		" -o skep:disk_id="+diskA+" "+vmDS+"/disk0" {
		t.Errorf("create = %q", firstCreate(run.calls))
	}
	if d.Type != "volume" || d.VolSizeBytes != 8<<30 || d.RefReservationBytes != 0 || d.VolMode != "dev" ||
		d.VolBlockSize != 16384 || d.VMID != vmA || d.DiskID != diskA {
		t.Errorf("dataset = %+v", d)
	}

	// A full reservation has no -s.
	run.calls = nil
	_, _ = h.CreateVolume(ctx, VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA, Slot: 1, SizeBytes: 1 << 30, BlockSize: 16384})
	if strings.Contains(firstCreate(run.calls), " -s ") || !strings.HasSuffix(firstCreate(run.calls), "/disk1") {
		t.Errorf("create = %q", firstCreate(run.calls))
	}

	// Typed values are checked before anything runs.
	run.calls = nil
	for name, spec := range map[string]VolumeSpec{
		"pool":       {Pool: "zroot", VMID: vmA, DiskID: diskA, SizeBytes: 1 << 30, BlockSize: 16384},
		"vm id":      {Pool: "skeptest", VMID: "x; rm -rf /", DiskID: diskA, SizeBytes: 1 << 30, BlockSize: 16384},
		"disk id":    {Pool: "skeptest", VMID: vmA, DiskID: "disk", SizeBytes: 1 << 30, BlockSize: 16384},
		"block size": {Pool: "skeptest", VMID: vmA, DiskID: diskA, SizeBytes: 1 << 30, BlockSize: 12345},
		"big block":  {Pool: "skeptest", VMID: vmA, DiskID: diskA, SizeBytes: 1 << 30, BlockSize: 1 << 20},
		"small":      {Pool: "skeptest", VMID: vmA, DiskID: diskA, SizeBytes: 1 << 20, BlockSize: 16384},
		"unaligned":  {Pool: "skeptest", VMID: vmA, DiskID: diskA, SizeBytes: 1<<30 + 1, BlockSize: 16384},
		"slot":       {Pool: "skeptest", VMID: vmA, DiskID: diskA, Slot: 99, SizeBytes: 1 << 30, BlockSize: 16384},
	} {
		if _, err := h.CreateVolume(ctx, spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(run.calls) != 0 {
		t.Errorf("commands ran for refused volumes: %v", run.calls)
	}

	// The pool filling up is a stable code.
	run.answers["zfs create -V 1073741824 -o volblocksize=16384 -o volmode=dev -o skep:vm_id="+vmA+" -o skep:disk_id="+diskA+" "+vmDS+"/disk2"] =
		&CommandError{Name: "zfs", ExitCode: 1, Stderr: "cannot create '" + vmDS + "/disk2': out of space"}
	_, err = h.CreateVolume(ctx, VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA, Slot: 2, SizeBytes: 1 << 30, BlockSize: 16384})
	wantCode(t, err, models.CodeInsufficientSpace)
}

func TestDestroyVMChecksOwnershipAndNeverUsesBigR(t *testing.T) {
	vmDS := "skeptest/skep/vm/" + vmA
	list := "zfs list -H -p -t filesystem,volume -o " + listCols + " -r " + vmDS
	run := &script{answers: map[string]any{
		list: fsys(vmDS, vmA, 73) + vol(vmDS+"/disk0", vmA, diskA, 77),
	}}
	h := testHost(t, run, true)
	ctx := context.Background()

	destroyed, err := h.DestroyVM(ctx, "skeptest", vmA)
	if err != nil || len(destroyed) != 2 {
		t.Fatalf("destroyed = %+v, %v", destroyed, err)
	}
	if got := run.calls[len(run.calls)-1]; got != "zfs destroy -r "+vmDS {
		t.Errorf("destroy = %q", got)
	}
	for _, c := range run.calls {
		if strings.Contains(c, "-R") || strings.Contains(c, "-f") {
			t.Errorf("destroy used a forbidden flag: %q", c)
		}
	}

	// A dataset that does not say it belongs to this VM is left alone.
	run.calls = nil
	run.answers[list] = fsys(vmDS, "", 73)
	if _, err := h.DestroyVM(ctx, "skeptest", vmA); err == nil {
		t.Error("a dataset without the owner property was destroyed")
	}
	for _, c := range run.calls {
		if strings.HasPrefix(c, "zfs destroy") {
			t.Errorf("destroy ran: %q", c)
		}
	}

	// Nothing there is not an error, and nothing is destroyed.
	run.calls = nil
	run.answers[list] = noSuchDataset()
	destroyed, err = h.DestroyVM(ctx, "skeptest", vmA)
	if err != nil || len(destroyed) != 0 || len(run.calls) != 1 {
		t.Errorf("destroyed = %v, %v, calls %v", destroyed, err, run.calls)
	}
}

func TestCapabilitiesAndBridge(t *testing.T) {
	run := &script{answers: map[string]any{
		"kldstat -q -m nmdm": "",
		"ifconfig bridge0": "bridge0: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500\n" +
			"\tether 58:9c:fc:10:ff:c5\n\tid 00:00:00:00:00:00 priority 32768 hellotime 2 fwddelay 15\n" +
			"\tmember: tap0 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>\n\t        ifmaxaddr 0 port 4 priority 128 path cost 2000000\n" +
			"\tmember: vtnet1 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>\n\tgroups: bridge\n",
	}}
	h := testHost(t, run, true)
	sys := h.sys.(fakeSystem)
	sys.oids[sysctlVMMMaxCPU], sys.oids[sysctlNCPU], sys.oids[sysctlPhysMem] = 256, 4, 6401998848
	sys.paths[bhyveBinary] = true
	sys.paths[filepath.Join(h.conf.FirmwareDir, FirmwareCodeFile)] = true
	sys.paths["/dev/vmm/skep-"+vmA] = true

	info := h.Capabilities(context.Background())
	want := VMMInfo{VMMLoaded: true, BhyvePresent: true, MaxCPU: 256, HostCPUs: 4, HostMemoryBytes: 6401998848,
		NmdmAvailable: true, FirmwareCode: true, FirmwareVars: false}
	if info != want {
		t.Errorf("capabilities = %+v", info)
	}
	if !h.InstanceExists(vmA) || h.InstanceExists("0b0b6f4e-5d0c-4a52-9a43-000000000000") || h.InstanceExists("../x") {
		t.Error("InstanceExists is wrong")
	}

	b, err := h.Bridge(context.Background(), "bridge0")
	if err != nil || !b.Exists || !b.Up || !reflect.DeepEqual(b.Members, []string{"tap0", "vtnet1"}) {
		t.Errorf("bridge = %+v, %v", b, err)
	}
	before := len(run.calls)
	_, err = h.Bridge(context.Background(), "vtnet0")
	wantCode(t, err, models.CodeBridgeNotAllowed)
	if len(run.calls) != before {
		t.Error("ifconfig ran for a bridge that is not allowed")
	}
	run.answers["ifconfig bridge0"] = &CommandError{Name: "ifconfig", ExitCode: 1, Stderr: "ifconfig: interface bridge0 does not exist"}
	if b, err := h.Bridge(context.Background(), "bridge0"); err != nil || b.Exists {
		t.Errorf("missing bridge = %+v, %v", b, err)
	}
}

func TestMediaListingAndResolution(t *testing.T) {
	h := testHost(t, &script{}, true)
	ctx := context.Background()

	// No directory: an empty listing, not an error.
	if list, err := h.Media(ctx); err != nil || len(list) != 0 {
		t.Fatalf("media = %v, %v", list, err)
	}
	dir := h.conf.MediaDir
	if err := os.MkdirAll(filepath.Join(dir, "dir.iso"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	for name, content := range map[string]string{"b.iso": "bb", "a.iso": "a", "notes.txt": "x", ".hidden.iso": "x"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.iso")); err != nil {
		t.Fatal(err)
	}

	list, err := h.Media(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "a.iso" || list[1].Name != "b.iso" || list[1].SizeBytes != 2 {
		t.Errorf("media = %+v", list)
	}
	path, err := h.ResolveMedia(ctx, "a.iso")
	if err != nil || path != filepath.Join(dir, "a.iso") {
		t.Errorf("resolve = %q, %v", path, err)
	}
	for _, bad := range []string{"link.iso", "dir.iso", "missing.iso", "../secret", "notes.txt", "a.iso/../b.iso", ""} {
		_, err := h.ResolveMedia(ctx, bad)
		wantCode(t, err, models.CodeMediaNotFound)
	}
}

func TestPrepareAndRemoveVM(t *testing.T) {
	h := testHost(t, &script{}, true)
	ctx := context.Background()

	// Without the firmware template there is nothing to copy.
	wantCode(t, h.PrepareVM(ctx, vmA), models.CodeBhyveUnavailable)

	if err := os.MkdirAll(h.conf.FirmwareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(h.conf.FirmwareDir, FirmwareVarsFile)
	if err := os.WriteFile(template, []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareVM(ctx, vmA); err != nil {
		t.Fatal(err)
	}
	vars, _ := h.conf.VMVarsPath(vmA)
	info, err := os.Stat(vars)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("variable store: %v, %v", info, err)
	}
	dirInfo, _ := os.Stat(filepath.Dir(vars))
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("state directory mode = %v", dirInfo.Mode().Perm())
	}
	// The VM keeps its variables: a second prepare does not overwrite.
	if err := os.WriteFile(vars, []byte("guest boot entries"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareVM(ctx, vmA); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(vars); string(b) != "guest boot entries" {
		t.Errorf("the variable store was overwritten: %q", b)
	}

	if err := h.RemoveVM(ctx, vmA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(vars)); !os.IsNotExist(err) {
		t.Error("the state directory survived")
	}
	if err := h.RemoveVM(ctx, vmA); err != nil {
		t.Errorf("removing what is gone: %v", err)
	}
	if err := h.RemoveVM(ctx, "../etc"); err == nil {
		t.Error("a path was accepted as a VM ID")
	}
	if _, err := os.Stat(template); err != nil {
		t.Error("the template was touched")
	}
}

// firstCreate is the first zfs create among the calls; the listings that
// check the parent's key come before it.
func firstCreate(calls []string) string {
	for _, c := range calls {
		if strings.HasPrefix(c, "zfs create") {
			return c
		}
	}
	return ""
}

// A failed jail -r's stderr line (amendments p and q): control
// characters, format characters (Cf), and bytes that are not UTF-8
// removed, at most 200 bytes on a character boundary.
func TestCleanLine(t *testing.T) {
	long := strings.Repeat("a", 300)
	accents := strings.Repeat("é", 150) // 300 bytes
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "jail: skep-x: failed", "jail: skep-x: failed"},
		{"escape sequence", "\x1b[2J\x1b[31mjail: red\x1b[0m", "[2J[31mjail: red[0m"},
		{"carriage return", "fake ok\rjail: real error", "fake okjail: real error"},
		{"tab nul del bell", "a\tb\x00c\x7fd\x07e", "abcde"},
		{"C1 as UTF-8", "a\u009b31mb\u0085c", "a31mbc"},
		{"C1 as a raw byte", "a\x9b31mb\xffc", "a31mbc"},
		{"surrounding spaces", "  \x1b jail: x \t", "jail: x"},
		{"over-long", long, long[:200]},
		{"over-long multibyte", accents, strings.Repeat("é", 100)},
		{"cut before a split character", "a" + accents, "a" + strings.Repeat("é", 99)},
		{"exactly 200", long[:200], long[:200]},
		{"empty", "", ""},
		{"only controls", "\x1b\r\x00", ""},
		{"printable unicode kept", "jail: ünïcödé ✓", "jail: ünïcödé ✓"},
		{"right-to-left override", "jail: \u202eko ekaf", "jail: ko ekaf"},
		{"bidi embeddings and overrides", "a\u202ab\u202bc\u202cd\u202de\u202ef", "abcdef"},
		{"bidi isolates", "a\u2066b\u2067c\u2068d\u2069e", "abcde"},
		{"other format characters", "a\u200bb\u200dc\u200ed\u00ade\ufefff\u2060g", "abcdefg"},
		{"format characters around spaces", "\u202e jail: x \u2069", "jail: x"},
		{"only format characters", "\u202e\u2066\ufeff", ""},
		{"cut after format characters removed", strings.Repeat("\u202e", 100) + long, long[:200]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanLine(tc.in, maxStderrLine)
			if got != tc.want {
				t.Fatalf("cleanLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, r := range got {
				if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
					t.Fatalf("cleanLine(%q) kept %U", tc.in, r)
				}
			}
			if len(got) > maxStderrLine || !utf8.ValidString(got) {
				t.Fatalf("cleanLine(%q) = %d bytes, valid %v", tc.in, len(got), utf8.ValidString(got))
			}
		})
	}
}

// Every character of category Cf is removed, not only the bidi ones
// (amendment q).
func TestCleanLineRemovesAllFormatCharacters(t *testing.T) {
	var n int
	for _, rt := range unicode.Cf.R16 {
		for r := rune(rt.Lo); r <= rune(rt.Hi); r += rune(rt.Stride) {
			if got := cleanLine("a"+string(r)+"b", maxStderrLine); got != "ab" {
				t.Errorf("cleanLine kept %U: %q", r, got)
			}
			n++
		}
	}
	for _, rt := range unicode.Cf.R32 {
		for r := rune(rt.Lo); r <= rune(rt.Hi); r += rune(rt.Stride) {
			if got := cleanLine("a"+string(r)+"b", maxStderrLine); got != "ab" {
				t.Errorf("cleanLine kept %U: %q", r, got)
			}
			n++
		}
	}
	if n < 100 {
		t.Fatalf("only %d format characters checked", n)
	}
}

func TestLastLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"only":                  "only",
		"first\nsecond\n":       "second",
		"first\nsecond\n\n \n":  "second",
		"first\r\nsecond\r\n":   "second",
		"\n\n":                  "",
		"rc: a\nrc: b\njail: x": "jail: x",
	} {
		if got := lastLine([]byte(in)); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}
