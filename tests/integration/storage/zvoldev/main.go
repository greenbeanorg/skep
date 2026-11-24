// Command zvoldev drives the agent's real hostops code against real ZFS on
// a FreeBSD lab host to measure the zvol device-node repair of ADR 0010
// amendment z: disk creates, clones, and rollbacks, each followed by
// Disks.EnsureDevice as the VM service calls it, and the refusals.
//
// It is test tooling, run as root on a lab VM, never on a host with Skep
// data in the dataset it uses: the confinement's dataset root is -root
// (default "zvolprobe"), so everything it creates is under <pool>/<root>,
// and it destroys that tree at the end. It needs no vmm, no firmware, and
// no agent; it does not exercise the API, the VM service, or bhyve.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

func main() {
	pool := flag.String("pool", "", "allow-listed ZFS pool (required)")
	root := flag.String("root", "zvolprobe", "dataset root under the pool; destroyed at the end")
	creates := flag.Int("creates", 50, "disk creates")
	clones := flag.Int("clones", 20, "clones of a disk holding a GPT")
	rollbacks := flag.Int("rollbacks", 20, "rollbacks of that disk")
	gpt := flag.String("gpt", "", "a GPT disk image the size of -gpt-size, written to the source disk (required)")
	gptSize := flag.Int64("gpt-size", 64<<20, "size of the source disk, bytes")
	logFile := flag.String("log", "", "write hostops' structured log here (default stderr)")
	flag.Parse()
	if *pool == "" || *gpt == "" {
		flag.Usage()
		os.Exit(2)
	}
	var logOut io.Writer = os.Stderr
	if *logFile != "" {
		f, err := os.Create(*logFile)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		logOut = f
	}
	log := slog.New(slog.NewTextHandler(logOut, nil))
	conf := hostops.Confinement{Pools: []string{*pool}, DatasetRoot: *root}
	ops, err := hostops.New(conf, hostops.Options{Logger: log})
	if err != nil {
		fatal(err)
	}
	r := &run{ctx: context.Background(), ops: ops, pool: *pool, root: *root}
	onFatal = r.cleanup
	defer r.cleanup()

	r.createLoop(*creates)
	src := r.source(*gpt, *gptSize)
	r.cloneLoop(src, *clones)
	r.rollbackLoop(src, *rollbacks)
	r.refusals(src)
}

type run struct {
	ctx  context.Context
	ops  hostops.Ops
	pool string
	root string
	snap string
}

type disk struct {
	vmID, diskID string
}

func (d disk) spec(pool string) hostops.VolumeSpec {
	return hostops.VolumeSpec{Pool: pool, VMID: d.vmID, DiskID: d.diskID, Slot: 0}
}

func stopped(context.Context) error { return nil }

func (r *run) path(d disk) string {
	return fmt.Sprintf("/dev/zvol/%s/%s/vm/%s/disk0", r.pool, r.root, d.vmID)
}

func isNode(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ensure calls EnsureDevice as the VM service does after an operation and
// reports whether the node was there before, the rounds, and the result.
func (r *run) ensure(what string, i int, d disk) (rounds int, ok bool) {
	before := isNode(r.path(d))
	t0 := time.Now()
	res, err := r.ops.Disks().EnsureDevice(r.ctx, d.spec(r.pool), stopped)
	after := isNode(r.path(d))
	fmt.Printf("%s %02d node_before=%v rounds=%d took=%s node_after=%v err=%v\n",
		what, i, yn(before), res.Rounds, time.Since(t0).Round(10*time.Millisecond), yn(after), err)
	return res.Rounds, err == nil && after
}

func yn(b bool) string {
	if b {
		return "Y"
	}
	return "N"
}

func (r *run) newDisk(size int64) disk {
	d := disk{vmID: uuid(), diskID: uuid()}
	if _, err := r.ops.ZFS.CreateVMDataset(r.ctx, r.pool, d.vmID); err != nil {
		fatal(err)
	}
	if _, err := r.ops.Disks().Create(r.ctx, hostops.VolumeSpec{Pool: r.pool, VMID: d.vmID, DiskID: d.diskID, Slot: 0,
		SizeBytes: size, BlockSize: models.DefaultVolBlockSize, Thin: true}); err != nil {
		fatal(err)
	}
	return d
}

func (r *run) createLoop(n int) {
	repaired, bad := 0, 0
	for i := 0; i < n; i++ {
		d := r.newDisk(64 << 20)
		rounds, ok := r.ensure("create", i, d)
		if rounds > 0 {
			repaired++
		}
		if !ok {
			bad++
		}
	}
	fmt.Printf("RESULT creates: %d, repaired %d, without a node at the end %d\n", n, repaired, bad)
}

// source makes the disk the clones and rollbacks start from: a GPT
// written through its node, and a snapshot.
func (r *run) source(image string, size int64) disk {
	d := r.newDisk(size)
	r.ensure("source", 0, d)
	sh("/bin/dd", "if="+image, "of="+r.path(d), "bs=1m")
	fmt.Printf("source %s: image %s zvol %s\n", d.vmID, sha(image), sha(r.path(d)))
	snap := uuid()
	if _, err := r.ops.ZFS.Snapshot(r.ctx, r.pool, hostops.Owner{Kind: hostops.OwnerVM, ID: d.vmID}, snap); err != nil {
		fatal(err)
	}
	r.snap = snap
	r.geomCheck("source")
	return d
}

func (r *run) cloneLoop(src disk, n int) {
	repaired, bad := 0, 0
	for i := 0; i < n; i++ {
		t := disk{vmID: uuid(), diskID: uuid()}
		if _, err := r.ops.ZFS.CloneVM(r.ctx, hostops.CloneSpec{Pool: r.pool, SourceVMID: src.vmID, SnapshotID: r.snap,
			TargetVMID: t.vmID, Disks: map[int]string{0: t.diskID}}); err != nil {
			fatal(err)
		}
		rounds, ok := r.ensure("clone", i, t)
		if rounds > 0 {
			repaired++
		}
		if !ok {
			bad++
		}
	}
	fmt.Printf("RESULT clones: %d, repaired %d, without a node at the end %d\n", n, repaired, bad)
	r.geomCheck("after the clones")
}

func (r *run) rollbackLoop(src disk, n int) {
	repaired, bad := 0, 0
	for i := 0; i < n; i++ {
		if !isNode(r.path(src)) {
			fatal(errors.New("the source has no node before a rollback"))
		}
		sh("/bin/dd", "if=/dev/random", "of="+r.path(src), "bs=1m", "count=1", "oseek=2")
		if _, err := r.ops.ZFS.Rollback(r.ctx, r.pool, hostops.Owner{Kind: hostops.OwnerVM, ID: src.vmID}, r.snap, false); err != nil {
			fatal(err)
		}
		rounds, ok := r.ensure("rollback", i, src)
		if rounds > 0 {
			repaired++
		}
		if !ok {
			bad++
		}
	}
	fmt.Printf("RESULT rollbacks: %d, repaired %d, without a node at the end %d\n", n, repaired, bad)
	r.geomCheck("after the rollbacks")
}

// refusals: a VM that is not stopped, and a disk set to volmode=none by
// hand. Neither may change the zvol.
func (r *run) refusals(src disk) {
	ds := fmt.Sprintf("%s/%s/vm/%s/disk0", r.pool, r.root, src.vmID)

	// A missing node and a VM that is not stopped: find a fresh disk
	// whose node is missing (most are on this release).
	var miss disk
	for i := 0; i < 20; i++ {
		d := r.newDisk(16 << 20)
		time.Sleep(time.Second)
		if !isNode(r.path(d)) {
			miss = d
			break
		}
	}
	if miss.vmID == "" {
		fmt.Println("refusal vm_not_stopped: no disk without a node in 20 creates; not run")
	} else {
		running := func(context.Context) error {
			return &models.Error{Code: models.CodeVMNotStopped, Message: "the virtual machine is running"}
		}
		_, err := r.ops.Disks().EnsureDevice(r.ctx, miss.spec(r.pool), running)
		fmt.Printf("refusal vm_not_stopped: %s; volmode now %s; node %s\n", errJSON(err),
			zget("volmode", fmt.Sprintf("%s/%s/vm/%s/disk0", r.pool, r.root, miss.vmID)), yn(isNode(r.path(miss))))
	}

	sh("/sbin/zfs", "set", "volmode=none", ds)
	_, err := r.ops.Disks().EnsureDevice(r.ctx, src.spec(r.pool), stopped)
	fmt.Printf("refusal volmode_none: %s; volmode now %s; node %s\n", errJSON(err), zget("volmode", ds), yn(isNode(r.path(src))))
	sh("/sbin/zfs", "set", "volmode=dev", ds)
	time.Sleep(time.Second)
	fmt.Printf("after the administrator's command: volmode %s, node %s\n", zget("volmode", ds), yn(isNode(r.path(src))))
}

func (r *run) geomCheck(when string) {
	out, _ := exec.Command("/sbin/sysctl", "-n", "kern.geom.conftxt").Output()
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "zvol") {
			n++
			fmt.Println("  geom:", l)
		}
	}
	p1, _ := exec.Command("/bin/sh", "-c", "ls /dev/zvol/"+r.pool+"/"+r.root+"/vm/*/ 2>/dev/null | grep -c p1").Output()
	fmt.Printf("RESULT geom %s: lines with zvol in kern.geom.conftxt: %d; p1 nodes: %s", when, n, p1)
}

func (r *run) cleanup() {
	ds := r.pool + "/" + r.root
	if err := exec.Command("/sbin/zfs", "destroy", "-r", ds).Run(); err != nil {
		fmt.Printf("cleanup: zfs destroy -r %s: %v\n", ds, err)
		return
	}
	fmt.Printf("cleanup: destroyed %s\n", ds)
}

func errJSON(err error) string {
	var me *models.Error
	if errors.As(err, &me) {
		b, _ := json.Marshal(me)
		return string(b)
	}
	return fmt.Sprint(err)
}

func zget(prop, ds string) string {
	out, err := exec.Command("/sbin/zfs", "get", "-H", "-o", "value", prop, ds).Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

func sha(path string) string {
	out, err := exec.Command("/sbin/sha256", "-q", path).Output()
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

func sh(name string, args ...string) {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		fatal(fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, out))
	}
}

func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// onFatal removes what the run made before fatal exits.
var onFatal = func() {}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "zvoldev:", err)
	onFatal()
	os.Exit(1)
}
